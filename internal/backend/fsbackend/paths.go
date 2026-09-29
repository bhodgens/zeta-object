// Package fsbackend — paths.go: on-disk path derivation, extracted from
// package main's object_paths.go (shadow layout, leaf 5.1 [a]-1) and
// object_handlers.go resolveObjectDataPath (leaf 2.4 fix 6). The sidecar
// directory name, the shadow directory name, and the fallback semantics are
// FROZEN — they define the on-disk contract existing buckets depend on.
package fsbackend

import (
	"net/url"
	"os"
	"path/filepath"
	"strings"
)

// metadataDirName is the bucket-relative directory holding per-object
// sidecar JSON files. FROZEN.
const metadataDirName = ".metadata"

// shadowDataDirName is the bucket-relative directory holding shadowed
// object data (key/directory path collisions). FROZEN.
const shadowDataDirName = "!data"

// sidecarPath returns <bucketPath>/.metadata/<key>.meta — the sidecar
// location for key.
func sidecarPath(bucketPath, key string) string {
	return filepath.Join(bucketPath, metadataDirName, key+".meta")
}

// shadowDataPath returns the shadow location for an object key:
// <bucketPath>/!data/<percent-encoded-key>. The encoding maps every "/"
// and "%" so the result is a single flat filename unique per key.
func shadowDataPath(bucketPath, key string) string {
	encoded := url.PathEscape(key)
	encoded = strings.ReplaceAll(encoded, "/", "%2F")
	return filepath.Join(bucketPath, shadowDataDirName, encoded)
}

// flatPathUnusable reports WHY a key's flat data path cannot be used for a
// new object data file (pre-seam flatDataPathUnusable):
//   - some ancestor of the path (inside the bucket) exists as a
//     non-directory — another object's data file occupies the prefix,
//   - the path itself exists as a directory (reverse collision).
//
// A missing path (or an existing regular file being overwritten) is usable.
// Stat errors OTHER than not-exist are NOT treated as unusable: a racing
// rename's transient error keeps the flat path and the pre-existing locking
// guarantees.
func flatPathUnusable(flatPath string) bool {
	for anc := filepath.Dir(flatPath); ; anc = filepath.Dir(anc) {
		info, err := os.Stat(anc) //nolint:gosec // G703: anc is derived from a validated object data path inside the bucket; validateKey guarantees no traversal.
		if err == nil && !info.IsDir() {
			return true // a FILE occupies an ancestor of the path
		}
		if parent := filepath.Dir(anc); parent == anc {
			break // reached the filesystem root
		}
	}
	info, err := os.Stat(flatPath) //nolint:gosec // G703: flatPath is bucketPath joined with a validateKey-approved key.
	if err != nil {
		return false // missing (or transient stat error) — usable
	}
	return info.IsDir() // target itself: file = plain overwrite (ok), dir = collision
}

// objectDataPathFor chooses the on-disk data location for a key: the flat
// bucket-relative path when usable, otherwise the shadow path. PURE with
// respect to the filesystem (leaf-4.8 lesson: layout decisions must not
// mutate state outside the per-key/parent locks).
func objectDataPathFor(bucketPath, key string) string {
	flatPath := filepath.Join(bucketPath, key)
	if flatPathUnusable(flatPath) {
		return shadowDataPath(bucketPath, key)
	}
	return flatPath
}

// resolveDataPath returns the data file path for an object, honoring the
// sidecar's storagePath with a fallback to the canonical location when the
// stored path is empty/corrupt (pre-seam resolveObjectDataPath, leaf-2.4
// fix 6).
//
// CONTAINMENT (bughunt B9 fix): a storagePath is honored only when it stays
// INSIDE the bucket root after Clean. A crafted/corrupt sidecar carrying an
// absolute path or a ../-escaping path is ignored — the canonical layout is
// used instead, so Get/Stat/Delete can never be steered outside the bucket
// by sidecar contents.
func resolveDataPath(bucketPath, key string, meta *legacyMeta) string {
	canonical := filepath.Join(bucketPath, key)
	if meta.StoragePath == "" {
		return canonical
	}
	cleaned := filepath.Clean(meta.StoragePath)
	rel, err := filepath.Rel(bucketPath, cleaned)
	if err != nil {
		return canonical
	}
	if rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || filepath.IsAbs(rel) {
		return canonical // escapes the bucket root: ignore the sidecar path
	}
	return cleaned
}

// validateKey mirrors the pre-seam validateObjectKey rules (object_handlers.go):
// non-empty, <= 1024 chars, no null bytes, no escaping the bucket, and no
// ".." or ".metadata" path segments. Handlers keep first-line validation;
// the backend re-validates defensively per the seam contract.
func validateKey(key string) error {
	if len(key) == 0 {
		return keyInvalidError("object key cannot be empty")
	}
	if len(key) > 1024 {
		return keyInvalidError("object key cannot exceed 1024 characters")
	}
	if strings.ContainsRune(key, 0) {
		return keyInvalidError("object key cannot contain null bytes")
	}
	cleaned := filepath.Clean("/" + key)
	if cleaned == "/.." || strings.HasPrefix(cleaned, "/../") {
		return keyInvalidError("object key cannot escape the bucket directory")
	}
	for seg := range strings.SplitSeq(key, "/") {
		if seg == ".." {
			return keyInvalidError(`object key cannot contain ".." path segments`)
		}
		if seg == ".metadata" {
			return keyInvalidError(`object key cannot contain ".metadata" path segments`)
		}
	}
	// CANONICAL FORM (bughunt B5 fix): reject keys that Clean would collapse
	// ("a//b", "a/./b", "/a", "a/", "a/b/"). filepath.Clean silently folds
	// these onto the same on-disk path, so Put("a//b") and Put("a/b") would
	// silently alias one object. S3 keys are byte-exact; such keys are
	// rejected as InvalidArgument instead of colliding.
	//
	// Exception: the bare key "." is S3-legal (a byte-exact key), but Clean
	// folds "/." to "/" (regression review). It cannot alias another key
	// (no other canonical key equals "/") and the traversal guard above
	// already forbids real escapes, so it is allowed through; resolveDataPath
	// handles it like any other key relative to the bucket dir.
	if cleaned != "/"+key && (key != "." || cleaned != "/") {
		return keyInvalidError(`object key must be in canonical form (no empty or "." path segments, no leading/trailing slash)`)
	}
	return nil
}

// keyInvalidError is validateKey's error type; callers wrap it into an
// objectmodel InvalidArgument.
type keyInvalidError string

func (e keyInvalidError) Error() string { return string(e) }

// cleanupEmptyDirs removes empty directories up to stopAt (pre-seam helper,
// object_handlers.go). Callers hold the parent-dir locks that close its
// check-then-act window.
func cleanupEmptyDirs(dir, stopAt string) {
	for dir != stopAt && dir != "." && dir != "/" {
		entries, err := os.ReadDir(dir)
		if err != nil || len(entries) > 0 {
			break
		}
		if err := os.Remove(dir); err != nil { //nolint:gosec // G703: dir derived from validated object metadata path.
			break
		}
		dir = filepath.Dir(dir)
	}
}
