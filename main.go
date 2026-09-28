package main

import (
	"log"
	"net/http"
	"os"
	"strings"
)

// main.go — server entrypoint and root request router

func main() {
	// Load configuration
	configPath := getEnvOrDefault("MINIS3_CONFIG", defaultConfigFile)
	if err := loadConfig(configPath); err != nil {
		log.Fatalf("Failed to load config: %v", err)
	}

	// Ensure data directory exists
	if _, err := os.Stat(serverConfig.DataDir); os.IsNotExist(err) {
		if err := os.MkdirAll(serverConfig.DataDir, 0755); err != nil {
			log.Fatalf("Failed to create data directory: %v", err)
		}
	}

	// Validate custom bucket paths exist
	for bucketName, bucketPath := range serverConfig.Buckets {
		info, err := os.Stat(bucketPath)
		if err != nil {
			log.Printf("Warning: Custom bucket '%s' path '%s' error: %v", bucketName, bucketPath, err)
			continue
		}
		if !info.IsDir() {
			log.Printf("Warning: Custom bucket '%s' path '%s' is not a directory", bucketName, bucketPath)
		}
	}

	// Initialize inactivity tracker and load bucket action timers
	InitInactivityTracker()
	initializeInactivityTimers()

	http.HandleFunc("/", rootHandler)
	log.Println("Starting S3 server on :8443 (HTTPS)")
	// Assumes certs/cert.pem and certs/key.pem exist
	err := http.ListenAndServeTLS(":8443", "certs/cert.pem", "certs/key.pem", nil)
	if err != nil {
		log.Fatalf("ListenAndServeTLS failed: %v. Please ensure certs/cert.pem and certs/key.pem are correctly generated and in place.", err)
	}
}

func rootHandler(w http.ResponseWriter, r *http.Request) {
	// Basic path parsing to differentiate between service-level and bucket-level requests
	pathParts := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
	bucketName := ""
	objectName := ""

	if len(pathParts) >= 1 && pathParts[0] != "" {
		bucketName = pathParts[0]
	}
	if len(pathParts) >= 2 {
		objectName = strings.Join(pathParts[1:], "/")
	}

	log.Printf("Request: %s %s, Bucket: '%s', Object: '%s'", r.Method, r.URL.Path, bucketName, objectName)

	// Authenticate request (placeholder - to be implemented with AWS SigV4)
	if !authenticateRequest(w, r) {
		// authenticateRequest will write the error response if authentication fails
		return
	}

	// ACL specific handling (stubbed)
	if _, aclPresent := r.URL.Query()["acl"]; aclPresent {
		handleACL(w, r, bucketName, objectName)
		return
	}

	if bucketName == "" { // Service-level operations
		switch r.Method {
		case "GET":
			listBucketsHandler(w, r)
		default:
			http.Error(w, "Method Not Allowed at service level", http.StatusMethodNotAllowed)
		}
	} else if objectName == "" { // Bucket-level operations
		// Check if location parameter is present for GetBucketLocation
		if _, ok := r.URL.Query()["location"]; ok && r.Method == "GET" {
			getBucketLocationHandler(w, r, bucketName)
			return
		}
		// Check if list-type=2 parameter is present for ListObjectsV2
		if val, ok := r.URL.Query()["list-type"]; ok && val[0] == "2" && r.Method == "GET" {
			listObjectsV2Handler(w, r, bucketName)
			return
		}

		switch r.Method {
		case "PUT":
			createBucketHandler(w, r, bucketName)
		case "GET": // This would be ListObjectsV1 or GetBucketACL etc.
			// For now, assume ListObjectsV2 is the primary way to list.
			// If no specific query params for listing, could be GetBucketACL or other bucket specific GETs.
			// We'll default to a simple "Not Implemented" or treat as ListObjectsV2 if query params match.
			listObjectsV2Handler(w, r, bucketName) // Or a more specific handler based on query params
		case "DELETE":
			deleteBucketHandler(w, r, bucketName)
		case "HEAD":
			headBucketHandler(w, r, bucketName)
		default:
			http.Error(w, "Method Not Allowed for bucket", http.StatusMethodNotAllowed)
		}
	} else { // Object-level operations
		// Check for multipart upload query parameters
		if _, ok := r.URL.Query()["uploads"]; ok && r.Method == "POST" {
			initiateMultipartUploadHandler(w, r, bucketName, objectName)
			return
		}
		if partNumber, ok := r.URL.Query()["partNumber"]; ok && r.Method == "PUT" {
			if uploadID, ok := r.URL.Query()["uploadId"]; ok {
				uploadPartHandler(w, r, bucketName, objectName, partNumber[0], uploadID[0])
				return
			}
		}
		if uploadID, ok := r.URL.Query()["uploadId"]; ok && r.Method == "POST" {
			completeMultipartUploadHandler(w, r, bucketName, objectName, uploadID[0])
			return
		}
		if uploadID, ok := r.URL.Query()["uploadId"]; ok && r.Method == "DELETE" {
			abortMultipartUploadHandler(w, r, bucketName, objectName, uploadID[0])
			return
		}

		switch r.Method {
		case "PUT":
			putObjectHandler(w, r, bucketName, objectName)
		case "GET":
			getObjectHandler(w, r, bucketName, objectName)
		case "DELETE":
			deleteObjectHandler(w, r, bucketName, objectName)
		case "HEAD":
			headObjectHandler(w, r, bucketName, objectName)
		default:
			http.Error(w, "Method Not Allowed for object", http.StatusMethodNotAllowed)
		}
	}
}
