// zmetadtagstore.go — the zmetad-backed tagStore (zfs-metadata#13
// consumer, tagging tree follow-up). Implements the tagStore seam over
// the zmetad CLI one-shot tag modes (zmetad(8), DB layout 9):
//
//	<zmetad> --tag-set <dataset> --tag-object <id> --tag k=v ...   (REPLACE)
//	<zmetad> --tag-get <dataset> --tag-object <id>                 (k=v lines)
//	<zmetad> --tag-clear <dataset> --tag-object <id>               (idempotent)
//
// The CLI is object_id-ONLY (non-numeric --tag-object is a usage error),
// so every operation resolves the S3 key -> ZFS object id FIRST, through
// the events table (zmetad_objectid.go: newest row whose full_path /
// old_full_path names the key — rename-stable, REMOVE-exact).
//
// Exec discipline copies zfsdatasets.go with its OWN runner var and its
// OWN semaphore (an independent bound): argv-only exec.CommandContext
// (never a shell string), 10s timeout, stdout+stderr captured, stderr
// rides in wrapped errors (zmetad's wording is never matched).
//
// Failure honesty: a zmetad/database failure fails the tagging request
// (500-class InternalError via writeS3ErrorFrom's default arm) — NEVER a
// silent fallback to the sidecar store for the same request (a fallback
// would fork the tag set across two stores with no merge rule).
package s3

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"github.com/bhodgens/zeta-object/internal/metadata"
	"github.com/bhodgens/zeta-object/internal/objectmodel"
)

// zmetadTagCmdTimeout bounds every tag-mode exec. One-shot CLI modes are
// a single SQLite transaction; 10s is generous (same bound as the purge
// exec, zmetadCmdTimeout).
const zmetadTagCmdTimeout = 10 * time.Second

// zmetadTagExecSem bounds concurrent zmetad tag processes from this seam
// (an INDEPENDENT bound — same discipline as zfsDatasetExecSem): every
// ?tagging request can exec, so an authenticated fan-out must not spawn
// an unbounded number of concurrent processes.
var zmetadTagExecSem = make(chan struct{}, 4)

// zmetadTagRunner is the seam tests replace. Production execs the
// configured zmetad binary: argv-only, context-bounded, stdout+stderr
// captured (stderr rides in wrapped errors).
var zmetadTagRunner = func(ctx context.Context, binary string, args ...string) ([]byte, string, error) {
	cmd := exec.CommandContext(ctx, binary, args...) // #nosec G204 // binary is config-controlled (zmetad_binary); args are argv-only (dataset names derived, object ids numeric, tag pairs validated) — never a shell string
	var stdout, stderr strings.Builder
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	return []byte(stdout.String()), strings.TrimSpace(stderr.String()), err
}

// runZmetadTag applies the timeout + semaphore discipline (copied from
// runZfsDataset): a request that already timed out or was canceled never
// queues behind the bound.
func runZmetadTag(ctx context.Context, binary string, args ...string) ([]byte, string, error) {
	ctx, cancel := context.WithTimeout(ctx, zmetadTagCmdTimeout)
	defer cancel()
	select {
	case zmetadTagExecSem <- struct{}{}:
		defer func() { <-zmetadTagExecSem }()
	case <-ctx.Done():
		return nil, "", ctx.Err()
	}
	return zmetadTagRunner(ctx, binary, args...)
}

// zmetadTagStore implements tagStore over the zmetad tag CLI, scoped to
// ONE dataset-backed bucket. dbPath/binary are the zmetad config values
// (zmetad_db_path / zmetad_binary — the SAME defaults the events
// provider uses); resolveDataset resolves the bucket's ZFS dataset name
// (the deterministic <zfsBucketDatasetParent>/<bucket> derivation, never
// a path resolution), and resolveObjectID turns an S3 key into the
// numeric ZFS object id.
type zmetadTagStore struct {
	binary          string
	dbPath          string
	dataset         string
	resolveObjectID func(ctx context.Context, dataset, key string) (uint64, error)
}

