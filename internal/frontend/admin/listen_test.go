package admin

import (
	"strings"
	"testing"
)

// TestLoopbackGuard covers the construction-time loopback guard: loopback
// hosts pass, every-interface and public binds fail without the option and
// pass with it, and an unresolvable host fails.
func TestLoopbackGuard(t *testing.T) {
	tests := []struct {
		name    string
		addr    string
		allow   bool
		wantErr bool
	}{
		{"ipv4 loopback", "127.0.0.1:9000", false, false},
		{"ipv6 loopback", "[::1]:9000", false, false},
		{"localhost", "localhost:9000", false, false},
		{"loopback port 0", "127.0.0.1:0", false, false},
		{"empty host binds all", ":9000", false, true},
		{"ipv4 any", "0.0.0.0:9000", false, true},
		{"private ip", "10.0.0.5:9000", false, true},
		{"unresolvable host", "no-such-host.invalid:9000", false, true},
		{"missing port", "127.0.0.1", false, true},
		{"empty host allowed with opt-out", ":9000", true, false},
		{"ipv4 any allowed with opt-out", "0.0.0.0:9000", true, false},
		{"private ip allowed with opt-out", "10.0.0.5:9000", true, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validateListenAddr(tt.addr, tt.allow)
			if (err != nil) != tt.wantErr {
				t.Fatalf("validateListenAddr(%q, %v) err = %v, wantErr %v", tt.addr, tt.allow, err, tt.wantErr)
			}
			if err != nil && !strings.Contains(err.Error(), tt.addr) {
				t.Fatalf("error %q must name the address %q", err, tt.addr)
			}
		})
	}
}

// A non-loopback address is rejected at CONSTRUCTION, not at request time, and
// the error names the address.
func TestNonLoopbackRejectedAtConstruction(t *testing.T) {
	dir := t.TempDir()
	caPath := writeTemp(t, dir, "ca.pem", newTestCA(t, "root").pemData)

	if _, err := New(Options{ListenAddr: "0.0.0.0:9000", ClientCAFile: caPath}); err == nil {
		t.Fatal("New accepted a non-loopback bind without allowNonLoopback")
	} else if !strings.Contains(err.Error(), "0.0.0.0:9000") {
		t.Fatalf("error %q must name the address", err)
	}

	// With the explicit opt-out the same address builds.
	if _, err := New(Options{ListenAddr: "0.0.0.0:9000", ClientCAFile: caPath, AllowNonLoopback: true}); err != nil {
		t.Fatalf("New with allowNonLoopback: %v", err)
	}
}
