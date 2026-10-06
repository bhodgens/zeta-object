// export_test_surface.go — EXPORTED ALIASES over the s3 package's
// unexported moved symbols, for the *_test.go files of OTHER packages
// (package main via testshim_test.go, and internal/frontend/webdav's).
// A dedicated cleanup leaf migrates those test files into this package
// and deletes this file.
//
// The header's old claim — "test surface only: no production code path
// depends on these names" — was ONCE true and is no longer: ValidateObjectKey
// was production API (webdav/batch.go passes s3.ValidateObjectKey to
// HandleBatchForBucket). It now lives in keyvalidate.go, production
// build, so the one symbol a non-test caller needed no longer depends on
// this file.
//
// THE FILES STILL COMPARE IN THE PRODUCTION BUILD, and a //go:build test
// constraint on them would break those consumers' test binaries: their
// aliases need these symbols from a dependency's PRODUCTION build (Go
// never compiles a dependency's test files into a consumer's test
// binary). Verified callers, all *_test.go in other packages:
//   - testshim_test.go: ~60 aliases here (SetRegionForTest, HashSHA256,
//     RootHandlerFn, ParseInt, MinPartSize, ...), consumed by
//     main_test.go, main_handler_test.go, config_store*_test.go, ...
//   - export_versioning_test_surface.go: consumed by
//     internal/frontend/webdav's and internal/frontend/h3's *_test.go
//     files (SetupVersioningTestEnv, TestBackend, ListVersionsForTest,
//     PutObjectHandlerForTest, ...).
//
// The pin that would FAIL first is `go test .` (package main) — the
// alias block in testshim_test.go references symbols that would no longer
// exist. SetRegionForTest carries the same explanation in place.
package s3

import (
	"context"
	"io"
	"net/http"
	"os"
	"sync"

	"github.com/bhodgens/zeta-object/internal/metadata"
	"github.com/bhodgens/zeta-object/internal/objectmodel"
)

type httpRequest = http.Request

type httpResponseWriter = http.ResponseWriter

type contextContext = context.Context

type syncMutex = sync.Mutex

type osFileMode = os.FileMode

type ioReader = io.Reader

// ---------- ZFS dataset provisioner (zfs-bucket-datasets leaf 03) ----------

// ZfsDatasetProvisionerInstalled reports whether the leaf-02 dataset
// hooks are installed (feature on). Test-only probe.
func ZfsDatasetProvisionerInstalled() bool {
	return zfsBucketCreate != nil && zfsBucketDestroy != nil && zfsBucketDatasetExists != nil
}

// ZfsBucketDatasetParentProbe exposes the recorded dataset parent
// prefix (empty when the feature is off). Test-only probe.
func ZfsBucketDatasetParentProbe() string { return zfsBucketDatasetParent }

// ---------- Constants (constants.go) ----------

const (
	AWSAlgorithm     = awsAlgorithm
	DefaultRegion    = defaultRegion
	ServiceName      = serviceName
	ISO8601Format    = iso8601Format
	ShortDateFormat  = shortDateFormat
	UnsignedPayload  = unsignedPayload
	StreamingPayload = streamingPayload
	S3XMLNamespace   = s3XMLNamespace
)

// ---------- SigV4 helpers (sigv4.go) ----------

func HashSHA256(data []byte) string { return hashSHA256(data) }

func CanonicalQueryEscape(s string) string { return canonicalQueryEscape(s) }

func HmacSHA256(key []byte, data string) []byte { return hmacSHA256(key, data) }

func GetSigningKey(secretKey, dateStamp, region, serviceName string) []byte {
	return getSigningKey(secretKey, dateStamp, region, serviceName)
}

func GetCanonicalURI(r *httpRequest) string { return getCanonicalURI(r) }

func GetCanonicalQueryString(r *httpRequest) string { return getCanonicalQueryString(r) }

