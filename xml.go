package main

import (
	"encoding/xml"
	"log"
	"net/http"
)

// xml.go — S3 XML error responses and ACL stub

// errorToXML converts an error code and message to S3 XML error format
func errorToXML(code, message string) string {
	s3Err := S3Error{
		Code:    code,
		Message: message,
	}
	x, err := xml.MarshalIndent(s3Err, "", "  ")
	if err != nil {
		log.Printf("Error marshalling S3Error to XML: %v", err)
		return "<Error><Code>InternalError</Code><Message>Failed to generate error XML.</Message></Error>"
	}
	return string(x)
}

// Placeholder ACL related requests.
func handleACL(w http.ResponseWriter, r *http.Request, bucketName, objectName string) {
	log.Printf("ACL request for Bucket: '%s', Object: '%s' - Not Implemented", bucketName, objectName)
	writeS3Error(w, "NotImplemented", "ACLs are not implemented.", http.StatusNotImplemented)
}

// writeS3Error writes an S3-compliant XML error response with proper Content-Type header
func writeS3Error(w http.ResponseWriter, code string, message string, statusCode int) {
	w.Header().Set("Content-Type", "application/xml")
	w.WriteHeader(statusCode)
	w.Write([]byte(errorToXML(code, message)))
}
