package owncloud

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"sync"

	"github.com/bhodgens/zeta-object/internal/auth"
	"github.com/bhodgens/zeta-object/internal/backend"
	"github.com/bhodgens/zeta-object/internal/frontend/webdav"
	"github.com/bhodgens/zeta-object/internal/objectmodel"
)

// stubAuth is the permissive test authenticator (mirrors webdav's
// stubauth_test.go; auth behavior itself is pinned by internal/auth).
type stubAuth struct {
	deny bool
}

func (s *stubAuth) Authenticate(_ *http.Request) (auth.Identity, error) {
	if s.deny {
		return auth.Identity{}, auth.ErrBasicMissing
	}
	return auth.WildcardIdentity("oc-user"), nil
}

// nilBackend satisfies backend.Backend via embedding; OCS handlers never
// call it, and the webdav data plane only calls it for mode-A root
// listings (denied before use in these tests via auth.deny or non-root
// paths). The embedded nil interface keeps the method set total without
// inventing storage behavior.
type nilBackend struct{ backend.Backend }

// newWrappedWebdav builds a REAL webdav frontend (the composition target)
// in single-bucket mode: bucket-mode B root authorization does not touch
// the backend (dispatch.go authorizeRoot returns after the configured-
// bucket grant check), so the nil embedded backend is never dereferenced
// in these tests.
func newWrappedWebdav(deny bool) *webdav.Frontend {
	f, _ := webdav.New(nilBackend{}, webdav.Config{Bucket: "oc-bkt"}, webdav.WithAuthenticator(&stubAuth{deny: deny}))
	return f
}

var (
	_ = bytes.MinRead // keep bytes imported (objectmodel readers below)
	_ = io.Discard
	_ = context.Background
	_ = sync.Mutex{}
	_ = objectmodel.Object{}
	_ = backend.Backend(nilBackend{})
)
