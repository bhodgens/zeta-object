// Package fsbackend — object.go: the object-level Backend methods
// (Put/Get/Stat/Delete), extracted from package main's object_handlers.go.
package fsbackend

import (
	"context"
	"crypto/md5" //nolint:gosec // G501: MD5 is the S3 ETag algorithm — protocol requirement, not crypto.
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"syscall"
	"time"

	"github.com/bhodgens/zeta-object/internal/backend"
	"github.com/bhodgens/zeta-object/internal/objectmodel"
)

// bucketPath resolves the on-disk directory for bucket. Normal mode (the
// dataDir-rooted default): <root>/<bucket> — the frozen pre-seam layout.
// Single-bucket mode (custom buckets via NewAt): root IS the bucket
// directory, so the path is root itself — one layout across the seam and
// the handler-side staging math (bughunt D1 split-brain fix).
func (f *FS) bucketPath(bucket string) string {
	if f.singleBucket != "" {
		return f.root
	}
	return filepath.Join(f.root, bucket)
}

// Put stores an object: MD5 ETag, per-object + parent-dir locks, atomic
// data write, then atomic sidecar write. Byte-for-byte the pre-seam
// putObjectHandler write path.
//
// RESIDUAL WINDOW (PINNED, leaf-2.4 fix 1): when the metadata write fails
// the data file is intentionally LEFT IN PLACE — removing it was the
// historical PUT-overwrite data-loss bug. This method preserves that:
// it never removes the data file on a metadata failure.
//
// The data path honors the shadow layout: when the flat path is unusable
// (a path component exists as a file, or the path exists as a directory)
// the data lands under <bucket>/!data/<encoded-key> and the sidecar's
// storagePath records it.
func (f *FS) Put(ctx context.Context, bucket, key string, data io.Reader, size int64, opts objectmodel.PutOptions) (objectmodel.Object, error) {
	if err := ctx.Err(); err != nil {
		return objectmodel.Object{}, err
	}
	if err := validateKey(key); err != nil {
		return objectmodel.Object{}, objectmodel.ErrInvalidArgument(err.Error())
	}

	bucketPath := f.bucketPath(bucket)
	// Bucket existence: the seam has no CreateBucket; a Put into a
	// well-formed bucket name that does not exist yet materializes it
	// (conformance pin). The pre-seam server required the bucket to exist
	// first, but handlers only route here after their own bucket check, so
	// implicit creation changes no served behavior — and every handler
	// caller pre-checks NoSuchBucket before calling through the seam.
	//
	// Shadow-aware data path choice (leaf 5.1 [a]-1) is made INSIDE the
	// locks (layout decisions must not race): objectDataPathFor only stats.
	flatPath := filepath.Join(bucketPath, key)
	unlockKey := fsLockObject(flatPath)
	defer unlockKey()
	unlockParent := fsLockObject(filepath.Dir(flatPath))
	defer unlockParent()
	dataPath := objectDataPathFor(bucketPath, key)
	// Principal breadcrumbs (auth extensions leaf 10): the object's owner
	// is read BEFORE the atomic write replaces the inode (the rename would
	// otherwise destroy the old object's stamps). carryOwner preserves the
	// creator across overwrites — owner is set-once, later writers never
	// change it (design 2a); "" means create (the writer becomes owner).
	carryOwner := ""
	var carryWriters []breadcrumbStamp
	if opts.Principal != "" {
		carryOwner, carryWriters = CollectBreadcrumbNames(dataPath)
	}
	// BUGHUNT B4: when the shadow layout wins, the write target's file and
	// directory differ from the locked flat path. Lock the ACTUAL target
	// (and its parent, <bucket>/!data) too, so a concurrent Put/Delete
	// touching the shadow file serializes with this write. Acquired in the
	// same order everywhere (flat first, shadow second) — no deadlock.
	if dataPath != flatPath {
		unlockShadow := fsLockObject(dataPath)
		defer unlockShadow()
		unlockShadowDir := fsLockObject(filepath.Dir(dataPath))
		defer unlockShadowDir()
	}
	metaPath := sidecarPath(bucketPath, key)

	// BUGHUNT B11: cap the buffered body at maxPutBytes (S3's 5 GiB
	// per-object limit by default). Read through a LimitReader so an
	// oversized body is rejected without buffering past the cap.
	if size >= 0 && size > f.maxPutBytes {
		return objectmodel.Object{}, objectmodel.ErrInvalidArgument(
			fmt.Sprintf("object size %d exceeds the maximum allowed size %d", size, f.maxPutBytes))
	}
	body, err := io.ReadAll(io.LimitReader(data, f.maxPutBytes+1))
	if err != nil {
		return objectmodel.Object{}, backend.ToObjectModelError(err)
	}
	if int64(len(body)) > f.maxPutBytes {
		return objectmodel.Object{}, objectmodel.ErrInvalidArgument(
			fmt.Sprintf("object size exceeds the maximum allowed size %d", f.maxPutBytes))
	}
	if size >= 0 && int64(len(body)) != size {
		return objectmodel.Object{}, objectmodel.ErrInvalidArgument("size mismatch")
	}
	if err := ctx.Err(); err != nil {
		return objectmodel.Object{}, err
	}

	hash := md5.Sum(body) //nolint:gosec // G401: S3 ETags are defined as MD5; protocol requirement, not crypto.
	eTag := hex.EncodeToString(hash[:])

	if err := os.MkdirAll(filepath.Dir(dataPath), 0755); err != nil { //nolint:gosec // G703: key validated by validateKey; no traversal possible.
		return objectmodel.Object{}, backend.ToObjectModelError(err)
	}
	if err := writeFileAtomic(dataPath, body, 0644); err != nil { //nolint:gosec // G703: validated key.
		return objectmodel.Object{}, backend.ToObjectModelError(err)
	}

	// Principal breadcrumbs (leaf 10, best-effort by contract): writer on
	// every mutating write; owner set-once — carried over from the
	// previous object version when one exists (overwrite keeps its
	// creator), else the current writer becomes the owner (create). An
	// xattr failure logs one WARN with the xattr name and NEVER fails the
	// Put.
	stampOwnerCarried(dataPath, opts.Principal, carryOwner)
	carryWriterBreadcrumbs(dataPath, carryWriters)
	stampWriterBreadcrumb(dataPath, opts.Principal, xattrOpPut)

	// Create the metadata directory (pre-seam putObjectHandler does the
	// same MkdirAll before the sidecar write).
	if err := os.MkdirAll(filepath.Dir(metaPath), 0755); err != nil { //nolint:gosec // G703: key validated by validateKey; no traversal possible.
		// RESIDUAL WINDOW: the data file has been written but metadata
		// creation failed — serve/return an error and leave the data in
		// place unindexed (never remove; historical data-loss bug).
		return objectmodel.Object{}, backend.ToObjectModelError(err)
	}

	// Sidecar: legacy on-disk form with x-amz-meta- prefixed custom keys
	// (FROZEN format). opts.Metadata carries canonical (prefix-less) keys;
	// the conversion re-adds the prefix. NOTE: custom key casing normalizes
	// to lower-case through the seam (the pre-seam handlers stored the raw
	// request-header casing). Byte-level casing of mixed-case custom keys
	// is the one documented drift of the flip; values and the key set are
	// identical, and the AWS wire form is lower-case.
	meta := legacyMeta{
		ContentType:    opts.ContentType,
		ContentLength:  int64(len(body)), // actual body length, not the declared size
		ETag:           eTag,
		CustomMetadata: prefixedMetadata(opts.Metadata),
		LastModified:   time.Now().UTC(),
		StoragePath:    dataPath,
	}
	if err := writeFileAtomicJSON(metaPath, meta, 0644); err != nil {
		// RESIDUAL WINDOW: data file STAYS; the previous sidecar (if any)
		// is untouched. Never "improve" this into a rollback.
		return objectmodel.Object{}, backend.ToObjectModelError(err)
	}

	return objectmodel.Object{
		Key:          key,
		Size:         meta.ContentLength,
		ETag:         eTag,
		LastModified: meta.LastModified,
		ContentType:  meta.ContentType,
		Metadata:     canonicalMetadata(meta.CustomMetadata),
	}, nil
}

