package main

import (
	"net/http"
	"testing"
)

// Tests for the loadConfig frontends semantics live in config_frontend_test.go;
// this file covers the env-override seam: MINIS3_LISTEN_ADDR beats the config
// file for the DEFAULT listener only; per-frontend listenAddr values are
// untouched.
func TestApplyListenAddrOverride_EnvBeatsConfigDefaultListenerOnly(t *testing.T) {
	cfg := ServerConfig{
		ListenAddr: ":8443",
		Frontends: []FrontendConfig{
			{Type: "s3"},
			{Type: "webdav", ListenAddr: ":8444"},
		},
	}
	t.Setenv("MINIS3_LISTEN_ADDR", ":9999")
	applyListenAddrOverride(&cfg)
	if cfg.ListenAddr != ":9999" {
		t.Fatalf("ListenAddr = %q, want :9999 (env override)", cfg.ListenAddr)
	}
	if len(cfg.Frontends) != 2 || cfg.Frontends[1].ListenAddr != ":8444" {
		t.Fatalf("Frontends = %+v, want per-frontend listenAddr untouched", cfg.Frontends)
	}
}

// Absent env var leaves the config-file address in place.
func TestApplyListenAddrOverride_UnsetEnvNoop(t *testing.T) {
	cfg := ServerConfig{ListenAddr: ":8443"}
	applyListenAddrOverride(&cfg)
	if cfg.ListenAddr != ":8443" {
		t.Fatalf("ListenAddr = %q, want :8443 (unchanged)", cfg.ListenAddr)
	}
}

var _ = http.NewServeMux // silence unused-import churn if imports shift during TDD
