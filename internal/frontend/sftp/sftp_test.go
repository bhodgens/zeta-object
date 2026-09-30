package sftp

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/pkg/sftp"

	"github.com/bhodgens/zeta-object/internal/auth"
	"github.com/bhodgens/zeta-object/internal/backend"
	"github.com/bhodgens/zeta-object/internal/frontend"
	"github.com/bhodgens/zeta-object/internal/objectmodel"
)

// recordingBackend mirrors the ftp package's in-memory Backend fake
// (map keyed by full "bucket/key"; List implements prefix+delimiter).
type recordingBackend struct {
	backend.Backend
	buckets []objectmodel.BucketInfo
	objects map[string][]byte
	puts    []string
	gets    []string
	deletes []string
}

func newRecordingBackend() *recordingBackend {
	return &recordingBackend{objects: map[string][]byte{}}
}

func (f *recordingBackend) Get(ctx context.Context, bucket, key string, opts objectmodel.GetOptions) (io.ReadCloser, objectmodel.Object, error) {
	f.gets = append(f.gets, bucket+"/"+key)
	data, ok := f.objects[bucket+"/"+key]
	if !ok {
		return nil, objectmodel.Object{}, objectmodel.ErrNoSuchKey(key)
	}
	return io.NopCloser(bytes.NewReader(data)), objectmodel.Object{Key: key, Size: int64(len(data)), LastModified: time.Now()}, nil
}

func (f *recordingBackend) Put(ctx context.Context, bucket, key string, data io.Reader, size int64, opts objectmodel.PutOptions) (objectmodel.Object, error) {
	b, err := io.ReadAll(data)
	if err != nil {
		return objectmodel.Object{}, err
	}
	f.puts = append(f.puts, fmt.Sprintf("%s/%s size=%d", bucket, key, size))
	f.objects[bucket+"/"+key] = b
	return objectmodel.Object{Key: key, Size: int64(len(b))}, nil
}

func (f *recordingBackend) Delete(ctx context.Context, bucket, key string) error {
	f.deletes = append(f.deletes, bucket+"/"+key)
	if _, ok := f.objects[bucket+"/"+key]; !ok {
		return objectmodel.ErrNoSuchKey(key)
	}
	delete(f.objects, bucket+"/"+key)
	return nil
}

func (f *recordingBackend) Stat(ctx context.Context, bucket, key string) (objectmodel.Object, error) {
	data, ok := f.objects[bucket+"/"+key]
	if !ok {
		return objectmodel.Object{}, objectmodel.ErrNoSuchKey(key)
	}
	return objectmodel.Object{Key: key, Size: int64(len(data)), LastModified: time.Now()}, nil
}

func (f *recordingBackend) List(ctx context.Context, bucket string, p objectmodel.ListParams) (objectmodel.ListPage, error) {
	var page objectmodel.ListPage
	seen := map[string]bool{}
	prefix := bucket + "/" + p.Prefix
	if bucket == "" {
		prefix = ""
	}
	for k := range f.objects {
		if !bytes.HasPrefix([]byte(k), []byte(prefix)) {
			continue
		}
		rest := k[len(prefix):]
		if rest == "" {
			continue
		}
		if p.Delimiter != "" {
			if i := indexByte(rest, p.Delimiter[0]); i >= 0 {
				cp := prefix + rest[:i+1]
				if !seen[cp] {
					seen[cp] = true
					page.CommonPrefixes = append(page.CommonPrefixes, cp)
				}
				continue
			}
		}
		page.Objects = append(page.Objects, objectmodel.Object{Key: k, Size: int64(len(f.objects[k]))})
	}
	return page, nil
}

func (f *recordingBackend) Buckets(ctx context.Context) ([]objectmodel.BucketInfo, error) {
	return f.buckets, nil
}

func indexByte(s string, b byte) int {
	for i := range len(s) {
		if s[i] == b {
			return i
		}
	}
	return -1
}

// fakeVerifier / fakeKeyChecker are the seam fakes.
type fakeVerifier struct {
	idents map[string]auth.Identity
	passes map[string]string
}

