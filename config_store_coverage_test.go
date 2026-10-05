// config_store_coverage_test.go — direct coverage for the ConfigStore
// patch-value decoders added by the config-store tree (config_store.go).
package main

import (
	"encoding/json"
	"strings"
	"testing"
)

// TestDecodeBool table-tests the boolean patch-value decoder: valid booleans
// decode, and a non-boolean JSON value is wrapped with the "invalid value"
// context the store's validator surfaces.
func TestDecodeBool(t *testing.T) {
	tests := []struct {
		name    string
		raw     string
		want    bool
		wantErr bool
	}{
		{name: "true", raw: `true`, want: true},
		{name: "false", raw: `false`, want: false},
		{name: "null decodes to false", raw: `null`, want: false},
		{name: "string rejected", raw: `"yes"`, wantErr: true},
		{name: "number rejected", raw: `1`, wantErr: true},
		{name: "object rejected", raw: `{}`, wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := decodeBool(json.RawMessage(tt.raw))
			if tt.wantErr {
				if err == nil {
					t.Fatalf("decodeBool(%s) = %v, want an error", tt.raw, got)
				}
				if !strings.Contains(err.Error(), "invalid value") {
					t.Fatalf("decodeBool(%s) err = %q, want the invalid-value context", tt.raw, err.Error())
				}
				return
			}
			if err != nil {
				t.Fatalf("decodeBool(%s): %v", tt.raw, err)
			}
			if got != tt.want {
				t.Fatalf("decodeBool(%s) = %v, want %v", tt.raw, got, tt.want)
			}
		})
	}
}
