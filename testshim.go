// testshim.go — IDENTIFIER ALIASES FOR PACKAGE-MAIN TESTS (leaf 02).
//
// The leaf moves the S3 layer into internal/frontend/s3; the package-main
// test files (main_test.go, main_handler_test.go, multipart_handlers_test.go,
// copy_batch_test.go, concurrency_stress_test.go, bench_test.go,
// multipart_sweeper_test.go, fuzz_test.go, routing_test.go, ...) still
// reference the moved type/function names. This file aliases the moved
// identifiers so those tests compile UNCHANGED — behavior preservation
// requires the tests to keep running, not to be rewritten mid-move. A
// dedicated cleanup leaf migrates the test files into the s3 package and
// deletes this file together with export_test_surface.go.
//
// Constants and storage/lock helpers that stayed in package main
// (config.go, storage.go) are NOT re-aliased — the originals remain.
package main

import (
	"mini-s3/internal/backend"
	s3 "mini-s3/internal/frontend/s3"
)

// XML/data-structure types (types.go move).
type (
	S3Error                        = s3.S3Error
	ListAllMyBucketsResult         = s3.ListAllMyBucketsResult
	Owner                          = s3.Owner
	Buckets                        = s3.Buckets
	Bucket                         = s3.Bucket
	ObjectMetadata                 = s3.ObjectMetadata
	ListBucketResult               = s3.ListBucketResult
	Object                         = s3.Object
	CommonPrefix                   = s3.CommonPrefix
	ObjectVersion                  = s3.ObjectVersion
	ListVersionsResult             = s3.ListVersionsResult
	LocationConstraint             = s3.LocationConstraint
	MultipartUpload                = s3.MultipartUpload
	PartMetadata                   = s3.PartMetadata
	InitiateMultipartUploadResult  = s3.InitiateMultipartUploadResult
	CompleteMultipartUpload        = s3.CompleteMultipartUpload
	PartToUpload                   = s3.PartToUpload
	CompletedMultipartUploadResult = s3.CompletedMultipartUploadResult
	MultipartUploadEntry           = s3.MultipartUploadEntry
	ListMultipartUploadsResult     = s3.ListMultipartUploadsResult
	CopyObjectResult               = s3.CopyObjectResult
	DeleteRequest                  = s3.DeleteRequest
	DeleteRequestObj               = s3.DeleteRequestObj
	DeletedEntry                   = s3.DeletedEntry
	DeleteErrorEntry               = s3.DeleteErrorEntry
	DeleteResult                   = s3.DeleteResult
	PartEntry                      = s3.PartEntry
	ListPartsResult                = s3.ListPartsResult
	listObjectsParams              = s3.ListObjectsParams
	rangeOutcome                   = s3.RangeOutcome
	rangeRequest                   = s3.RangeRequest
)

// SigV4 protocol constants (moved to the s3 package; test-only aliases —
// production package-main code no longer references these).
const (
	awsAlgorithm     = s3.AWSAlgorithm
	defaultRegion    = s3.DefaultRegion
	serviceName      = s3.ServiceName
	iso8601Format    = s3.ISO8601Format
	shortDateFormat  = s3.ShortDateFormat
	unsignedPayload  = s3.UnsignedPayload
	streamingPayload = s3.StreamingPayload
	s3XMLNamespace   = s3.S3XMLNamespace

	chunkSigAlgorithm  = s3.ChunkSigAlgorithm
	emptyPayloadSHA256 = s3.EmptyPayloadSHA256

	streamingSignedPayload = s3.StreamingSignedPayload
)

