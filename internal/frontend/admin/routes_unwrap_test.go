package admin

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestStatusRecorder_ResponseControllerReachesUnderlyingWriter is the L3 pin:
// the admin audit wrapper sits in front of EVERY authenticated admin request,
// and an embedded http.ResponseWriter promotes nothing, so without Unwrap()
// http.ResponseController degrades every optional capability to
// ErrNotSupported (bughunt 2026-10-06 L3; 2e0d736 fixed the s3 twin and the
// alt-svc wrapper but not this one). The control arm proves the check bites.
func TestStatusRecorder_ResponseControllerReachesUnderlyingWriter(t *testing.T) {
	rec := &statusRecorder{ResponseWriter: httptest.NewRecorder()}
	rc := http.NewResponseController(rec)
	if err := rc.Flush(); err != nil {
		t.Errorf("ResponseController.Flush through the admin wrapper = %v, want nil (the wrapper blocks it)", err)
	}
	// A control: a wrapper WITHOUT Unwrap must fail, proving the check bites.
	plain := struct{ http.ResponseWriter }{httptest.NewRecorder()}
	if err := http.NewResponseController(plain).Flush(); err == nil {
		t.Error("control: an unwrapping-free wrapper reported Flush support; the pin cannot fail")
	}
}
