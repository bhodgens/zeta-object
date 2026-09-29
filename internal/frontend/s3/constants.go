package s3

// constants.go — SigV4 protocol constants (moved verbatim from the former
// package-main config.go block; these are wire-protocol values owned by
// the s3 frontend, not server configuration).

const (
	awsAlgorithm     = "AWS4-HMAC-SHA256"
	defaultRegion    = "us-east-1" // Default region for our S3 server
	serviceName      = "s3"
	unsignedPayload  = "UNSIGNED-PAYLOAD"
	streamingPayload = "STREAMING-AWS4-HMAC-SHA256-PAYLOAD"
	iso8601Format    = "20060102T150405Z"
	shortDateFormat  = "20060102"
)