// Get opens an object for streaming. Semantics of the pre-seam
// getObjectHandler read path: sidecar read → parse → corrupt-storagePath
// fallback to the canonical path → per-dir reader locks → stat (serves the
// ACTUAL file size; a lying sidecar ContentLength is tolerated) → open.
// Missing sidecar or missing data file → NoSuchKey.
//
// opts is accepted but not applied here: preconditions/range evaluation
// stays protocol-side in v1 (the pre-seam handlers evaluate them after
// stat; GetOptions exists for future frontends that push them below the
// seam).
func (f *FS) Get(ctx context.Context, bucket, key string, opts objectmodel.GetOptions) (io.ReadCloser, objectmodel.Object, error) { //nolint:revive // unused-parameter: opts is seam-reserved, see above
	if err := ctx.Err(); err != nil {
		return nil, objectmodel.Object{}, err
	}
	// BUGHUNT B3: stat AND open happen inside statLocked's reader-lock
	// region, so a concurrent Delete's cleanupEmptyDirs cannot remove a
	// directory component between the stat and the open (the old open
	// AFTER the locks released was a TOCTOU that surfaced as a spurious
	// 500 on ENOTDIR-class races).
	meta, file, err := f.statLocked(ctx, bucket, key)
	if err != nil {
		return nil, objectmodel.Object{}, err
	}
	obj := objectFromLegacy(key, meta, meta.ContentLength)
	return file, obj, nil
}

