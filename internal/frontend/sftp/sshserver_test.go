// sshserver_test.go — grant serialization determinism (T5 follow-up).
package sftp

import (
	"testing"

	"github.com/bhodgens/zeta-object/internal/auth"
)

// T5: grant serialization is deterministic (sorted) — the map range order
// used to make the CriticalOptions string vary run to run.
func TestRenderGrantsSorted(t *testing.T) {
	id := auth.Identity{AccessKeyID: "k", BucketGrants: map[string]auth.Grant{
		"zeta":  {Read: true, Write: true},
		"alpha": {Read: true},
		"*":     {Read: true, Write: true},
	}}
	got := renderGrants(id)
	want := "*=rw,alpha=r,zeta=rw"
	if got != want {
		t.Fatalf("renderGrants = %q, want %q", got, want)
	}
	id2 := auth.Identity{AccessKeyID: "k", BucketGrants: map[string]auth.Grant{
		"alpha": {Read: true},
		"zeta":  {Read: true, Write: true},
		"*":     {Read: true, Write: true},
	}}
	if renderGrants(id2) != want {
		t.Fatalf("renderGrants not deterministic: %q", renderGrants(id2))
	}
}
