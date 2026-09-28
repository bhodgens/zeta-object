package main

// object_paths.go — shadow data layout for key/directory path collisions
// (leaf 5.1 [a]-1, found by ceph/s3-tests test_bucket_list_delimiter_basic).
//
// mini-s3 maps object keys onto the filesystem by joining the key into the
// bucket directory. Two legal S3 keys can therefore collide on one path:
// "foo/bar" (a file) and "foo/bar/xyzzy" (which needs "foo/bar" as a parent
// DIRECTORY). Real S3 has a flat namespace and permits both simultaneously;
// mini-s3 returned 500 InternalError on the second PUT.
//
// Fix: when a key's flat data path is unusable (a path component exists as a
// non-directory, or the path exists as a directory), the object's data is
// stored in the bucket's SHADOW DATA DIRECTORY: <bucket>/!data/<encoded-key>.
// The key is percent-encoded (plus "%" itself) so the shadow filename is
// always a flat file that can never collide with another shadow entry.
// ObjectMetadata.StoragePath records the shadow location; every reader
// (GET/HEAD/COPY/DELETE) already resolves the data path through
// resolveObjectDataPath, so shadowed objects are served transparently.
// Listing walks the metadata directory (not the data tree), so shadowed
// objects list normally. Object data under the flat layout is untouched —
// the shadow dir is only used when the flat path cannot be.

import (
	"net/url"
	"os"
	"path/filepath"
	"strings"
)

// shadowDataDirName is the bucket-relative directory holding shadowed object
// data. A "!" prefix keeps it grouped with the other dot-style control dirs
// while never colliding with a legal S3 key segment (.metadata and .uploads
// are already rejected as key segments by validateObjectKey; "!"-prefixed
// keys are legal in S3 but cannot collide because shadow entries are the
// FULL percent-encoded key under this one fixed directory).
const shadowDataDirName = "!data"

// shadowDataPath returns the shadow location for an object key:
// <bucket>/!data/<percent-encoded-key>. The encoding maps every "/" and "%"
// so the result is a single flat filename unique per key.
func shadowDataPath(bucketPath, objectName string) string {
	encoded := url.PathEscape(objectName)
	encoded = strings.ReplaceAll(encoded, "/", "%2F")
	return filepath.Join(bucketPath, shadowDataDirName, encoded)
}

// flatDataPathUnusable reports WHY a key's flat data path cannot be used for
// a new object data file:
//   - some ancestor of the path (inside the bucket) exists as a
//     non-directory — another object's data file occupies the prefix (key
//     "foo/bar/xyzzy" with "foo/bar" stored as a file),
//   - the path itself exists as a directory (reverse case: "a/b/c" stored
//     first, then key "a/b" arrives and its flat path is a directory).
//
// A missing path (or an existing regular file being overwritten) is usable.
// Stat errors OTHER than not-exist are NOT treated as unusable (leaf-4.8
// lesson: under concurrent load transient stat errors must not flip the
// storage layout — shadowing is reserved for proven collisions, so a racing
// rename's transient error keeps the flat path and the pre-existing locking
// guarantees). Ancestors at or above the bucket directory are always
// directories in practice; walking to the filesystem root is harmless.
func flatDataPathUnusable(flatPath string) bool {
	for anc := filepath.Dir(flatPath); ; anc = filepath.Dir(anc) {
		info, err := os.Stat(anc) //nolint:gosec // G703: anc is derived from a validated object data path inside the bucket; validateObjectKey guarantees no traversal.
		if err == nil && !info.IsDir() {
			return true // a FILE occupies an ancestor of the path
		}
		// err != nil: missing or transient — keep walking up.
		if parent := filepath.Dir(anc); parent == anc {
			break // reached the filesystem root
		}
	}
	info, err := os.Stat(flatPath) //nolint:gosec // G703: flatPath is bucketPath joined with a validateObjectKey-approved key; no traversal possible.
	if err != nil {
		return false // missing (or transient stat error) — usable
	}
	return info.IsDir() // target itself: file = plain overwrite (ok), dir = collision
}

// objectDataPathFor chooses the on-disk data location for a key: the flat
// bucket-relative path when usable, otherwise the shadow path. PURE with
// respect to the filesystem (leaf-4.8 lesson: layout decisions must not
// mutate state outside the per-key/parent locks — the caller's existing
// MkdirAll(filepath.Dir(chosenPath)) inside its locked region creates the
// shadow directory when one is chosen).
func objectDataPathFor(bucketPath, objectName string) string {
	flatPath := filepath.Join(bucketPath, objectName)
	if flatDataPathUnusable(flatPath) {
		return shadowDataPath(bucketPath, objectName)
	}
	return flatPath
}
