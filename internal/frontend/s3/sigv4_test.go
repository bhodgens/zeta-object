package s3_test

import (
	"net/http"
	"testing"

	"github.com/bhodgens/zeta-object/internal/frontend/s3"
)

// TestSigV4_HeaderAndPresigned migrates representative SigV4 cases from
// package main's sigv4 test files into the s3 frontend package
// (leaf-02 Task 2).
func TestSigV4_HeaderAndPresigned(t *testing.T) {
	tests := []struct {
		name    string
		kind    string // "header" | "presigned"
		expired bool
		opts    map[string]string
		wantErr bool
	}{
		{name: "valid header signature", kind: "header", wantErr: false},
		{name: "valid presigned URL", kind: "presigned", wantErr: false},
		{name: "bad signature rejected", kind: "header", opts: map[string]string{"signature": "0000000000000000000000000000000000000000000000000000000000000000"}, wantErr: true},
		{name: "non-hex signature rejected", kind: "header", opts: map[string]string{"signature": "zz-not-hex-or-full-length"}, wantErr: true},
		{name: "expired presigned rejected", kind: "presigned", expired: true, wantErr: true},
		// region-config-2026-10 leaf 02 (Contract 2): default mode is
		// permissive - a well-formed non-default client region is accepted
		// with a notice. Strict mismatch rejection is pinned by
		// TestRegionMatrix and TestRegionMatrix_StrictMismatchCode.
		{name: "different region accepted in permissive default mode", kind: "header", opts: map[string]string{"region": "eu-west-1"}, wantErr: false},
		{name: "scope date mismatch rejected", kind: "header", opts: map[string]string{"scopeDate": "20000101"}, wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := s3.New(nil, s3.WithCredentialSource(staticCreds{"zetaadmin": "zetaadmin"}))
			a := f.Authenticator()

			var req *http.Request
			switch tt.kind {
			case "presigned":
				req = buildPresignedRequestHelper(t, "zetaadmin", "zetaadmin", tt.expired)
			default:
				req = buildSignedRequestHelper(t, "zetaadmin", "zetaadmin", tt.opts)
			}

			_, err := a.Authenticate(req)
			if (err != nil) != tt.wantErr {
				t.Fatalf("%s: Authenticate() err = %v, wantErr %v", tt.kind, err, tt.wantErr)
			}
		})
	}
}
