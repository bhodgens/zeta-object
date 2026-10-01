package auth

import (
	"sync"
	"testing"
)

// reload_test.go — design-leaf 08 Contract 1 unit locks: swap visibility,
// delegation of all four methods (LookupByAccessKey / LookupByBasicCredential
// / LookupByPublicKey / SecretKey), the nil-swap contract, and a concurrent
// swap+lookup race run under -race (the design's risk-table mitigation).

// reloadFakeRegistry answers canned hits and records delegation targets.
// NOT safe for concurrent use — the concurrency test uses reloadStaticRegistry.
type reloadFakeRegistry struct {
	hitAccessKey string
	hitUser      string
	hitPassword  string
	hitKeyLine   string
	// delegation records (who the wrapper called, with what)
	sawAccessKey   string
	sawUser        string
	sawPassword    string
	sawKeyLine     string
	sawSecretKeyID string
}

func (f *reloadFakeRegistry) LookupByAccessKey(accessKeyID string) (Identity, bool) {
	f.sawAccessKey = accessKeyID
	if accessKeyID == f.hitAccessKey {
		return Identity{AccessKeyID: accessKeyID}, true
	}
	return Identity{}, false
}

func (f *reloadFakeRegistry) LookupByBasicCredential(username, password string) (Identity, bool) {
	f.sawUser, f.sawPassword = username, password
	if username == f.hitUser && password == f.hitPassword {
		return Identity{AccessKeyID: username}, true
	}
	return Identity{}, false
}

func (f *reloadFakeRegistry) LookupByPublicKey(authorizedKey string) (Identity, bool) {
	f.sawKeyLine = authorizedKey
	if authorizedKey == f.hitKeyLine {
		return Identity{AccessKeyID: "key-identity"}, true
	}
	return Identity{}, false
}

func (f *reloadFakeRegistry) SecretKey(accessKeyID string) (string, bool) {
	f.sawSecretKeyID = accessKeyID
	if accessKeyID == f.hitAccessKey {
		return "sk", true
	}
	return "", false
}

// reloadStaticRegistry is a stateless (concurrency-safe) fake: fixed hits,
// no recorded fields. The -race hammer uses this so the only unsynchronized
// memory left is the wrapper's own inner field — exactly what's under test.
type reloadStaticRegistry struct{}

func (reloadStaticRegistry) LookupByAccessKey(accessKeyID string) (Identity, bool) {
	if accessKeyID == "ak" {
		return Identity{AccessKeyID: accessKeyID}, true
	}
	return Identity{}, false
}

func (reloadStaticRegistry) LookupByBasicCredential(username, password string) (Identity, bool) {
	if username == "ak" && password == "sk" {
		return Identity{AccessKeyID: username}, true
	}
	return Identity{}, false
}

func (reloadStaticRegistry) LookupByPublicKey(authorizedKey string) (Identity, bool) {
	if authorizedKey == "ssh-ed25519 AAAA" {
		return Identity{AccessKeyID: "key-identity"}, true
	}
	return Identity{}, false
}

func (reloadStaticRegistry) SecretKey(accessKeyID string) (string, bool) {
	if accessKeyID == "ak" {
		return "sk", true
	}
	return "", false
}

// newTestReloadable builds a wrapper over a recording fake keyed to the
// canonical hits and hands both back.
func newTestReloadable(t *testing.T) (*ReloadableRegistry, *reloadFakeRegistry) {
	t.Helper()
	inner := &reloadFakeRegistry{
		hitAccessKey: "ak",
		hitUser:      "ak",
		hitPassword:  "sk",
		hitKeyLine:   "ssh-ed25519 AAAA",
	}
	return NewReloadableRegistry(inner), inner
}

func TestReloadableRegistry_InitialInnerIsStartupBuild(t *testing.T) {
	wrapper, inner := newTestReloadable(t)
	if wrapper.Swap(inner) != IdentityRegistry(inner) {
		t.Fatal("swapping the construction-time inner back must return it as prev")
	}
}

func TestReloadableRegistry_DelegatesAllFourMethods(t *testing.T) {
	wrapper, inner := newTestReloadable(t)

	id, ok := wrapper.LookupByAccessKey("ak")
	if !ok || id.AccessKeyID != "ak" {
		t.Fatalf("LookupByAccessKey = (%v, %v)", id, ok)
	}
	if inner.sawAccessKey != "ak" {
		t.Fatalf("LookupByAccessKey did not delegate (saw %q)", inner.sawAccessKey)
	}

	id, ok = wrapper.LookupByBasicCredential("ak", "sk")
	if !ok || id.AccessKeyID != "ak" {
		t.Fatalf("LookupByBasicCredential = (%v, %v)", id, ok)
	}
	if inner.sawUser != "ak" || inner.sawPassword != "sk" {
		t.Fatalf("LookupByBasicCredential did not delegate (saw %q/%q)", inner.sawUser, inner.sawPassword)
	}

	id, ok = wrapper.LookupByPublicKey("ssh-ed25519 AAAA")
	if !ok || id.AccessKeyID != "key-identity" {
		t.Fatalf("LookupByPublicKey = (%v, %v)", id, ok)
	}
	if inner.sawKeyLine != "ssh-ed25519 AAAA" {
		t.Fatalf("LookupByPublicKey did not delegate (saw %q)", inner.sawKeyLine)
	}

	secret, ok := wrapper.SecretKey("ak")
	if !ok || secret != "sk" {
		t.Fatalf("SecretKey = (%q, %v)", secret, ok)
	}
	if inner.sawSecretKeyID != "ak" {
		t.Fatalf("SecretKey did not delegate (saw %q)", inner.sawSecretKeyID)
	}

	// Misses delegate too and stay misses.
	if _, ok := wrapper.LookupByAccessKey("miss"); ok {
		t.Fatal("miss must stay a miss")
	}
	if _, ok := wrapper.LookupByBasicCredential("miss", "miss"); ok {
		t.Fatal("basic miss must stay a miss")
	}
	if _, ok := wrapper.LookupByPublicKey("malformed"); ok {
		t.Fatal("public-key miss must stay a miss")
	}
	if _, ok := wrapper.SecretKey("miss"); ok {
		t.Fatal("secret miss must stay a miss")
	}
}

