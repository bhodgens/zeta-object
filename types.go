package main

import (
	"encoding/xml"
	"time"
)

// types.go — S3 XML/JSON data structures

// XML Structures for S3 Responses

// S3Error defines the structure for S3 compatible XML error responses
type S3Error struct {
	XMLName   xml.Name `xml:"http://s3.amazonaws.com/doc/2006-03-01/ Error"`
	Code      string   `xml:"Code"`
	Message   string   `xml:"Message"`
	RequestID string   `xml:"RequestId,omitempty"` // Optional
	HostID    string   `xml:"HostId,omitempty"`    // Optional
}

// ListAllMyBucketsResult is the top-level structure for listing buckets
type ListAllMyBucketsResult struct {
	XMLName xml.Name `xml:"http://s3.amazonaws.com/doc/2006-03-01/ ListAllMyBucketsResult"`
	Owner   Owner    `xml:"Owner"`
	Buckets Buckets  `xml:"Buckets"`
}

// Owner defines the owner of the buckets
type Owner struct {
	ID          string `xml:"ID"`
	DisplayName string `xml:"DisplayName"`
}

// Buckets is a slice of Bucket
type Buckets struct {
	Bucket []Bucket `xml:"Bucket"`
}

// Bucket defines a single bucket entry
type Bucket struct {
	Name         string `xml:"Name"`
	CreationDate string `xml:"CreationDate"` // S3 format: 2006-02-03T16:45:09.000Z
}

// ObjectMetadata holds metadata for an object
// This will be stored as a JSON file in the .metadata directory
// for each object.
type ObjectMetadata struct {
	ContentType    string            `json:"contentType"`
	ContentLength  int64             `json:"contentLength"`
	ETag           string            `json:"eTag"`
	CustomMetadata map[string]string `json:"customMetadata"` // For x-amz-meta-* headers
	LastModified   time.Time         `json:"lastModified"`
	StoragePath    string            `json:"storagePath"` // Actual path to the object data on disk
}

// ListBucketResult is the S3 response structure for listing objects (ListObjectsV2)
// See: https://docs.aws.amazon.com/AmazonS3/latest/API/API_ListObjectsV2.html
type ListBucketResult struct {
	XMLName               xml.Name       `xml:"http://s3.amazonaws.com/doc/2006-03-01/ ListBucketResult"`
	IsTruncated           bool           `xml:"IsTruncated"`
	Contents              []Object       `xml:"Contents,omitempty"`
	Name                  string         `xml:"Name"`
	Prefix                string         `xml:"Prefix"`
	Delimiter             string         `xml:"Delimiter,omitempty"`
	MaxKeys               int            `xml:"MaxKeys"`
	CommonPrefixes        []CommonPrefix `xml:"CommonPrefixes,omitempty"`
	EncodingType          string         `xml:"EncodingType,omitempty"` // Not implementing URL encoding for now
	KeyCount              int            `xml:"KeyCount"`
	ContinuationToken     string         `xml:"ContinuationToken,omitempty"`
	NextContinuationToken string         `xml:"NextContinuationToken,omitempty"`
	StartAfter            string         `xml:"StartAfter,omitempty"`
}

// Object represents a single object in the ListBucketResult
type Object struct {
	Key          string `xml:"Key"`
	LastModified string `xml:"LastModified"` // Format: 2006-01-02T15:04:05.000Z
	ETag         string `xml:"ETag"`
	Size         int64  `xml:"Size"`
	StorageClass string `xml:"StorageClass"`    // Placeholder, e.g., "STANDARD"
	Owner        *Owner `xml:"Owner,omitempty"` // Optional, can be omitted for simplicity
}

// CommonPrefix represents a prefix rolled up by a delimiter
type CommonPrefix struct {
	Prefix string `xml:"Prefix"`
}

// LocationConstraint is for GetBucketLocation
type LocationConstraint struct {
	XMLName  xml.Name `xml:"http://s3.amazonaws.com/doc/2006-03-01/ LocationConstraint"`
	Location string   `xml:",chardata"`
}

// MultipartUpload represents an active multipart upload session
type MultipartUpload struct {
	UploadID       string            `json:"uploadId"`
	Key            string            `json:"key"`
	Initiated      time.Time         `json:"initiated"`
	CustomMetadata map[string]string `json:"customMetadata,omitempty"` // x-amz-meta-* headers from initiate
	ContentType    string            `json:"contentType,omitempty"`    // Content-Type from initiate, propagated to final object meta
	// Parts will store metadata about each uploaded part
	Parts map[int]PartMetadata `json:"parts"` // Keyed by PartNumber
}

