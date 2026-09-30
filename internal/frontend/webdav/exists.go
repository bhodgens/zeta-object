// exists.go — existence/existence-detection helpers shared by the read and
// write paths. The collection rule is master Contract 3: a prefix p/ is a
// collection iff List(bucket, {Prefix: p, Delimiter: "/", MaxKeys: 1})
// yields a common prefix or ≥1 key under it. No marker objects are read or
// written.
package webdav

import (
	"context"
	"errors"

	"github.com/bhodgens/zeta-object/internal/backend"
	"github.com/bhodgens/zeta-object/internal/objectmodel"
)

// statFile reports whether (bucket, key) names an existing object.
func statFile(ctx context.Context, be backend.Backend, bucket, key string) (objectmodel.Object, bool, error) {
	if key == "" {
		return objectmodel.Object{}, false, nil
	}
	obj, err := be.Stat(ctx, bucket, key)
	if err != nil {
		var oe *objectmodel.Error
		if errors.As(err, &oe) && (oe.Code == objectmodel.CodeNoSuchKey || oe.Code == objectmodel.CodeNoSuchBucket) {
			return objectmodel.Object{}, false, nil
		}
		return objectmodel.Object{}, false, err
	}
	return obj, true, nil
}

// collectionExists reports whether the collection prefix has any listed
// content (a common prefix or a key under the prefix). Bucket-level
// collections (key == "") exist iff the bucket itself exists (List on a
// missing bucket returns NoSuchBucket).
func collectionExists(ctx context.Context, be backend.Backend, bucket, prefix string) (bool, error) {
	page, err := be.List(ctx, bucket, objectmodel.ListParams{
		Prefix:    prefix,
		Delimiter: "/",
		MaxKeys:   1,
	})
	if err != nil {
		var oe *objectmodel.Error
		if errors.As(err, &oe) && oe.Code == objectmodel.CodeNoSuchBucket {
			return false, nil
		}
		return false, err
	}
	return len(page.Objects) > 0 || len(page.CommonPrefixes) > 0, nil
}

// resolveKind classifies a non-root resource as one of file, collection, or
// missing. Precedence: file wins when both a plain key and keys under the
// prefix exist (a plain key "dir" and a prefix "dir/" are distinct —
// master Contract 3).
func (f *Frontend) resolveKind(ctx context.Context, r resource) (objectmodel.Object, string, error) {
	if r.isRoot {
		return objectmodel.Object{}, kindRoot, nil
	}
	obj, isFile, err := statFile(ctx, f.be, r.bucket, r.key)
	if err != nil {
		return objectmodel.Object{}, "", err
	}
	if isFile {
		return obj, kindFile, nil
	}
	// Prefix-existence check ONLY when the client asked for a collection
	// (trailing slash). A slash-less path that exists only as a prefix is
	// a 404 (pinned: clients take hrefs from PROPFIND, which carries the
	// slash; GET /photos/2024 vs /photos/2024/ must not silently agree).
	if !r.isCollection {
		return objectmodel.Object{}, kindMissing, nil
	}
	exists, cerr := collectionExists(ctx, f.be, r.bucket, r.collectionPrefix())
	if cerr != nil {
		return objectmodel.Object{}, "", cerr
	}
	if exists {
		return objectmodel.Object{}, kindCollection, nil
	}
	return objectmodel.Object{}, kindMissing, nil
}

// Resource kinds returned by resolveKind.
const (
	kindFile       = "file"
	kindCollection = "collection"
	kindMissing    = "missing"
	kindRoot       = "root"
)
