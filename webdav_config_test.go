// webdav_config_test.go — webdav-2026-09 leaf 01 Task 2 (config wiring)
// + leaf 04 Task 3 (auth adapter wiring): the "bucket" key, fail-loud
// unknown keys, factory round-trip, and the Basic authenticator injection.
package main

import (
	"strings"
	"testing"

	"github.com/bhodgens/zeta-object/internal/auth"
)

func TestLoadConfig_WebdavBucketKey(t *testing.T) {
	cfg := loadConfigForTest(t, writeTempConfig(t,
		`{"frontends":[{"type":"webdav","listenAddr":":8444","bucket":"photos"}]}`))
	if len(cfg.Frontends) != 1 {
		t.Fatalf("Frontends = %+v", cfg.Frontends)
	}
	fc := cfg.Frontends[0]
	if fc.Type != "webdav" || fc.ListenAddr != ":8444" || fc.Bucket != "photos" {
		t.Fatalf("entry = %+v", fc)
	}
}

func TestLoadConfig_WebdavUnknownKeyFailsLoud(t *testing.T) {
	if err := loadConfig(writeTempConfig(t,
		`{"frontends":[{"type":"webdav","bogus":1}]}`)); err == nil {
		t.Fatal("unknown key in a webdav entry must abort the parse")
	} else if !strings.Contains(err.Error(), "bogus") {
		t.Fatalf("error must name the key: %v", err)
	}
}

func TestLoadConfig_WebdavWhitespaceBucketFailsAtConstruction(t *testing.T) {
	// Parsing accepts it (JSON is valid); the webdav constructor must
	// reject the whitespace-only bucket (deliberate-mode rule).
	if err := loadConfig(writeTempConfig(t,
		`{"frontends":[{"type":"webdav","bucket":" "}]}`)); err != nil {
		t.Fatalf("parse: %v", err)
	}
	prev := identityRegistry
	reg, err := auth.NewMultiRegistry([]auth.IdentityConfig{{Name: "t", AccessKey: "ak", SecretKey: "sk"}})
	if err != nil {
		t.Fatal(err)
	}
	identityRegistry = reg
	defer func() { identityRegistry = prev }()
	if _, _, err := buildFrontends(serverConfig.Frontends, nilBackend{}, stubCreds{}); err == nil {
		t.Fatal("whitespace-only bucket must fail the webdav constructor")
	}
}

func TestBuildFrontends_WebdavAuthenticatorIsBasicAdapter(t *testing.T) {
	prev := identityRegistry
	reg, err := auth.NewMultiRegistry([]auth.IdentityConfig{{Name: "t", AccessKey: "ak", SecretKey: "sk"}})
	if err != nil {
		t.Fatal(err)
	}
	identityRegistry = reg
	defer func() { identityRegistry = prev }()

	freg, _, err := buildFrontends([]FrontendConfig{{Type: "webdav", Bucket: "b"}}, nilBackend{}, stubCreds{})
	if err != nil {
		t.Fatalf("buildFrontends: %v", err)
	}
	f, ok := freg.Lookup("webdav")
	if !ok {
		t.Fatal("Lookup(webdav) missed")
	}
	if f.Name() != "webdav" {
		t.Fatalf("Name = %q", f.Name())
	}
	// The injected authenticator is the auth tree's BasicAuthenticator.
	if _, isBasic := f.Authenticator().(*auth.BasicAuthenticator); !isBasic {
		t.Fatalf("Authenticator = %T, want *auth.BasicAuthenticator", f.Authenticator())
	}
	// Mode B: Buckets capability off.
	if f.Capabilities().Buckets {
		t.Fatal("mode B must not advertise Buckets")
	}
}
