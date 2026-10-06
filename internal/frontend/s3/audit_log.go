// audit_log.go — the gateway SigV4 request audit log (auth extensions leaf
// 10; charter exception decided 2026-10-02, AGENTS.md): an append-only
// JSONL record of authenticated requests, written by the gateway for
// forensic attribution.
//
// CHARTER DISCIPLINE (binding): WRITER ONLY. Nothing in this package — or
// anywhere else — ever reads the file. The type exposes Append and Close
// and nothing else; there is no read, list, tail, or query path. Treating
// the audit log as queryable server state violates the charter.
//
// Best-effort by contract: a failed audit write logs one WARN and never
// breaks the request. The handle is an os.File opened
// O_APPEND|O_CREATE|O_WRONLY at wiring time (fail-loud at startup on an
// unwritable path); appends are mutex-serialized.
package s3

import (
	"encoding/json"
	"log"
	"net/http"
	"os"
	"sync"
	"time"
)

// auditRecord is one JSONL line: ts RFC3339Nano, principal (AccessKeyID —
// log-safe), method, bucket, key, op (internal/auth Op vocabulary:
// read|write|list|delete|create, plus admin for authenticated management
// requests), status, denied. The key set is the pinned wire contract (e2e
// case 28 jq-asserts it) and is UNCHANGED by the management surface.
type auditRecord struct {
	TS        string `json:"ts"`
	Principal string `json:"principal"`
	Method    string `json:"method"`
	Bucket    string `json:"bucket"`
	Key       string `json:"key"`
	Op        string `json:"op"`
	Status    int    `json:"status"`
	Denied    bool   `json:"denied"`
}

// AuditWriter is the writer-only audit sink. nil (the default, nothing
// installed) = the audit log is disabled.
type AuditWriter struct {
	mu sync.Mutex
	f  *os.File
}

// NewAuditWriter opens (or creates) the append-only audit file at path.
// The caller (package main wiring) treats an error as FATAL at startup —
// fail-loud per the config contract.
func NewAuditWriter(path string) (*AuditWriter, error) {
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o640) //nolint:gosec // G304,G703: path comes from the operator's config.json, not request input.
	if err != nil {
		return nil, err
	}
	return &AuditWriter{f: f}, nil
}

// Append writes one record as a JSONL line. Best-effort: a marshalling or
// write failure logs one WARN (principal is log-safe) and returns — the
// request that produced the record is never affected.
func (a *AuditWriter) Append(rec auditRecord) {
	if a == nil || a.f == nil {
		return
	}
	line, err := json.Marshal(rec)
	if err != nil {
		log.Printf("WARNING: audit log: marshalling record: %v", err)
		return
	}
	line = append(line, '\n')
	a.mu.Lock()
	defer a.mu.Unlock()
	if _, err := a.f.Write(line); err != nil {
		log.Printf("WARNING: audit log: append failed: %v", err)
	}
}

// Close flushes and closes the file (server shutdown).
func (a *AuditWriter) Close() error {
	if a == nil || a.f == nil {
		return nil
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.f.Close()
}

// auditWriterHook is the installed sink (wiring seam, hookMu-guarded like
// every runtime-writable hook in seam.go). nil = disabled.
var auditWriterHook *AuditWriter

// InstallAuditWriter installs the process audit sink (exported wiring
// entry). nil restores the disabled default (tests).
func InstallAuditWriter(w *AuditWriter) {
	hookMu.Lock()
	defer hookMu.Unlock()
	auditWriterHook = w
}

// auditWriterFor returns the installed sink under a read lock (nil =
// disabled — the caller skips entirely).
func auditWriterFor() *AuditWriter {
	hookMu.RLock()
	defer hookMu.RUnlock()
	return auditWriterHook
}

// recordAudit is the single wire point (dispatch.go's serveHTTP, after
// authentication and after the authorization decision). A disabled or nil
// sink costs one nil check.
func recordAudit(principal, method, bucket, key, op string, status int, denied bool) {
	w := auditWriterFor()
	if w == nil {
		return
	}
	w.Append(auditRecord{
		TS:        time.Now().UTC().Format(time.RFC3339Nano),
		Principal: principal,
		Method:    method,
		Bucket:    bucket,
		Key:       key,
		Op:        op,
		Status:    status,
		Denied:    denied,
	})
}

// statusRecorder wraps http.ResponseWriter to capture the handler's
// response status for the audit record (WriteHeader defaulting to 200 via
// the implicit-Write path).
type statusRecorder struct {
	http.ResponseWriter
	status int
}

// Unwrap returns the wrapped http.ResponseWriter so http.ResponseController
// can walk down to the real response and reach its optional capabilities
// (Flush, Hijack, SetReadDeadline, SetWriteDeadline).
//
// The embedded field is an INTERFACE, so nothing is promoted through it: an
// embedded concrete *http.response's Flush would promote onto this struct,
// but an embedded http.ResponseWriter's method set is exactly the interface's
// six methods. Without this explicit Unwrap, the controller's rwUnwrapper
// walk stops here and EVERY capability degrades to ErrNotSupported for every
// authorized s3 request on a server with the audit log configured — the same
// defect commit bf77bef fixed one layer out on the Alt-Svc wrapper
// (altsvc.go's altSvcWriter.Unwrap). The mirror comment lives there.
func (s *statusRecorder) Unwrap() http.ResponseWriter { return s.ResponseWriter }

func (s *statusRecorder) WriteHeader(code int) {
	s.status = code
	s.ResponseWriter.WriteHeader(code)
}

func (s *statusRecorder) Write(b []byte) (int, error) {
	if s.status == 0 {
		s.status = http.StatusOK
	}
	return s.ResponseWriter.Write(b)
}
