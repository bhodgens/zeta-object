// keyvalidate.go — the EXPORTED object-key validator (production API, not
// test surface).
//
// ValidateObjectKey used to live as an alias in export_test_surface.go —
// a file whose own header declares "no production code path depends on
// these names". That had quietly stopped being true: internal/frontend/
// webdav/batch.go passes s3.ValidateObjectKey to HandleBatchForBucket as
// the mounting frontend's ValidateKey, so constraining that file to a
// test-only build would have broken the webdav/h3 batch mount at compile
// time. The symbol now lives here, in a production file, where the
// dependency is visible: the two ?batch mounts (s3 and webdav) govern
// manifest keys with this ONE rule set, so both surfaces reject identical
// keys by construction instead of by alias coincidence.
package s3

// ValidateObjectKey is the shared object-key rule set: the traversal
// gates (no "..", no leading "/", no empty segment escape), the reserved
// control-directory segments (.metadata, .uploads, the shadow !data dir),
// and the length ceiling. Every single-op handler enforces it and so does
// every ?batch manifest key — the pure passthrough, exported because
// mounting frontends own their own dispatch and must hand this one
// validator to the batch bridge.
func ValidateObjectKey(key string) error { return validateObjectKey(key) }