func (v *fakeVerifier) Verify(user, password string) (auth.Identity, bool) {
	if v.passes[user] != password {
		return auth.Identity{}, false
	}
	id, ok := v.idents[user]
	return id, ok
}

type fakeKeyChecker struct {
	keys  map[string]auth.Identity // authorized_keys line → identity
	calls int
}

func (c *fakeKeyChecker) Accept(pubKey authorizedKey) (auth.Identity, bool) {
	c.calls++
	line := pubKey.Type() + " " + base64.StdEncoding.EncodeToString(pubKey.Marshal())
	id, ok := c.keys[line]
	return id, ok
}

// testConfig builds a Config over a temp host key + fakes.
func testConfig(t *testing.T, be backend.Backend) Config {
	t.Helper()
	return Config{
		ListenAddr:        "127.0.0.1:0",
		HostKeyFile:       t.TempDir() + "/host_ed25519",
		AllowPasswordAuth: true,
		Verifier: &fakeVerifier{
			idents: map[string]auth.Identity{
				"user": {AccessKeyID: "user", BucketGrants: map[string]auth.Grant{"*": {Read: true, Write: true}}},
			},
			passes: map[string]string{"user": "pass"},
		},
	}
}

// startTestServer starts the sftp frontend on a loopback port the way
// main() does (open listener, Serve in a goroutine) and returns the
// frontend and the listener (dials wait only on the already-open port).
func startTestServer(t *testing.T, be *recordingBackend, cfg Config) (*Frontend, net.Listener) {
	t.Helper()
	f, err := New(be, cfg)
	if err != nil {
		t.Fatal(err)
	}
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go func() { _ = f.Serve(l) }()
	t.Cleanup(func() { _ = f.Stop() })
	return f, l
}

