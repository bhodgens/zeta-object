package main

import (
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bhodgens/zeta-object/internal/auth"
)

// config_reload_test.go — design-leaf 08 (SIGHUP key rotation/revocation):
// the reload path reuses the exact startup sequence (loadConfig +
// buildIdentityRegistry), is fail-closed on ANY error, swaps atomically, and
// logs identity NAMES only (never secrets).

// writeReloadConfig writes a config.json with the given identities and
// returns its path.
func writeReloadConfig(t *testing.T, identitiesJSON string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.json")
	body := `{
  "dataDir": "` + t.TempDir() + `",
  "listenAddr": "127.0.0.1:0",
  "identities": ` + identitiesJSON + `
}`
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// reloadTestEnv installs a fresh ReloadableRegistry as the process reload
// target plus a valid startup config, restoring both afterwards.
func reloadTestEnv(t *testing.T, identitiesJSON string) {
	t.Helper()
	prevConfigPath := serverConfigPath
	prevRegistry := identityRegistry
	t.Cleanup(func() {
		serverConfigPath = prevConfigPath
		identityRegistry = prevRegistry
	})
	serverConfigPath = writeReloadConfig(t, identitiesJSON)
	identityRegistry = nil
	if err := loadConfig(serverConfigPath); err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	reg, err := buildIdentityRegistry()
	if err != nil {
		t.Fatalf("buildIdentityRegistry: %v", err)
	}
	identityRegistry = auth.NewReloadableRegistry(reg)
}

func TestReloadIdentityRegistry_SwapsInNewIdentities(t *testing.T) {
	reloadTestEnv(t, `[{"name":"one","accessKey":"ak-one","secretKey":"sk-one"}]`)
	if _, ok := identityRegistry.LookupByAccessKey("ak-two"); ok {
		t.Fatal("ak-two must not resolve before the reload")
	}
	// Operator adds a second identity on disk.
	if err := os.WriteFile(serverConfigPath, []byte(`{
  "identities": [
    {"name":"one","accessKey":"ak-one","secretKey":"sk-one"},
    {"name":"two","accessKey":"ak-two","secretKey":"sk-two"}
  ]
}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := reloadIdentityRegistry(); err != nil {
		t.Fatalf("reloadIdentityRegistry: %v", err)
	}
	if _, ok := identityRegistry.LookupByAccessKey("ak-two"); !ok {
		t.Fatal("ak-two must resolve after a successful reload")
	}
}

func TestReloadIdentityRegistry_RevokedKeyStopsResolving(t *testing.T) {
	reloadTestEnv(t, `[{"name":"one","accessKey":"ak-one","secretKey":"sk-one"}]`)
	if err := os.WriteFile(serverConfigPath, []byte(`{
  "identities": [{"name":"two","accessKey":"ak-two","secretKey":"sk-two"}]
}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := reloadIdentityRegistry(); err != nil {
		t.Fatalf("reloadIdentityRegistry: %v", err)
	}
	if _, ok := identityRegistry.LookupByAccessKey("ak-one"); ok {
		t.Fatal("revoked ak-one must not resolve after the reload")
	}
	if _, ok := identityRegistry.LookupByAccessKey("ak-two"); !ok {
		t.Fatal("ak-two must resolve after the reload")
	}
}

func TestReloadIdentityRegistry_FailClosedKeepsOldRegistry(t *testing.T) {
	reloadTestEnv(t, `[{"name":"one","accessKey":"ak-one","secretKey":"sk-one"}]`)
	if _, ok := identityRegistry.LookupByAccessKey("ak-one"); !ok {
		t.Fatal("precondition: ak-one resolves")
	}
	// Syntactically valid JSON, duplicate access key: the builder must
	// reject it and the reload must keep the OLD registry serving.
	if err := os.WriteFile(serverConfigPath, []byte(`{
  "identities": [
    {"name":"dup-one","accessKey":"ak-dup","secretKey":"sk-a"},
    {"name":"dup-two","accessKey":"ak-dup","secretKey":"sk-b"}
  ]
}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := reloadIdentityRegistry(); err == nil {
		t.Fatal("reload of a duplicate-accessKey config must fail")
	}
	if _, ok := identityRegistry.LookupByAccessKey("ak-one"); !ok {
		t.Fatal("old registry must keep serving after a failed reload")
	}
	if _, ok := identityRegistry.LookupByAccessKey("ak-dup"); ok {
		t.Fatal("the rejected config must not leak into the serving registry")
	}
}

func TestReloadIdentityRegistry_TornFileKeepsOldRegistry(t *testing.T) {
	reloadTestEnv(t, `[{"name":"one","accessKey":"ak-one","secretKey":"sk-one"}]`)
	// Editor torn mid-write: truncated JSON.
	if err := os.WriteFile(serverConfigPath, []byte(`{"identities": [{"name":"`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := reloadIdentityRegistry(); err == nil {
		t.Fatal("reload of a torn JSON file must fail")
	}
	if _, ok := identityRegistry.LookupByAccessKey("ak-one"); !ok {
		t.Fatal("old registry must keep serving after a torn-file reload")
	}
}

func TestReloadIdentityRegistry_ConcurrentReloadsAndLookups(t *testing.T) {
	reloadTestEnv(t, `[{"name":"one","accessKey":"ak-one","secretKey":"sk-one"}]`)
	// One goroutine hammers reloads (alternating valid / torn files),
	// several hammer lookups. Race detector must stay quiet.
	stop := make(chan struct{})
	var wg sync.WaitGroup
	wg.Go(func() {
		bodies := []string{
			`{"identities": [{"name":"one","accessKey":"ak-one","secretKey":"sk-one"}]}`,
			`{"identities": [{"name":"one","accessKey":"ak-one","secretKey":"sk-one"},{"name":"two","accessKey":"ak-two","secretKey":"sk-two"}]}`,
			`{"identities": [`,
		}
		for i := 0; ; i++ {
			select {
			case <-stop:
				return
			default:
			}
			os.WriteFile(serverConfigPath, []byte(bodies[i%len(bodies)]), 0o600)
			// The outcome itself is irrelevant (torn files fail closed);
			// only the race between Swap and the lookups is under test.
			_ = reloadIdentityRegistry()
		}
	})
	for range 4 {
		wg.Go(func() {
			for {
				select {
				case <-stop:
					return
				default:
					identityRegistry.LookupByAccessKey("ak-one")
					identityRegistry.LookupByBasicCredential("ak-one", "sk-one")
					identityRegistry.SecretKey("ak-one")
				}
			}
		})
	}
	time.Sleep(300 * time.Millisecond)
	close(stop)
	wg.Wait()
}

// captureReloadLogs redirects the std logger while fn runs and returns
// everything it logged.
func captureReloadLogs(t *testing.T, fn func()) string {
	t.Helper()
	prev := log.Writer()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	log.SetOutput(w)
	done := make(chan string, 1)
	go func() {
		buf := make([]byte, 4096)
		var sb strings.Builder
		for {
			n, err := r.Read(buf)
			sb.Write(buf[:n])
			if err != nil {
				done <- sb.String()
				return
			}
		}
	}()
	fn()
	log.SetOutput(prev)
	w.Close()
	return <-done
}

func TestReloadIdentityRegistry_LogsNamesNotSecrets(t *testing.T) {
	reloadTestEnv(t, `[{"name":"one","accessKey":"ak-one","secretKey":"sk-one"}]`)
	if err := os.WriteFile(serverConfigPath, []byte(`{
  "identities": [{"name":"two","accessKey":"ak-two","secretKey":"super-secret-value"}]
}`), 0o600); err != nil {
		t.Fatal(err)
	}
	var reloadErr error
	logs := captureReloadLogs(t, func() {
		reloadErr = reloadIdentityRegistry()
	})
	if reloadErr != nil {
		t.Fatalf("reloadIdentityRegistry: %v", reloadErr)
	}
	if !strings.Contains(logs, "now serving [env two]") {
		t.Errorf("reload log must name identities (log-safe), got: %s", logs)
	}
	if strings.Contains(logs, "super-secret-value") {
		t.Errorf("reload log leaked a secret: %s", logs)
	}
}

func TestReloadIdentityRegistry_FailClosedLogNamesOffender(t *testing.T) {
	reloadTestEnv(t, `[{"name":"one","accessKey":"ak-one","secretKey":"sk-one"}]`)
	if err := os.WriteFile(serverConfigPath, []byte(`{
  "identities": [
    {"name":"dup-one","accessKey":"ak-dup","secretKey":"sk-a"},
    {"name":"dup-two","accessKey":"ak-dup","secretKey":"sk-b"}
  ]
}`), 0o600); err != nil {
		t.Fatal(err)
	}
	var reloadErr error
	logs := captureReloadLogs(t, func() {
		reloadErr = reloadIdentityRegistry()
	})
	if reloadErr == nil {
		t.Fatal("duplicate-accessKey reload must fail")
	}
	if !strings.Contains(logs, "auth identity reload failed, keeping previous registry") {
		t.Errorf("missing fail-closed log line: %s", logs)
	}
	if !strings.Contains(logs, "configured more than once") {
		t.Errorf("log must carry the validator's named-offender error: %s", logs)
	}
}
