// Command admin-server is the zeta-object web console: a separate, loopback
// process that serves the browser UI and proxies the gateway's management API
// over mTLS. It holds no persistent state — session state is in-memory and
// invalidated on restart.
package main

import (
	"context"
	"errors"
	"log"
	"net/http"
	"os/signal"
	"syscall"
	"time"

	"github.com/bhodgens/zeta-object/internal/adminserver"
	"github.com/bhodgens/zeta-object/internal/adminserver/gateway"
)

const (
	shutdownTimeout         = 30 * time.Second
	serverReadTimeout       = 30 * time.Second
	serverReadHeaderTimeout = 10 * time.Second
	serverWriteTimeout      = 5 * time.Minute
	serverIdleTimeout       = 120 * time.Second
)

func main() {
	cfgPath := adminserver.ConfigPath()
	cfg, err := adminserver.LoadConfig(cfgPath)
	if err != nil {
		log.Fatalf("admin-server: %v", err)
	}

	// The console's identity TO the gateway is its mTLS client pair; the
	// listener certificate pair (certFile/keyFile) is separate and optional.
	gw, err := gateway.New(gateway.Config{
		BaseURL:    cfg.GatewayURL,
		CAFile:     cfg.CAFile,
		ClientCert: cfg.ClientCert,
		ClientKey:  cfg.ClientKey,
	})
	if err != nil {
		log.Fatalf("admin-server: %v", err)
	}

	srv, err := adminserver.NewServer(cfg, gw)
	if err != nil {
		log.Fatalf("admin-server: %v", err)
	}

	httpServer := &http.Server{
		Addr:              cfg.ListenAddr,
		Handler:           srv.Handler(),
		ReadTimeout:       serverReadTimeout,
		ReadHeaderTimeout: serverReadHeaderTimeout,
		WriteTimeout:      serverWriteTimeout,
		IdleTimeout:       serverIdleTimeout,
	}
	// TLS mode (certFile+keyFile set) makes every cookie Secure via the
	// transport; plain loopback mode is only reachable when neither is set and
	// allowNonLoopback is false (LoadConfig enforces that).
	tlsCfg := cfg.TLSConfig()

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	errCh := make(chan error, 1)
	go func() {
		if tlsCfg != nil {
			httpServer.TLSConfig = tlsCfg
			log.Printf("admin-server listening on https://%s", cfg.ListenAddr)
			if err := httpServer.ListenAndServeTLS(cfg.CertFile, cfg.KeyFile); err != nil && !errors.Is(err, http.ErrServerClosed) {
				errCh <- err
			}
			return
		}
		log.Printf("admin-server listening on http://%s", cfg.ListenAddr)
		if err := httpServer.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
		}
	}()

	select {
	case err := <-errCh:
		log.Fatalf("admin-server: %v", err)
	case <-ctx.Done():
		log.Printf("admin-server: shutting down")
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
	defer cancel()
	if err := httpServer.Shutdown(shutdownCtx); err != nil {
		log.Printf("admin-server: graceful shutdown failed: %v", err)
	}
}
