package auth

import (
	"context"
	"fmt"
	"net/http"
)

// IssuerURL returns the issuer this registry was configured with. The
// console-OIDC wiring (accessstor appliance leaf 03) needs it to fetch
// the IdP discovery document; the config field itself is unexported.
func (r *FederationRegistry) IssuerURL() string {
	return r.cfg.IssuerURL
}

// FetchDiscoveryDocument retrieves and normalizes the IdP discovery
// document for issuerURL: authorization_endpoint, token_endpoint, and
// issuer (echoed back so callers can re-verify). Exported for the
// console-OIDC wiring; the registry's own startup path uses the
// unexported fetchDiscovery with stricter requirements (jwks_uri).
func FetchDiscoveryDocument(ctx context.Context, client *http.Client, issuerURL string) (authorizationEndpoint, tokenEndpoint, issuer string, err error) {
	raw, err := fetchDiscovery(ctx, client, issuerURL)
	if err != nil {
		return "", "", "", err
	}
	authz, _ := raw["authorization_endpoint"].(string)
	token, _ := raw["token_endpoint"].(string)
	iss, _ := raw["issuer"].(string)
	if authz == "" || token == "" {
		return "", "", "", fmt.Errorf("discovery for %s: missing authorization_endpoint or token_endpoint", issuerURL)
	}
	return authz, token, iss, nil
}
