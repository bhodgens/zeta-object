package ipc

// handler_test.go - the wire-level tests for the leaf-07 additions over
// a REAL unix socket: pause/resume, pin/pins, tombstones, and the
// unknown-type rejection preserved (leaf-01 contract).

import (
	"testing"
)

// stubHandler records calls; returns canned values.
type stubHandler struct {
	paused   bool
	pins     []string
	tombs    []Tombstone
	lastPin  string
	lastFlag bool
}

func (h *stubHandler) Pause() (bool, error)  { h.paused = true; return h.paused, nil }
func (h *stubHandler) Resume() (bool, error) { h.paused = false; return h.paused, nil }
func (h *stubHandler) SetPin(path string, pinned bool) error {
	h.lastPin, h.lastFlag = path, pinned
	return nil
}
func (h *stubHandler) Pins() ([]string, error) { return h.pins, nil }
func (h *stubHandler) Tombstones() ([]Tombstone, error) {
	return h.tombs, nil
}

// startHandlerServer serves with a stub handler.
func startHandlerServer(t *testing.T, h *stubHandler) string {
	t.Helper()
	path := shortSocket(t)
	srv, err := ServeWithHandler(path, "https://srv", "bkt", nil, h)
	if err != nil {
		t.Fatalf("ServeWithHandler: %v", err)
	}
	t.Cleanup(func() { srv.Stop() })
	return path
}

func TestPauseResumeWire(t *testing.T) {
	h := &stubHandler{}
	path := startHandlerServer(t, h)
	resp := ask(t, path, Request{V: Version, Type: "pause"})
	if !resp.OK || resp.Data == nil || !resp.Data.Paused {
		t.Fatalf("pause = (%v, %+v), want ok + paused:true", resp.OK, resp.Data)
	}
	if !h.paused {
		t.Error("handler.Pause not called")
	}
	resp = ask(t, path, Request{V: Version, Type: "resume"})
	if !resp.OK || resp.Data == nil || resp.Data.Paused {
		t.Fatalf("resume = (%v, %+v), want ok + paused:false", resp.OK, resp.Data)
	}
}

func TestPinWire(t *testing.T) {
	h := &stubHandler{pins: []string{"a"}}
	path := startHandlerServer(t, h)
	flag := true
	resp := ask(t, path, Request{V: Version, Type: "pin", Path: "a", Pin: &flag})
	if !resp.OK {
		t.Fatalf("pin = %q", resp.Error)
	}
	if h.lastPin != "a" || !h.lastFlag {
		t.Errorf("handler got path=%q pin=%v, want a/true", h.lastPin, h.lastFlag)
	}
	// pins list
	resp = ask(t, path, Request{V: Version, Type: "pins"})
	if !resp.OK || resp.Extra == nil {
		t.Fatalf("pins = (%v, %+v), want ok + extra", resp.OK, resp.Extra)
	}
	// pin requires path and flag
	resp = ask(t, path, Request{V: Version, Type: "pin"})
	if resp.OK || resp.Error == "" {
		t.Errorf("bare pin = ok, want validation error")
	}
}

func TestTombstonesWire(t *testing.T) {
	h := &stubHandler{tombs: []Tombstone{{Path: "gone", DeletedAt: 1, ExpiresAt: 2}}}
	path := startHandlerServer(t, h)
	resp := ask(t, path, Request{V: Version, Type: "tombstones"})
	if !resp.OK || resp.Extra == nil {
		t.Fatalf("tombstones = (%v, %+v), want ok + extra", resp.OK, resp.Extra)
	}
}

func TestStateChangingWithoutHandlerRejected(t *testing.T) {
	// Plain Serve (nil handler): the leaf-01 surface must be unchanged.
	path := startServer(t)
	resp := ask(t, path, Request{V: Version, Type: "pause"})
	if resp.OK || resp.Error == "" {
		t.Error("pause without handler = ok, want capability error")
	}
	resp = ask(t, path, Request{V: Version, Type: "tombstones"})
	if resp.OK {
		t.Error("tombstones without handler = ok, want error")
	}
}

func TestUnknownTypeStillRejected(t *testing.T) {
	h := &stubHandler{}
	path := startHandlerServer(t, h)
	resp := ask(t, path, Request{V: Version, Type: "frobnicate"})
	if resp.OK || resp.Error != "unknown request type" {
		t.Errorf("unknown type = (%v, %q), want rejection", resp.OK, resp.Error)
	}
}

func TestStatusStillWorksWithHandler(t *testing.T) {
	h := &stubHandler{}
	path := startHandlerServer(t, h)
	resp := ask(t, path, Request{V: Version, Type: "status"})
	if !resp.OK || resp.Data == nil || resp.Data.State != ServerState {
		t.Fatalf("status with handler = (%v, %+v), want leaf-01 shape", resp.OK, resp.Data)
	}
}