// PartMetadata stores information about a single uploaded part
type PartMetadata struct {
	PartNumber int    `json:"partNumber"`
	ETag       string `json:"eTag"`
	Size       int64  `json:"size"`
	StoredPath string `json:"storedPath"` // Path to the temporary file for this part
}

// InitiateMultipartUploadResult is the XML response for initiating a multipart upload
type InitiateMultipartUploadResult struct {
	XMLName  xml.Name `xml:"InitiateMultipartUploadResult"`
	Bucket   string   `xml:"Bucket"`
	Key      string   `xml:"Key"`
	UploadID string   `xml:"UploadId"`
}

// CompleteMultipartUpload is the structure for the request body of CompleteMultipartUpload
type CompleteMultipartUpload struct {
	XMLName xml.Name       `xml:"CompleteMultipartUpload"`
	Parts   []PartToUpload `xml:"Part"`
}

// PartToUpload represents a part in the CompleteMultipartUpload request
type PartToUpload struct {
	PartNumber int    `xml:"PartNumber"`
	ETag       string `xml:"ETag"`
}

// CompletedMultipartUploadResult is the XML response for completing a multipart upload
type CompletedMultipartUploadResult struct {
	XMLName  xml.Name `xml:"CompleteMultipartUploadResult"`
	Location string   `xml:"Location"` // URL of the created object
	Bucket   string   `xml:"Bucket"`
	Key      string   `xml:"Key"`
	ETag     string   `xml:"ETag"` // ETag of the assembled object (often MD5 of part ETags + count)
}

// Leaf 3.3 — ListMultipartUploads / ListParts XML documents

// MultipartUploadEntry is a single in-progress upload in a
// ListMultipartUploadsResult.
type MultipartUploadEntry struct {
	Key       string `xml:"Key"`
	UploadID  string `xml:"UploadId"`
	Initiated string `xml:"Initiated"` // S3 timestamp: 2006-01-02T15:04:05.000Z
}

// ListMultipartUploadsResult is the XML response for GET /bucket?uploads.
// The S3 xmlns is pinned via the XMLName tag literal (Go's encoder only
// honors the tag, not runtime XMLName values) — same pattern as the other
// result structs above.
type ListMultipartUploadsResult struct {
	XMLName            xml.Name               `xml:"http://s3.amazonaws.com/doc/2006-03-01/ ListMultipartUploadsResult"`
	Bucket             string                 `xml:"Bucket"`
	KeyMarker          string                 `xml:"KeyMarker"`
	UploadIDMarker     string                 `xml:"UploadIdMarker,omitempty"`
	NextKeyMarker      string                 `xml:"NextKeyMarker,omitempty"`
	NextUploadIDMarker string                 `xml:"NextUploadIdMarker,omitempty"`
	Prefix             string                 `xml:"Prefix"`
	MaxUploads         int                    `xml:"MaxUploads"`
	IsTruncated        bool                   `xml:"IsTruncated"`
	Upload             []MultipartUploadEntry `xml:"Upload"`
}

// PartEntry is a single part in a ListPartsResult.
type PartEntry struct {
	PartNumber   int    `xml:"PartNumber"`
	ETag         string `xml:"ETag"` // quoted, e.g. "\"<md5hex>\""
	Size         int64  `xml:"Size"`
	LastModified string `xml:"LastModified"` // S3 timestamp: 2006-01-02T15:04:05.000Z
}

// ListPartsResult is the XML response for GET /object?uploadId=...
type ListPartsResult struct {
	XMLName              xml.Name    `xml:"http://s3.amazonaws.com/doc/2006-03-01/ ListPartsResult"`
	Bucket               string      `xml:"Bucket"`
	Key                  string      `xml:"Key"`
	UploadID             string      `xml:"UploadId"`
	Initiated            string      `xml:"Initiated"`
	PartNumberMarker     int         `xml:"PartNumberMarker"`
	NextPartNumberMarker int         `xml:"NextPartNumberMarker,omitempty"`
	MaxParts             int         `xml:"MaxParts"`
	IsTruncated          bool        `xml:"IsTruncated"`
	Part                 []PartEntry `xml:"Part"`
}
