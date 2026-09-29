package main

import (
	"context"
	"crypto/tls"
	"errors"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	// Blank imports register the built-in storage backends with the
	// internal/backend registry (each package's init() calls
	// backend.Register). Without them the production binary's registry is
	// empty and every bucket lookup fails with unknown backend type "fs".
	_ "mini-s3/internal/backend/fsbackend"

	// Blank import links internal/metadata into the production binary so
	// installS3Seams (s3_wiring.go) can register the built-in
	// "zfs-events" MetadataProvider; without it every ?events request
	// 503s regardless of the host's ZFS state (bughunt C1).
	_ "mini-s3/internal/metadata"
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
	applyListenAddrOverride(&serverConfig)

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

	// Leaf 3.3: hourly lazy expiry of abandoned multipart uploads (>7d old).
	// The sweep logic moved with the multipart staging into the s3
	// frontend; package main keeps the hourly ticker and calls the
	// frontend's exported sweep entry.
	startMultipartExpirySweeper()

	// Build and install the config-driven bucket→Backend table BEFORE the
	// listener opens; an unknown backend type name aborts startup loudly
	// (no silent fs fallback — leaf 03).
	if err := initBackendLookup(); err != nil {
		log.Fatalf("Backend initialization failed: %v", err)
	}

	// Leaf 03 frontend registry: construct the configured frontends via the
	// factory map, register them, and mount: handlers without their own
	// listenAddr go on the shared mux; entries with a listenAddr get a
	// dedicated TLS listener (same cert pair). Any failure (unknown type,
	// duplicate, factory error) aborts startup loudly.
	installS3Seams()
	plan, err := startupPlan(serverConfig.Frontends, nil, mainCredentialSource{})
	if err != nil {
		log.Fatalf("Frontend initialization failed: %v", err)
	}
	srv := newServer(serverConfig.ListenAddr, plan.mux, serverConfig.CertFile, serverConfig.KeyFile)

	// Dedicated-listener servers (leaf 03 multi-listener decision): one
	// http.Server per frontend entry that carries its own listenAddr, all
	// sharing the default cert/key pair. Drained by the same graceful
	// shutdown window below.
	var extraServers []*http.Server
	for _, ls := range plan.listeners {
		extraServers = append(extraServers, newServer(ls.addr, ls.frontend.Handler(),
			serverConfig.CertFile, serverConfig.KeyFile))
	}

	// Graceful shutdown: SIGINT/SIGTERM stop accepting new connections and
	// drain in-flight requests within serverShutdownTimeout (default
	// listener first, then every dedicated-listener server).
	shutdownCtx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	serverErr := make(chan error, 1)
	go func() {
		log.Printf("Starting S3 server on %s (HTTPS, cert=%s, key=%s)",
			srv.Addr, serverConfig.CertFile, serverConfig.KeyFile)
		serverErr <- srv.ListenAndServeTLS(serverConfig.CertFile, serverConfig.KeyFile)
	}()
	for _, es := range extraServers {
		es := es
		go func() {
			log.Printf("Starting dedicated frontend listener on %s (HTTPS)", es.Addr)
			_ = es.ListenAndServeTLS(serverConfig.CertFile, serverConfig.KeyFile)
		}()
	}

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
		for _, es := range extraServers {
			if err := es.Shutdown(drainCtx); err != nil {
				log.Printf("Dedicated listener %s shutdown failed (forcing close): %v", es.Addr, err)
			}
		}
	}
}
