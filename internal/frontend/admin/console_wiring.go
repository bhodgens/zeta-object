package admin

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/bhodgens/zeta-object/internal/auth"
	"github.com/bhodgens/zeta-object/internal/frontend"
)

// ConsoleOIDCWiring is the exported, config-shaped surface package main
// uses to attach console-OIDC federation (leaf 03) to a built admin
// frontend. It mirrors federationDeps without exposing the unexported
// type. IssuerURL comes from the live federation registry.
type ConsoleOIDCWiring struct {
	Enabled          bool
	SessionTTL       time.Duration // 0 = defaultSessionTTL
	ClientSecretFile string        // PATH reference; never inline secrets
	ClientID         string        // mirrors zeta_federation.clientID
}

// AttachConsoleOIDC validates the wiring, resolves the client secret,
// fetches the IdP discovery document, and attaches the flow to f.
// An absent/empty ClientSecretFile or a failed discovery fetch is a
// fail-loud construction error. The registry must be the live
// federation registry (federationRegistryFor in package main).
func AttachConsoleOIDC(ctx context.Context, f frontend.Frontend, client *http.Client, reg *auth.FederationRegistry, w ConsoleOIDCWiring) error {
	if !w.Enabled {
		return nil
	}
	if reg == nil {
		return fmt.Errorf("admin: console oidc enabled but no zeta_federation registry is configured")
	}
	if w.ClientSecretFile == "" {
		return fmt.Errorf("admin: console oidc enabled but client_secret_file is empty")
	}
	secretRaw, err := os.ReadFile(w.ClientSecretFile)
	if err != nil {
		return fmt.Errorf("admin: read client_secret_file: %w", err)
	}
	secret := strings.TrimSpace(string(secretRaw))
	if secret == "" {
		return fmt.Errorf("admin: client_secret_file %q is empty", w.ClientSecretFile)
	}
	authz, token, issuer, err := auth.FetchDiscoveryDocument(ctx, client, reg.IssuerURL())
	if err != nil {
		return fmt.Errorf("admin: oidc discovery: %w", err)
	}
	deps := federationDeps{
		Cfg: ConsoleOIDCConfig{
			Enabled:          true,
			SessionTTL:       w.SessionTTL,
			ClientSecretFile: w.ClientSecretFile,
			ClientID:         w.ClientID,
		},
		Registry:  reg,
		Discovery: &oidcDiscovery{AuthorizationEndpoint: authz, TokenEndpoint: token, Issuer: issuer},
		Secret:    secret,
	}
	return NewConsoleOIDC(f, &deps, nil)
}