// newZmetadTagStore builds the store for one dataset-backed bucket.
// dbPath/binary come from the installed tag-config seam (zmetad config
// values, defaulted the same way the events provider defaults them).
func newZmetadTagStore(dbPath, binary, dataset string) *zmetadTagStore {
	return &zmetadTagStore{
		binary:          binary,
		dbPath:          dbPath,
		dataset:         dataset,
		resolveObjectID: defaultObjectIDResolver(dbPath),
	}
}

// defaultObjectIDResolver returns the production key -> object_id
// resolver: one read-only DB handle per call (opened/closed per call so
// a reader never pins the WAL against the live daemon — the same shape
// openDB uses). Two distinct failure classes (found live on zfs-meta):
//   - the dataset has NO sync_state row yet (zmetad has not completed a
//     poll — a freshly created bucket dataset): zmetadTagUnavailableError,
//     the honest retryable answer — a 404 here would LIE that the object
//     does not exist;
//   - the dataset IS tracked but no event row names the path:
//     objectmodel.ErrNoSuchKey (permanent — the id is not derivable).
func defaultObjectIDResolver(dbPath string) func(context.Context, string, string) (uint64, error) {
	return func(ctx context.Context, dataset, key string) (uint64, error) {
		db, err := metadata.OpenZmetadDB(ctx, dbPath)
		if err != nil {
			return 0, fmt.Errorf("s3: tag object-id resolution: %w", err)
		}
		defer db.Close() //nolint:errcheck // read-only handle; the query result already carries the error
		polled, err := db.HasDataset(dataset)
		if err != nil {
			return 0, fmt.Errorf("s3: tag object-id resolution: %w", err)
		}
		if !polled {
			return 0, &zmetadTagUnavailableError{cause: fmt.Errorf(
				"dataset %s not polled by zmetad yet", dataset)}
		}
		return db.ResolveObjectID(dataset, key)
	}
}

// zmetadTagUnavailableError marks a zmetad/database/exec failure: the
// store could not answer honestly. Tagging handlers map it to 500-class
// InternalError (writeS3ErrorFrom's default arm) — the request fails,
// it never silently degrades to the sidecar store.
type zmetadTagUnavailableError struct{ cause error }

func (e *zmetadTagUnavailableError) Error() string {
	return "s3: zmetad tag store unavailable: " + e.cause.Error()
}

func (e *zmetadTagUnavailableError) Unwrap() error { return e.cause }

// objectID resolves the key -> object id, wrapping resolution failures
// in the unavailable error (resolution IS the store working — a missing
// row is ObjectIDNotFoundError, which rides through unchanged so the
// Get-missing path can map it to ErrNoSuchKey).
func (s *zmetadTagStore) objectID(ctx context.Context, key string) (uint64, error) {
	id, err := s.resolveObjectID(ctx, s.dataset, key)
	if err != nil {
		if _, notFound := errors.AsType[*metadata.ObjectIDNotFoundError](err); notFound {
			return 0, objectmodel.ErrNoSuchKey(key)
		}
		return 0, &zmetadTagUnavailableError{cause: err}
	}
	return id, nil
}

// dbArgs returns the `-d <dbPath>` argv pair when a db path is configured:
// without it the tag CLI writes/reads /var/lib/zfs/zmetad.db (the compiled
// default), NOT the database the events provider serves — found live on
// zfs-meta (the store silently opened the wrong database). An empty dbPath
// (tests) sends no flag and keeps the PATH default.
func (s *zmetadTagStore) dbArgs() []string {
	if s.dbPath == "" {
		return nil
	}
	return []string{"-d", s.dbPath}
}

