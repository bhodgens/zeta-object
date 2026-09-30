package ftp

import (
	"strings"
	"testing"
)

// TestConfigFromOptions — fail-loud option validation (leaf 03): unknown
// key names the key; malformed values error; good values map through.
func TestConfigFromOptions(t *testing.T) {
	tests := []struct {
		name    string
		opts    map[string]string
		wantErr string
		check   func(t *testing.T, cfg Config)
	}{
		{
			name:    "unknown key is loud",
			opts:    map[string]string{"bogus": "1"},
			wantErr: "bogus",
		},
		{
			name:    "non-numeric passivePortMin",
			opts:    map[string]string{"passivePortMin": "abc"},
			wantErr: "passivePortMin",
		},
		{
			name: "valid range + publicIP",
			opts: map[string]string{"passivePortMin": "50000", "passivePortMax": "50100", "publicIP": "127.0.0.1"},
			check: func(t *testing.T, cfg Config) {
				if cfg.PassivePortMin != 50000 || cfg.PassivePortMax != 50100 || cfg.PublicIP != "127.0.0.1" {
					t.Fatalf("cfg = %+v", cfg)
				}
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg, err := ConfigFromOptions(":2121", tt.opts, nil, &staticVerifier{})
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("err = %v, want containing %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected err: %v", err)
			}
			if tt.check != nil {
				tt.check(t, cfg)
			}
		})
	}
}

// TestNewValidation — listenAddr and verifier are required (non-HTTP
// frontends cannot share the mux); a bad passive range fails.
func TestNewValidation(t *testing.T) {
	if _, err := New(newRecordingBackend(), Config{}); err == nil || !strings.Contains(err.Error(), "listenAddr") {
		t.Fatalf("New without listenAddr: err=%v, want listenAddr required", err)
	}
	if _, err := New(nil, Config{ListenAddr: ":2121", Verifier: &staticVerifier{}}); err == nil {
		t.Fatal("New(nil backend) must fail")
	}
	if _, err := New(newRecordingBackend(), Config{ListenAddr: ":2121"}); err == nil || !strings.Contains(err.Error(), "verifier") {
		t.Fatalf("New without verifier: err=%v", err)
	}
	if _, err := New(newRecordingBackend(), Config{
		ListenAddr: ":2121", Verifier: &staticVerifier{},
		PassivePortMin: 50100, PassivePortMax: 50000,
	}); err == nil || !strings.Contains(err.Error(), "passivePortMax") {
		t.Fatalf("New with inverted range: err=%v", err)
	}
}
