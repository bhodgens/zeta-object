package main

import (
	"crypto/tls"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"

	"github.com/bhodgens/zeta-object/zeta-cache/internal/config"
	"github.com/bhodgens/zeta-object/zeta-cache/internal/index"
	"github.com/bhodgens/zeta-object/zeta-cache/internal/ipc"
)

// Version is the zeta-cache binary version. Leaf-scoped builds report this
// constant; it moves to ldflags injection whenever a release leaf wants it.
const Version = "0.1.0"

func main() {
	log.SetFlags(log.LstdFlags | log.LUTC)
	log.SetPrefix("zeta-cache: ")

	configPath := flag.String("config", "", "path to zeta-cache.json (required except for -version)")
	showVersion := flag.Bool("version", false, "print version and exit")
	flag.Parse()

	if *showVersion {
		fmt.Printf("zeta-cache %s\n", Version)
		os.Exit(0)
	}
	if *configPath == "" {
		log.Fatal("zeta-cache: -config is required")
	}

	cfg, err := config.Load(*configPath)
	if err != nil {
		log.Fatalf("zeta-cache: %v", err)
	}
	log.Printf("zeta-cache: starting %s (bucket %s at %s)", Version, cfg.Bucket, cfg.ServerURL)

	db, err := index.Open(cfg.IndexDB)
	if err != nil {
		log.Fatalf("zeta-cache: %v", err)
	}

	// Startup connectivity probe: ONE authenticated OPTIONS on the bucket
	// path. Failure is a WARNING, never an abort - the daemon still starts
	// (and, once leaf 02 lands, still mounts), state stays idle.
	if err := probe(cfg); err != nil {
		log.Printf("zeta-cache: WARNING connectivity probe failed: %v", err)
	} else {
		log.Printf("zeta-cache: connectivity probe ok")
	}

	srv, err := ipc.Serve(cfg.IPCSocket, cfg.ServerURL, cfg.Bucket)
	if err != nil {
		_ = db.Close()
		log.Fatalf("zeta-cache: %v", err)
	}
	log.Printf("zeta-cache: IPC listening on %s", cfg.IPCSocket)

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
	s := <-sig
	log.Printf("zeta-cache: %v received, shutting down", s)

	// Shutdown order (leaf 01 contract): stop IPC first (clients see the
	// socket vanish instead of getting errors from a half-closed daemon),
	// then close the DB, then the unmount hook (leaf 02 fills it in).
	srv.Stop()
	if err := db.Close(); err != nil {
		log.Printf("zeta-cache: WARNING closing index DB: %v", err)
	}
	unmountHook()
	log.Printf("zeta-cache: shutdown complete")
	// exit 0
}

// probe sends exactly one authenticated request (OPTIONS on the bucket path)
// over the configured server URL. Leaf 06 replaces this with the real
// transport (Basic and mTLS); leaf 01 does Basic auth over plain HTTP
// semantics via http.Client, which works for both http and https URLs.
func probe(cfg *config.Config) error {
	req, err := http.NewRequest(http.MethodOptions, cfg.ServerURL+"/"+cfg.Bucket+"/", nil)
	if err != nil {
		return fmt.Errorf("building probe request: %w", err)
	}
	req.SetBasicAuth(cfg.Auth.AccessKey, cfg.Auth.SecretKey)
	// Leaf 01 probes with the server's own self-signed cert accepted:
	// the gateway ships a private CA / self-signed pair, and real trust
	// handling (CA pool, mTLS, h3) is leaf 06. The probe still proves the
	// URL is reachable and the credentials authenticate.
	// #nosec G402 -- deliberate: self-signed private-server certs until
	// leaf 06 lands CA/mTLS trust handling.
	client := &http.Client{
		Timeout:   10e9,                                                                    // 10s
		Transport: &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}}, //nolint:gosec // G402: leaf 06 owns real trust handling
	}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		return fmt.Errorf("server rejected credentials (HTTP %d)", resp.StatusCode)
	}
	// Any other status (200, 207, 404, 405, ...) proves the server spoke
	// to us over the wire; the bucket's existence is sync's problem.
	return nil
}

// unmountHook is the leaf 02 seam: when the FUSE layer lands it unmounts
// cfg.Mountpoint here, refusing with the reason when dirty files remain.
// Leaf 01 has nothing mounted, so it is a no-op.
func unmountHook() {}