// Functions and vars moved with the S3 layer (sigv4.go, xml.go, handler
// helpers). Constants that stayed in config.go and the storage/lock
// helpers that stayed in storage.go are the originals, untouched.
var (
	hashSHA256                     = s3.HashSHA256
	canonicalQueryEscape           = s3.CanonicalQueryEscape
	rangeFull                      = s3.RangeFull
	rangePartial                   = s3.RangePartial
	rangeUnsatisfiable             = s3.RangeUnsatisfiable
	hmacSHA256                     = s3.HmacSHA256
	getSigningKey                  = s3.GetSigningKey
	getCanonicalURI                = s3.GetCanonicalURI
	getCanonicalQueryString        = s3.GetCanonicalQueryString
	getCanonicalHeaders            = s3.GetCanonicalHeaders
	getPayloadHash                 = s3.GetPayloadHash
	isLowercaseHex64               = s3.IsLowercaseHex64
	VerifyDecodedLength            = s3.VerifyDecodedLength
	decodeAWSChunked               = s3.DecodeAWSChunked
	decodeAndVerifyChunked         = s3.DecodeAndVerifyChunked
	isDecodedStreaming             = s3.IsDecodedStreaming
	withDecodedStreaming           = s3.WithDecodedStreaming
	debugAuthEnabled               = s3.DebugAuthEnabled
	isPresignedRequest             = s3.IsPresignedRequest
	requiredPresignedParams        = s3.RequiredPresignedParams
	authenticateRequest            = s3.AuthenticateRequestFn
	authenticatePresigned          = s3.AuthenticatePresignedFn
	errorToXML                     = s3.ErrorToXML
	writeXML                       = s3.WriteXML
	writeS3Error                   = s3.WriteS3Error
	handleACL                      = s3.HandleACL
	parseInt                       = s3.ParseInt
	validateObjectKey              = s3.ValidateObjectKey
	validateBucketName             = s3.ValidateBucketName
	bucketExists                   = s3.BucketExists
	validBucket                    = s3.ValidBucket
	cleanupEmptyDirs               = s3.CleanupEmptyDirs
	parseRangeHeader               = s3.ParseRangeHeader
	parseCopySource                = s3.ParseCopySource
	listObjectsFromKeys            = s3.ListObjectsFromKeys
	collectObjectKeys              = s3.CollectObjectKeys
	readMetasBatch                 = s3.ReadMetasBatch
	sweepExpiredUploads            = s3.SweepExpiredUploads
	getMultipartLock               = s3.GetMultipartLock
	rootHandler                    = s3.RootHandlerFn
	listBucketsHandler             = s3.ListBucketsHandlerFn
	createBucketHandler            = s3.CreateBucketHandlerFn
	deleteBucketHandler            = s3.DeleteBucketHandlerFn
	getBucketLocationHandler       = s3.GetBucketLocationHandlerFn
	headBucketHandler              = s3.HeadBucketHandlerFn
	listObjectsV2Handler           = s3.ListObjectsV2HandlerFn
	listObjectVersionsHandler      = s3.ListObjectVersionsHandlerFn
	listMultipartUploadsHandler    = s3.ListMultipartUploadsHandlerFn
	deleteObjectsHandler           = s3.DeleteObjectsHandlerFn
	bucketLevelDispatch            = s3.BucketLevelDispatchFn
	putObjectHandler               = s3.PutObjectHandlerFn
	getObjectHandler               = s3.GetObjectHandlerFn
	headObjectHandler              = s3.HeadObjectHandlerFn
	deleteObjectHandler            = s3.DeleteObjectHandlerFn
	copyObjectHandler              = s3.CopyObjectHandlerFn
	initiateMultipartUploadHandler = s3.InitiateMultipartUploadHandlerFn
	uploadPartHandler              = s3.UploadPartHandlerFn
	completeMultipartUploadHandler = s3.CompleteMultipartUploadHandlerFn
	abortMultipartUploadHandler    = s3.AbortMultipartUploadHandlerFn
	listPartsHandler               = s3.ListPartsHandlerFn
	s3URLEncode                    = s3.S3URLEncode
	etagMatches                    = s3.EtagMatches
	evaluatePreconditions          = s3.EvaluatePreconditions
	checkObjectPreconditions       = s3.CheckObjectPreconditions
	serveObjectRange               = s3.ServeObjectRange
	serveObjectRangeFrom           = s3.ServeObjectRangeFrom
	deleteObjectCore               = s3.DeleteObjectCore
	buildCopyMetadata              = s3.BuildCopyMetadata
	resolveObjectDataPath          = s3.ResolveObjectDataPath
	objectDataPathFor              = s3.ObjectDataPathFor
	shadowDataPath                 = s3.ShadowDataPath
	flatDataPathUnusable           = s3.FlatDataPathUnusable
	readObjectMetaForList          = s3.ReadObjectMetaForList
	rawMetaHeaders                 = s3.RawMetaHeaders
	uploadIDRegex                  = &s3.UploadIDRegex
	validateUploadID               = s3.ValidateUploadID
	minPartSize                    = s3.MinPartSize
	multipartUploadExpiry          = s3.MultipartUploadExpiry
	s3Timestamp                    = s3.S3Timestamp
	computeMultipartETag           = s3.ComputeMultipartETag
	assembleCompletedObject        = s3.AssembleCompletedObject
	copyPartsToAssembly            = s3.CopyPartsToAssembly
	finalizeComplete               = s3.FinalizeComplete
	appendEntries                  = s3.AppendEntries
	gatherListWindow               = s3.GatherListWindow
	noteBudgetExhausted            = s3.NoteBudgetExhausted
	keyExcludedByCursor            = s3.KeyExcludedByCursor
	keyMatchesPrefixFilter         = s3.KeyMatchesPrefixFilter
	gatherDelimiterKey             = s3.GatherDelimiterKey
	groupConsumedByCursor          = s3.GroupConsumedByCursor
	batchWorkerCount               = s3.BatchWorkerCount
	parseListObjectsParams         = s3.ParseListObjectsParams
	rawSourceSidecarMeta           = s3.RawSourceSidecarMeta
	nullVersionID                  = s3.NullVersionID
	backendCall                    = s3.BackendCallFn
	backendCallBucket              = s3.BackendCallBucketFn
	backendCallBucketErr           = s3.BackendCallBucketErrFn
	backendCallBucketStat          = s3.BackendCallBucketStatFn
	backendCallBucket2             = s3.BackendCallBucket2Fn
	s3ErrorFrom                    = s3.S3ErrorFromFn
	writeS3ErrorFrom               = s3.WriteS3ErrorFromFn
)

