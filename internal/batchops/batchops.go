// Package batchops is the protocol-free batch execution core
// (quic-h3-2026-10 leaf 07): one request carries a manifest of explicit
// copy/move/delete operations, the runner executes them SEQUENTIALLY in
// manifest order through an injected op interface, and one result per
// item reports the outcome in manifest order.
//
// Guarantees and honest limits (Contract 9):
//   - NO cross-item atomicity: a mid-manifest failure does not roll back
//     earlier items; later items still execute (partial success). The
//     per-item results are the contract.
//   - A malformed manifest (bad JSON, unknown op, invalid key, over the
//     1000-operation limit) is a request error and NOTHING executes —
//     validation is a pure pass over the parsed manifest before the
//     first op call (pinned by tests asserting the double saw zero
//     calls).
//   - Each item's op call is the SAME code path its single-op equivalent
//     uses (the frontends wire the Executor to their existing
//     get/put/delete orchestration), so versioning capture and other
//     write-path behavior applies per item automatically.
//
// The package imports NO frontend: the op interface (Executor) is the
// only coupling point, and the key validator is injected (the packaged
// DefaultKeyValidator pins the shared segment rules).
package batchops

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/bhodgens/zeta-object/internal/objectmodel"
)

// MaxOperations is the per-request manifest cap (the S3 DeleteObjects
// limit, applied to the JSON batch surface too). More than this is a
// request error; nothing executes.
const MaxOperations = 1000

// Per-item result status values (Contract 9's response shape).
const (
	StatusOK       = "ok"
	StatusError    = "error"
	StatusConflict = "conflict"
)

// Operation is one manifest row: a flat explicit op — no recursive or
// prefix expansion happens server-side (the client expands from its
// index). Copy/move carry From and To; delete carries From only.
type Operation struct {
	Op      string `json:"op"`
	From    string `json:"from"`
	To      string `json:"to,omitempty"`
	IfMatch string `json:"ifMatch,omitempty"`
}

// Manifest is the request body of POST /{bucket}?batch.
type Manifest struct {
	Operations []Operation `json:"operations"`
}

// ItemResult is one per-item outcome in manifest order: ok, or error /
// conflict naming the problem (code per the objectmodel taxonomy).
type ItemResult struct {
	Index   int    `json:"index"`
	Status  string `json:"status"`
	Code    string `json:"code,omitempty"`
	Message string `json:"message,omitempty"`
}

// Response is the 200 body: results in manifest order, one per item.
type Response struct {
	Results []ItemResult `json:"results"`
}

// RequestError marks a malformed-manifest failure: the mounting frontend
// renders it as 400 and NOTHING executed. The message names the problem.
type RequestError struct{ msg string }

func (e *RequestError) Error() string { return e.msg }

func reqErr(format string, args ...any) *RequestError {
	return &RequestError{msg: fmt.Sprintf(format, args...)}
}

// Executor is the op interface the runner drives per item. Implementations
// are the frontends' existing single-op orchestration (get/put/delete
// sequences above the Backend seam) — the same code a single request
// runs, including versioning capture and If-Match enforcement. ifMatch
// is the manifest item's precondition ("" = none) and is the EXECUTOR's
// to enforce.
type Executor interface {
	Copy(ctx context.Context, from, to, ifMatch string) error
	Move(ctx context.Context, from, to, ifMatch string) error
	Delete(ctx context.Context, key, ifMatch string) error
}

// DefaultKeyValidator pins the SAME key rules the frontends' validators
// enforce for single ops (s3 validateObjectKey / fsbackend validateKey):
// no empty key, ≤1024 bytes, no null bytes, and no ".", "..",
// ".metadata" or ".zfs" path segments. Batch keys MUST pass the same
// rules single ops do — this validator is the shared shape, and the
// frontends ALSO run their own per-key validation on Executor calls
// (defense in depth, identical rules).
func DefaultKeyValidator(key string) error {
	if key == "" {
		return errors.New("object key cannot be empty")
	}
	if len(key) > 1024 {
		return errors.New("object key cannot exceed 1024 characters")
	}
	if strings.ContainsRune(key, 0) {
		return errors.New("object key cannot contain null bytes")
	}
	cleaned := filepath.Clean("/" + key)
	if cleaned == "/." || cleaned == "/.." || strings.HasPrefix(cleaned, "/../") {
		return fmt.Errorf("object key cannot contain %q path segments", "..")
	}
	for seg := range strings.SplitSeq(key, "/") {
		switch seg {
		case ".":
			return fmt.Errorf("object key cannot contain %q path segments", ".")
		case "..":
			return fmt.Errorf("object key cannot contain %q path segments", "..")
		case ".metadata":
			return fmt.Errorf("object key cannot contain %q path segments", ".metadata")
		case ".zfs":
			// .zfs is the ZFS control directory at every dataset
			// mountpoint (zfs-bucket-datasets, 2026-10-03): rejected
			// exactly like .metadata.
			return fmt.Errorf("object key cannot contain %q path segments", ".zfs")
		}
	}
	return nil
}

