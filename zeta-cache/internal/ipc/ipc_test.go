package ipc

import (
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// ask sends one JSON request over a fresh connection and returns the
// response. One request per connection is the protocol contract.
func ask(t *testing.T, socketPath string, req Request) Response {
	t.Helper()
	conn, err := net.Dial("unix", socketPath)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()
	if err := json.NewEncoder(conn).Encode(req); err != nil {
		t.Fatalf("encode: %v", err)
	}
	var resp Response
	conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	if err := json.NewDecoder(conn).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	return resp
}

// shortSocket returns a socket path short enough for macOS's ~104-char
// sockaddr_un limit, keyed by test name under /tmp.
func shortSocket(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "zc-ipc")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	return filepath.Join(dir, "z.ipc")
}

// startServer serves on a short temp socket path and registers cleanup.
func startServer(t *testing.T) string {
	t.Helper()
	path := shortSocket(t)
	srv, err := Serve(path, "https://srv", "bkt")
	if err != nil {
		t.Fatalf("Serve: %v", err)
	}
	t.Cleanup(func() { srv.Stop() })
	return path
}

func TestStatusShape(t *testing.T) {
	path := startServer(t)
	resp := ask(t, path, Request{V: Version, Type: "status"})
	if !resp.OK {
		t.Fatalf("status: ok = false, error %q", resp.Error)
	}
	if resp.V != 1 {
		t.Errorf("status: v = %d, want 1", resp.V)
	}
	if resp.Data == nil {
		t.Fatal("status: data is nil")
	}
	d := resp.Data
	if d.State != "idle" {
		t.Errorf("state = %q, want idle", d.State)
	}
	if d.Server != "https://srv" || d.Bucket != "bkt" {
		t.Errorf("server/bucket wrong: %+v", d)
	}
	if d.LastSync != nil {
		t.Errorf("lastSync = %v, want null", d.LastSync)
	}
	if d.Dirty != 0 || d.Cached != 0 || d.Conflicts != 0 {
		t.Errorf("counters not zero: %+v", d)
	}
}

func TestUnknownType(t *testing.T) {
	path := startServer(t)
	resp := ask(t, path, Request{V: Version, Type: "frobnicate"})
	if resp.OK {
		t.Error("unknown type: ok = true, want false")
	}
	if resp.Error != "unknown request type" {
		t.Errorf("error = %q, want 'unknown request type'", resp.Error)
	}
	if resp.V != 1 {
		t.Errorf("error response v = %d, want 1", resp.V)
	}
}

func TestBadVersionRejected(t *testing.T) {
	path := startServer(t)
	resp := ask(t, path, Request{V: 99, Type: "status"})
	if resp.OK || resp.Error == "" {
		t.Errorf("v=99: ok = %v error = %q, want rejection", resp.OK, resp.Error)
	}
}

func TestStaleSocketRemoved(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "zeta-cache.ipc")
	// Plant a stale socket-like file: a plain file at the socket path must
	// be removed by Serve, not cause EADDRINUSE.
	if err := os.WriteFile(path, []byte("stale"), 0o600); err != nil {
		t.Fatal(err)
	}
	srv, err := Serve(path, "https://srv", "bkt")
	if err != nil {
		t.Fatalf("Serve over stale file = %v, want nil", err)
	}
	t.Cleanup(func() { srv.Stop() })

	// The live socket must be 0600.
	fi, err := os.Lstat(path)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode()&os.ModeSocket == 0 {
		t.Fatalf("%s is not a socket (mode %s)", path, fi.Mode())
	}
	if fi.Mode().Perm() != 0o600 {
		t.Errorf("socket perms = %o, want 600", fi.Mode().Perm())
	}
	// And the daemon answers through it.
	if resp := ask(t, path, Request{V: Version, Type: "status"}); !resp.OK {
		t.Errorf("status after stale cleanup: %q", resp.Error)
	}
}

func TestRestartDropsClientsCleanly(t *testing.T) {
	path := shortSocket(t)
	srv1, err := Serve(path, "https://srv", "bkt")
	if err != nil {
		t.Fatal(err)
	}
	if resp := ask(t, path, Request{V: Version, Type: "status"}); !resp.OK {
		t.Fatalf("first daemon: %q", resp.Error)
	}
	srv1.Stop()
	if _, err := os.Lstat(path); !os.IsNotExist(err) {
		t.Fatalf("socket file survived Stop: %v", err)
	}
	// A second daemon takes over the same path and answers.
	srv2, err := Serve(path, "https://srv", "bkt")
	if err != nil {
		t.Fatalf("restart: %v", err)
	}
	t.Cleanup(func() { srv2.Stop() })
	if resp := ask(t, path, Request{V: Version, Type: "status"}); !resp.OK {
		t.Errorf("second daemon: %q", resp.Error)
	}
}

func TestParentDirCreated(t *testing.T) {
	path := shortSocket(t)
	// Remove the leaf component so Serve must create its parent.
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	srv, err := Serve(path, "https://srv", "bkt")
	if err != nil {
		t.Fatalf("Serve into missing parent = %v, want nil", err)
	}
	t.Cleanup(func() { srv.Stop() })
	if resp := ask(t, path, Request{V: Version, Type: "status"}); !resp.OK {
		t.Errorf("status: %q", resp.Error)
	}
}