func TestReloadableRegistry_SwapIsImmediatelyVisible(t *testing.T) {
	wrapper, old := newTestReloadable(t)
	if _, ok := wrapper.LookupByAccessKey("ak"); !ok {
		t.Fatal("precondition: old inner resolves ak")
	}

	// The fresh registry answers a DIFFERENT key, so the post-swap probes
	// can distinguish which inner served them.
	fresh := &reloadFakeRegistry{hitAccessKey: "ak-new", hitUser: "ak-new", hitPassword: "sk-new", hitKeyLine: "ssh-rsa BBBB"}
	prev := wrapper.Swap(fresh)
	if prev != IdentityRegistry(old) {
		t.Fatal("Swap must return the PREVIOUS inner registry")
	}
	// A swap is observable on the very next call (per-call delegation).
	if _, ok := wrapper.LookupByAccessKey("ak"); ok {
		t.Fatal("post-swap lookup still hit the OLD inner registry")
	}
	if _, ok := wrapper.LookupByAccessKey("ak-new"); !ok {
		t.Fatal("post-swap lookup missed the NEW inner registry")
	}
	secret, ok := wrapper.SecretKey("ak-new")
	if !ok || secret != "sk" {
		t.Fatalf("post-swap SecretKey = (%q, %v)", secret, ok)
	}
	if _, ok := wrapper.LookupByBasicCredential("ak-new", "sk-new"); !ok {
		t.Fatal("post-swap basic lookup missed the NEW inner registry")
	}
	if _, ok := wrapper.LookupByPublicKey("ssh-rsa BBBB"); !ok {
		t.Fatal("post-swap public-key lookup missed the NEW inner registry")
	}
	// Swapping back restores the old answers.
	wrapper.Swap(old)
	if _, ok := wrapper.LookupByAccessKey("ak"); !ok {
		t.Fatal("swap-back must restore the previous behavior")
	}
	if _, ok := wrapper.LookupByAccessKey("ak-new"); ok {
		t.Fatal("swap-back must retire the interim registry's key")
	}
}

func TestReloadableRegistry_NilSwapPanics(t *testing.T) {
	wrapper, _ := newTestReloadable(t)
	defer func() {
		if recover() == nil {
			t.Fatal("Swap(nil) must panic — a nil inner is a wiring bug")
		}
	}()
	wrapper.Swap(nil)
}

func TestReloadableRegistry_SatisfiesFrozenSeams(t *testing.T) {
	// The compile-time assertions live in reload.go; this test fails to
	// compile if either seam ever drifts (belt and suspenders, mirroring
	// registry_test.go's pin for MultiRegistry).
	var _ IdentityRegistry = (*ReloadableRegistry)(nil)
	var _ CredentialSource = (*ReloadableRegistry)(nil)
}

// TestReloadableRegistry_ConcurrentSwapAndLookups is the design's risk-table
// mitigation: run under -race and hammer Swap + every delegated lookup from
// several goroutines. The fakes are stateless, so any race reported is the
// wrapper's.
func TestReloadableRegistry_ConcurrentSwapAndLookups(t *testing.T) {
	wrapper := NewReloadableRegistry(reloadStaticRegistry{})
	stop := make(chan struct{})
	var wg sync.WaitGroup
	wg.Go(func() {
		for {
			select {
			case <-stop:
				return
			default:
				wrapper.Swap(reloadStaticRegistry{})
			}
		}
	})
	for range 4 {
		wg.Go(func() {
			for {
				select {
				case <-stop:
					return
				default:
					wrapper.LookupByAccessKey("ak")
					wrapper.LookupByBasicCredential("ak", "sk")
					wrapper.LookupByPublicKey("ssh-ed25519 AAAA")
					wrapper.SecretKey("ak")
				}
			}
		})
	}
	// Bounded hammer from the test goroutine so -race runs stay
	// deterministic even on loaded machines.
	for range 20000 {
		wrapper.LookupByAccessKey("ak")
	}
	close(stop)
	wg.Wait()
}