// Get returns the object's tags from zmetad. An object with no event row
// (unresolvable id) is objectmodel.ErrNoSuchKey — the SAME taxonomy the
// sidecar store uses for a missing object (the ?tagging handlers already
// map it to NoSuchKey / the NoSuchTagSet empty-tags flow). An object
// that exists but has no tags yields an empty non-nil map (--tag-get
// prints nothing on zero pairs, exit 0).
func (s *zmetadTagStore) Get(key string) (map[string]string, error) {
	if ValidateObjectKey(key) != nil {
		return nil, objectmodel.ErrNoSuchKey(key)
	}
	ctx := context.Background()
	id, err := s.objectID(ctx, key)
	if err != nil {
		return nil, err
	}
	stdout, stderr, err := runZmetadTag(ctx, s.binary,
		append([]string{"--tag-get", s.dataset, "--tag-object", strconv.FormatUint(id, 10)}, s.dbArgs()...)...)
	if err != nil {
		return nil, &zmetadTagUnavailableError{
			cause: fmt.Errorf("zmetad --tag-get %s object %d: %w (stderr: %s)", s.dataset, id, err, stderr),
		}
	}
	return parseZmetadTagGetOutput(stdout), nil
}

// Put REPLACES the object's whole tag set (--tag-set semantics: the
// stored set is deleted and the new set inserted in one transaction —
// never a merge). An empty/nil tags map is S3-legal only through the
// DELETE sub-resource (the handlers never call Put with an empty map
// after objectmodel's >=1 validation, but the store maps it to
// --tag-clear anyway for contract safety: an empty --tag-set is a
// usage error upstream).
func (s *zmetadTagStore) Put(key string, tags map[string]string) error {
	if ValidateObjectKey(key) != nil {
		return objectmodel.ErrNoSuchKey(key)
	}
	if len(tags) == 0 {
		return s.Delete(key)
	}
	ctx := context.Background()
	id, err := s.objectID(ctx, key)
	if err != nil {
		return err
	}
	args := append([]string{"--tag-set", s.dataset, "--tag-object", strconv.FormatUint(id, 10)}, s.dbArgs()...)
	for k, v := range tags {
		args = append(args, "--tag", k+"="+v)
	}
	stdout, stderr, err := runZmetadTag(ctx, s.binary, args...)
	if err != nil {
		// A limit/EINVAL rejection (exit 1) and a DB failure both land
		// here; the stored set is untouched either way (upstream
		// validates the whole set before the transaction opens), so
		// surfacing the failure honestly is correct in both arms.
		return &zmetadTagUnavailableError{
			cause: fmt.Errorf("zmetad --tag-set %s object %d: %w (stderr: %s)", s.dataset, id, err, stderr),
		}
	}
	_ = stdout // success prints a count line; the argv is the contract
	return nil
}

// Delete removes the object's tags (--tag-clear; idempotent upstream).
// A missing object (unresolvable id) is ErrNoSuchKey, matching the
// sidecar store's Delete contract.
func (s *zmetadTagStore) Delete(key string) error {
	if ValidateObjectKey(key) != nil {
		return objectmodel.ErrNoSuchKey(key)
	}
	ctx := context.Background()
	id, err := s.objectID(ctx, key)
	if err != nil {
		return err
	}
	_, stderr, err := runZmetadTag(ctx, s.binary,
		append([]string{"--tag-clear", s.dataset, "--tag-object", strconv.FormatUint(id, 10)}, s.dbArgs()...)...)
	if err != nil {
		return &zmetadTagUnavailableError{
			cause: fmt.Errorf("zmetad --tag-clear %s object %d: %w (stderr: %s)", s.dataset, id, err, stderr),
		}
	}
	return nil
}

// parseZmetadTagGetOutput parses `zmetad --tag-get` output into the tag
// map: one `key=value` line per pair, FIRST '=' splits (a value may
// contain '='), blank lines skipped. Empty output is the empty (non-nil)
// map — upstream exit 0 with zero pairs is an empty result, not an
// error. Duplicate keys cannot occur (PRIMARY KEY (dataset, object_id,
// key)); if a broken binary ever emitted one, last-write-wins matches
// map semantics harmlessly.
func parseZmetadTagGetOutput(stdout []byte) map[string]string {
	tags := map[string]string{}
	for line := range strings.SplitSeq(string(stdout), "\n") {
		line = strings.TrimRight(line, "\r")
		if line == "" {
			continue
		}
		k, v, found := strings.Cut(line, "=")
		if !found {
			continue // a pair without '=' is not a parseable tag; skip, never fabricate
		}
		tags[k] = v
	}
	return tags
}