// installTestConfigSync installs the config-view sync hook used by the
// test helpers (setupTestEnv / mpTestConfig): any test mutation of
// serverConfig re-installs the frontend's config view so the moved
// handlers observe the same values the pre-move globals had.
func installTestConfigSync() {
	s3.SetConfigSyncHook(func() {
		s3.InstallServerConfigView(s3.ServerConfigView{
			Buckets: serverConfig.Buckets,
			DataDir: serverConfig.DataDir,
		})
	})
	// Install once immediately so tests that never call the hook still
	// observe a view.
	s3.InstallServerConfigView(s3.ServerConfigView{
		Buckets: serverConfig.Buckets,
		DataDir: serverConfig.DataDir,
	})
}

func init() { installTestConfigSync() }

// Actions: the test seam (actionCommandRunner swap) and the inactivity
// tracker are package-main globals; the frontend's action trigger is
// installed eagerly so handler-level action tests observe the same
// package-main triggerActions path they did pre-move.
func init() {
	s3.InstallActionTrigger(func(eventType string, ctx s3.ActionContext) {
		triggerActions(eventType, mainActionContext(ctx))
	})
	s3.InstallLockObject(lockObject)
	s3.InstallWriteFileAtomic(writeFileAtomic)
	sweepAllBucketsOnce = s3.SweepAllBucketsOnce
}

// Test files sign requests with the serverCredentials global directly;
// the frontend's credential source must therefore follow it. Sync via a
// credential hook consulted per lookup.
func init() {
	s3.SetCredentialSyncHook(func(accessKeyID string) (string, bool) {
		if accessKeyID == serverCredentials.AccessKeyID {
			return serverCredentials.SecretAccessKey, true
		}
		return "", false
	})
}

// The sweep alias must be live before any test runs (installS3Seams is a
// main()-time call; the test binary never runs main). Install eagerly.
var _ = func() bool {
	sweepAllBucketsOnce = s3.SweepAllBucketsOnce
	return true
}()

// The s3 frontend's backendLookup seam must follow package main's
// backendFor var (tests swap the latter). Sync via the backend hook:
// every consult re-reads the var.
func init() {
	s3.SetBackendSyncHook(func(bucket string) (backend.Backend, error) {
		return backendFor(bucket)
	})
}

// serverConfigSyncsMirror documents that serverConfig is mirrored into
// the s3 package on each setupTestEnv call (see SetConfigSyncHook).