func GetCanonicalHeaders(r *httpRequest, signedHeaderNames []string) (string, string) {
	return getCanonicalHeaders(r, signedHeaderNames)
}

func GetPayloadHash(r *httpRequest) (string, []byte, error) { return getPayloadHash(r) }

func IsLowercaseHex64(s string) bool { return isLowercaseHex64(s) }

func DebugAuthEnabled() bool { return debugAuthEnabled() }

func IsPresignedRequest(r *httpRequest) bool { return isPresignedRequest(r) }

func IsDecodedStreaming(ctx contextContext) bool { return isDecodedStreaming(ctx) }

func WithDecodedStreaming(ctx contextContext) contextContext {
	return withDecodedStreaming(ctx)
}

func DecodeAWSChunked(body []byte) ([]byte, error) { return decodeAWSChunked(body) }

func DecodeAndVerifyChunked(body []byte, seedSignature string, signingKey []byte, timestamp, scope string) ([]byte, error) {
	return decodeAndVerifyChunked(body, seedSignature, signingKey, timestamp, scope)
}

// ---------- XML / errors (xml.go) ----------

func ErrorToXML(code, message string) string { return errorToXML(code, message) }

func WriteXML(w httpResponseWriter, status int, v any) { writeXML(w, status, v) }

func WriteS3Error(w httpResponseWriter, code, message string, statusCode int) {
	writeS3Error(w, code, message, statusCode)
}

func HandleACL(w httpResponseWriter, r *httpRequest, bucketName, objectName string) {
	handleACL(w, r, bucketName, objectName)
}

// ---------- Region config (region.go, region-config-2026-10) ----------

// SetRegionForTest exposes the startup-only region setter to test shims
// (region-config-2026-10 leaf 02). Not production surface.
//
// WHY THIS CANNOT LEAVE THE PRODUCTION BUILD YET (a //go:build test
// constraint on this file would break package main's test binary):
// every caller is a _test.go file in ANOTHER package — testshim_test.go
// (`SetRegionForTest = s3.SetRegionForTest`), aliased from
// config_store_test.go, config_store_state_test.go and main_test.go. A
// dependency's test files are never compiled into a consumer's test
// binary, so those files need this symbol in package s3's PRODUCTION
// build. The same is true of every other alias here, and of all of
// export_versioning_test_surface.go (internal/frontend/webdav's and
// internal/frontend/h3's *_test.go files consume it). The migration that
// unblocks the constraint is the one this file's header already names:
// move the package-main and webdav test files INTO this package and
// delete both export files. Until then the aliases are load-bearing, and
// ValidateObjectKey — the one symbol production code genuinely calls —
// now lives in keyvalidate.go, outside this file, so nothing here is
// required to keep the webdav batch mount compiling.
//
// The unlocked global this writes is region.go's; whoever makes it atomic
// must keep SetRegion as the single writer (region-config-2026-10).
func SetRegionForTest(r string) { SetRegion(r) }

// ---------- Auth adapters (auth_adapter.go) ----------

// AuthenticateRequestFn adapts the Frontend method to the pre-move
// package-level shape (writes the error response; returns ok).
func AuthenticateRequestFn(w httpResponseWriter, r *httpRequest) bool {
	f := &Frontend{}
	_, failure, ok := f.authenticateRequest(r)
	if !ok && failure != nil {
		writeAuthFailure(w, *failure)
	}
	return ok
}

// AuthenticatePresignedFn is the pre-move package-level presigned entry.
func AuthenticatePresignedFn(w httpResponseWriter, r *httpRequest) bool {
	f := &Frontend{}
	return f.authenticatePresigned(w, r)
}

// ---------- Validation / small helpers (object_handlers.go,
// bucket_handlers.go, multipart_handlers.go) ----------

func ParseInt(valueStr, paramName string) (int, error) { return parseInt(valueStr, paramName) }

