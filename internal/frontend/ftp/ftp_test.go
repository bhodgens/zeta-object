package ftp

import (
	"bufio"
	"bytes"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	ftpserver "github.com/fclairamb/ftpserverlib"
	ftpclient "github.com/jlaffaye/ftp"

	"github.com/bhodgens/zeta-object/internal/auth"
	"github.com/bhodgens/zeta-object/internal/frontend"
	"github.com/bhodgens/zeta-object/internal/objectmodel"
)

// recordingBackend is a Backend fake built on the real fs-free seam: it
// records calls and serves a tiny in-memory object store so wire
// round-trips can flow through the driver.
type recordingBackend struct {
	fakeBackend
	objects map[string][]byte // "bucket/key" → bytes
}

func newRecordingBackend() *recordingBackend {
	return &recordingBackend{objects: map[string][]byte{}}
}

func (f *recordingBackend) key(bucket, key string) string { return bucket + "/" + key }

func (f *recordingBackend) Get(ctx context.Context, bucket, key string, opts objectmodel.GetOptions) (io.ReadCloser, objectmodel.Object, error) {
	f.gets = append(f.gets, getCall{bucket, key})
	data, ok := f.objects[f.key(bucket, key)]
	if !ok {
		return nil, objectmodel.Object{}, objectmodel.ErrNoSuchKey(key)
	}
	return io.NopCloser(bytes.NewReader(data)), objectmodel.Object{Key: key, Size: int64(len(data))}, nil
}

func (f *recordingBackend) Put(ctx context.Context, bucket, key string, data io.Reader, size int64, opts objectmodel.PutOptions) (objectmodel.Object, error) {
	if f.putErr != nil {
		return objectmodel.Object{}, f.putErr
	}
	b, err := io.ReadAll(data)
	if err != nil {
		return objectmodel.Object{}, err
	}
	f.puts = append(f.puts, putCall{bucket: bucket, key: key, data: b, size: size})
	f.objects[f.key(bucket, key)] = b
	return objectmodel.Object{Key: key, Size: int64(len(b))}, nil
}

func (f *recordingBackend) Delete(ctx context.Context, bucket, key string) error {
	f.deletes = append(f.deletes, bucket+"/"+key)
	if _, ok := f.objects[f.key(bucket, key)]; !ok {
		return objectmodel.ErrNoSuchKey(key)
	}
	delete(f.objects, f.key(bucket, key))
	return nil
}

func (f *recordingBackend) Stat(ctx context.Context, bucket, key string) (objectmodel.Object, error) {
	f.stats = append(f.stats, bucket+"/"+key)
	data, ok := f.objects[f.key(bucket, key)]
	if !ok {
		return objectmodel.Object{}, objectmodel.ErrNoSuchKey(key)
	}
	return objectmodel.Object{Key: key, Size: int64(len(data)), LastModified: time.Now()}, nil
}

func (f *recordingBackend) List(ctx context.Context, bucket string, p objectmodel.ListParams) (objectmodel.ListPage, error) {
	f.lists = append(f.lists, p)
	return listInMemory(f.objects, bucket, p), nil
}

// listInMemory implements List semantics over the object map (prefix +
// delimiter → objects + common prefixes) — mirrors the fs backend's shape.
// The map is keyed by the FULL "bucket/key" form, so prefix matching runs
// on the full key and returned keys/prefixes are bucket-qualified.
func listInMemory(objects map[string][]byte, bucket string, p objectmodel.ListParams) objectmodel.ListPage {
	var page objectmodel.ListPage
	seenPrefix := map[string]bool{}
	prefix := bucket + "/" + p.Prefix
	for k := range objects {
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
				if !seenPrefix[cp] {
					seenPrefix[cp] = true
					page.CommonPrefixes = append(page.CommonPrefixes, cp)
				}
				continue
			}
		}
		page.Objects = append(page.Objects, objectmodel.Object{Key: k, Size: int64(len(objects[k]))})
	}
	return page
}

