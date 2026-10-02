package main

import (
	"crypto/tls"
	"log"
	"net/http"
	"os"
	"time"

	// Blank imports register the built-in storage backends with the
	// internal/backend registry (each package's init() calls
	// backend.Register). Without them the production binary's registry is
	// empty and every bucket lookup fails with unknown backend type "fs".
	_ "github.com/bhodgens/zeta-object/internal/backend/fsbackend"

	// Blank import links internal/metadata into the production binary so
	// installS3Seams (s3_wiring.go) can register the built-in
	// "zfs-events" MetadataProvider; without it every ?events request
	// 503s regardless of the host's ZFS state (bughunt C1).
	_ "github.com/bhodgens/zeta-object/internal/metadata"

	// Design-leaf 08: the ReloadableRegistry the SIGHUP loop swaps lives
	// in internal/auth.
	"github.com/bhodgens/zeta-object/internal/auth"
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
	configPath := getEnvOrDefaultLegacy("ZETAOBJECT_CONFIG", defaultConfigFile)
	serverConfigPath = configPath
	if err := loadConfig(configPath); err != nil {
		log.Fatalf("Failed to load config: %v", err)
	}
	// Explicitly load credentials from environment (warn on empty values)
	loadCredentials()

	// Multi-identity registry (pluggable-authentication tree leaf 01):
	// env pair + config identities, validated fail-loud. Duplicate access
	// keys or any invalid identity abort startup — never a silent fallback
	// (same semantic as unknown-backend).
	reg, err := buildIdentityRegistry()
	if err != nil {
		log.Fatalf("Authentication configuration failed: %v", err)
	}
	// Design-leaf 08 (key rotation/revocation): the startup registry is
	// wrapped in a ReloadableRegistry and the WRAPPER is what the rest of
	// the process sees — the s3 identity-registry hook AND the
	// env-fallback credential source (recommended Open Decision 1: wrap at
	// construction so the fallback path rotates too). The SIGHUP loop
	// below swaps the wrapper's inner registry; this pointer is fixed.
	identityRegistry = auth.NewReloadableRegistry(reg)

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
	// ambiguous duplicate mount, factory error) aborts startup loudly.
	installS3Seams()
	// Resolve the REAL default backend (the dataDir-rooted fs instance the
	// s3 seam's backendFor("") returns) before the plan is built: the
	// webdav/owncloud constructors reject a nil backend (bughunt C1), so
	// passing nil here made every webdav/owncloud config fatal before
	// listen. ftp/sftp keep resolving per-bucket lazily through the same
	// installed table; initBackendLookup above guarantees it exists.
	defaultBackend, err := backendFor("")
	if err != nil {
		log.Fatalf("Default backend initialization failed: %v", err)
	}
	plan, err := startupPlan(serverConfig.Frontends, defaultBackend, mainCredentialSource{})
	if err != nil {
		log.Fatalf("Frontend initialization failed: %v", err)
	}
	srv := newServer(serverConfig.ListenAddr, plan.mux, serverConfig.CertFile, serverConfig.KeyFile)
	extraServers, nonHTTPServers := buildDedicatedListeners(plan.listeners)
	runServer(srv, extraServers, nonHTTPServers)
}