// ValidateObjectKey is NOT here: it is PRODUCTION API (webdav/batch.go
// passes s3.ValidateObjectKey to HandleBatchForBucket as the mounting
// frontend's key validator) and lives in keyvalidate.go, so constraining
// this file to a test-only build can never break the webdav batch mount.

func ValidateBucketName(name string) error { return validateBucketName(name) }

func BucketExists(bucketName string) bool { return bucketExists(bucketName) }

func ValidBucket(name string) bool { return validBucket(name) }

func CleanupEmptyDirs(dir, stopAt string) { cleanupEmptyDirs(dir, stopAt) }

func ParseRangeHeader(spec string, size int64) rangeRequest {
	return parseRangeHeader(spec, size)
}

func ParseCopySource(copySource string) (string, string, bool) {
	return parseCopySource(copySource)
}

// ---------- Listing internals (object_handlers.go / object_meta_batch.go) ----------

// ListObjectsParams is the exported alias of the params struct.
type ListObjectsParams = listObjectsParams

func ListObjectsFromKeys(allObjectKeys []string, p ListObjectsParams, bucketName, metadataDir string) (bool, string, []Object, []string, string) {
	return listObjectsFromKeys(allObjectKeys, p, bucketName, metadataDir)
}

func CollectObjectKeys(metadataDir string) ([]string, error) {
	return collectObjectKeys(metadataDir)
}

func ReadMetasBatch(paths []string, workers int) []metaReadResult {
	return readMetasBatch(paths, workers)
}

// ---------- Storage / locks / multipart (seam.go, multipart_handlers.go) ----------

func LockObject(path string) func() { return lockObject(path) }

func GetMultipartLock(path string) *syncMutex { return getMultipartLock(path) }

func WriteFileAtomicShim(path string, data []byte, perm osFileMode) error {
	return writeFileAtomic(path, data, perm)
}

func WriteFileAtomicJSONShim(path string, v any, perm osFileMode) error {
	return writeFileAtomicJSON(path, v, perm)
}

func GetBucketPathShim(bucket string) string { return getBucketPath(bucket) }

func SweepExpiredUploads(bucketPath string) int { return sweepExpiredUploads(bucketPath) }

// ---------- Range/conditional helpers ----------

// RangeOutcome is the exported alias of rangeOutcome.
type RangeOutcome = rangeOutcome

// RangeRequest is the exported alias of rangeRequest.
type RangeRequest = rangeRequest

// Exported range outcome constants.
const (
	RangeFull          = rangeFull
	RangePartial       = rangePartial
	RangeUnsatisfiable = rangeUnsatisfiable
)

func EtagMatches(headerValue, etag string) bool { return etagMatches(headerValue, etag) }

func EvaluatePreconditions(r *httpRequest, etag string, lastModified timeTime) (int, bool) {
	return evaluatePreconditions(r, etag, lastModified)
}

func CheckObjectPreconditions(w httpResponseWriter, r *httpRequest, etag string, lastModified timeTime) bool {
	return checkObjectPreconditions(w, r, etag, lastModified)
}

func ServeObjectRange(w httpResponseWriter, file *os.File, rr RangeRequest, actualSize int64, isHead bool, logPrefix string) bool {
	return serveObjectRange(w, file, rr, actualSize, isHead, logPrefix)
}

func ServeObjectRangeFrom(ctx contextContext, w httpResponseWriter, rc ioReader, rr RangeRequest, actualSize int64, isHead bool, logPrefix string) bool {
	return serveObjectRangeFrom(ctx, w, rc, rr, actualSize, isHead, logPrefix)
}

func S3URLEncode(key string) string { return s3URLEncode(key) }

// ---------- Copy / batch-delete helpers ----------

func BuildCopyMetadata(srcMeta *ObjectMetadata, r *httpRequest, data []byte, eTag, dstDataPath string) ObjectMetadata {
	return buildCopyMetadata(srcMeta, r, data, eTag, dstDataPath)
}

func DeleteObjectCore(bucketPath, bucketName, objectName string) error {
	return deleteObjectCore(bucketPath, bucketName, objectName)
}

