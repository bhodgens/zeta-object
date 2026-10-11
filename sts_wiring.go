// sts_wiring.go — package-main wiring for the /sts token-exchange
// frontend (accessstor appliance tree leaf 02, Task 3).
//
// Owns:
//   - the process signing-key slot (shared by the sts frontend and the
//     s3 resolver), loaded from zeta_sts.signing_key_file;
//   - the CredentialResolver over the LIVE federation registry;
//   - the "sts" frontend factory (dedicated TLS listener);
//   - the signing-key reload hook, invoked by the SIGHUP / POST
//     /auth/reload identity-reload sequence (a rotation invalidates
//     outstanding ZSTSID- credentials fail-closed).
//
// Frontend-list semantics (gotcha 4): "sts" is a separate frontend NAME;
// an explicit frontends array lists it as its own entry and "s3" stays
// listed — this factory never mounts or unmounts anything else.
package main

import (
	"fmt"
	"log"
	"sync/atomic"
	"time"

	"github.com/bhodgens/zeta-object/internal/auth"
	"github.com/bhodgens/zeta-object/internal/backend"
	"github.com/bhodgens/zeta-object/internal/frontend"
	"github.com/bhodgens/zeta-object/internal/frontend/s3"
	"github.com/bhodgens/zeta-object/internal/frontend/sts"
)

// stsSigningKey is the process signing-key slot. nil/empty = sts
// disabled (or not yet loaded); every derivation and resolution reads it
// atomically, so the reload swap is race-free.
var stsSigningKey *atomic.Pointer[[]byte]

// stsSigningKeyPath records the configured path so the reload sequence
// re-reads the SAME file (an operator edit to zeta_sts.signing_key_file
// is restart-required; the path itself is fixed per boot).
var stsSigningKeyPath string

// initSTS loads the signing key, builds the resolver, and installs the
// s3 hook. Called from main() AFTER buildIdentityRegistry (the resolver
// reads the live registry through the getter) and BEFORE the frontends
// are constructed. With sts disabled (absent zeta_sts block) this is a
// no-op: no hook installed, every ZSTSID- key stays an unknown access
// key — the exact pre-leaf behavior.
func initSTS() error {
	cfg := serverConfig().ZetaSTS
	if cfg == nil || !cfg.Enabled {
		return nil
	}
	if serverConfig().ZetaFederation == nil {
		return fmt.Errorf("zeta_sts.enabled requires a zeta_federation registry (leaf 01)")
	}
	key, err := resolveSigningKey(cfg.SigningKeyFile)
	if err != nil {
		return err
	}
	stsSigningKeyPath = cfg.SigningKeyFile
	slot := &atomic.Pointer[[]byte]{}
	slot.Store(&key)
	stsSigningKey = slot

	liveRegistry := func() *auth.FederationRegistry {
		return federationRegistryFor()
	}
	resolver := sts.NewCredentialResolver(liveRegistry, slot)
	// The SigV4 path learns the ZSTSID- prefix here (leaf 02 Task 2).
	s3.InstallTempCredentialSource(resolver)
	log.Printf("STS token exchange enabled: ttl=%s max_ttl=%s signing_key=%s",
		stsDurationOr(cfg.TTL), stsDurationOr(cfg.MaxTTL), cfg.SigningKeyFile)
	return nil
}

// stsDurationOr renders a duration with the leaf default named for logs.
func stsDurationOr(d time.Duration) string {
	if d > 0 {
		return d.String()
	}
	return "default"
}

// federationRegistryFor returns the LIVE federation registry: the one
// the reload sequence rebuilt, or nil when federation is disabled. The
// registry is rebuilt (not mutated) by reloadIdentityRegistry, so the
// getter is the only race-free way to observe a reload.
func federationRegistryFor() *auth.FederationRegistry {
	return federationRegistry.Load()
}

// federationRegistry mirrors the startup/reload-built registry. The
// ReloadableRegistry wrapper serves access KEYS; the federation registry
// is a separate decision point (leaf 01) with no wrapper, so this atomic
// mirror is the process handle. Set at startup and on every reload.
var federationRegistry atomic.Pointer[auth.FederationRegistry]

// installFederationRegistry publishes reg to every consumer that reads
// the LIVE registry (the sts resolver). Called at startup and on reload.
func installFederationRegistry(reg *auth.FederationRegistry) {
	federationRegistry.Store(reg)
}

// reloadSTSSigningKey re-reads the signing-key file into the slot. The
// identity-reload path invokes it; a failure keeps the OLD key serving
// (fail-closed against rotation, fail-open against downtime — the same
// semantics as the identity reload).
func reloadSTSSigningKey() error {
	if stsSigningKey == nil || stsSigningKeyPath == "" {
		return nil
	}
	key, err := resolveSigningKey(stsSigningKeyPath)
	if err != nil {
		return err
	}
	stsSigningKey.Store(&key)
	log.Printf("Reloaded sts signing key from %s", stsSigningKeyPath)
	return nil
}

// stsFrontendFactory is the "sts" entry in frontendFactories (wired in
// frontends.go's map init via registerSTSFactory). Options come from the
// frontends entry's options map:
//
//	clientCAFile      optional PEM bundle (mTLS like admin)
//	allowNonLoopback  "true" opts out of the loopback guard
func stsFrontendFactory(cfg FrontendConfig, b backend.Backend, creds auth.CredentialSource) (frontend.Frontend, error) {
	if cfg.Bucket != "" {
		return nil, fmt.Errorf(`frontend type "sts" does not accept the "bucket" key (webdav only)`)
	}
	if err := validateOptions(cfg.Type, cfg.Options, sts.KnownOptionKeys); err != nil {
		return nil, err
	}
	blockCfg := serverConfig().ZetaSTS
	if blockCfg == nil || !blockCfg.Enabled || stsSigningKey == nil {
		return nil, fmt.Errorf("frontend type %q requires the zeta_sts block enabled (enabled=true and signing_key_file set)", cfg.Type)
	}
	sc := serverConfig()
	f, err := sts.New(sts.Options{
		ListenAddr:       cfg.ListenAddr,
		ClientCAFile:     cfg.Options["clientCAFile"],
		AllowNonLoopback: cfg.Options["allowNonLoopback"] == "true",
		CertFile:         sc.CertFile,
		KeyFile:          sc.KeyFile,
		TTL:              blockCfg.TTL,
		MaxTTL:           blockCfg.MaxTTL,
		Registry:         federationRegistryFor,
		ServerKey:        stsSigningKey,
		Audit:            s3.AppendAudit,
	})
	if err != nil {
		return nil, err
	}
	// Join the existing /auth/reload CA fan (shape-compatible with
	// admin.ClientCAReloader) when mTLS mode is on.
	if ca, ok := f.(sts.ClientCAReloader); ok && cfg.Options["clientCAFile"] != "" {
		registerClientCAReloader(f.Name(), ca.ReloadClientCA)
	}
	return f, nil
}
