package main

import (
	"context"
	"crypto/tls"
	"errors"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// main.go — server entrypoint and root request router

// TLS/server timeout configuration for the explicit http.Server
const (
	serverReadTimeout       = 30 * time.Second  // full request (incl. body) — generous for uploads
	serverReadHeaderTimeout = 10 * time.Second  // headers only — protects against slowloris
	serverWriteTimeout      = 5 * time.Minute   // large object PUT/GET responses
	serverIdleTimeout       = 120 * time.Second // keep-alive idle between requests
	serverShutdownTimeout   = 30 * time.Second  // drain window on SIGINT/SIGTERM
	serverMinTLSVersion     = tls.VersionTLS12
)

// newServer builds the explicit http.Server with lifecycle hardening:
// timeouts on all phases and TLS 1.2 as the minimum protocol version.
func newServer(addr string, handler http.Handler, certFile, keyFile string) *http.Server {
	tlsConfig := &tls.Config{
		MinVersion: serverMinTLSVersion,
	}
	return &http.Server{
		Addr:              addr,
		Handler:           handler,
		TLSConfig:         tlsConfig,
		ReadTimeout:       serverReadTimeout,
		ReadHeaderTimeout: serverReadHeaderTimeout,
		WriteTimeout:      serverWriteTimeout,
		IdleTimeout:       serverIdleTimeout,
	}
}

func main() {
	// Load configuration (fatal on any error other than a missing file)
	configPath := getEnvOrDefault("MINIS3_CONFIG", defaultConfigFile)
	if err := loadConfig(configPath); err != nil {
		log.Fatalf("Failed to load config: %v", err)
	}
	// Explicitly load credentials from environment (warn on empty values)
	loadCredentials()

	// Environment override for the listen address (beats config file)
	if listenAddr := os.Getenv("MINIS3_LISTEN_ADDR"); listenAddr != "" {
		serverConfig.ListenAddr = listenAddr
	}

	// Ensure data directory exists; any stat error other than IsNotExist is fatal
	if _, err := os.Stat(serverConfig.DataDir); err != nil {
		if !os.IsNotExist(err) {
			log.Fatalf("Cannot access data directory %s: %v", serverConfig.DataDir, err)
		}
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

	// Leaf 3.3: hourly lazy expiry of abandoned multipart uploads (>7d old)
	startMultipartExpirySweeper()

	mux := http.NewServeMux()
	mux.HandleFunc("/", rootHandler)
	srv := newServer(serverConfig.ListenAddr, mux, serverConfig.CertFile, serverConfig.KeyFile)

	// Graceful shutdown: SIGINT/SIGTERM stop accepting new connections and
	// drain in-flight requests within serverShutdownTimeout.
	shutdownCtx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	serverErr := make(chan error, 1)
	go func() {
		log.Printf("Starting S3 server on %s (HTTPS, cert=%s, key=%s)",
			srv.Addr, serverConfig.CertFile, serverConfig.KeyFile)
		serverErr <- srv.ListenAndServeTLS(serverConfig.CertFile, serverConfig.KeyFile)
	}()

	select {
	case err := <-serverErr:
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Fatalf("ListenAndServeTLS failed: %v. Please ensure %s and %s are correctly generated and in place.",
				err, serverConfig.CertFile, serverConfig.KeyFile)
		}
	case <-shutdownCtx.Done():
		log.Println("Shutdown signal received, draining in-flight requests...")
		drainCtx, cancel := context.WithTimeout(context.Background(), serverShutdownTimeout)
		defer cancel()
		if err := srv.Shutdown(drainCtx); err != nil {
			log.Printf("Graceful shutdown failed (forcing close): %v", err)
		} else {
			log.Println("Server shut down cleanly")
		}
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

	log.Printf("Request: %s %s, Bucket: '%s', Object: '%s'", strconv.Quote(r.Method), strconv.Quote(r.URL.Path), strconv.Quote(bucketName), strconv.Quote(objectName))

	// Authenticate request: presigned query auth when X-Amz-* params present
	// and no Authorization header; header auth wins otherwise (leaf 3.2).
	if isPresignedRequest(r) {
		if !authenticatePresigned(w, r) {
			// authenticatePresigned writes the error response on failure.
			return
		}
	} else if !authenticateRequest(w, r) {
		// authenticateRequest will write the error response if authentication fails
		return
	}

	// ACL specific handling (stubbed)
	if _, aclPresent := r.URL.Query()["acl"]; aclPresent {
		handleACL(w, r, bucketName, objectName)
		return
	}

	switch {
	case bucketName == "":
		serviceLevelDispatch(w, r)
	case objectName == "":
		bucketLevelDispatch(w, r, bucketName)
	default:
		objectLevelDispatch(w, r, bucketName, objectName)
	}
}

// serviceLevelDispatch routes service-level (no bucket) requests.
func serviceLevelDispatch(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case "GET":
		listBucketsHandler(w, r)
	default:
		http.Error(w, "Method Not Allowed at service level", http.StatusMethodNotAllowed)
	}
}

// bucketLevelDispatch routes bucket-level requests (bucket set, no object),
// including the ?location and ?list-type=2 sub-resources.
func bucketLevelDispatch(w http.ResponseWriter, r *http.Request, bucketName string) {
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
	// Leaf 3.3: ListMultipartUploads sub-resource
	if _, ok := r.URL.Query()["uploads"]; ok && r.Method == "GET" {
		listMultipartUploadsHandler(w, r, bucketName)
		return
	}
	// Leaf 3.5: DeleteObjects sub-resource (POST /bucket?delete)
	if _, ok := r.URL.Query()["delete"]; ok && r.Method == "POST" {
		deleteObjectsHandler(w, r, bucketName)
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
}

// objectLevelDispatch routes object-level requests, including the multipart
// sub-resources (?uploads, ?partNumber, ?uploadId).
func objectLevelDispatch(w http.ResponseWriter, r *http.Request, bucketName, objectName string) {
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
	// Leaf 3.3: ListParts sub-resource
	if uploadID, ok := r.URL.Query()["uploadId"]; ok && r.Method == "GET" {
		listPartsHandler(w, r, bucketName, objectName, uploadID[0])
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