func ResolveObjectDataPath(bucketPath, objectName string, meta *ObjectMetadata) string {
	return resolveObjectDataPath(bucketPath, objectName, meta)
}

// ---------- Object paths (object_paths.go) ----------

func ObjectDataPathFor(bucketPath, objectName string) string {
	return objectDataPathFor(bucketPath, objectName)
}

func ShadowDataPath(bucketPath, objectName string) string {
	return shadowDataPath(bucketPath, objectName)
}

func FlatDataPathUnusable(flatPath string) bool { return flatDataPathUnusable(flatPath) }

// ---------- Bucket versions (bucket_versions.go) ----------

func ReadObjectMetaForList(metadataDir, key string) (ObjectMetadata, bool) {
	return readObjectMetaForList(metadataDir, key)
}

// NullVersionID is the exported null version ID constant.
const NullVersionID = nullVersionID

// ---------- Metadata headers (object_handlers.go) ----------

func RawMetaHeaders(r *httpRequest) (names, values []string) { return rawMetaHeaders(r) }

func RawSourceSidecarMeta(bucketPath, key string) map[string]string {
	return rawSourceSidecarMeta(bucketPath, key)
}

// ---------- Multipart (multipart_handlers.go) ----------

// UploadIDRegex is the exported alias of uploadIDRegex.
var UploadIDRegex = uploadIDRegex

// RequiredPresignedParams is the exported alias of requiredPresignedParams.
var RequiredPresignedParams = requiredPresignedParams

func ValidateUploadID(id string) (string, error) { return validateUploadID(id) }

// MinPartSize is the exported minimum part size constant.
const MinPartSize = minPartSize

// MultipartUploadExpiry is the exported session expiry constant.
const MultipartUploadExpiry = multipartUploadExpiry

func S3Timestamp(t timeTime) string { return s3Timestamp(t) }

func ComputeMultipartETag(partETags []string) string { return computeMultipartETag(partETags) }

func AssembleCompletedObject(w httpResponseWriter, r *httpRequest, bucketPath, objectName, uploadID string, mpUpload MultipartUpload, completeRequest CompleteMultipartUpload) (string, string, ObjectMetadata, string, int64, bool) {
	return assembleCompletedObject(w, r, bucketPath, objectName, uploadID, mpUpload, completeRequest)
}

func CopyPartsToAssembly(w httpResponseWriter, uploadID string, mpUpload MultipartUpload, completeRequest CompleteMultipartUpload, finalTempFile *os.File) (int64, []string, bool) {
	return copyPartsToAssembly(w, uploadID, mpUpload, completeRequest, finalTempFile)
}

func FinalizeComplete(w httpResponseWriter, r *httpRequest, bucketName, objectName, uploadID, bucketPath, mpUploadMetaPath, partsDir, finalObjectPath, objectMetadataPath, finalTempPath string, meta ObjectMetadata, finalETag string, totalSize int64, failCleanup *bool) {
	finalizeComplete(w, r, bucketName, objectName, uploadID, bucketPath, mpUploadMetaPath, partsDir, finalObjectPath, objectMetadataPath, finalTempPath, meta, finalETag, totalSize, failCleanup)
}

// ---------- Listing internals (object_handlers.go) ----------

func AppendEntries(p *ListObjectsParams, allObjectKeys []string, bucketName, metadataDir string, truncated bool, nextToken string, objects []Object, commonPrefixes []string, processedCount *int, seenPrefixes map[string]struct{}, lastEmitted *string) (bool, string, []Object, []string) {
	return appendEntries(p, allObjectKeys, bucketName, metadataDir, truncated, nextToken, objects, commonPrefixes, processedCount, seenPrefixes, lastEmitted)
}

