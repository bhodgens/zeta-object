package ftp

import (
	"bytes"
	"testing"
	"time"

	ftpclient "github.com/jlaffaye/ftp"

	"github.com/bhodgens/zeta-object/internal/auth"
)

// readOnlyVerifier returns an identity with Read-only grants on bucket "b"
// and no grant on bucket "c" (leaf 05 authorization matrix).
type readOnlyVerifier struct {
	writeBucket string // second bucket with write grant ("" = none)
}

func (v *readOnlyVerifier) Verify(user, password string) (auth.Identity, bool) {
	if user != "ro" || password != "pass" {
		return auth.Identity{}, false
	}
	grants := map[string]auth.Grant{
		"b": {Read: true},
		"c": {}, // explicit no-grant bucket
	}
	if v.writeBucket != "" {
		grants[v.writeBucket] = auth.Grant{Read: true, Write: true}
	}
	return auth.Identity{AccessKeyID: "ro", BucketGrants: grants}, true
}

// TestAuthz_ReadOnlyIdentity — RETR/LIST OK; STOR/DELE → 550.
func TestAuthz_ReadOnlyIdentity(t *testing.T) {
	be := newRecordingBackend()
	be.objects["b/data.txt"] = []byte("data")
	_, port := startTestServer(t, be, Config{Verifier: &readOnlyVerifier{}})
	c, err := ftpclient.Dial("127.0.0.1:"+itoa(port), ftpclient.DialWithTimeout(5*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	defer c.Quit()
	if err := c.Login("ro", "pass"); err != nil {
		t.Fatalf("login: %v", err)
	}

	// Read path OK.
	if r, err := c.Retr("b/data.txt"); err != nil {
		t.Fatalf("RETR (read-only): %v", err)
	} else {
		_ = r.Close()
	}
	if _, err := c.List("b"); err != nil {
		t.Fatalf("LIST (read-only): %v", err)
	}

	// Write paths → 550.
	if err := c.Stor("b/new.txt", bytes.NewReader(nil)); err == nil {
		t.Fatal("STOR on read-only bucket must fail (550)")
	}
	if err := c.Delete("b/data.txt"); err == nil {
		t.Fatal("DELE on read-only bucket must fail (550)")
	}
	if err := c.MakeDir("b/newdir"); err == nil {
		t.Fatal("MKD on read-only bucket must fail (550)")
	}
}

// TestAuthz_DeniedBucketZeroBackendCalls — no grant on bucket "c": every op
// fails and the backend records ZERO calls (authorization precedes I/O).
func TestAuthz_DeniedBucketZeroBackendCalls(t *testing.T) {
	be := newRecordingBackend()
	_, port := startTestServer(t, be, Config{Verifier: &readOnlyVerifier{}})
	c, err := ftpclient.Dial("127.0.0.1:"+itoa(port), ftpclient.DialWithTimeout(5*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	defer c.Quit()
	if err := c.Login("ro", "pass"); err != nil {
		t.Fatalf("login: %v", err)
	}

	if err := c.Stor("c/x.txt", bytes.NewReader([]byte("x"))); err == nil {
		t.Fatal("STOR on denied bucket must fail")
	}
	if r, err := c.Retr("c/x.txt"); err == nil {
		_ = r.Close()
		t.Fatal("RETR on denied bucket must fail")
	}
	if err := c.Delete("c/x.txt"); err == nil {
		t.Fatal("DELE on denied bucket must fail")
	}
	if _, err := c.List("c"); err == nil {
		t.Fatal("LIST on denied bucket must fail")
	}
	if err := c.MakeDir("c/adir"); err == nil {
		t.Fatal("MKD on denied bucket must fail")
	}
	if err := c.RemoveDir("c/adir"); err == nil {
		t.Fatal("RMD on denied bucket must fail")
	}

	// Zero Backend calls on denied paths (authorization precedes I/O): the
	// client-level existence probes above must all be grant-gated.
	calls := len(be.puts) + len(be.gets) + len(be.deletes) + len(be.stats) + len(be.lists)
	if calls != 0 {
		t.Fatalf("backend saw %d calls on denied paths, want 0", calls)
	}
}

// TestAuthz_WriteGrantSecondBucket — a write grant on "w" admits STOR there
// while "b" stays read-only.
func TestAuthz_WriteGrantSecondBucket(t *testing.T) {
	be := newRecordingBackend()
	be.objects["b/data.txt"] = []byte("data")
	_, port := startTestServer(t, be, Config{Verifier: &readOnlyVerifier{writeBucket: "w"}})
	c, err := ftpclient.Dial("127.0.0.1:"+itoa(port), ftpclient.DialWithTimeout(5*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	defer c.Quit()
	if err := c.Login("ro", "pass"); err != nil {
		t.Fatalf("login: %v", err)
	}

	if err := c.Stor("w/ok.txt", bytes.NewReader([]byte("ok"))); err != nil {
		t.Fatalf("STOR on writable bucket: %v", err)
	}
	if err := c.Stor("b/new.txt", bytes.NewReader(nil)); err == nil {
		t.Fatal("STOR on read-only bucket must still fail")
	}
}

var _ PasswordVerifier = (*readOnlyVerifier)(nil)
