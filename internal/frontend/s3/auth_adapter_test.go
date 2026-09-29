package s3_test

import (
	"net/http"
	"testing"

	"mini-s3/internal/frontend/s3"
)

type staticCreds map[string]string

func (m staticCreds) SecretKey(accessKeyID string) (string, bool) {
	k, ok := m[accessKeyID]
	return k, ok
}

// mustSignRequest builds a SigV4-signed request using the same canonical
// construction the production code uses (migrated from package main's
// sigv4 test helpers during the leaf-02 move).
func mustSignRequest(t *testing.T, accessKey, secret string) *http.Request {
	t.Helper()
	return buildSignedRequestHelper(t, accessKey, secret, map[string]string{})
}

func TestS3Frontend_Authenticator(t *testing.T) {
	tests := []struct {
		name    string
		creds   staticCreds
		request *http.Request
		wantErr bool
	}{
		{
			name:    "unknown access key rejected",
			creds:   staticCreds{"minioadmin": "minioadmin"},
			request: mustSignRequest(t, "nobody", "nobody"),
			wantErr: true,
		},
		{
			name:    "known access key accepted",
			creds:   staticCreds{"minioadmin": "minioadmin"},
			request: buildSignedRequestHelper(t, "minioadmin", "minioadmin", nil),
			wantErr: false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := s3.New(nil /* backend unused by auth */, s3.WithCredentialSource(tt.creds))
			a := f.Authenticator()
			if a == nil {
				t.Fatal("Authenticator() = nil, want non-nil")
			}
			_, err := a.Authenticate(tt.request)
			if (err != nil) != tt.wantErr {
				t.Fatalf("Authenticate() err = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}
