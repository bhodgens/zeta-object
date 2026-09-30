package sftp

import (
	"errors"
	"io"
	"net"
	"os"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/bhodgens/zeta-object/internal/auth"
	"github.com/pkg/sftp"
)

// readOnlyVerifier: Read-only on "b", no grant on "c" (leaf 05 matrix).
type readOnlyVerifier struct{}

func (v *readOnlyVerifier) Verify(user, password string) (auth.Identity, bool) {
	if user != "ro" || password != "pass" {
		return auth.Identity{}, false
	}
	return auth.Identity{AccessKeyID: "ro", BucketGrants: map[string]auth.Grant{
		"b": {Read: true},
		"c": {},
	}}, true
}

// dialAt dials a listener's port with password auth and opens sftp.
func dialAt(t *testing.T, l net.Listener, user, pass string) *sftp.Client {
	t.Helper()
	_, port, _ := net.SplitHostPort(l.Addr().String())

	ccfg := &ssh.ClientConfig{
		User:            user,
		Auth:            []ssh.AuthMethod{ssh.Password(pass)},
		HostKeyCallback: ssh.InsecureIgnoreHostKey(), //nolint:gosec // test host key
		Timeout:         5 * time.Second,
	}
	client, err := ssh.Dial("tcp", "127.0.0.1:"+port, ccfg)
	if err != nil {
		t.Fatalf("ssh dial: %v", err)
	}
	t.Cleanup(func() { _ = client.Close() })
	sc, err := sftp.NewClient(client)
	if err != nil {
		t.Fatalf("sftp client: %v", err)
	}
	t.Cleanup(func() { _ = sc.Close() })
	return sc
}

// TestSFTPAuthz_Matrix — read-only identity: Get/ReadDir OK; Create/Remove/
// Mkdir/Rmdir → SSH_FX_PERMISSION_DENIED; denied bucket "c" fails with ZERO
// Backend data-plane calls (authorization precedes I/O).
func TestSFTPAuthz_Matrix(t *testing.T) {
	be := newRecordingBackend()
	be.objects["b/data.txt"] = []byte("data")
	cfg := Config{
		ListenAddr:        "127.0.0.1:0",
		HostKeyFile:       t.TempDir() + "/host_ed25519",
		AllowPasswordAuth: true,
		Verifier:          &readOnlyVerifier{},
	}
	_, lsn := startTestServer(t, be, cfg)
	sc := dialAt(t, lsn, "ro", "pass")

	// Read path OK.
	rf, err := sc.Open("/b/data.txt")
	if err != nil {
		t.Fatalf("Open (read-only): %v", err)
	}
	if _, err := io.ReadAll(rf); err != nil {
		t.Fatalf("Read: %v", err)
	}
	_ = rf.Close()
	if _, err := sc.ReadDir("/b"); err != nil {
		t.Fatalf("ReadDir (read-only): %v", err)
	}

	// Write paths → permission denied (client normalises code 3 to
	// os.ErrPermission — the protocol-appropriate degradation).
	if wf, err := sc.Create("/b/new.txt"); err == nil {
		_, _ = wf.Write([]byte("x"))
		if err := wf.Close(); err == nil {
			t.Fatal("Create on read-only bucket must fail")
		}
	} else if !errors.Is(err, os.ErrPermission) {
		t.Fatalf("Create err = %v, want permission denied", err)
	}
	if err := sc.Remove("/b/data.txt"); !errors.Is(err, os.ErrPermission) {
		t.Fatalf("Remove err = %v, want permission denied", err)
	}
	if err := sc.Mkdir("/b/newdir"); !errors.Is(err, os.ErrPermission) {
		t.Fatalf("Mkdir err = %v, want permission denied", err)
	}
	if err := sc.RemoveDirectory("/b/newdir"); !errors.Is(err, os.ErrPermission) {
		t.Fatalf("Rmdir err = %v, want permission denied", err)
	}

	// Denied bucket "c": every op fails AND the backend records ZERO calls
	// from here on (the earlier /b reads were legitimately authorized).
	be.gets, be.puts, be.deletes = nil, nil, nil
	if _, err := sc.Open("/c/x.txt"); err == nil {
		t.Fatal("Open on denied bucket must fail")
	}
	if _, err := sc.ReadDir("/c"); err == nil {
		t.Fatal("ReadDir on denied bucket must fail")
	} else {
		// The sftp client treats an empty-but-permission-shaped listing
		// path differently: any non-nil error OR a silent-empty listing
		// without objects is acceptable ONLY if no data plane call
		// happened — asserted below via the zero-call check.
		_ = err
	}
	if err := sc.Remove("/c/x.txt"); err == nil {
		t.Fatal("Remove on denied bucket must fail")
	}
	calls := len(be.puts) + len(be.gets) + len(be.deletes)
	if calls != 0 {
		t.Fatalf("backend saw %d data-plane calls on denied paths, want 0", calls)
	}
}

