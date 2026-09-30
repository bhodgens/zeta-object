// devmode_test.go — DevAuthenticator (pluggable-authentication tree leaf 05
// Task 1): anonymous wildcard identity, loud banner, per-request WARNING,
// injectable logger.
package auth_test

import (
	"bytes"
	"log"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/bhodgens/zeta-object/internal/auth"
)

func newDevWithBuffer(t *testing.T) (*auth.DevAuthenticator, *bytes.Buffer) {
	t.Helper()
	buf := &bytes.Buffer{}
	return auth.NewDevAuthenticator(log.New(buf, "", 0)), buf
}

func TestDevAuthenticatorIdentity(t *testing.T) {
	dev, _ := newDevWithBuffer(t)
	r := httptest.NewRequest("GET", "/some/bucket", nil)
	id, err := dev.Authenticate(r)
	if err != nil {
		t.Fatalf("Authenticate: %v", err)
	}
	if id.AccessKeyID != "anonymous" {
		t.Fatalf("AccessKeyID = %q, want anonymous", id.AccessKeyID)
	}
	for _, bucket := range []string{"a", "photos", "anything-at-all"} {
		if !id.CanRead(bucket) || !id.CanWrite(bucket) {
			t.Fatalf("dev identity denied on %q", bucket)
		}
	}
}

func TestDevAuthenticatorBanner(t *testing.T) {
	dev, buf := newDevWithBuffer(t)
	dev.Banner()
	out := buf.String()
	lines := strings.Split(strings.TrimRight(out, "\n"), "\n")
	if len(lines) < 3 {
		t.Fatalf("banner = %d lines, want >= 3:\n%s", len(lines), out)
	}
	if !strings.Contains(strings.ToUpper(out), "AUTHENTICATION DISABLED") {
		t.Errorf("banner missing AUTHENTICATION DISABLED:\n%s", out)
	}
	if !strings.Contains(strings.ToLower(out), "development only") {
		t.Errorf("banner missing 'development only':\n%s", out)
	}
}

func TestDevAuthenticatorPerRequestWarning(t *testing.T) {
	dev, buf := newDevWithBuffer(t)
	r := httptest.NewRequest("PUT", "/bkt/key", nil)
	if _, err := dev.Authenticate(r); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	lines := nonEmptyLines(out)
	if len(lines) != 1 {
		t.Fatalf("want exactly one log line per Authenticate, got %d:\n%s", len(lines), out)
	}
	if !strings.Contains(out, "WARNING") || !strings.Contains(out, "anonymous") {
		t.Fatalf("log line missing WARNING/anonymous:\n%s", out)
	}
}

// TestDevAuthenticatorSatisfiesSeam pins the frozen Authenticator shape.
func TestDevAuthenticatorSatisfiesSeam(t *testing.T) {
	var _ auth.Authenticator = (*auth.DevAuthenticator)(nil)
}

func nonEmptyLines(s string) []string {
	var out []string
	for l := range strings.SplitSeq(s, "\n") {
		if strings.TrimSpace(l) != "" {
			out = append(out, l)
		}
	}
	return out
}