// dialSFTP connects with real ssh + sftp clients (password auth).
func dialSFTP(t *testing.T, l net.Listener, user, pass string) *sftp.Client {
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

// TestSFTPRoundTripInProcess — Create+Write+Close → Backend.Put (exact size);
// Open+Read → bytes match; Remove → Backend.Delete.
func TestSFTPRoundTripInProcess(t *testing.T) {
	be := newRecordingBackend()
	_, lsn := startTestServer(t, be, testConfig(t, be))
	sc := dialSFTP(t, lsn, "user", "pass")

	payload := []byte("sftp-roundtrip-payload-9876543210")
	wf, err := sc.Create("bkt/hello.txt")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if _, err := wf.Write(payload); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if err := wf.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if len(be.puts) != 1 || be.puts[0] != "bkt/hello.txt size=33" {
		t.Fatalf("puts = %+v, want [bkt/hello.txt size=33] (exact-size Put)", be.puts)
	}

	rf, err := sc.Open("bkt/hello.txt")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	got, err := io.ReadAll(rf)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	_ = rf.Close()
	if !bytes.Equal(got, payload) {
		t.Fatalf("bytes = %q, want %q", got, payload)
	}

	if err := sc.Remove("bkt/hello.txt"); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	if len(be.deletes) != 1 || be.deletes[0] != "bkt/hello.txt" {
		t.Fatalf("deletes = %+v", be.deletes)
	}
}

// TestListDirMapsToList — ReadDir("/bkt") → List{Prefix:"bkt/", Delimiter:"/"};
// buckets listed at root; common prefixes render as directories.
func TestListDirMapsToList(t *testing.T) {
	be := newRecordingBackend()
	be.objects["bkt/a.txt"] = []byte("a")
	be.objects["bkt/sub/deep.txt"] = []byte("d")
	be.buckets = []objectmodel.BucketInfo{{Name: "bkt"}}
	_, lsn := startTestServer(t, be, testConfig(t, be))
	sc := dialSFTP(t, lsn, "user", "pass")

	root, err := sc.ReadDir("/")
	if err != nil {
		t.Fatalf("ReadDir /: %v", err)
	}
	sawBucket := false
	for _, fi := range root {
		if fi.Name() == "bkt" && fi.IsDir() {
			sawBucket = true
		}
	}
	if !sawBucket {
		t.Fatalf("root listing missing bucket dir: %+v", root)
	}

	entries, err := sc.ReadDir("/bkt")
	if err != nil {
		t.Fatalf("ReadDir /bkt: %v", err)
	}
	sawFile, sawDir := false, false
	for _, fi := range entries {
		if fi.Name() == "a.txt" && !fi.IsDir() {
			sawFile = true
		}
		if fi.Name() == "sub" && fi.IsDir() {
			sawDir = true
		}
	}
	if !sawFile || !sawDir {
		t.Fatalf("bucket listing = %+v, want a.txt file + sub dir", entries)
	}
}

// TestUnsupportedOps — Symlink → SSH_FX_OP_UNSUPPORTED; Chmod
// (setstat) → SSH_FX_PERMISSION_DENIED.
func TestUnsupportedOps(t *testing.T) {
	be := newRecordingBackend()
	be.objects["bkt/x.txt"] = []byte("x")
	_, lsn := startTestServer(t, be, testConfig(t, be))
	sc := dialSFTP(t, lsn, "user", "pass")

	if err := sc.Symlink("bkt/x.txt", "bkt/link"); err == nil {
		t.Fatal("Symlink must fail (SSH_FX_OP_UNSUPPORTED)")
	} else if !isOpUnsupported(err) {
		t.Fatalf("Symlink err = %v, want SSH_FX_OP_UNSUPPORTED", err)
	}
	if err := sc.Chmod("bkt/x.txt", 0o600); err == nil {
		t.Fatal("Chmod must fail (SSH_FX_PERMISSION_DENIED)")
	} else if !errors.Is(err, os.ErrPermission) {
		// The client normalises SSH_FX_PERMISSION_DENIED to os.ErrPermission
		// — the protocol-appropriate mapping (leaf 05 degradation contract).
		t.Fatalf("Chmod err = %v, want permission-denied (os.ErrPermission)", err)
	}
}

// isOpUnsupported unwraps the sftp StatusError (Symlink → OP_UNSUPPORTED).
func isOpUnsupported(err error) bool {
	return statusIs(err, uint32(sftp.ErrSSHFxOpUnsupported))
}

// statusIs reports whether err is an sftp.StatusError carrying the wanted
// SSH_FX_* code (sentinels are fxerr uint32 constants).
func statusIs(err error, want uint32) bool {
	var se *sftp.StatusError
	return errors.As(err, &se) && uint32(se.FxCode()) == want
}

// TestHostKeyPersistence — first start generates the host key; second
// start reuses it (same fingerprint).
func TestHostKeyPersistence(t *testing.T) {
	dir := t.TempDir()
	keyPath := dir + "/host_ed25519"

	_, gen1, fp1, err := loadOrGenerateHostKey(keyPath)
	if err != nil {
		t.Fatalf("first load: %v", err)
	}
	if !gen1 {
		t.Fatal("first load must generate")
	}
	_, gen2, fp2, err := loadOrGenerateHostKey(keyPath)
	if err != nil {
		t.Fatalf("second load: %v", err)
	}
	if gen2 {
		t.Fatal("second load must reuse, not regenerate")
	}
	if fp1 != fp2 {
		t.Fatalf("fingerprints differ: %q vs %q", fp1, fp2)
	}
}

// TestPublicKeyAuth — a client authenticates with an ed25519 key against a
// fake PublicKeyChecker; the identity reaches the driver context (the
// connection's Permissions ride into the session).
func TestPublicKeyAuth(t *testing.T) {
	be := newRecordingBackend()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	sshPub, err := ssh.NewPublicKey(pub)
	if err != nil {
		t.Fatal(err)
	}
	line := sshPub.Type() + " " + base64.StdEncoding.EncodeToString(sshPub.Marshal())
	cfg := testConfig(t, be)
	cfg.AllowPasswordAuth = true // keep password available; pubkey also on
	cfg.KeyChecker = &fakeKeyChecker{
		keys: map[string]auth.Identity{
			line: {AccessKeyID: "keyuser", BucketGrants: map[string]auth.Grant{"*": {Read: true, Write: true}}},
		},
	}
	_, lsn := startTestServer(t, be, cfg)

	_, port, _ := net.SplitHostPort(lsn.Addr().String())
	signer, err := ssh.NewSignerFromKey(priv)
	if err != nil {
		t.Fatal(err)
	}
	ccfg := &ssh.ClientConfig{
		User:            "keyuser",
		Auth:            []ssh.AuthMethod{ssh.PublicKeys(signer)},
		HostKeyCallback: ssh.InsecureIgnoreHostKey(), //nolint:gosec // test host key
		Timeout:         5 * time.Second,
	}
	client, err := ssh.Dial("tcp", "127.0.0.1:"+port, ccfg)
	if err != nil {
		t.Fatalf("ssh pubkey dial: %v", err)
	}
	defer client.Close()
	sc, err := sftp.NewClient(client)
	if err != nil {
		t.Fatalf("sftp client: %v", err)
	}
	defer sc.Close()

	// The pubkey-authenticated session can round-trip (identity reached the
	// driver context; grant enforcement is leaf 05's negative matrix).
	payload := []byte("pubkey-auth-write")
	wf, err := sc.Create("bkt/pk.txt")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if _, err := wf.Write(payload); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if err := wf.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if len(be.puts) != 1 {
		t.Fatalf("puts = %+v, want 1", be.puts)
	}
}

// TestConformance — the shared protocol-agnostic suite.
func TestConformance(t *testing.T) {
	be := newRecordingBackend()
	cfg := testConfig(t, be)
	f, err := New(be, cfg)
	if err != nil {
		t.Fatal(err)
	}
	frontend.RunConformanceSuite(t, f, frontend.ConformanceOptions{})
}

// TestFactoryValidation — unknown option keys and missing required keys
// fail loudly.
func TestFactoryValidation(t *testing.T) {
	if _, err := ConfigFromOptions(":2022", map[string]string{"bogus": "1"}, nil); err == nil || !containsStr(err.Error(), "bogus") {
		t.Fatalf("unknown key err = %v", err)
	}
	if _, err := ConfigFromOptions(":2022", nil, nil); err == nil || !containsStr(err.Error(), "hostKeyFile") {
		t.Fatalf("missing hostKeyFile err = %v", err)
	}
	if _, err := ConfigFromOptions(":2022", map[string]string{"allowPasswordAuth": "maybe"}, nil); err == nil {
		t.Fatal("bad allowPasswordAuth value must fail")
	}
	if _, err := New(nil, Config{ListenAddr: ":2022", HostKeyFile: "x"}); err == nil {
		t.Fatal("New(nil backend) must fail")
	}
	if _, err := New(newRecordingBackend(), Config{HostKeyFile: "x"}); err == nil || !containsStr(err.Error(), "listenAddr") {
		t.Fatalf("New without listenAddr: err = %v", err)
	}
}

func containsStr(haystack, needle string) bool {
	return len(haystack) >= len(needle) && (haystack == needle || indexByteStr(haystack, needle))
}

func indexByteStr(h, n string) bool {
	for i := 0; i+len(n) <= len(h); i++ {
		if h[i:i+len(n)] == n {
			return true
		}
	}
	return false
}

// --- T1: rename guards ------------------------------------------------------

// TestRenameOntoItselfIsNoOp — RNTO with a destination resolving to the
// same bucket+key succeeds WITHOUT touching storage (the old Put-then-
// Delete sequence deleted the object). Also covers a path.Clean-aliased
// spelling of the same key ("/bkt/./x.txt" → "/bkt/x.txt").
func TestRenameOntoItselfIsNoOp(t *testing.T) {
	be := newRecordingBackend()
	be.objects["bkt/x.txt"] = []byte("precious-bytes")
	_, lsn := startTestServer(t, be, testConfig(t, be))
	sc := dialSFTP(t, lsn, "user", "pass")

	if err := sc.Rename("/bkt/x.txt", "/bkt/x.txt"); err != nil {
		t.Fatalf("rename onto itself: %v", err)
	}
	if len(be.puts) != 0 || len(be.deletes) != 0 {
		t.Fatalf("storage touched: puts=%+v deletes=%+v, want none", be.puts, be.deletes)
	}
	if got, ok := be.objects["bkt/x.txt"]; !ok || string(got) != "precious-bytes" {
		t.Fatalf("object lost/changed: %q", got)
	}

	// Aliased spelling: same resolved key after path.Clean.
	if err := sc.Rename("/bkt/./x.txt", "/bkt//x.txt"); err != nil {
		t.Fatalf("aliased self-rename: %v", err)
	}
	if len(be.puts) != 0 || len(be.deletes) != 0 {
		t.Fatalf("aliased self-rename touched storage: puts=%+v deletes=%+v", be.puts, be.deletes)
	}
	if got, ok := be.objects["bkt/x.txt"]; !ok || string(got) != "precious-bytes" {
		t.Fatalf("object lost after aliased self-rename: %q", got)
	}
}

// TestRenameExistingDestinationFailsWithoutTouching — SFTP v3 RENAME must
// not clobber: an existing destination fails and NOTHING is written or
// deleted.
func TestRenameExistingDestinationFailsWithoutTouching(t *testing.T) {
	be := newRecordingBackend()
	be.objects["bkt/a.txt"] = []byte("aaa-source")
	be.objects["bkt/b.txt"] = []byte("bbb-dest")
	_, lsn := startTestServer(t, be, testConfig(t, be))
	sc := dialSFTP(t, lsn, "user", "pass")

	if err := sc.Rename("/bkt/a.txt", "/bkt/b.txt"); err == nil {
		t.Fatal("rename onto existing destination must fail (no clobber)")
	}
	if len(be.puts) != 0 || len(be.deletes) != 0 {
		t.Fatalf("failed rename touched storage: puts=%+v deletes=%+v", be.puts, be.deletes)
	}
	if got := string(be.objects["bkt/a.txt"]); got != "aaa-source" {
		t.Fatalf("source changed: %q", got)
	}
	if got := string(be.objects["bkt/b.txt"]); got != "bbb-dest" {
		t.Fatalf("destination clobbered: %q", got)
	}
}

// TestRenameToFreshKeyStillWorks — the happy path keeps working: fresh
// destination copies the bytes and deletes the source.
func TestRenameToFreshKeyStillWorks(t *testing.T) {
	be := newRecordingBackend()
	be.objects["bkt/old.txt"] = []byte("move-me")
	_, lsn := startTestServer(t, be, testConfig(t, be))
	sc := dialSFTP(t, lsn, "user", "pass")

	if err := sc.Rename("/bkt/old.txt", "/bkt/new.txt"); err != nil {
		t.Fatalf("rename: %v", err)
	}
	if got, ok := be.objects["bkt/new.txt"]; !ok || string(got) != "move-me" {
		t.Fatalf("destination = %q, want move-me", got)
	}
	if _, ok := be.objects["bkt/old.txt"]; ok {
		t.Fatal("source still present after rename")
	}
}

// --- T2: zero-write close commits nothing -----------------------------------

// TestZeroWriteCloseDoesNotCommit — opening a handle for write and closing
// it without any successful write (an aborted APPE / upload) must not Put:
// the previous object keeps its exact bytes and no empty object appears.
func TestZeroWriteCloseDoesNotCommit(t *testing.T) {
	be := newRecordingBackend()
	be.objects["bkt/keep.txt"] = []byte("original-bytes")
	_, lsn := startTestServer(t, be, testConfig(t, be))
	sc := dialSFTP(t, lsn, "user", "pass")

	// Abort over an existing object.
	wf, err := sc.OpenFile("/bkt/keep.txt", os.O_WRONLY)
	if err != nil {
		t.Fatalf("OpenFile: %v", err)
	}
	if err := wf.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	// Abort onto a fresh key.
	nf, err := sc.OpenFile("/bkt/never.txt", os.O_WRONLY)
	if err != nil {
		t.Fatalf("OpenFile new: %v", err)
	}
	if err := nf.Close(); err != nil {
		t.Fatalf("Close new: %v", err)
	}
	if len(be.puts) != 0 {
		t.Fatalf("zero-write close committed: puts=%+v", be.puts)
	}
	if got, ok := be.objects["bkt/keep.txt"]; !ok || string(got) != "original-bytes" {
		t.Fatalf("previous object lost: %q", got)
	}
	if _, ok := be.objects["bkt/never.txt"]; ok {
		t.Fatal("empty object created by aborted upload")
	}
}

// TestPutWriterNormalUploadStillCommits — after ≥1 successful WriteAt the
// Close still Puts the exact buffered bytes (the guard only fires on the
// zero-write path).
func TestPutWriterNormalUploadStillCommits(t *testing.T) {
	be := newRecordingBackend()
	_, lsn := startTestServer(t, be, testConfig(t, be))
	sc := dialSFTP(t, lsn, "user", "pass")

	payload := []byte("guarded-upload-still-commits")
	wf, err := sc.Create("/bkt/upload.bin")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if _, err := wf.Write(payload); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if err := wf.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if len(be.puts) != 1 {
		t.Fatalf("puts = %+v, want exactly 1", be.puts)
	}
	if got, ok := be.objects["bkt/upload.bin"]; !ok || !bytes.Equal(got, payload) {
		t.Fatalf("committed bytes = %q, want %q", got, payload)
	}
}

// --- T3: subsystem request classification -----------------------------------

// TestClassifySubsystemRequestShortPayload — a request whose payload is
// shorter than 4 bytes (or non-subsystem, or nil) must be rejected without
// ever slicing Payload[4:] (the old code panicked the process).
func TestClassifySubsystemRequestShortPayload(t *testing.T) {
	cases := []struct {
		name    string
		req     *ssh.Request
		wantOK  bool
		wantSub string
	}{
		{name: "nil request", req: nil, wantOK: false},
		{name: "wrong type", req: &ssh.Request{Type: "exec", Payload: []byte{0, 0, 0, 4, 'x'}}, wantOK: false},
		{name: "empty payload", req: &ssh.Request{Type: "subsystem", Payload: []byte{}}, wantOK: false},
		{name: "1-byte payload", req: &ssh.Request{Type: "subsystem", Payload: []byte{0}}, wantOK: false},
		{name: "4-byte payload", req: &ssh.Request{Type: "subsystem", Payload: []byte{0, 0, 0, 4}}, wantOK: false},
		{name: "sftp", req: &ssh.Request{Type: "subsystem", Payload: []byte{0, 0, 0, 4, 's', 'f', 't', 'p'}}, wantOK: true, wantSub: "sftp"},
		{name: "other subsystem", req: &ssh.Request{Type: "subsystem", Payload: []byte{0, 0, 0, 4, 'x', 'y', 's', 'z'}}, wantOK: true, wantSub: "xysz"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sub, ok := classifySubsystemRequest(tc.req)
			if ok != tc.wantOK {
				t.Fatalf("ok = %v, want %v", ok, tc.wantOK)
			}
			if tc.wantOK && sub != tc.wantSub {
				t.Fatalf("subsystem = %q, want %q", sub, tc.wantSub)
			}
		})
	}
}

