// factory.go — FrontendConfig.Options → Config translation with fail-loud
// validation. This package OWNS these option keys (unknown keys abort
// startup, same contract as unknown frontend/backend types):
//
//	hostKeyFile       — SSH host key path (REQUIRED)
//	allowPasswordAuth — "true"/"false" (default "true" = pubkey + password)
package sftp

import (
	"crypto/ed25519"
	"fmt"
	"strconv"
	"strings"

	"github.com/bhodgens/zeta-object/internal/auth"
)

// IdentityRegistries is the narrow view ConfigFromOptions needs of the
// process identity registry: the IdentityRegistry (password seam) and the
// PublicKeyAuthenticator (public-key seam). It exists so package main can
// hand over its ReloadableRegistry (design-leaf 08) without this package
// depending on a concrete wrapper type — any registry exposing both seams
// fits.
type IdentityRegistries interface {
	Registry() interface {
		LookupByBasicCredential(username, password string) (auth.Identity, bool)
	}
	Keys() auth.PublicKeyAuthenticator
}

// KnownOptionKeys lists every option key this frontend accepts.
var KnownOptionKeys = map[string]bool{
	"hostKeyFile":       true,
	"allowPasswordAuth": true,
}

// ConfigFromOptions builds a Config from a frontends entry's options map.
// hostKeyFile is REQUIRED (clients pin host keys; a random temp key per
// boot would break pinning). The identity registry provides both auth
// adapters (password = LookupByBasicCredential, pubkey =
// AuthenticatePublicKey). Design-leaf 08: reg is the process-wide
// ReloadableRegistry — the adapters keep calling through it per
// connection, so SIGHUP swaps are visible to NEW sessions without a
// restart (existing sessions persist by design — per-connection auth).
func ConfigFromOptions(listenAddr string, options map[string]string, reg IdentityRegistries) (Config, error) {
	cfg := Config{
		ListenAddr:        listenAddr,
		AllowPasswordAuth: true, // default
	}
	if reg != nil && reg.Registry() != nil {
		cfg.Verifier = NewRegistryVerifier(reg.Registry())
		cfg.KeyChecker = NewRegistryKeyChecker(reg.Keys())
	}
	for k, v := range options {
		if !KnownOptionKeys[k] {
			return Config{}, fmt.Errorf("sftp: unknown option key %q (known: hostKeyFile, allowPasswordAuth)", k)
		}
		switch k {
		case "hostKeyFile":
			if strings.TrimSpace(v) == "" {
				return Config{}, fmt.Errorf("sftp: hostKeyFile must not be empty")
			}
			cfg.HostKeyFile = v
		case "allowPasswordAuth":
			b, err := strconv.ParseBool(v)
			if err != nil {
				return Config{}, fmt.Errorf("sftp: allowPasswordAuth %q must be \"true\" or \"false\"", v)
			}
			cfg.AllowPasswordAuth = b
		}
	}
	if cfg.HostKeyFile == "" {
		return Config{}, fmt.Errorf(`sftp: the "hostKeyFile" option is required (e.g. "certs/host_ed25519"; the key is generated there on first start)`)
	}
	return cfg, nil
}

// ensure the ed25519 import stays tied to the host-key generator contract.
var _ ed25519.PrivateKey
