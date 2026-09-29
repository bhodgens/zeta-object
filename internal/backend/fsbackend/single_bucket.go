// Package fsbackend — single_bucket.go: the custom-bucket constructor.
//
// HISTORICAL BUG (bughunt D1, fixed): a custom bucket configured via the
// "buckets" map used to be served by an FS rooted at the custom path, so
// object.go's filepath.Join(root, bucket) nested every object at
// <custom>/<bucket>/key while the handlers' above-seam staging (multipart,
// copy metadata, sweep, actions) addressed the same object at <custom>/key
// — a path split-brain (Put-via-seam landed nested; multipart-complete
// wrote flat; GET then 404'd on the nested path).
//
// The fix: an explicitly-configured custom bucket is served by a
// single-bucket backend whose root IS the bucket directory, so the seam
// path, the handler staging path, and the on-disk path are all exactly
// <custom>/key. The dataDir-rooted default backend keeps the
// join(root, bucket) layout unchanged.
package fsbackend

import "fmt"

// optSingleBucketBucket is the BackendConfig.Options key that pins the
// backend to a single bucket: when set, the FS treats its root as that
// bucket's directory (object paths resolve to root/key, never
// root/<bucket>/key). Set by backend_lookup.go for every explicitly
// configured custom bucket; never set for the dataDir-rooted default.
const optSingleBucketBucket = "single_bucket_bucket"

// OptSingleBucketBucket is the exported name of the fs-backend option that
// switches a constructed FS into single-bucket (custom bucket) mode: the
// root IS the named bucket's directory. Consumed by backend_lookup.go.
const OptSingleBucketBucket = optSingleBucketBucket

// NewAt returns a single-bucket FS backend treating root as the bucket
// directory ITSELF: Put/Get/Stat/Delete/List resolve keys directly under
// root (root/key), not under root/<bucket>/key. This is the custom-bucket
// mode — object data, .metadata sidecars, and the above-seam multipart
// staging all land at the one path the handler-side getBucketPath math
// reports. The bucket argument is carried for error diagnostics only.
func NewAt(root, bucket string) (*FS, error) {
	if root == "" {
		return nil, fmt.Errorf("fsbackend: single-bucket root for bucket %q must not be empty", bucket)
	}
	return &FS{root: root, singleBucket: bucket, maxPutBytes: maxPutBytesDefault}, nil
}