// Stat reads an object's metadata without opening the data file. Serves the
// ACTUAL file size when it differs from the sidecar (pre-seam leaf-2.4
// fix 5 semantics).
//
// statLocked opens the data file inside its reader-lock region (bughunt B3),
// so Stat MUST close the descriptor it discards — otherwise every Stat leaked
// one descriptor, and a caller doing a Stat-per-item operation (a batch
// delete, a List-then-stat sweep) leaked one per item until the process ran
// out. The open stays inside the lock: closing early here would not move the
// open out of statLocked, it would only release the descriptor.
func (f *FS) Stat(ctx context.Context, bucket, key string) (objectmodel.Object, error) {
	if err := ctx.Err(); err != nil {
		return objectmodel.Object{}, err
	}
	meta, file, err := f.statLocked(ctx, bucket, key)
	if err != nil {
		return objectmodel.Object{}, err
	}
	// Best-effort close of a read-only descriptor: a close failure here has
	// no effect on the stat that was already answered, and the only
	// observable effect of ignoring it is a log line.
	if cerr := file.Close(); cerr != nil {
		log.Printf("fsbackend: stat %s/%s: closing data file: %v", bucket, key, cerr)
	}
	return objectFromLegacy(key, meta, meta.ContentLength), nil
}

// statLocked is the shared Get/Stat core: bucket check, sidecar read+parse,
// corrupt-storagePath fallback, reader locks (leaf-4.8), data stat AND data
// open (bughunt B3: the open moved inside the lock region so a concurrent
// delete's empty-dir prune cannot win the race between stat and open). The
// caller's returned object carries the ACTUAL file size; Get additionally
// receives the opened *os.File.
func (f *FS) statLocked(ctx context.Context, bucket, key string) (legacyMeta, *os.File, error) {
	if err := ctx.Err(); err != nil {
		return legacyMeta{}, nil, err
	}
	if err := validateKey(key); err != nil {
		return legacyMeta{}, nil, objectmodel.ErrInvalidArgument(err.Error())
	}
	bucketPath := f.bucketPath(bucket)
	if _, err := os.Stat(bucketPath); err != nil {
		if os.IsNotExist(err) {
			return legacyMeta{}, nil, objectmodel.ErrNoSuchBucket(bucket)
		}
		return legacyMeta{}, nil, backend.ToObjectModelError(err)
	}

	metaPath := sidecarPath(bucketPath, key)
	metaJSON, err := os.ReadFile(metaPath) //nolint:gosec // G703: key validated.
	if err != nil {
		if os.IsNotExist(err) {
			return legacyMeta{}, nil, objectmodel.ErrNoSuchKey(key)
		}
		return legacyMeta{}, nil, backend.ToObjectModelError(err)
	}
	var meta legacyMeta
	if err := json.Unmarshal(metaJSON, &meta); err != nil {
		return legacyMeta{}, nil, backend.ToObjectModelError(fmt.Errorf("parsing sidecar for %s: %w", key, err))
	}

	dataPath := resolveDataPath(bucketPath, key, &meta)

	// Leaf-4.8 reader locks: hold data-dir + meta-dir locks across
	// stat→open so a concurrent delete's empty-dir prune cannot remove
	// directories out from under the read (bughunt B3: BOTH now happen
	// here, inside the region).
	unlockDataDir := fsLockObject(filepath.Dir(dataPath))
	defer unlockDataDir()
	unlockMetaDir := fsLockObject(filepath.Dir(metaPath))
	defer unlockMetaDir()

	info, err := os.Stat(dataPath)
	if err != nil {
		// BUGHUNT B7: ENOTDIR on the data path means a key-prefix file
		// occupies an ancestor directory — for Get/Stat that is the
		// not-found class (NoSuchKey), not an internal error.
		if os.IsNotExist(err) || errors.Is(err, syscall.ENOTDIR) {
			return legacyMeta{}, nil, objectmodel.ErrNoSuchKey(key)
		}
		return legacyMeta{}, nil, backend.ToObjectModelError(err)
	}
	// Serve the ACTUAL size (leaf-2.4 fix 5): a lying sidecar never aborts.
	meta.ContentLength = info.Size()

	// BUGHUNT B3: open INSIDE the lock region. Post-stat ENOTDIR/ENOENT
	// from a racing delete is impossible here (the dir locks are held);
	// any remaining open error maps through B7's not-found rule.
	file, err := os.Open(dataPath) //nolint:gosec // G703: resolved via validateKey-checked key.
	if err != nil {
		if os.IsNotExist(err) || errors.Is(err, syscall.ENOTDIR) {
			return legacyMeta{}, nil, objectmodel.ErrNoSuchKey(key)
		}
		return legacyMeta{}, nil, backend.ToObjectModelError(err)
	}
	return meta, file, nil
}

