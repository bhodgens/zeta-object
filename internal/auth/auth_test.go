package auth_test

import (
	"net/http"
	"testing"

	"mini-s3/internal/auth"
)

type stubAuth struct{}

func (stubAuth) Authenticate(r *http.Request) (auth.Identity, error) {
	return auth.Identity{AccessKeyID: "AKID"}, nil
}

func TestAuthenticator_InterfaceShape(t *testing.T) {
	tests := []struct {
		name string
		a    auth.Authenticator
		want string
	}{
		{"stub returns identity", stubAuth{}, "AKID"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			id, err := tt.a.Authenticate(&http.Request{})
			if err != nil {
				t.Fatalf("Authenticate: %v", err)
			}
			if id.AccessKeyID != tt.want {
				t.Fatalf("AccessKeyID = %q, want %q", id.AccessKeyID, tt.want)
			}
		})
	}
}

func TestGrant_Fields(t *testing.T) {
	g := auth.Grant{Read: true, Write: false}
	if !g.Read || g.Write {
		t.Fatalf("Grant = %+v, want Read=true Write=false", g)
	}
}