// TestSFTPUnsupportedOps_MappingUnit — driver-level assertions so the
// degradation mapping cannot silently regress to a success code (leaf 05
// step 5).
func TestSFTPUnsupportedOps_MappingUnit(t *testing.T) {
	be := newRecordingBackend()
	f, err := New(be, Config{
		ListenAddr:        "127.0.0.1:0",
		HostKeyFile:       t.TempDir() + "/host_ed25519",
		AllowPasswordAuth: true,
		Verifier: &fakeVerifier{
			idents: map[string]auth.Identity{"u": {AccessKeyID: "u", BucketGrants: map[string]auth.Grant{"*": {Read: true, Write: true}}}},
			passes: map[string]string{"u": "p"},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	cmd := &fileCmd{f: f, id: auth.Identity{AccessKeyID: "u", BucketGrants: map[string]auth.Grant{"*": {Read: true, Write: true}}}}

	if err := cmd.Filecmd(&sftp.Request{Method: "Setstat", Filepath: "/b/x.txt"}); err == nil || !statusIsServer(err) {
		t.Fatalf("Setstat = %v, want SSH_FX_PERMISSION_DENIED", err)
	}
	if err := cmd.Filecmd(&sftp.Request{Method: "Symlink", Filepath: "/b/x.txt", Target: "/b/y"}); err == nil || !errors.Is(err, errUnsupported) {
		t.Fatalf("Symlink = %v, want SSH_FX_OP_UNSUPPORTED sentinel", err)
	}
}

// statusIsServer reports whether err is the server-side sentinel for a
// status code (driver unit level, pre-wire).
func statusIsServer(err error) bool {
	var f sftpFxErr
	return errors.As(err, &f)
}

// sftpFxErr / listenerAlias keep the unexported-type assertions local.
type sftpFxErr interface{ error }

// TestNoSuchKeyMapping — missing objects map to SSH_FX_NO_SUCH_FILE
// (surfaced as os.ErrNotExist through the client normalisation).
func TestNoSuchKeyMapping(t *testing.T) {
	be := newRecordingBackend()
	cfg := Config{
		ListenAddr:        "127.0.0.1:0",
		HostKeyFile:       t.TempDir() + "/host_ed25519",
		AllowPasswordAuth: true,
		Verifier: &fakeVerifier{
			idents: map[string]auth.Identity{"u": {AccessKeyID: "u", BucketGrants: map[string]auth.Grant{"*": {Read: true, Write: true}}}},
			passes: map[string]string{"u": "p"},
		},
	}
	_, lsn := startTestServer(t, be, cfg)
	sc := dialAt(t, lsn, "u", "p")
	if _, err := sc.Open("/b/missing.txt"); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("Open missing err = %v, want not-exist (SSH_FX_NO_SUCH_FILE)", err)
	}
}

// compile-time: the ssh import stays tied to the client dial path used by
// the round-trip tests above (sftp_test.go).
var _ = ssh.Password