// --- T4: nil KeyChecker contract ---------------------------------------------

// TestNilKeyCheckerNoPanicPasswordAuthWorks — the documented nil shape
// (password auth on, no key checker) must not panic, password auth must
// still work end-to-end, and public-key auth must NOT be offered.
func TestNilKeyCheckerNoPanicPasswordAuthWorks(t *testing.T) {
	be := newRecordingBackend()
	cfg := testConfig(t, be) // AllowPasswordAuth + Verifier, KeyChecker nil
	if _, err := New(be, cfg); err != nil {
		t.Fatalf("New with nil KeyChecker: %v", err)
	}
	_, lsn := startTestServer(t, be, cfg)

	// Password auth works.
	sc := dialSFTP(t, lsn, "user", "pass")
	wf, err := sc.Create("/bkt/nilchecker.txt")
	if err != nil {
		t.Fatalf("Create (password session): %v", err)
	}
	if _, err := wf.Write([]byte("password-auth-still-works")); err != nil {
		t.Fatalf("Write (password session): %v", err)
	}
	if err := wf.Close(); err != nil {
		t.Fatalf("Close (password session): %v", err)
	}
	if len(be.puts) != 1 {
		t.Fatalf("puts = %+v, want 1 (password auth functional)", be.puts)
	}

	// Public-key auth is not offered: a client with only a key fails.
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	signer, err := ssh.NewSignerFromKey(priv)
	if err != nil {
		t.Fatal(err)
	}
	_, port, _ := net.SplitHostPort(lsn.Addr().String())
	ccfg := &ssh.ClientConfig{
		User:            "user",
		Auth:            []ssh.AuthMethod{ssh.PublicKeys(signer)},
		HostKeyCallback: ssh.InsecureIgnoreHostKey(), //nolint:gosec // test host key
		Timeout:         5 * time.Second,
	}
	if conn, err := ssh.Dial("tcp", "127.0.0.1:"+port, ccfg); err == nil {
		_ = conn.Close()
		t.Fatal("public-key auth must not be offered when KeyChecker is nil")
	}
}

