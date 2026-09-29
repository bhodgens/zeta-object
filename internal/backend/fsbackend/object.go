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
	"os"
	"path/filepath"
	"time"

	"mini-s3/internal/backend"
	"mini-s3/internal/objectmodel"
)

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

	bucketPath := filepath.Join(f.root, bucket)
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
	metaPath := sidecarPath(bucketPath, key)

	body, err := io.ReadAll(data)
	if err != nil {
		return objectmodel.Object{}, backend.ToObjectModelError(err)
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
func (f *FS) Get(ctx context.Context, bucket, key string, opts objectmodel.GetOptions) (io.ReadCloser, objectmodel.Object, error) {
	_ = opts
	if err := ctx.Err(); err != nil {
		return nil, objectmodel.Object{}, err
	}
	meta, dataPath, _, err := f.statLocked(ctx, bucket, key)
	if err != nil {
		return nil, objectmodel.Object{}, err
	}

	file, err := os.Open(dataPath) //nolint:gosec // G703: resolved via validateKey-checked key.
	if err != nil {
		if os.IsNotExist(err) {
			return nil, objectmodel.Object{}, objectmodel.ErrNoSuchKey(key)
		}
		return nil, objectmodel.Object{}, backend.ToObjectModelError(err)
	}
	obj := objectFromLegacy(key, meta, meta.ContentLength)
	return file, obj, nil
}

// Stat reads an object's metadata without opening the data file. Serves the
// ACTUAL file size when it differs from the sidecar (pre-seam leaf-2.4
// fix 5 semantics).
func (f *FS) Stat(ctx context.Context, bucket, key string) (objectmodel.Object, error) {
	if err := ctx.Err(); err != nil {
		return objectmodel.Object{}, err
	}
	meta, _, _, err := f.statLocked(ctx, bucket, key)
	if err != nil {
		return objectmodel.Object{}, err
	}
	return objectFromLegacy(key, meta, meta.ContentLength), nil
}

// statLocked is the shared Get/Stat core: bucket check, sidecar read+parse,
// corrupt-storagePath fallback, reader locks (leaf-4.8), data stat. The
// caller's returned object carries the ACTUAL file size.
func (f *FS) statLocked(ctx context.Context, bucket, key string) (legacyMeta, string, string, error) {
	if err := ctx.Err(); err != nil {
		return legacyMeta{}, "", "", err
	}
	if err := validateKey(key); err != nil {
		return legacyMeta{}, "", "", objectmodel.ErrInvalidArgument(err.Error())
	}
	bucketPath := filepath.Join(f.root, bucket)
	if _, err := os.Stat(bucketPath); err != nil {
		if os.IsNotExist(err) {
			return legacyMeta{}, "", "", objectmodel.ErrNoSuchBucket(bucket)
		}
		return legacyMeta{}, "", "", backend.ToObjectModelError(err)
	}

	metaPath := sidecarPath(bucketPath, key)
	metaJSON, err := os.ReadFile(metaPath) //nolint:gosec // G703: key validated.
	if err != nil {
		if os.IsNotExist(err) {
			return legacyMeta{}, "", "", objectmodel.ErrNoSuchKey(key)
		}
		return legacyMeta{}, "", "", backend.ToObjectModelError(err)
	}
	var meta legacyMeta
	if err := json.Unmarshal(metaJSON, &meta); err != nil {
		return legacyMeta{}, "", "", backend.ToObjectModelError(fmt.Errorf("parsing sidecar for %s: %w", key, err))
	}

	dataPath := resolveDataPath(bucketPath, key, &meta)

	// Leaf-4.8 reader locks: hold data-dir + meta-dir locks across
	// stat→open so a concurrent delete's empty-dir prune cannot remove
	// directories out from under the read.
	unlockDataDir := fsLockObject(filepath.Dir(dataPath))
	defer unlockDataDir()
	unlockMetaDir := fsLockObject(filepath.Dir(metaPath))
	defer unlockMetaDir()

	info, err := os.Stat(dataPath)
	if err != nil {
		if os.IsNotExist(err) {
			return legacyMeta{}, "", "", objectmodel.ErrNoSuchKey(key)
		}
		return legacyMeta{}, "", "", backend.ToObjectModelError(err)
	}
	// Serve the ACTUAL size (leaf-2.4 fix 5): a lying sidecar never aborts.
	meta.ContentLength = info.Size()
	return meta, dataPath, metaPath, nil
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
	bucketPath := filepath.Join(f.root, bucket)
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

	// Sidecar-aware actual data path (pre-seam deleteObjectCore).
	actualDataPath := filepath.Join(bucketPath, key)
	if metaJSON, err := os.ReadFile(metaPath); err == nil { //nolint:gosec // G703: key validated.
		var meta legacyMeta
		if jsonErr := json.Unmarshal(metaJSON, &meta); jsonErr == nil && meta.StoragePath != "" {
			actualDataPath = meta.StoragePath
		}
	}

	if err := os.Remove(actualDataPath); err != nil && !os.IsNotExist(err) { //nolint:gosec // G703: sidecar or validated canonical path.
		return backend.ToObjectModelError(err)
	}
	// Sidecar removal never fails the delete: data is already gone
	// (pre-seam deleteObjectCore behavior). Best-effort empty-parent prune
	// (pre-seam cleanupEmptyDirs).
	_ = os.Remove(metaPath)
	cleanupEmptyDirs(filepath.Dir(actualDataPath), bucketPath)
	cleanupEmptyDirs(filepath.Dir(metaPath), filepath.Join(bucketPath, metadataDirName))
	return nil
}

// errors guard: keep errors imported for the reserved errors.Is extension
// point (mapping chained causes is expected during review).
var _ = errors.Is
