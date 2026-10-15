package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/bhodgens/zeta-object/zeta-cache/internal/config"
	"github.com/bhodgens/zeta-object/zeta-cache/internal/fusefs"
	"github.com/bhodgens/zeta-object/zeta-cache/internal/index"
	"github.com/bhodgens/zeta-object/zeta-cache/internal/ipc"
	"github.com/bhodgens/zeta-object/zeta-cache/internal/scheduler"
	"github.com/bhodgens/zeta-object/zeta-cache/internal/sync"
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
	if cfg.InsecureSkipVerify {
		// The leaf's locked rule: verification is only ever disabled by
		// an EXPLICIT config key, and that choice logs a WARNING.
		log.Printf("zeta-cache: WARNING insecureSkipVerify=true: server certificate verification is DISABLED (config %s)", *configPath)
	}
	log.Printf("zeta-cache: starting %s (bucket %s at %s)", Version, cfg.Bucket, cfg.ServerURL)

	// W2-M6: sweep staging/ BEFORE the mount - a crash mid-hydrate
	// leaves unbounded staging bytes invisible to quota, and at start
	// no write handles exist yet, so every staging file is orphaned.
	if n, b, serr := sweepStagingAtStart(cfg.CacheDir); serr != nil {
		// Partial failure still reclaimed some files (M8): report
		// both the count and the error, do not drop the count.
		log.Printf("zeta-cache: WARNING staging sweep: reclaimed %d file(s), %d bytes; %v", n, b, serr)
	} else if n > 0 {
		log.Printf("zeta-cache: staging sweep reclaimed %d file(s), %d bytes", n, b)
	}

	// W2-M2: wire the REAL disk probe. Rule R2 ("journalled clean +
	// hydrated but the file is gone from disk -> demote") is
	// production-dead with a nil probe: nil means "treat every path as
	// present" (the H1 sentinel semantics - keep it, tests rely on
	// it), so a hydrated file deleted behind the daemon stayed
	// hydrated=1 forever and quota summed phantom bytes. The probe
	// stats the same path the FUSE layer's cachePath builds.
	db, err := index.OpenWithDiskCheck(cfg.IndexDB, cacheDiskProbe(cfg.CacheDir))
	if err != nil {
		log.Fatalf("zeta-cache: %v", err)
	}

	// Leaf 06: the REAL transport. Basic (accessKey/secretKey) or mTLS
	// (clientCert/clientKey); CAFile pins the gateway's self-signed cert
	// for server verification. The alt-svc/h3 upgrade arms itself lazily
	// (only with a client cert configured).
	store := transport.NewFileCertStore(cfg.Auth.ClientCert, cfg.Auth.ClientKey, cfg.CAFile)
	tr, err := transport.NewClient(transport.Options{
		ServerURL:          cfg.ServerURL,
		Bucket:             cfg.Bucket,
		BasicAuthUser:      cfg.Auth.AccessKey,
		BasicAuthPass:      cfg.Auth.SecretKey,
		Store:              store,
		InsecureSkipVerify: cfg.InsecureSkipVerify,
		Logf:               func(f string, a ...any) { log.Printf("zeta-cache: "+f, a...) },
	})
	if err != nil {
		log.Fatalf("zeta-cache: transport: %v", err)
	}
	defer tr.Close()

	// Startup connectivity probe: ONE authenticated PROPFIND (Depth 0)
	// on the bucket path. Failure is a WARNING, never an abort - the
	// daemon still starts and still mounts (locked lifecycle rule: mount
	// succeeds even when the server is unreachable).
	if err := probe(context.Background(), tr); err != nil {
		log.Printf("zeta-cache: WARNING connectivity probe failed: %v", err)
	} else {
		log.Printf("zeta-cache: connectivity probe ok")
	}

	// Leaf 04 + leaf 06: exactly ONE initial sync after the probe over
	// the REAL transport (leaf 07 owns the scheduler loops). Failure is
	// logged, never fatal - the next sync retries.
	runInitialSync(cfg, db, tr)

	mainCtx, mainCancel := context.WithCancel(context.Background())
	defer mainCancel()

	srv, err := ipc.Serve(cfg.IPCSocket, cfg.ServerURL, cfg.Bucket, statusSource{db: db})
	if err != nil {
		_ = db.Close()
		log.Fatalf("zeta-cache: %v", err)
	}
	log.Printf("zeta-cache: IPC listening on %s", cfg.IPCSocket)

	// Leaf 08: the daemon handler and the live status source. The IPC
	// server is REBOUND below once the engine/scheduler exist so the
	// GUI sees the full surface (status, pause/resume, conflicts,
	// deleted, evict); until then the leaf-01 shape answers.
	var handler *guiHandler
	liveStatus := guiStatusSource{db: db, cfg: cfg}
	handler = newGUIHandler(cfg, db, nil, nil)
	// Leaf 08: swap in the full GUI surface on the LIVE listener - no
	// socket teardown (the old re-Serve deleted the live socket file,
	// racing every early client).
	srv.ReplaceHandler(liveStatus, handler)

	// Leaf 02: mount the bucket namespace. The transport is still the
	// leaf-01-shaped seam - leaf 06 fills internal/transport's real HTTP
	// implementation; until then an in-memory backing would serve nothing,
	// so Mount runs with the transport interface's stub ONLY when a
	// placeholder is explicitly wanted. v1 ships the mount wired to the
	// interface: without leaf 06's client the transport calls fail (EIO),
	// which is the documented degraded state.
	mounted, err := mountBucket(cfg, db, tr)
	if err != nil {
		log.Printf("zeta-cache: WARNING mount failed: %v (daemon stays up; status reports the error state)", err)
	} else if mounted {
		log.Printf("zeta-cache: mounted %s at %s", cfg.Bucket, cfg.Mountpoint)
	}

	// Leaf 07: the scheduler loops (prompt upload, periodic sync, quota
	// eviction). MountedFS is the per-file uploader (nil when the mount
	// failed - prompt-upload then falls back to engine.SyncOnce). One
	// Engine shared with the initial sync keeps index state consistent.
	feed, ferr := sync.NewCursorFeed(eventsSourceAdapter{tr}, db, cfg.Bucket, log.Default())
	if ferr != nil {
		feed = nil // scan-only fallback; Delta absence = full scan
	}
	engine, err := sync.NewEngine(sync.Options{
		Transport: tr,
		Store:     db,
		CacheDir:  cfg.CacheDir,
		Logger:    log.Default(),
		Feed:      feed,
	})
	if err != nil {
		log.Printf("zeta-cache: WARNING sync engine construction: %v (scheduler disabled)", err)
	} else {
		sched, err := scheduler.New(scheduler.Options{
			Store:    db,
			Engine:   engine,
			Config:   cfg,
			Uploader: mountedFS,
		})
		if err != nil {
			log.Printf("zeta-cache: WARNING scheduler construction: %v (scheduler disabled)", err)
		} else {
			go sched.Start(mainCtx)
		}
		// Leaf 08: the full daemon handler is live - swap it in on the
		// SAME listener (ReplaceHandler: no socket teardown racing early
		// clients) so conflicts/deleted/evict/pause answer over the
		// socket, and the status reports the live engine state.
		handler = newGUIHandler(cfg, db, engine, sched)
		liveStatus.engine = engine
		liveStatus.sched = sched
		srv.ReplaceHandler(liveStatus, handler)
	}

	// The prompt-upload hook (leaf-07 contract): the scheduler's watcher
	// uploads a dirty file the moment its Flush lands. mountedFS IS the
	// per-file uploader (UploadFile); the prompt loop calls it through
	// the adapter below, so ONE implementation serves both loops.
	if mountedFS != nil {
		upFS := mountedFS
		mountedFS.SetUploader(uploaderFunc(func(ctx context.Context, key string) {
			// Fire-and-forget: Flush must never block on the upload.
			go func() {
				if err := upFS.UploadFile(ctx, key); err != nil {
					// Stay dirty; the watcher's poll fallback retries.
					log.Printf("zeta-cache: prompt upload %s: %v (stays dirty)", key, err)
				}
			}()
		}))
	}

	sig := make(chan os.Signal, 1)
	// SIGUSR1 is part of the shutdown contract: after a REFUSED unmount
	// (bounded flush hit its deadline), SIGUSR1 retries the unmount.
	// Without it in Notify, the default action would kill the daemon
	// while it is still mounted - the exact outcome the refusal avoids.
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM, syscall.SIGUSR1)
	s := <-sig
	log.Printf("zeta-cache: %v received, shutting down", s)
	mainCancel()

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
		// The refused-shutdown wait loop: SIGUSR1 retries the unmount,
		// a second SIGTERM force-stops. The loop (not a single if) is
		// what keeps a SIGUSR1 from being consumed as a no-op while the
		// mount is still there.
		ipcReserve, rerr := ipc.Serve(cfg.IPCSocket, cfg.ServerURL, cfg.Bucket, statusSource{db: db})
		if rerr != nil {
			log.Printf("zeta-cache: WARNING refused-shutdown IPC re-serve failed: %v (SIGTERM still force-stops)", rerr)
			s2 := <-sig
			log.Printf("zeta-cache: %v during refused-shutdown wait", s2)
			if s2 == syscall.SIGUSR1 {
				if retryErr := unmountHook(); retryErr != nil {
					log.Printf("zeta-cache: unmount still refused: %v", retryErr)
				}
			}
		} else {
			defer func() { ipcReserve.Stop() }()
			for {
				s2 := <-sig
				log.Printf("zeta-cache: %v during refused-shutdown wait", s2)
				if s2 != syscall.SIGUSR1 {
					break // SIGTERM (or other): force-stop, do not unmount
				}
				if retryErr := unmountHook(); retryErr != nil {
					// Still dirty: keep waiting; the NEXT SIGUSR1
					// retries again (unmountHook kept mountedFS).
					log.Printf("zeta-cache: unmount still refused: %v (SIGUSR1 retries; SIGTERM force-stops)", retryErr)
					continue
				}
				log.Printf("zeta-cache: unmount completed on retry")
				break
			}
		}
	}
	if err := db.Close(); err != nil {
		log.Printf("zeta-cache: WARNING closing index DB: %v", err)
	}
	log.Printf("zeta-cache: shutdown complete")
	// exit 0
}

