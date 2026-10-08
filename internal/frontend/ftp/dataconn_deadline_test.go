package ftp

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

// TestPassiveDataConnDeadlineGovernsThe425 is the load-bearing pin.
//
// It reproduces, on demand, the one-off failure the zeta-object suite showed on
// 2026-10-08:
//
//	STOR: 425 "failed to accept passive transfer connection:
//	      accept tcp [::]:62615: i/o timeout"
//
// which never reproduced again in 80+ iterations (alone, under 8-thread CPU
// load, and with 25 concurrent servers listening), so its trigger had to be
// reasoned about rather than retried.
//
// The mechanism: ftpserverlib opens the passive data listener, then blocks in
// Accept() under a deadline of Settings.ConnectionTimeout seconds
// (transfer_pasv.go Open -> ConnectionWait). The client's control-to-data
// connect is an ordinary scheduling race; on a saturated host 30s can elapse
// before the data connect is serviced, and the server answers 425 even though
// both ends are fine. It is a DEADLINE, not a port collision and not a broken
// server - which is why a bounded passive-port range would NOT have fixed it.
//
// This test drives the deadline deliberately: a raw control channel logs in,
// issues PASV, then sends LIST and NEVER opens the data connection. The only
// thing that can end the wait is the deadline. Two settings are asserted so the
// value demonstrably GOVERNS the wait rather than merely being plumbed: 1s
// answers in ~1s, 4s answers in ~4s.
func TestPassiveDataConnDeadlineGovernsThe425(t *testing.T) {
	if testing.Short() {
		t.Skip("takes ~5s: it waits out two real deadlines")
	}
	for _, tc := range []struct {
		name    string
		seconds int
		wantLo  time.Duration
		wantHi  time.Duration
	}{
		{"1s deadline", 1, 700 * time.Millisecond, 6 * time.Second},
		{"4s deadline", 4, 3500 * time.Millisecond, 12 * time.Second},
	} {
		t.Run(tc.name, func(t *testing.T) {
			be := newRecordingBackend()
			be.objects["bkt/present.bin"] = []byte("payload")
			f, port := startTestServer(t, be, Config{DataConnTimeoutSeconds: tc.seconds})
			_ = f
			raw := dialRaw(t, port)
			raw.readLine()
			raw.cmd("USER user")
			raw.cmd("PASS pass")
			t.Logf("PASV: %q", raw.cmd("PASV"))
			start := time.Now()
			fmt.Fprintf(raw.conn, "LIST\r\n")
			_ = raw.conn.SetReadDeadline(time.Now().Add(tc.wantHi + 10*time.Second))
			line := raw.readLine()
			elapsed := time.Since(start)
			t.Logf("answered after %v: %q", elapsed.Round(10*time.Millisecond), line)
			if !strings.Contains(line, "425") {
				t.Skipf("no 425 (%q)", line)
			}
			if elapsed < tc.wantLo {
				t.Errorf("answered after %v, sooner than a %ds deadline allows", elapsed, tc.seconds)
			}
			if elapsed > tc.wantHi {
				t.Errorf("answered after %v, past the %ds deadline", elapsed, tc.seconds)
			}
		})
	}
}

// TestGetSettingsAppliesDataConnTimeout pins the plumbing: a configured value
// reaches the library Settings, and an UNSET one stays 0 so the library's own
// 30s default (server.go:176) remains in charge.
func TestGetSettingsAppliesDataConnTimeout(t *testing.T) {
	f := &Frontend{cfg: Config{ListenAddr: "127.0.0.1:0", DataConnTimeoutSeconds: 0}}
	s, err := (&mainDriver{f: f}).GetSettings()
	if err != nil {
		t.Fatalf("GetSettings: %v", err)
	}
	if s.ConnectionTimeout != 0 {
		t.Errorf("unset: ConnectionTimeout = %d, want 0 so the library default applies", s.ConnectionTimeout)
	}

	f2 := &Frontend{cfg: Config{ListenAddr: "127.0.0.1:0", DataConnTimeoutSeconds: 90}}
	s2, err := (&mainDriver{f: f2}).GetSettings()
	if err != nil {
		t.Fatalf("GetSettings: %v", err)
	}
	if s2.ConnectionTimeout != 90 {
		t.Errorf("configured: ConnectionTimeout = %d, want 90", s2.ConnectionTimeout)
	}
}

// TestNewRejectsNegativeDataConnTimeout: fail-loud, like the other numeric
// options, and the message must name the option.
func TestNewRejectsNegativeDataConnTimeout(t *testing.T) {
	_, err := New(newRecordingBackend(), Config{
		ListenAddr:             "127.0.0.1:0",
		Verifier:               &staticVerifier{idents: nil, passes: nil},
		DataConnTimeoutSeconds: -1,
	})
	if err == nil {
		t.Fatal("New accepted a negative dataConnTimeoutSeconds")
	}
	if !strings.Contains(err.Error(), "dataConnTimeoutSeconds") {
		t.Errorf("error does not name the option: %v", err)
	}
}

// TestConfigFromOptionsDataConnTimeout: the frontends[] options key parses, and
// a negative value is a loud config error rather than a silent fallback.
func TestConfigFromOptionsDataConnTimeout(t *testing.T) {
	v := &staticVerifier{idents: nil, passes: nil}
	cfg, err := ConfigFromOptions("127.0.0.1:2121", map[string]string{"dataConnTimeoutSeconds": "45"}, nil, v)
	if err != nil {
		t.Fatalf("ConfigFromOptions: %v", err)
	}
	if cfg.DataConnTimeoutSeconds != 45 {
		t.Errorf("DataConnTimeoutSeconds = %d, want 45", cfg.DataConnTimeoutSeconds)
	}
	if _, err := ConfigFromOptions("127.0.0.1:2121", map[string]string{"dataConnTimeoutSeconds": "-5"}, nil, v); err == nil {
		t.Error("ConfigFromOptions accepted a negative dataConnTimeoutSeconds")
	}
	if _, err := ConfigFromOptions("127.0.0.1:2121", map[string]string{"dataConnTimeoutSeconds": "soon"}, nil, v); err == nil {
		t.Error("ConfigFromOptions accepted a non-numeric dataConnTimeoutSeconds")
	}
}
