// conformance_test.go — leaf 05 Task 1: the frontend conformance suite
// instantiated for webdav in BOTH bucket modes.
package webdav

import (
	"testing"

	"github.com/bhodgens/zeta-object/internal/frontend"
)

func TestWebdavFrontend_Conformance_ModeB(t *testing.T) {
	be := newStubBackend()
	f, err := New(be, Config{Bucket: "conf"}, WithAuthenticator(newStubAuth()))
	if err != nil {
		t.Fatal(err)
	}
	frontend.RunConformanceSuite(t, f, frontend.ConformanceOptions{})
}

func TestWebdavFrontend_Conformance_ModeA(t *testing.T) {
	be := newStubBackend()
	be.seed("photos", "a.txt", []byte("x"))
	f, err := New(be, Config{}, WithAuthenticator(newStubAuth()))
	if err != nil {
		t.Fatal(err)
	}
	frontend.RunConformanceSuite(t, f, frontend.ConformanceOptions{})
}