// cacheDiskProbe returns the reconcile disk probe over cacheDir: does
// files/<path> exist as a FILE right now? Same layout fusefs's cachePath
// builds (files/<key>, slash keys). Directories (dir mirrors) do not
// count: only hydrated file rows are reconciled. Never nil at runtime -
// a nil probe means "skip the disk column" and leaves rule R2 dead.
func cacheDiskProbe(cacheDir string) func(path string) bool {
	return func(path string) bool {
		fi, err := os.Stat(filepath.Join(cacheDir, "files", filepath.FromSlash(path)))
		if err != nil {
			// Only a confirmed absence demotes (R2). EACCES and
			// other stat failures mean the file may still exist;
			// treating them as absent mass-demotes live rows (M2).
			return !os.IsNotExist(err)
		}
		return !fi.IsDir()
	}
}

// sweepStagingAtStart reclaims staging/ bytes a crash mid-hydrate left
// behind (W2-M6). At daemon start NO write handles exist yet, so every
// staging file is by definition orphaned - remove all of them, before
// the mount's first Create can stage anything. Returns the count and
// bytes reclaimed (logged by the caller).
func sweepStagingAtStart(cacheDir string) (int, int64, error) {
	return fusefs.SweepStaging(cacheDir)
}

// mountBucket wires the FUSE layer to the index + the REAL transport
// (leaf 06). The daemon now mounts with a live webdav client; transport
// failures surface as EIO per the leaf-02 degraded-state contract, and
// the sync engine repairs state on its next pass.
func mountBucket(cfg *config.Config, db *index.Store, tr transport.Transport) (bool, error) {
	fs, err := fusefs.Mount(fusefs.Options{
		CacheDir:   cfg.CacheDir,
		Bucket:     cfg.Bucket,
		Mountpoint: cfg.Mountpoint,
		Index:      newIndexAdapter(db),
		Transport:  tr,
	})
	if err != nil {
		return false, err
	}
	mountedFS = fs
	return fs.Mounted(), nil
}

