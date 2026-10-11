// config_federation_test.go — the zeta_federation config surface
// (accessstor appliance tree leaf 01 Task 1): JSON parse + DisallowUnknown
// fields inside the block, startup validation wiring through
// buildIdentityRegistry, restart-required patch classification, and
// persist round-trip.
package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/bhodgens/zeta-object/internal/auth"
)

// TestConfigFederationParse pins the zeta_federation JSON decoding and the
// unknown-key rejection inside the block (fail-loud, same posture as every
// other nested config object).
func TestConfigFederationParse(t *testing.T) {
	raw := `{"zeta_federation":{"mode":"oidc","issuerURL":"https://idp.example",
		"clientID":"zeta-object","audience":"zeta-object",
		"trustedCAs":"/etc/pki/idp.pem","groupsClaim":"roles","usernameClaim":"email"}}`
	var cfg ServerConfig
	if err := json.Unmarshal([]byte(raw), &cfg); err != nil {
		t.Fatalf("parse: %v", err)
	}
	fed := cfg.ZetaFederation
	if fed == nil {
		t.Fatal("zeta_federation lost in parse")
	}
	if fed.Mode != "oidc" || fed.IssuerURL != "https://idp.example" ||
		fed.ClientID != "zeta-object" || fed.Audience != "zeta-object" ||
		fed.TrustedCAs != "/etc/pki/idp.pem" || fed.GroupsClaim != "roles" ||
		fed.UsernameClaim != "email" {
		t.Errorf("zeta_federation fields wrong: %+v", fed)
	}

	// Unknown key inside the block aborts the parse.
	bad := `{"zeta_federation":{"mode":"oidc","issuerURL":"https://x","bogus":true}}`
	var cfg2 ServerConfig
	if err := json.Unmarshal([]byte(bad), &cfg2); err == nil {
		t.Fatal("unknown zeta_federation key accepted, want fail-loud")
	}

	// Legacy configs without the key are untouched (nil, zero impact).
	var cfg3 ServerConfig
	if err := json.Unmarshal([]byte(`{"dataDir":"./d/"}`), &cfg3); err != nil {
		t.Fatalf("legacy parse: %v", err)
	}
	if cfg3.ZetaFederation != nil {
		t.Errorf("absent zeta_federation = %+v, want nil", cfg3.ZetaFederation)
	}
}

// TestBuildIdentityRegistryFederationValidation pins the startup contract:
// a configured federation block is VALIDATED fail-loud inside
// buildIdentityRegistry (mode "saml" parses and fails with a
// broker-directed error; oidc with an unreachable issuer fails naming the
// discovery fetch) — and the env-only registry builds byte-identically
// when no federation block is configured.
func TestBuildIdentityRegistryFederationValidation(t *testing.T) {
	withIdentityEnv(t, "AKENV", "sk-env")
	loadCredentials()
	saved := *serverConfig()
	defer func() { setServerConfig(saved) }()

	t.Run("absent block: env-only, untouched", func(t *testing.T) {
		setServerConfig(defaultServerConfig())
		reg, err := buildIdentityRegistry()
		if err != nil {
			t.Fatalf("env-only build failed: %v", err)
		}
		if names := reg.Names(); len(names) != 1 || names[0] != "env" {
			t.Fatalf("Names() = %v, want [env]", names)
		}
	})

	t.Run("saml parses and fails broker-directed", func(t *testing.T) {
		setServerConfig(defaultServerConfig())
		setServerConfigFieldT(t, func(c *ServerConfig) {
			c.ZetaFederation = &auth.FederationConfig{
				Mode: "saml", IssuerURL: "https://idp.example", ClientID: "c",
			}
		})
		_, err := buildIdentityRegistry()
		if err == nil {
			t.Fatal("saml federation accepted, want broker-directed error")
		}
		if !strings.Contains(err.Error(), "broker") {
			t.Errorf("err = %v, want broker-directed", err)
		}
	})

	t.Run("oidc unreachable issuer fails loudly", func(t *testing.T) {
		setServerConfig(defaultServerConfig())
		setServerConfigFieldT(t, func(c *ServerConfig) {
			c.ZetaFederation = &auth.FederationConfig{
				Mode: "oidc", IssuerURL: "http://127.0.0.1:1", ClientID: "c",
			}
		})
		_, err := buildIdentityRegistry()
		if err == nil || !strings.Contains(err.Error(), "discovery") {
			t.Fatalf("err = %v, want discovery failure", err)
		}
	})
}

