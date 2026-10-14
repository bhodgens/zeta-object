package ipc

// w2_m10_null_arrays_test.go - the W2-M10 regression: EVERY response
// slice must hit the wire as [] not null (the m27 pins rule, extended).
// The macOS Swift decoder discards a response whose arrays are null, so
// a nil tombstones/conflicts/deleted-paths slice silently ate the whole
// response. Byte-pinned like the m27 test.

import (
	"encoding/json"
	"strings"
	"testing"
)

// TestW2M10TombstonesWireNilEncodesAsEmptyArray
func TestW2M10TombstonesWireNilEncodesAsEmptyArray(t *testing.T) {
	h := &stubHandler{tombs: nil} // no tombstones recorded
	path := startHandlerServer(t, h)
	resp := ask(t, path, Request{V: Version, Type: "tombstones"})
	if !resp.OK {
		t.Fatalf("tombstones = %q", resp.Error)
	}
	raw, err := json.Marshal(resp.Extra)
	if err != nil {
		t.Fatal(err)
	}
	if string(raw) != `{"tombstones":[]}` {
		t.Fatalf("raw tombstones payload = %s, want {\"tombstones\":[]}", raw)
	}
	if strings.Contains(string(raw), "null") {
		t.Fatalf("raw tombstones payload = %s, must not carry a null array", raw)
	}
}

// TestW2M10ConflictsWireNilEncodesAsEmptyArray
func TestW2M10ConflictsWireNilEncodesAsEmptyArray(t *testing.T) {
	h := &leaf08Stub{conflicts: nil}
	path := startLeaf08Server(t, h)
	resp := ask(t, path, Request{V: Version, Type: "conflicts.list"})
	if !resp.OK {
		t.Fatalf("conflicts.list = %q", resp.Error)
	}
	raw, err := json.Marshal(resp.Extra)
	if err != nil {
		t.Fatal(err)
	}
	if string(raw) != `{"conflicts":[]}` {
		t.Fatalf("raw conflicts payload = %s, want {\"conflicts\":[]}", raw)
	}
	if strings.Contains(string(raw), "null") {
		t.Fatalf("raw conflicts payload = %s, must not carry a null array", raw)
	}
}

// TestW2M10DeletedListWireNilEncodesAsEmptyArray
func TestW2M10DeletedListWireNilEncodesAsEmptyArray(t *testing.T) {
	h := &leaf08Stub{deleted: nil}
	path := startLeaf08Server(t, h)
	resp := ask(t, path, Request{V: Version, Type: "deleted.list"})
	if !resp.OK {
		t.Fatalf("deleted.list = %q", resp.Error)
	}
	raw, err := json.Marshal(resp.Extra)
	if err != nil {
		t.Fatal(err)
	}
	if string(raw) != `{"tombstones":[]}` {
		t.Fatalf("raw deleted.list payload = %s, want {\"tombstones\":[]}", raw)
	}
	if strings.Contains(string(raw), "null") {
		t.Fatalf("raw deleted.list payload = %s, must not carry a null array", raw)
	}
}