// mountedFS holds the live mount for the unmount hook.
var mountedFS *fusefs.FS

// uploaderFunc adapts a function to fusefs.Uploader (the prompt-upload
// hook; the same shape scheduler.UploadFunc uses on its side).
type uploaderFunc func(ctx context.Context, key string)

// UploadNow implements fusefs.Uploader.
func (f uploaderFunc) UploadNow(ctx context.Context, key string) { f(ctx, key) }

// unmountHook is the leaf 02 unmount: bounded flush, then either a clean
// detach or a refusal naming the files that stayed dirty. The global is
// cleared ONLY on success - a refused unmount keeps the live FS so a
// SIGUSR1 retry actually retries instead of returning nil immediately.
func unmountHook() error {
	if mountedFS == nil {
		return nil
	}
	err := mountedFS.Unmount(context.Background())
	if err == nil {
		mountedFS = nil
	}
	return err
}

// probe sends exactly one authenticated request (PROPFIND Depth 0 on the
// bucket path) through the REAL transport (leaf 06): it exercises the
// same TLS trust + auth material every later request uses.
func probe(ctx context.Context, tr transport.Transport) error {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	_, err := tr.Propfind(ctx, "", false)
	if err != nil {
		if errors.Is(err, transport.ErrNotExist) {
			// The bucket does not exist yet - the server answered, so
			// reachability and auth are proven; the bucket's existence
			// is sync's problem.
			return nil
		}
		return err
	}
	return nil
}