func GatherListWindow(allObjectKeys []string, start int, p *ListObjectsParams, processedCount *int, seenPrefixes map[string]struct{}, commonPrefixes []string, truncated *bool, nextToken *string) listWindow {
	return gatherListWindow(allObjectKeys, start, p, processedCount, seenPrefixes, commonPrefixes, truncated, nextToken)
}

func NoteBudgetExhausted(allObjectKeys []string, i int, p *ListObjectsParams, seenPrefixes map[string]struct{}, commonPrefixes []string, truncated *bool, nextToken *string) (int, []string) {
	return noteBudgetExhausted(allObjectKeys, i, p, seenPrefixes, commonPrefixes, truncated, nextToken)
}

func KeyExcludedByCursor(objectKey string, p *ListObjectsParams) bool {
	return keyExcludedByCursor(objectKey, p)
}

func KeyMatchesPrefixFilter(objectKey string, p *ListObjectsParams) bool {
	return keyMatchesPrefixFilter(objectKey, p)
}

func GatherDelimiterKey(window *listWindow, objectKey string, p *ListObjectsParams, seenPrefixes map[string]struct{}) bool {
	return gatherDelimiterKey(window, objectKey, p, seenPrefixes)
}

func GroupConsumedByCursor(commonPrefixValue string, p *ListObjectsParams) bool {
	return groupConsumedByCursor(commonPrefixValue, p)
}

func BatchWorkerCount() int { return batchWorkerCount() }

func ParseListObjectsParams(r *httpRequest) ListObjectsParams {
	return parseListObjectsParams(r)
}

// ---------- Backend call helpers + error mapping (errors_to_s3.go) ----------
// The backendCall helpers take the Backend seam type directly; the Object
// alias below disambiguates from the s3 XML Object struct.

type modelObject = objectmodel.Object

func BackendCallFn(bucket string, fn func(b backendIface) (modelObject, error)) (modelObject, error) {
	return backendCall(bucket, fn)
}

func BackendCallBucketFn(bucket string, fn func(b backendIface) error) error {
	return backendCallBucket(bucket, fn)
}

func BackendCallBucketErrFn(bucket string, fn func(b backendIface) error) error {
	return backendCallBucketErr(bucket, fn)
}

func BackendCallBucketStatFn(bucket string, fn func(b backendIface) (modelObject, error)) (modelObject, error) {
	return backendCallBucketStat(bucket, fn)
}

func BackendCallBucket2Fn(bucket string, fn func(b backendIface) (io.ReadCloser, modelObject, error)) (io.ReadCloser, modelObject, error) {
	return backendCallBucket2(bucket, fn)
}

func S3ErrorFromFn(err error) (string, string, int) { return s3ErrorFrom(err) }

func WriteS3ErrorFromFn(w httpResponseWriter, err error) { writeS3ErrorFrom(w, err) }

// ---------- HTTP entry points (dispatch.go + handler files) ----------
// Each is the pre-move package-level shape bound to a throwaway Frontend;
// the process seams (backend lookup, config view) are globals inside this
// package, so the bound frontend's nil fields are never dereferenced on
// these paths — exactly like the pre-move package-level functions.

func RootHandlerFn(w httpResponseWriter, r *httpRequest) {
	defaultTestFrontend().Handler().ServeHTTP(w, r)
}

func ListBucketsHandlerFn(w httpResponseWriter, r *httpRequest) { listBucketsHandler(w, r, nil) }

func CreateBucketHandlerFn(w httpResponseWriter, r *httpRequest, bucketName string) {
	createBucketHandler(w, r, bucketName)
}

func DeleteBucketHandlerFn(w httpResponseWriter, r *httpRequest, bucketName string) {
	deleteBucketHandler(w, r, bucketName)
}

func GetBucketLocationHandlerFn(w httpResponseWriter, r *httpRequest, bucketName string) {
	getBucketLocationHandler(w, r, bucketName)
}

func HeadBucketHandlerFn(w httpResponseWriter, r *httpRequest, bucketName string) {
	headBucketHandler(w, r, bucketName)
}