// Runner executes manifests against an injected Executor and key
// validator. The zero value is not useful — construct with both fields.
type Runner struct {
	Exec        Executor
	ValidateKey func(key string) error
}

// Process is the full pipeline for a raw JSON body: parse → validate →
// execute. Any malformed-manifest class returns a *RequestError (the
// frontend renders 400) with the Executor untouched.
func (r *Runner) Process(ctx context.Context, body []byte) (Response, error) {
	var m Manifest
	dec := json.NewDecoder(strings.NewReader(string(body)))
	if err := dec.Decode(&m); err != nil {
		return Response{}, reqErr("malformed manifest: invalid JSON: %v", err)
	}
	return r.RunValidated(ctx, m)
}

// RunValidated validates then executes a parsed manifest. Validation is
// pure and completes BEFORE the first Executor call (the
// 400-before-any-execution rule).
func (r *Runner) RunValidated(ctx context.Context, m Manifest) (Response, error) {
	if err := r.validate(m); err != nil {
		return Response{}, err
	}
	return Response{Results: r.Run(ctx, m)}, nil
}

// validate is the pure gate: structure, ops, keys, limit. It calls
// nothing — the Runner's Executor is not consulted.
func (r *Runner) validate(m Manifest) error {
	if len(m.Operations) == 0 {
		return reqErr("malformed manifest: operations must not be empty")
	}
	if len(m.Operations) > MaxOperations {
		return reqErr("malformed manifest: %d operations exceeds the %d-operation limit", len(m.Operations), MaxOperations)
	}
	for i, op := range m.Operations {
		switch op.Op {
		case "copy", "move":
			if op.From == "" {
				return reqErr("malformed manifest: operation %d (%s): \"from\" is required", i, op.Op)
			}
			if op.To == "" {
				return reqErr("malformed manifest: operation %d (%s): \"to\" is required", i, op.Op)
			}
			if op.From == op.To {
				return reqErr("malformed manifest: operation %d (%s): \"from\" and \"to\" are identical", i, op.Op)
			}
		case "delete":
			if op.From == "" {
				return reqErr("malformed manifest: operation %d (delete): \"from\" is required", i)
			}
		default:
			return reqErr("malformed manifest: operation %d: unknown op %q", i, op.Op)
		}
		if err := r.keyCheck(op.From); err != nil {
			return reqErr("malformed manifest: operation %d: invalid \"from\" key: %v", i, err)
		}
		if op.To != "" {
			if err := r.keyCheck(op.To); err != nil {
				return reqErr("malformed manifest: operation %d: invalid \"to\" key: %v", i, err)
			}
		}
	}
	return nil
}

// keyCheck runs the injected validator (falling back to the packaged
// DefaultKeyValidator when none was supplied).
func (r *Runner) keyCheck(key string) error {
	if r.ValidateKey != nil {
		return r.ValidateKey(key)
	}
	return DefaultKeyValidator(key)
}

// Run executes a PRE-VALIDATED manifest sequentially in manifest order,
// collecting one result per item. An item failure does not stop later
// items and does not roll back earlier ones (partial success — the
// honest, documented contract).
func (r *Runner) Run(ctx context.Context, m Manifest) []ItemResult {
	results := make([]ItemResult, len(m.Operations))
	for i, op := range m.Operations {
		var err error
		switch op.Op {
		case "copy":
			err = r.Exec.Copy(ctx, op.From, op.To, op.IfMatch)
		case "move":
			err = r.Exec.Move(ctx, op.From, op.To, op.IfMatch)
		case "delete":
			err = r.Exec.Delete(ctx, op.From, op.IfMatch)
		default:
			// Unreachable after validate; defensive per-item error.
			err = reqErr("unknown op %q", op.Op)
		}
		results[i] = resultFor(i, err)
	}
	return results
}

// resultFor maps one item's outcome onto the wire result: ok, conflict
// (precondition class), or error with the objectmodel code. A failure
// without objectmodel identity is a real I/O failure → InternalError.
func resultFor(index int, err error) ItemResult {
	if err == nil {
		return ItemResult{Index: index, Status: StatusOK}
	}
	res := ItemResult{Index: index, Status: StatusError, Code: objectmodel.CodeInternalError, Message: err.Error()}
	if oe, ok := errors.AsType[*objectmodel.Error](err); ok {
		res.Code = oe.Code
		res.Message = oe.Message
		if oe.Code == objectmodel.CodePreconditionFailed {
			res.Status = StatusConflict
		}
	}
	return res
}