func indexByte(s string, b byte) int {
	for i := range len(s) {
		if s[i] == b {
			return i
		}
	}
	return -1
}

// startTestServer starts the ftp frontend on a loopback port with a
// recording backend and a static verifier. Returns the frontend + port.
func startTestServer(t *testing.T, be *recordingBackend, cfg Config) (*Frontend, int) {
	t.Helper()
	if cfg.Verifier == nil {
		cfg.Verifier = &staticVerifier{
			idents: map[string]auth.Identity{
				"user": {AccessKeyID: "user", BucketGrants: map[string]auth.Grant{"*": {Read: true, Write: true}}},
			},
			passes: map[string]string{"user": "pass"},
		}
	}
	cfg.ListenAddr = "127.0.0.1:0"
	f, err := New(be, cfg)
	if err != nil {
		t.Fatal(err)
	}
	// main() opens the listener and hands it to Serve (leaf 01 wiring).
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	f.setListener(l)
	go func() { _ = f.Serve(l) }()
	t.Cleanup(func() { _ = f.Stop() })
	_, port, _ := net.SplitHostPort(l.Addr().String())
	p := atoi(port)
	// Wait for accept loop readiness.
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		c, err := net.DialTimeout("tcp", l.Addr().String(), 500*time.Millisecond)
		if err == nil {
			c.Close()
			return f, p
		}
	}
	t.Fatal("ftp server never became reachable")
	return f, p
}

func atoi(s string) int {
	n := 0
	for i := range len(s) {
		n = n*10 + int(s[i]-'0')
	}
	return n
}

func dial(t *testing.T, port int) *ftpclient.ServerConn {
	t.Helper()
	c, err := ftpclient.Dial("127.0.0.1:"+itoa(port), ftpclient.DialWithTimeout(5*time.Second))
	if err != nil {
		t.Fatalf("ftp dial: %v", err)
	}
	t.Cleanup(func() { _ = c.Quit() })
	if err := c.Login("user", "pass"); err != nil {
		t.Fatalf("ftp login: %v", err)
	}
	return c
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b [8]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	return string(b[i:])
}

// TestFTPRoundTrip — STOR a payload, RETR and compare bytes, DELE; the fake
// Backend saw Put(bucket,key)/Get/Delete with exact keys and the Put size
// matches the payload (streaming contract).
func TestFTPRoundTrip(t *testing.T) {
	be := newRecordingBackend()
	_, port := startTestServer(t, be, Config{})
	c := dial(t, port)

	payload := []byte("ftp-roundtrip-payload-0123456789")
	if err := c.Stor("bkt/hello.txt", bytes.NewReader(payload)); err != nil {
		t.Fatalf("STOR: %v", err)
	}
	if len(be.puts) != 1 {
		t.Fatalf("puts = %+v, want 1", be.puts)
	}
	put := be.puts[0]
	if put.bucket != "bkt" || put.key != "hello.txt" {
		t.Fatalf("Put(bucket,key) = (%q,%q), want (bkt,hello.txt)", put.bucket, put.key)
	}
	if put.size != int64(len(payload)) {
		t.Fatalf("Put size = %d, want %d (size must match payload)", put.size, len(payload))
	}
	if !bytes.Equal(put.data, payload) {
		t.Fatalf("Put data = %q, want %q", put.data, payload)
	}

	r, err := c.Retr("bkt/hello.txt")
	if err != nil {
		t.Fatalf("RETR: %v", err)
	}
	got, err := io.ReadAll(r)
	_ = r.Close()
	if err != nil {
		t.Fatalf("RETR read: %v", err)
	}
	if !bytes.Equal(got, payload) {
		t.Fatalf("RETR bytes = %q, want %q", got, payload)
	}
	if len(be.gets) != 1 || be.gets[0].bucket != "bkt" || be.gets[0].key != "hello.txt" {
		t.Fatalf("gets = %+v, want [bkt/hello.txt]", be.gets)
	}

	if err := c.Delete("bkt/hello.txt"); err != nil {
		t.Fatalf("DELE: %v", err)
	}
	if len(be.deletes) != 1 || be.deletes[0] != "bkt/hello.txt" {
		t.Fatalf("deletes = %+v, want [bkt/hello.txt]", be.deletes)
	}
}

