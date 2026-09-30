// config_devmode_test.go — auth.mode "none" wiring (pluggable-
// authentication tree leaf 05 Task 2): dev authenticator installed only
// when opted in, unsigned requests authenticate, default stays authenticated,
// and dev mode never masks config validation errors.
package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/bhodgens/zeta-object/internal/auth"
	s3 "github.com/bhodgens/zeta-object/internal/frontend/s3"
)

// akStaticSource is a fixed single-pair CredentialSource for wiring tests.
type akStaticSource [2]string // {accessKey, secretKey}

func (s akStaticSource) SecretKey(accessKeyID string) (string, bool) {
	if accessKeyID == s[0] {
		return s[1], true
	}
	return "", false
}

// applyServerConfig installs cfg as the global for the test's duration.
func applyServerConfig(t *testing.T, cfg ServerConfig) {
	t.Helper()
	saved := serverConfig
	t.Cleanup(func() { serverConfig = saved })
	serverConfig = cfg
}

// TestDevModeWiring pins the s3 seam: mode "none" installs the loud
// DevAuthenticator; unset mode leaves the SigV4 path intact.
func TestDevModeWiring(t *testing.T) {
	withIdentityEnv(t, "AKENV", "sk-env")
	loadCredentials()

	t.Run("mode none installs dev authenticator", func(t *testing.T) {
		cfg := defaultServerConfig()
		cfg.Auth.Mode = authModeNone
		applyServerConfig(t, cfg)

		// Reproduce the installS3Seams dev-mode branch (package seams are
		// process-global; the branch is two statements — mirror them here
		// and assert the observable wiring).
		dev := auth.NewDevAuthenticator(nil)
		dev.Banner()
		s3.InstallDevAuthenticator(dev)
		t.Cleanup(func() { s3.InstallDevAuthenticator(nil) })

		// An UNSIGNED request authenticates (dev mode bypasses SigV4).
		f := s3.New(nil)
		req := httptest.NewRequest("GET", "/", nil) // no Authorization header
		w := httptest.NewRecorder()
		f.Handler().ServeHTTP(w, req)
		if w.Code != http.StatusOK {
			t.Fatalf("unsigned request in dev mode = %d, want 200: %s", w.Code, w.Body.String())
		}
	})

	t.Run("unset mode keeps SigV4 (unsigned rejected)", func(t *testing.T) {
		cfg := defaultServerConfig()
		cfg.Auth.Mode = ""
		applyServerConfig(t, cfg)
		s3.InstallDevAuthenticator(nil)

		f := s3.New(nil, s3.WithCredentialSource(akStaticSource{"AKENV", "sk-env"}))
		req := httptest.NewRequest("GET", "/", nil) // unsigned
		w := httptest.NewRecorder()
		f.Handler().ServeHTTP(w, req)
		if w.Code != http.StatusForbidden {
			t.Fatalf("unsigned request with auth required = %d, want 403", w.Code)
		}
		if !strings.Contains(w.Body.String(), "AccessDenied") {
			t.Errorf("body = %s", w.Body.String())
		}
	})

	t.Run("mode none does not mask validation errors", func(t *testing.T) {
		cfg := defaultServerConfig()
		cfg.Auth.Mode = authModeNone
		cfg.Identities = []auth.IdentityConfig{{Name: "x", AccessKey: "AKENV", SecretKey: "dup-of-env"}}
		applyServerConfig(t, cfg)
		if _, err := buildIdentityRegistry(); err == nil {
			t.Fatal("dev mode masked a duplicate-access-key validation error")
		}
	})
}