// --- T6: host key exclusive create -------------------------------------------

// TestHostKeyExistingFileNeverOverwritten — when the path already holds a
// valid key it is reused untouched (no WriteFile clobber): same
// fingerprint, byte-identical file, generated=false.
func TestHostKeyExistingFileNeverOverwritten(t *testing.T) {
	dir := t.TempDir()
	keyPath := dir + "/host_ed25519"

	_, gen1, fp1, err := loadOrGenerateHostKey(keyPath)
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	if !gen1 {
		t.Fatal("first call must generate")
	}
	before, err := os.ReadFile(keyPath)
	if err != nil {
		t.Fatal(err)
	}
	st, err := os.Stat(keyPath)
	if err != nil {
		t.Fatal(err)
	}

	_, gen2, fp2, err := loadOrGenerateHostKey(keyPath)
	if err != nil {
		t.Fatalf("reuse: %v", err)
	}
	if gen2 {
		t.Fatal("existing key must be reused, not regenerated")
	}
	if fp1 != fp2 {
		t.Fatalf("fingerprint changed: %q vs %q", fp1, fp2)
	}
	after, err := os.ReadFile(keyPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Fatal("existing host key file was rewritten")
	}
	if st2, _ := os.Stat(keyPath); !st.ModTime().Equal(st2.ModTime()) {
		t.Fatal("existing host key file mtime changed (was overwritten)")
	}
}