func ListObjectsV2HandlerFn(w httpResponseWriter, r *httpRequest, bucketName string) {
	listObjectsV2Handler(w, r, bucketName)
}

func ListObjectVersionsHandlerFn(w httpResponseWriter, r *httpRequest, bucketName string) {
	listObjectVersionsHandler(w, r, bucketName)
}

func ListMultipartUploadsHandlerFn(w httpResponseWriter, r *httpRequest, bucketName string) {
	listMultipartUploadsHandler(w, r, bucketName)
}

func DeleteObjectsHandlerFn(w httpResponseWriter, r *httpRequest, bucketName string) {
	deleteObjectsHandler(w, r, bucketName)
}

func BucketLevelDispatchFn(w httpResponseWriter, r *httpRequest, bucketName string) {
	defaultTestFrontend().bucketLevelDispatch(w, r, bucketName)
}

func PutObjectHandlerFn(w httpResponseWriter, r *httpRequest, bucketName, objectName string) {
	putObjectHandler(w, r, bucketName, objectName)
}

func GetObjectHandlerFn(w httpResponseWriter, r *httpRequest, bucketName, objectName string) {
	getObjectHandler(w, r, bucketName, objectName)
}

func HeadObjectHandlerFn(w httpResponseWriter, r *httpRequest, bucketName, objectName string) {
	headObjectHandler(w, r, bucketName, objectName)
}

func DeleteObjectHandlerFn(w httpResponseWriter, r *httpRequest, bucketName, objectName string) {
	deleteObjectHandler(w, r, bucketName, objectName)
}

func CopyObjectHandlerFn(w httpResponseWriter, r *httpRequest, bucketName, objectName string) {
	copyObjectHandler(w, r, bucketName, objectName)
}

func InitiateMultipartUploadHandlerFn(w httpResponseWriter, r *httpRequest, bucketName, objectName string) {
	initiateMultipartUploadHandler(w, r, bucketName, objectName)
}

func UploadPartHandlerFn(w httpResponseWriter, r *httpRequest, bucketName, objectName, partNumberStr, uploadID string) {
	uploadPartHandler(w, r, bucketName, objectName, partNumberStr, uploadID)
}

func CompleteMultipartUploadHandlerFn(w httpResponseWriter, r *httpRequest, bucketName, objectName, uploadID string) {
	completeMultipartUploadHandler(w, r, bucketName, objectName, uploadID)
}

func AbortMultipartUploadHandlerFn(w httpResponseWriter, r *httpRequest, bucketName, objectName, uploadID string) {
	abortMultipartUploadHandler(w, r, bucketName, objectName, uploadID)
}

func ListPartsHandlerFn(w httpResponseWriter, r *httpRequest, bucketName, objectName, uploadID string) {
	listPartsHandler(w, r, bucketName, objectName, uploadID)
}

// defaultTestFrontend returns a Frontend whose serveHTTP path uses the
// package seams (installed config view/backend lookup) — mirroring the
// pre-move package-level rootHandler.
func defaultTestFrontend() *Frontend {
	if testFrontendSingleton == nil {
		testFrontendSingleton = &Frontend{}
	}
	return testFrontendSingleton
}

var testFrontendSingleton *Frontend

// MetadataProviderFor exposes metadataProviderFor for the s3_test package
// (capability_endpoints_test.go pins the hook wiring).
func MetadataProviderFor(bucketPath string) metadata.MetadataProvider {
	return metadataProviderFor(bucketPath)
}

// ---------- Audit log (audit_log.go, auth extensions leaf 10) ----------

// AuditRecordForTest constructs an auditRecord (the s3_test package pins
// the best-effort append contract against a closed writer).
func AuditRecordForTest(ts, principal, method, bucket, key, op string, status int, denied bool) auditRecord {
	return auditRecord{TS: ts, Principal: principal, Method: method, Bucket: bucket, Key: key, Op: op, Status: status, Denied: denied}
}