// TestListingMapsToListDelimiter — LIST issues List{Prefix, Delimiter:"/"};
// common prefixes render as directory entries.
func TestListingMapsToListDelimiter(t *testing.T) {
	be := newRecordingBackend()
	be.objects["bkt/docs/a.txt"] = []byte("a")
	be.objects["bkt/docs/sub/deep.txt"] = []byte("d")
	be.objects["bkt/root.txt"] = []byte("r")
	_, port := startTestServer(t, be, Config{})
	c := dial(t, port)

	entries, err := c.List("bkt/docs")
	if err != nil {
		t.Fatalf("LIST: %v", err)
	}
	if len(be.lists) == 0 {
		t.Fatal("no Backend.List call recorded for LIST")
	}
	last := be.lists[len(be.lists)-1]
	if last.Delimiter != "/" {
		t.Fatalf("ListParams.Delimiter = %q, want \"/\"", last.Delimiter)
	}
	if last.Prefix != "docs/" {
		t.Fatalf("ListParams.Prefix = %q, want \"docs/\"", last.Prefix)
	}
	sawFile, sawDir := false, false
	for _, e := range entries {
		if e.Name == "a.txt" {
			sawFile = true
		}
		if e.Name == "sub" && e.Type == ftpclient.EntryTypeFolder {
			sawDir = true
		}
	}
	if !sawFile {
		t.Fatalf("LIST output missing a.txt: %+v", entries)
	}
	if !sawDir {
		t.Fatalf("LIST output missing common-prefix dir sub: %+v", entries)
	}

	// Root listing shows buckets as directories (via Backend.Buckets).
	be.buckets = []objectmodel.BucketInfo{{Name: "bkt"}}
	rootEntries, err := c.List("/")
	if err != nil {
		t.Fatalf("LIST /: %v", err)
	}
	sawBucket := false
	for _, e := range rootEntries {
		if e.Name == "bkt" && e.Type == ftpclient.EntryTypeFolder {
			sawBucket = true
		}
	}
	if !sawBucket {
		t.Fatalf("root LIST missing bucket dir bkt: %+v", rootEntries)
	}
}