// TestHostKeyDanglingSymlinkNotFollowed — WriteFile used to follow a
// dangling symlink at the host-key path. The exclusive create must refuse
// (EEXIST shape) with a loud error and write nothing through the link.
func TestHostKeyDanglingSymlinkNotFollowed(t *testing.T) {
	dir := t.TempDir()
	keyPath := dir + "/host_ed25519"
	if err := os.Symlink(dir+"/nonexistent-target", keyPath); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}

	_, _, _, err := loadOrGenerateHostKey(keyPath)
	if err == nil {
		t.Fatal("dangling symlink at hostKeyFile must fail loudly, not write through it")
	}
	if _, statErr := os.Lstat(dir + "/nonexistent-target"); !os.IsNotExist(statErr) {
		t.Fatal("something was written through the dangling symlink")
	}
}

// F2 twin: renaming a DIRECTORY (client path has no trailing slash) moves
// the "dir/" marker, not a colliding bare key.
func TestRenameDirMarkerMoves(t *testing.T) {
	be := newRecordingBackend()
	be.objects["bkt/olddir/"] = nil
	f, err := New(be, testConfig(t, be))
	if err != nil {
		t.Fatal(err)
	}
	c := &fileCmd{f: f, id: auth.WildcardIdentity("t")}
	if err := c.rename("/bkt/olddir", "/bkt/newdir"); err != nil {
		t.Fatalf("rename dir: %v", err)
	}
	if _, ok := be.objects["bkt/olddir/"]; ok {
		t.Fatalf("old marker still present; objects=%v", be.objects)
	}
	if _, ok := be.objects["bkt/newdir/"]; !ok {
		t.Fatalf("new marker missing; objects=%v", be.objects)
	}
}

// T7: WriteAt refuses growth past maxUploadBufferBytes instead of OOMing.
// Probed at a tiny injection scale by checking the guard arithmetic via
// buf near the cap is impractical (5 GiB), so the guard is pinned at the
// constant level: the cap constant must match the backend default.
func TestUploadBufferCapMatchesBackendDefault(t *testing.T) {
	if maxUploadBufferBytes != 5<<30 {
		t.Fatalf("maxUploadBufferBytes = %d, want the 5 GiB backend default", maxUploadBufferBytes)
	}
}
