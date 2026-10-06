// serve_deadcode_test.go — the h3 package must not expose a nil-returning
// TLS-config helper.
//
// ListenerTLSConfig() *tls.Config discarded the error from the real
// TLSConfig() and could therefore hand a nil *tls.Config to any caller that
// passed it to tls.NewListener / quic.ListenEarly — a nil config reaching
// the listener is exactly the crash class this repo already paid for once.
// It had NO callers anywhere in the repo (the sole QUIC-serving path is
// h3.Frontend.Listen, which propagates the error, and the seam interface
// frontend.QUICListenerFrontend already declares TLSConfig() (*tls.Config,
// error)), so it was pure sharp-edged surface.
//
// The guard is a source-level pin because "a symbol does not exist" cannot be
// expressed as a compile-time reference: any mention of it anywhere in the
// module - production or test code - fails this test.
package h3

import (
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// deadListenerTLSConfig is the removed helper's name, kept here so the guard
// has something to look for.
const deadListenerTLSConfig = "ListenerTLSConfig"

// TestNoNilReturningListenerTLSConfig pins that no source file in the module
// declares, references or documents the removed nil-returning helper. It
// scans every .go file except this one (which necessarily spells the name).
func TestNoNilReturningListenerTLSConfig(t *testing.T) {
	root := repoRoot(t)
	self, err := filepath.Abs(filepath.Join(pkgDir(t), "serve_deadcode_test.go"))
	if err != nil {
		t.Fatal(err)
	}
	var offenders []string
	err = filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			switch d.Name() {
			case ".git", "testdata", "vendor", "node_modules", "coverage":
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") {
			return nil
		}
		abs, err := filepath.Abs(path)
		if err != nil {
			return err
		}
		if abs == self {
			return nil
		}
		src, err := os.ReadFile(path) //nolint:gosec // G304: repo-local source walk.
		if err != nil {
			return err
		}
		if strings.Contains(string(src), deadListenerTLSConfig) {
			offenders = append(offenders, abs)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walking module sources: %v", err)
	}
	if len(offenders) > 0 {
		t.Fatalf("%s still appears in %d file(s):\n  %s\nthe nil-returning TLS-config helper must stay deleted (a caller would hand a nil *tls.Config to the QUIC listener)",
			deadListenerTLSConfig, len(offenders), strings.Join(offenders, "\n  "))
	}
}

// TestTLSConfig_NeverReturnsNilConfig pins the surviving contract: the only
// way to get the listener's TLS config is TLSConfig(), and a cert-load
// failure is an ERROR, never a nil config. This is the shape the deleted
// helper used to flatten.
func TestTLSConfig_NeverReturnsNilConfig(t *testing.T) {
	env := newH3TestEnv(t)
	f, err := New(newH3MemBackend(), env.validConfig(), env.registry, nil)
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := f.TLSConfig()
	if err != nil {
		t.Fatalf("TLSConfig on a valid pair: %v", err)
	}
	if cfg == nil {
		t.Fatal("TLSConfig returned (nil, nil): a nil config would crash the QUIC listener")
	}
	// Cert-load failure: an error, and no config at all.
	bad := env.validConfig()
	bad.CertFile = "no-such-cert.pem"
	bad.KeyFile = "no-such-key.pem"
	bf, err := New(newH3MemBackend(), bad, env.registry, nil)
	if err != nil {
		t.Fatal(err)
	}
	cfg, err = bf.TLSConfig()
	if err == nil {
		t.Fatalf("TLSConfig with a missing cert pair = nil error, want an error (config = %v)", cfg)
	}
	if cfg != nil {
		t.Fatalf("TLSConfig error path also returned a config: %v", cfg)
	}
	if !strings.Contains(err.Error(), "no-such-cert.pem") {
		t.Fatalf("error = %v, want it to name the missing cert path", err)
	}
}

// repoRoot walks up from the test's directory to the module root (go.mod).
func repoRoot(t *testing.T) string {
	t.Helper()
	dir := pkgDir(t)
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatalf("no go.mod above %s", dir)
		}
		dir = parent
	}
}

// pkgDir returns the h3 package directory (the working directory Go runs
// package tests in). It is checked for a package clause so a mis-set working
// directory fails loudly instead of scanning the wrong tree.
func pkgDir(t *testing.T) string {
	t.Helper()
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := parser.ParseFile(token.NewFileSet(), filepath.Join(wd, "serve.go"), nil, parser.PackageClauseOnly); err != nil {
		t.Fatalf("package directory %s does not look like the h3 package: %v", wd, err)
	}
	return wd
}
