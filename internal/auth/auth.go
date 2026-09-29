// Package auth defines the v1 authenticator seam. The open GitHub issue
// "auth: pluggable authentication architecture" will replace this placeholder
// with a real identity model; until then this shape is frozen.
package auth

import "net/http"

type Authenticator interface {
	Authenticate(r *http.Request) (Identity, error)
}

type Identity struct {
	AccessKeyID  string
	BucketGrants map[string]Grant
}

type Grant struct {
	Read  bool
	Write bool
}
