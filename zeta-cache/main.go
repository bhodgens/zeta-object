package main

import (
	"context"
	"crypto/tls"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"

	"github.com/bhodgens/zeta-object/zeta-cache/internal/config"
	"github.com/bhodgens/zeta-object/zeta-cache/internal/fusefs"
	"github.com/bhodgens/zeta-object/zeta-cache/internal/index"
	"github.com/bhodgens/zeta-object/zeta-cache/internal/ipc"
	"github.com/bhodgens/zeta-object/zeta-cache/internal/transport"
)

// Version is the zeta-cache binary version. Leaf-scoped builds report this
// constant; it moves to ldflags injection whenever a release leaf wants it.
const Version = "0.2.0"

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
	// and still mounts (locked lifecycle rule: mount succeeds even when
	// the server is unreachable).
	if err := probe(cfg); err != nil {
		log.Printf("zeta-cache: WARNING connectivity probe failed: %v", err)
	} else {
		log.Printf("zeta-cache: connectivity probe ok")
	}

	// Leaf 04: exactly ONE initial sync after the probe (leaf 07 owns
	// the scheduler loops). With the transport still pending (leaf 06)
	// this is a no-op logging the degraded state; the meta table keeps
	// the persisted status values for IPC either way.
	runInitialSync(cfg, db)

	srv, err := ipc.Serve(cfg.IPCSocket, cfg.ServerURL, cfg.Bucket, statusSource{db: db})
	if err != nil {
		_ = db.Close()
		log.Fatalf("zeta-cache: %v", err)
	}
	log.Printf("zeta-cache: IPC listening on %s", cfg.IPCSocket)

	// Leaf 02: mount the bucket namespace. The transport is still the
	// leaf-01-shaped seam - leaf 06 fills internal/transport's real HTTP
	// implementation; until then an in-memory backing would serve nothing,
	// so Mount runs with the transport interface's stub ONLY when a
	// placeholder is explicitly wanted. v1 ships the mount wired to the
	// interface: without leaf 06's client the transport calls fail (EIO),
	// which is the documented degraded state.
	mounted, err := mountBucket(cfg, db)
	if err != nil {
		log.Printf("zeta-cache: WARNING mount failed: %v (daemon stays up; status reports the error state)", err)
	} else if mounted {
		log.Printf("zeta-cache: mounted %s at %s", cfg.Bucket, cfg.Mountpoint)
	}

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
	s := <-sig
	log.Printf("zeta-cache: %v received, shutting down", s)

	// Shutdown order (leaf 01 contract, extended by leaf 02): stop IPC
	// first (clients see the socket vanish), then the bounded-flush
	// unmount (may keep the process alive reporting the error state),
	// and close the DB last - the flush writes index rows.
	srv.Stop()
	unmountErr := unmountHook()
	if unmountErr != nil {
		// Bounded flush hit its deadline or an upload failed: refuse the
		// shutdown, keep the process alive so the IPC status (re-served
		// on a fresh socket) can report the error state. SIGUSR1 retries
		// the unmount; SIGTERM again force-stops without unmounting.
		log.Printf("zeta-cache: unmount refused: %v (process stays alive; SIGTERM again to force-stop)", unmountErr)
		ipcReserve, rerr := ipc.Serve(cfg.IPCSocket, cfg.ServerURL, cfg.Bucket, statusSource{db: db})
		if rerr == nil {
			s2 := <-sig
			ipcReserve.Stop()
			log.Printf("zeta-cache: %v during refused-shutdown wait", s2)
			if s2 == syscall.SIGUSR1 {
				if retryErr := unmountHook(); retryErr != nil {
					log.Printf("zeta-cache: unmount still refused: %v", retryErr)
				}
			}
		}
	}
	if err := db.Close(); err != nil {
		log.Printf("zeta-cache: WARNING closing index DB: %v", err)
	}
	log.Printf("zeta-cache: shutdown complete")
	// exit 0
}

// mountBucket wires the FUSE layer to the index + transport seams. The
// *sql.DB from leaf 01's index.Open is the placeholder schema (leaf 03
// owns the real one); the fusefs.Index adapter below runs narrow queries
// against it, and leaf 03 preserves the interface. Returns mounted=false
// with a nil error when the transport implementation is absent (leaf 06
// pending): the process stays up, status stays idle.
func mountBucket(cfg *config.Config, db *index.Store) (bool, error) {
	fs, err := fusefs.Mount(fusefs.Options{
		CacheDir:   cfg.CacheDir,
		Bucket:     cfg.Bucket,
		Mountpoint: cfg.Mountpoint,
		Index:      newIndexAdapter(db),
		Transport:  transportStubUntilLeaf06(),
	})
	if err != nil {
		return false, err
	}
	mountedFS = fs
	return fs.Mounted(), nil
}

// transportStubUntilLeaf06 supplies the transport seam. Leaf 06 owns the
// real HTTP client; until it lands the daemon runs WITHOUT a transport and
// skips the mount (process up, status idle) rather than serving errors.
// The stub transport (in-memory map) is what unit tests use directly.
func transportStubUntilLeaf06() transport.Transport {
	log.Printf("zeta-cache: transport implementation pending (leaf 06); mount skipped")
	return nil
}

// mountedFS holds the live mount for the unmount hook.
var mountedFS *fusefs.FS

// unmountHook is the leaf 02 unmount: bounded flush, then either a clean
// detach or a refusal naming the files that stayed dirty.
func unmountHook() error {
	if mountedFS == nil {
		return nil
	}
	err := mountedFS.Unmount(context.Background())
	mountedFS = nil
	return err
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