// Delete removes an object: sidecar-aware data removal (honors
// storagePath with canonical fallback), sidecar removal, empty-dir prune
// — all under per-dir locks. Missing key in an existing bucket is a
// pinned idempotent no-op (nil); missing bucket is NoSuchBucket.
func (f *FS) Delete(ctx context.Context, bucket, key string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := validateKey(key); err != nil {
		return objectmodel.ErrInvalidArgument(err.Error())
	}
	bucketPath := f.bucketPath(bucket)
	if _, err := os.Stat(bucketPath); err != nil {
		if os.IsNotExist(err) {
			return objectmodel.ErrNoSuchBucket(bucket)
		}
		return backend.ToObjectModelError(err)
	}

	metaPath := sidecarPath(bucketPath, key)

	// Leaf-4.8: hold parent-dir locks across delete + prune (closes the
	// check-then-act window against concurrent PUTs' MkdirAll). The key
	// lock matches Put's key lock; the data-parent + metadata-parent dir
	// locks match Put's parent-dir locks so a delete's prune cannot race a
	// concurrent PUT's MkdirAll for the same key prefix.
	unlockKey := fsLockObject(filepath.Join(bucketPath, key))
	defer unlockKey()
	unlockDataDir := fsLockObject(filepath.Dir(filepath.Join(bucketPath, key)))
	defer unlockDataDir()
	unlockMetaDir := fsLockObject(filepath.Dir(metaPath))
	defer unlockMetaDir()

	// Sidecar-aware actual data path (pre-seam deleteObjectCore), with the
	// bughunt B9 containment check: a sidecar storagePath outside the
	// bucket root is ignored in favor of the canonical path — Delete can
	// never be steered outside the bucket by crafted sidecar contents.
	var meta legacyMeta
	if metaJSON, err := os.ReadFile(metaPath); err == nil { //nolint:gosec // G703: key validated.
		// A malformed sidecar degrades to the canonical path (pre-seam
		// behavior); a non-syntax failure is surfaced, never swallowed.
		var syntax *json.SyntaxError
		if err := json.Unmarshal(metaJSON, &meta); err != nil && !errors.As(err, &syntax) {
			fmt.Fprintf(os.Stderr, "fsbackend: delete %s/%s: reading sidecar %s: %v\n", bucketPath, key, metaPath, err)
		}
	}
	actualDataPath := resolveDataPath(bucketPath, key, &meta)

	if err := os.Remove(actualDataPath); err != nil && !os.IsNotExist(err) && !errors.Is(err, syscall.ENOTDIR) { //nolint:gosec // G703: contained sidecar or validated canonical path.
		// BUGHUNT B6: ENOTDIR (a key-prefix file occupies an ancestor of
		// the canonical path) means no data file can exist at this key —
		// S3 semantics make Delete idempotent: 204-class nil, not an error.
		return backend.ToObjectModelError(err)
	}
	// Sidecar removal never fails the delete: data is already gone
	// (pre-seam deleteObjectCore behavior). Best-effort empty-parent prune
	// (pre-seam cleanupEmptyDirs).
	if err := os.Remove(metaPath); err != nil && !os.IsNotExist(err) {
		// Unexpected sidecar-removal failure: the data file is already
		// gone, so the delete still succeeds, but the stale sidecar must
		// not vanish silently — surface it on stderr for operators.
		fmt.Fprintf(os.Stderr, "fsbackend: delete %s/%s: removing sidecar %s: %v\n", bucketPath, key, metaPath, err)
	}
	cleanupEmptyDirs(filepath.Dir(actualDataPath), bucketPath)
	cleanupEmptyDirs(filepath.Dir(metaPath), filepath.Join(bucketPath, metadataDirName))
	return nil
}