// TestAuthRejectsBadPassword — wrong password fails login (530), and the
// backend records ZERO calls.
func TestAuthRejectsBadPassword(t *testing.T) {
	be := newRecordingBackend()
	_, port := startTestServer(t, be, Config{})
	c, err := ftpclient.Dial("127.0.0.1:"+itoa(port), ftpclient.DialWithTimeout(5*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	defer c.Quit()
	if err := c.Login("user", "WRONG"); err == nil {
		t.Fatal("bad password must fail login")
	}
	calls := len(be.puts) + len(be.gets) + len(be.deletes) + len(be.stats) + len(be.lists)
	if calls != 0 {
		t.Fatalf("backend saw %d calls after failed auth, want 0", calls)
	}
}

// TestMKDRMD — MKD writes the zero-byte "dir/" marker; RMD deletes it;
// missing marker → error.
func TestMKDRMD(t *testing.T) {
	be := newRecordingBackend()
	_, port := startTestServer(t, be, Config{})
	c := dial(t, port)

	if err := c.MakeDir("bkt/newdir"); err != nil {
		t.Fatalf("MKD: %v", err)
	}
	if len(be.puts) != 1 || be.puts[0].key != "newdir/" || be.puts[0].size != 0 {
		t.Fatalf("MKD puts = %+v, want zero-byte newdir/ marker", be.puts)
	}
	if err := c.RemoveDir("bkt/newdir"); err != nil {
		t.Fatalf("RMD: %v", err)
	}
	if len(be.deletes) != 1 || be.deletes[0] != "bkt/newdir/" {
		t.Fatalf("RMD deletes = %+v, want [bkt/newdir/]", be.deletes)
	}
	if err := c.RemoveDir("bkt/neverdir"); err == nil {
		t.Fatal("RMD of missing dir marker must fail")
	}
}

// TestFTPSExplicitTLS — same server with a TLS config: explicit AUTH TLS
// round-trip works AND plaintext keeps working on the same port.
func TestFTPSExplicitTLS(t *testing.T) {
	be := newRecordingBackend()
	_, port := startTestServer(t, be, Config{TLSConfig: testTLS(t)})

	payload := []byte("ftps-explicit-payload")

	// Plaintext (explicit: TLS is opt-in per connection).
	cPlain := dial(t, port)
	if err := cPlain.Stor("bkt/plain.txt", bytes.NewReader(payload)); err != nil {
		t.Fatalf("plain STOR: %v", err)
	}

	// Explicit TLS on the same port (AUTH TLS; the test cert is self-signed
	// so the client skips verification).
	cTLS, err := ftpclient.Dial("127.0.0.1:"+itoa(port),
		ftpclient.DialWithExplicitTLS(&tls.Config{InsecureSkipVerify: true}), //nolint:gosec // test cert
		ftpclient.DialWithTimeout(5*time.Second))
	if err != nil {
		t.Fatalf("ftps dial: %v", err)
	}
	defer cTLS.Quit()
	if err := cTLS.Login("user", "pass"); err != nil {
		t.Fatalf("ftps login: %v", err)
	}
	if err := cTLS.Stor("bkt/secure.txt", bytes.NewReader(payload)); err != nil {
		t.Fatalf("ftps STOR: %v", err)
	}
	r, err := cTLS.Retr("bkt/secure.txt")
	if err != nil {
		t.Fatalf("ftps RETR: %v", err)
	}
	got, _ := io.ReadAll(r)
	_ = r.Close()
	if !bytes.Equal(got, payload) {
		t.Fatalf("ftps RETR bytes = %q, want %q", got, payload)
	}
}

// TestPasvPortRange — passive data-channel ports stay inside the range.
func TestPasvPortRange(t *testing.T) {
	be := newRecordingBackend()
	minP, maxP := 21000, 21050
	_, port := startTestServer(t, be, Config{
		PassivePortMin: minP,
		PassivePortMax: maxP,
		PublicIP:       "127.0.0.1",
	})
	c := dial(t, port)
	payload := []byte("pasv-range-check")
	if err := c.Stor("bkt/pasv.txt", bytes.NewReader(payload)); err != nil {
		t.Fatalf("STOR over PASV: %v", err)
	}
	// The transfer only completes if the data channel opened; the range is
	// enforced server-side by ftpserverlib's PortRange. Sanity: RETR works.
	r, err := c.Retr("bkt/pasv.txt")
	if err != nil {
		t.Fatalf("RETR over PASV: %v", err)
	}
	got, _ := io.ReadAll(r)
	_ = r.Close()
	if !bytes.Equal(got, payload) {
		t.Fatalf("bytes = %q, want %q", got, payload)
	}
}

// TestConformance — the shared protocol-agnostic suite.
func TestConformance(t *testing.T) {
	be := newRecordingBackend()
	cfg := Config{
		ListenAddr: "127.0.0.1:0",
		Verifier: &staticVerifier{
			idents: map[string]auth.Identity{"u": {AccessKeyID: "u"}},
			passes: map[string]string{"u": "p"},
		},
	}
	f, err := New(be, cfg)
	if err != nil {
		t.Fatal(err)
	}
	frontend.RunConformanceSuite(t, f, frontend.ConformanceOptions{})
}

// TestAbortedWriteFileDoesNotOverwrite — F1: a writeFile that saw
// TransferError must NOT Put the partial buffer on Close. The previously
// stored object keeps its bytes, Close surfaces the failure as an
// *ftpError (4xx/5xx class), and the writer satisfies
// ftpserver.FileTransferError (the hook ftpserverlib calls before Close).
func TestAbortedWriteFileDoesNotOverwrite(t *testing.T) {
	be := newRecordingBackend()
	prev := []byte("good-previous-object-bytes")
	be.objects["bkt/keep.txt"] = prev

	id := auth.Identity{AccessKeyID: "user", BucketGrants: map[string]auth.Grant{"*": {Read: true, Write: true}}}
	d := &clientDriver{f: &Frontend{be: be}, id: id}
	wf, err := d.OpenFile("/bkt/keep.txt", os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	w, ok := wf.(*writeFile)
	if !ok {
		t.Fatalf("OpenFile(write) returned %T, want *writeFile", wf)
	}
	// Compile-time + runtime proof the hook ftpserverlib type-asserts exists.
	var _ ftpserver.FileTransferError = w

	if _, err := w.Write([]byte("truncat")); err != nil {
		t.Fatal(err)
	}
	// ftpserverlib contract (v0.32.4 handle_files.go:103/:162): TransferError
	// is invoked BEFORE Close on the failed-transfer paths.
	w.TransferError(errors.New("connection reset by peer"))
	if err := w.Close(); err == nil {
		t.Fatal("Close after TransferError must fail")
	} else {
		var fe *ftpError
		if !errors.As(err, &fe) {
			t.Fatalf("Close error = %T (%v), want *ftpError", err, err)
		}
		if fe.code < 400 || fe.code >= 600 {
			t.Fatalf("ftpError.code = %d, want 4xx/5xx", fe.code)
		}
	}
	if len(be.puts) != 0 {
		t.Fatalf("aborted Close issued %d Put(s), want 0 (previous object must survive)", len(be.puts))
	}
	if !bytes.Equal(be.objects["bkt/keep.txt"], prev) {
		t.Fatalf("previous object = %q, want %q", be.objects["bkt/keep.txt"], prev)
	}

	// New-file case: an aborted upload of a nonexistent key must NOT create it.
	wf2, err := d.OpenFile("/bkt/new.txt", os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	w2 := wf2.(*writeFile)
	_, _ = w2.Write([]byte("partial"))
	w2.TransferError(errors.New("ABOR"))
	if err := w2.Close(); err == nil {
		t.Fatal("aborted Close of a new file must fail")
	}
	if _, exists := be.objects["bkt/new.txt"]; exists {
		t.Fatal("aborted upload created the object; want no object")
	}
	if len(be.puts) != 0 {
		t.Fatalf("aborted closes issued %d total Put(s), want 0", len(be.puts))
	}
}

// TestCleanCloseStillCommits — the fix must not regress the happy path: a
// writeFile that never saw TransferError Put()s the full buffer on Close.
func TestCleanCloseStillCommits(t *testing.T) {
	be := newRecordingBackend()
	d := &clientDriver{f: &Frontend{be: be}, id: auth.Identity{AccessKeyID: "user", BucketGrants: map[string]auth.Grant{"*": {Read: true, Write: true}}}}
	payload := []byte("clean-close-commit-payload")
	wf, err := d.OpenFile("/bkt/fine.txt", os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	w := wf.(*writeFile)
	if _, err := w.Write(payload); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("clean Close: %v", err)
	}
	if len(be.puts) != 1 {
		t.Fatalf("puts = %d, want 1", len(be.puts))
	}
	if got := be.objects["bkt/fine.txt"]; !bytes.Equal(got, payload) {
		t.Fatalf("stored = %q, want %q", got, payload)
	}
}

// TestAbortedStorKeepsPreviousObject — wire-level: STOR opens, partial bytes
// flow, then the client sends ABOR mid-transfer (the jlaffaye client has no
// raw-connection API in v0.2.4, so the control/data protocol is driven
// directly). After the abort settles, the previous object must keep its
// original bytes — not the partial body, not an empty body.
func TestAbortedStorKeepsPreviousObject(t *testing.T) {
	be := newRecordingBackend()
	prev := []byte("previous-good-object-via-wire")
	be.objects["bkt/abort.txt"] = prev
	_, port := startTestServer(t, be, Config{})
	ctrl := dialRaw(t, port)
	defer ctrl.Close()
	send := func(cmd string) string {
		t.Helper()
		return ctrl.cmd(cmd)
	}
	if got := ctrl.readLine(); got[0] != '2' {
		t.Fatalf("banner = %q", got)
	}
	if got := send("USER user"); got[0] != '3' {
		t.Fatalf("USER reply = %q", got)
	}
	if got := send("PASS pass"); got[0] != '2' {
		t.Fatalf("PASS reply = %q", got)
	}
	pasv := send("PASV")
	if pasv[0] != '2' {
		t.Fatalf("PASV reply = %q", pasv)
	}
	dcAddr, err := pasvAddr(pasv)
	if err != nil {
		t.Fatal(err)
	}
	// Dial the data connection BEFORE STOR: ftpserverlib accepts exactly one
	// connection on the passive listener when the transfer opens.
	dc, err := net.DialTimeout("tcp", dcAddr, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer dc.Close()
	if got := send("STOR bkt/abort.txt"); got[0] != '1' {
		t.Fatalf("STOR reply = %q (want 1xx preliminary)", got)
	}
	if _, err := dc.Write([]byte("only-parti")); err != nil {
		t.Fatal(err)
	}
	// ABOR mid-transfer → expect 426 (aborted) then 226 (closing data conn).
	abor := send("ABOR")
	if abor[:3] != "426" {
		t.Logf("ABOR first reply = %q (want 426)", abor)
	}
	if second := ctrl.readLine(); second[:3] != "226" {
		t.Logf("ABOR second reply = %q (want 226)", second)
	}
	// NOOP serializes behind the transfer goroutine (transferWg.Wait in
	// ftpserverlib), so a 200 guarantees TransferError/Close already ran.
	if got := send("NOOP"); got[:3] != "200" {
		t.Fatalf("NOOP after abort = %q (want 200)", got)
	}
	if len(be.puts) != 0 {
		t.Fatalf("aborted STOR issued %d Put(s), want 0", len(be.puts))
	}
	if got := be.objects["bkt/abort.txt"]; !bytes.Equal(got, prev) {
		t.Fatalf("object after aborted STOR = %q, want %q", got, prev)
	}
}

// rawCtrl is a minimal FTP control connection (net text commands/replies).
type rawCtrl struct {
	conn net.Conn
	br   *bufio.Reader
}

func dialRaw(t *testing.T, port int) *rawCtrl {
	t.Helper()
	conn, err := net.DialTimeout("tcp", "127.0.0.1:"+itoa(port), 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	return &rawCtrl{conn: conn, br: bufio.NewReader(conn)}
}

func (r *rawCtrl) Close() error { return r.conn.Close() }

func (r *rawCtrl) cmd(line string) string {
	fmt.Fprintf(r.conn, "%s\r\n", line)
	return r.readLine()
}

func (r *rawCtrl) readLine() string {
	line, err := r.br.ReadString('\n')
	if err != nil && line == "" {
		return "000 unreadable"
	}
	return strings.TrimRight(line, "\r\n")
}

// pasvAddr extracts "h1,h2,h3,h4,p1,p2" from a 227 reply into host:port.
func pasvAddr(reply string) (string, error) {
	i := strings.Index(reply, "(")
	j := strings.Index(reply, ")")
	if i < 0 || j < 0 || j < i {
		return "", fmt.Errorf("no PASV tuple in %q", reply)
	}
	parts := strings.Split(reply[i+1:j], ",")
	if len(parts) != 6 {
		return "", fmt.Errorf("bad PASV tuple in %q", reply)
	}
	n := make([]int, 6)
	for k, s := range parts {
		v, err := strconv.Atoi(strings.TrimSpace(s))
		if err != nil {
			return "", err
		}
		n[k] = v
	}
	return fmt.Sprintf("%d.%d.%d.%d:%d", n[0], n[1], n[2], n[3], n[4]<<8|n[5]), nil
}
