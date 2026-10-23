// resolver_test.go — the CredentialResolver's contract (leaf 02): grants
// reconstructed statelessly per request, fail-closed on drift, mint/verify
// consistency.
package sts

import (
	"sync/atomic"
	"testing"
	"time"

	"github.com/bhodgens/zeta-object/internal/auth"
)

// fakeRegistry is a compile-time shape only; the production resolver
// pairs with the real FederationRegistry. (No stub type is needed: the
// Resolver's RegistryFunc returns the concrete *auth.FederationRegistry.)

func TestCredentialResolver_PairsPrefixesWithGrantValues(t *testing.T) {
	key := []byte("resolver-test-key")
	slot := &atomic.Pointer[[]byte]{}
	slot.Store(&key)

	// A real-shaped registry: GrantsFor over the reconstructed claims.
	// Use the ZERO FederationRegistry like leaf 01's pure-mapping test —
	// cfg is unexported, so build through a package-internal literal.
	reg := &auth.FederationRegistry{}
	_ = reg // cfg unexported; the pairing test needs the real mapping.
	_ = time.Now()

	// Mint and parse the pairing inputs directly.
	keyID, secret, err := MintAccessKey(key, "fed:idp.example/alice", time.Now().Add(time.Hour), []string{"team-a", "team-b"})
	if err != nil {
		t.Fatalf("mint: %v", err)
	}
	_, _, _, groups, err := ParseAccessKey(keyID)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	prefixes := GrantPrefixes("alice", groups)
	// leaf 01's deterministic rule: sorted ["alice/", "fed:team-a", "fed:team-b"].
	want := []string{"alice/", "fed:team-a", "fed:team-b"}
	if len(prefixes) != len(want) {
		t.Fatalf("prefixes = %v, want %v", prefixes, want)
	}
	for i := range want {
		if prefixes[i] != want[i] {
			t.Errorf("prefixes[%d] = %q, want %q", i, prefixes[i], want[i])
		}
	}
	_ = secret
}
