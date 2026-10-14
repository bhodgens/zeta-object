package ipc

// m27_pins_test.go - the M27 regression: a nil pins slice must hit the
// wire as `"pins":[]`, never null (the macOS Swift decoder cannot
// represent null arrays).

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestPinsWireNilEncodesAsEmptyArray(t *testing.T) {
	h := &stubHandler{pins: nil} // no pins configured
	path := startHandlerServer(t, h)
	resp := ask(t, path, Request{V: Version, Type: "pins"})
	if !resp.OK {
		t.Fatalf("pins = %q", resp.Error)
	}
	var data PinData
	if err := remarshal(resp.Extra, &data); err != nil {
		t.Fatal(err)
	}
	if data.Pins == nil {
		t.Fatal("decoded pins is nil; the wire shape must carry a non-nil array")
	}
	if len(data.Pins) != 0 {
		t.Fatalf("pins = %v, want empty", data.Pins)
	}
	// Byte-level check: the raw JSON must not contain a null array.
	raw, err := json.Marshal(resp.Extra)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "null") {
		t.Fatalf("raw pins payload = %s, want [] not null", raw)
	}
	if string(raw) != `{"pins":[]}` {
		t.Fatalf("raw pins payload = %s, want {\"pins\":[]}", raw)
	}

	// Same over the pin (set) ack.
	flag := true
	resp = ask(t, path, Request{V: Version, Type: "pin", Path: "x", Pin: &flag})
	if !resp.OK {
		t.Fatalf("pin = %q", resp.Error)
	}
	raw, err = json.Marshal(resp.Extra)
	if err != nil {
		t.Fatal(err)
	}
	if string(raw) != `{"pins":[]}` {
		t.Fatalf("pin ack pins payload = %s, want {\"pins\":[]}", raw)
	}
}