// TestConfigPatchFederationRestartRequired pins the patch classification:
// zeta_federation is a restart-required key (validate-at-startup rule; hot
// apply is explicitly NOT supported in this leaf), applied to the DESIRED
// config only, never live, and reported in restartRequired. The patch is
// VALIDATED (a real discovery fetch against a local IdP) but not applied.
func TestConfigPatchFederationRestartRequired(t *testing.T) {
	cfg := defaultServerConfig()
	cfg.DataDir = t.TempDir() + "/"
	store := NewConfigStore(&cfg)

	// A real (httptest) IdP so the patch passes validation; the point of
	// the test is the applied/restart-required classification, which is
	// independent of the issuer's reachability.
	idp := newFedTestIDPForPatch(t)
	patch := fmt.Sprintf(`{"zeta_federation":{"mode":"oidc","issuerURL":%q,"clientID":"c"}}`, idp.String())
	applied, restartRequired, err := store.Apply(ConfigPatch{JSON: []byte(patch)})
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if len(applied) != 0 {
		t.Errorf("applied = %v, want empty (federation is restart-required)", applied)
	}
	if !contains(restartRequired, "zeta_federation") {
		t.Errorf("restartRequired = %v, want zeta_federation", restartRequired)
	}
	// Running configuration keeps serving without federation...
	if store.Snapshot().ZetaFederation != nil {
		t.Error("restart-required federation value leaked into live config")
	}
	// ...while the desired config carries it for the next boot.
	if got := store.desired.ZetaFederation; got == nil || got.IssuerURL != idp.String() {
		t.Errorf("desired federation = %+v, want the pending value", got)
	}
}

// newFedTestIDPForPatch spins a minimal OIDC discovery server for the
// config-store patch tests (discovery only; no JWKS is fetched at this
// layer because NewFederationRegistry only validates discovery eagerly).
func newFedTestIDPForPatch(t *testing.T) *url.URL {
	t.Helper()
	mux := http.NewServeMux()
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	mux.HandleFunc("/.well-known/openid-configuration", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"issuer":   srv.URL,
			"jwks_uri": srv.URL + "/jwks",
		})
	})
	u, err := url.Parse(srv.URL)
	if err != nil {
		t.Fatalf("parse %s: %v", srv.URL, err)
	}
	return u
}

// TestConfigPatchFederationInvalidFails pins that an invalid federation
// patch (saml mode) is rejected, never stored — the store reuses the same
// fail-loud validation as startup.
func TestConfigPatchFederationInvalidFails(t *testing.T) {
	cfg := defaultServerConfig()
	cfg.DataDir = t.TempDir() + "/"
	store := NewConfigStore(&cfg)

	if _, _, err := store.Apply(ConfigPatch{JSON: []byte(
		`{"zeta_federation":{"mode":"saml","issuerURL":"https://idp.example","clientID":"c"}}`)}); err == nil {
		t.Fatal("saml federation patch accepted, want validation failure")
	}
	if store.Snapshot().ZetaFederation != nil {
		t.Error("rejected federation patch leaked into live config")
	}
}

// TestConfigFederationPersistRoundTrip pins that the desired federation
// block survives Persist → loadConfig byte-semantically (key names, all
// seven fields).
func TestConfigFederationPersistRoundTrip(t *testing.T) {
	cfg := defaultServerConfig()
	cfg.DataDir = t.TempDir() + "/"
	cfg.ZetaFederation = &auth.FederationConfig{
		Mode: "oidc", IssuerURL: "https://idp.example", ClientID: "zeta-object",
		Audience: "aud-1", TrustedCAs: "/pki.pem", GroupsClaim: "roles", UsernameClaim: "email",
	}
	data, err := marshalConfigDocument(&cfg)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var back ServerConfig
	if err := json.Unmarshal(data, &back); err != nil {
		t.Fatalf("round-trip parse: %v", err)
	}
	got := back.ZetaFederation
	if got == nil {
		t.Fatal("zeta_federation dropped by the persist document")
	}
	if got.Mode != "oidc" || got.IssuerURL != "https://idp.example" ||
		got.ClientID != "zeta-object" || got.Audience != "aud-1" ||
		got.TrustedCAs != "/pki.pem" || got.GroupsClaim != "roles" ||
		got.UsernameClaim != "email" {
		t.Errorf("round-trip lost fields: %+v", got)
	}
}
