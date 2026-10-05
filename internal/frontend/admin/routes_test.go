package admin

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// auditCall is one recorded invocation of the injected audit seam.
type auditCall struct {
	principal, method, bucket, key, op string
	status                             int
	denied                             bool
}

// newRouteEnv builds a test frontend with the given services and an optional
// audit recorder.
func newRouteEnv(t *testing.T, svc Services, calls *[]auditCall) *testEnv {
	t.Helper()
	opts := Options{Services: svc}
	if calls != nil {
		opts.Audit = func(principal, method, bucket, key, op string, status int, denied bool) {
			*calls = append(*calls, auditCall{principal, method, bucket, key, op, status, denied})
		}
	}
	return newTestEnv(t, opts)
}

// authedJSON builds an authenticated request with a JSON body.
func (e *testEnv) authedJSON(method, path, body string) *http.Request {
	var r *http.Request
	if body == "" {
		r = httptest.NewRequest(method, path, nil)
	} else {
		r = httptest.NewRequest(method, path, strings.NewReader(body))
	}
	r.TLS = &tls.ConnectionState{PeerCertificates: []*x509.Certificate{e.client}}
	return r
}

// serve runs one authenticated request and returns the recorder.
func serve(t *testing.T, e *testEnv, method, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	rr := httptest.NewRecorder()
	e.frontend.Handler().ServeHTTP(rr, e.authedJSON(method, path, body))
	if ct := rr.Header().Get("Content-Type"); ct != "application/json" {
		t.Fatalf("%s %s: Content-Type = %q, want application/json", method, path, ct)
	}
	return rr
}

func mustJSON[T any](t *testing.T, rr *httptest.ResponseRecorder) T {
	t.Helper()
	var v T
	if err := json.Unmarshal(rr.Body.Bytes(), &v); err != nil {
		t.Fatalf("decode %q: %v", rr.Body.String(), err)
	}
	return v
}

// TestRoutes_Status pins GET /status passthrough.
func TestRoutes_Status(t *testing.T) {
	env := newRouteEnv(t, Services{Status: func(context.Context) (StatusReport, error) {
		return StatusReport{Version: "9.9.9", Listeners: []string{"127.0.0.1:9000"},
			MetadataProvider: MetadataProviderStatus{Available: false, Reason: "not ZFS"}}, nil
	}}, nil)
	rr := serve(t, env, http.MethodGet, "/status", "")
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rr.Code)
	}
	got := mustJSON[StatusReport](t, rr)
	if got.Version != "9.9.9" || got.MetadataProvider.Available {
		t.Fatalf("report = %+v", got)
	}
}

// TestRoutes_ConfigGetMasksSecrets pins that the config body never carries the
// literal secret the service was handed (the seam is the store's masked
// snapshot; here the stub asserts the admin package passes it through intact
// and the literal is absent).
func TestRoutes_ConfigGetMasksSecrets(t *testing.T) {
	const secret = "super-secret-value"
	body := `{"config":{"identities":[{"name":"a","accessKey":"AK","secretKey":"********"}]},"restartRequired":["listenAddr"]}`
	env := newRouteEnv(t, Services{GetConfig: func(context.Context) (json.RawMessage, error) {
		return json.RawMessage(body), nil
	}}, nil)
	rr := serve(t, env, http.MethodGet, "/config", "")
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rr.Code)
	}
	if strings.Contains(rr.Body.String(), secret) {
		t.Fatalf("config body leaked a secret: %s", rr.Body.String())
	}
	got := mustJSON[map[string]any](t, rr)
	if _, ok := got["restartRequired"]; !ok {
		t.Fatalf("config body missing restartRequired: %s", rr.Body.String())
	}
}

// TestRoutes_ConfigPut pins the 200 {applied,restartRequired} shape and the
// restart-required key landing ONLY under restartRequired.
func TestRoutes_ConfigPut(t *testing.T) {
	var gotPatch json.RawMessage
	env := newRouteEnv(t, Services{PutConfig: func(_ context.Context, patch json.RawMessage) (ConfigApplyResult, error) {
		gotPatch = patch
		return ConfigApplyResult{Applied: []string{"region"}, RestartRequired: []string{"listenAddr"}}, nil
	}}, nil)
	rr := serve(t, env, http.MethodPut, "/config", `{"region":"eu-west-1","listenAddr":"127.0.0.1:1"}`)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rr.Code)
	}
	if !strings.Contains(string(gotPatch), "eu-west-1") {
		t.Fatalf("patch not passed through: %s", gotPatch)
	}
	res := mustJSON[ConfigApplyResult](t, rr)
	if len(res.Applied) != 1 || res.Applied[0] != "region" {
		t.Fatalf("applied = %v", res.Applied)
	}
	if len(res.RestartRequired) != 1 || res.RestartRequired[0] != "listenAddr" {
		t.Fatalf("restartRequired = %v", res.RestartRequired)
	}
	for _, k := range res.Applied {
		if k == "listenAddr" {
			t.Fatal("restart-required key leaked into applied")
		}
	}
}

// TestRoutes_ConfigPutInvalid400 pins the validator message on 400.
func TestRoutes_ConfigPutInvalid400(t *testing.T) {
	env := newRouteEnv(t, Services{PutConfig: func(context.Context, json.RawMessage) (ConfigApplyResult, error) {
		return ConfigApplyResult{}, &ServiceError{Status: http.StatusBadRequest, Code: "InvalidConfiguration",
			Message: `json: unknown field "nope"`}
	}}, nil)
	rr := serve(t, env, http.MethodPut, "/config", `{"nope":1}`)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rr.Code)
	}
	if body := mustJSON[errorEnvelope](t, rr); !strings.Contains(body.Error.Message, "unknown field") {
		t.Fatalf("validator message lost: %+v", body.Error)
	}
}

// TestRoutes_ConfigSave pins POST /config/save.
func TestRoutes_ConfigSave(t *testing.T) {
	called := false
	env := newRouteEnv(t, Services{SaveConfig: func(context.Context) error { called = true; return nil }}, nil)
	rr := serve(t, env, http.MethodPost, "/config/save", "")
	if rr.Code != http.StatusOK || !called {
		t.Fatalf("status=%d called=%v", rr.Code, called)
	}
}

// TestRoutes_AuthReload pins POST /auth/reload wiring and the 500 failure path
// carrying the service error message.
func TestRoutes_AuthReload(t *testing.T) {
	env := newRouteEnv(t, Services{ReloadAuth: func(context.Context) error { return nil }}, nil)
	if rr := serve(t, env, http.MethodPost, "/auth/reload", ""); rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rr.Code)
	}
	fail := newRouteEnv(t, Services{ReloadAuth: func(context.Context) error {
		return &ServiceError{Status: http.StatusInternalServerError, Code: "ReloadFailed", Message: "duplicate access key AK"}
	}}, nil)
	rr := serve(t, fail, http.MethodPost, "/auth/reload", "")
	if rr.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", rr.Code)
	}
	if body := mustJSON[errorEnvelope](t, rr); !strings.Contains(body.Error.Message, "duplicate access key") {
		t.Fatalf("validator error lost: %+v", body.Error)
	}
}

// TestRoutes_ListBuckets pins GET /buckets passthrough.
func TestRoutes_ListBuckets(t *testing.T) {
	env := newRouteEnv(t, Services{ListBuckets: func(context.Context) (json.RawMessage, error) {
		return json.RawMessage(`{"buckets":[{"name":"a","backend":"fs","auditReads":false}]}`), nil
	}}, nil)
	rr := serve(t, env, http.MethodGet, "/buckets", "")
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rr.Code)
	}
	if !strings.Contains(rr.Body.String(), `"a"`) {
		t.Fatalf("body = %s", rr.Body.String())
	}
}

// TestRoutes_CreateBucket pins POST /buckets: the name comes from the body and
// is passed to the service.
func TestRoutes_CreateBucket(t *testing.T) {
	var created string
	env := newRouteEnv(t, Services{CreateBucket: func(_ context.Context, name string) error {
		created = name
		return nil
	}}, nil)
	rr := serve(t, env, http.MethodPost, "/buckets", `{"name":"newbkt"}`)
	if rr.Code != http.StatusOK || created != "newbkt" {
		t.Fatalf("status=%d created=%q", rr.Code, created)
	}
	// Missing name is a 400.
	if rr := serve(t, env, http.MethodPost, "/buckets", `{}`); rr.Code != http.StatusBadRequest {
		t.Fatalf("empty name: status = %d, want 400", rr.Code)
	}
}

// TestRoutes_BucketDetail pins GET /buckets/{name}.
func TestRoutes_BucketDetail(t *testing.T) {
	env := newRouteEnv(t, Services{BucketDetail: func(_ context.Context, name string) (json.RawMessage, error) {
		if name != "photos" {
			t.Errorf("name = %q, want photos", name)
		}
		return json.RawMessage(`{"name":"photos","backend":"fs","isDataset":true}`), nil
	}}, nil)
	rr := serve(t, env, http.MethodGet, "/buckets/photos", "")
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rr.Code)
	}
	got := mustJSON[map[string]any](t, rr)
	if got["isDataset"] != true {
		t.Fatalf("body = %s", rr.Body.String())
	}
	if _, ok := got["objectCount"]; ok {
		t.Fatal("objectCount must not be reported (no index exists)")
	}
}

// TestRoutes_DeleteBucketDatasetRefused pins the dataset-backed 409 with code
// DatasetBucketNotDeletable and the operator-workflow message.
func TestRoutes_DeleteBucketDatasetRefused(t *testing.T) {
	env := newRouteEnv(t, Services{DeleteBucket: func(_ context.Context, name string) error {
		return &ServiceError{Status: http.StatusConflict, Code: "DatasetBucketNotDeletable",
			Message: `bucket "photos" is a ZFS dataset; run "zfs destroy <dataset>" on the host (the management API never destroys datasets)`}
	}}, nil)
	rr := serve(t, env, http.MethodDelete, "/buckets/photos", "")
	if rr.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409", rr.Code)
	}
	body := mustJSON[errorEnvelope](t, rr)
	if body.Error.Code != "DatasetBucketNotDeletable" || !strings.Contains(body.Error.Message, "zfs destroy") {
		t.Fatalf("body = %+v, want DatasetBucketNotDeletable + zfs destroy hint", body.Error)
	}
}

// TestRoutes_BucketSettings pins PUT /buckets/{name}/settings passthrough.
func TestRoutes_BucketSettings(t *testing.T) {
	var got json.RawMessage
	env := newRouteEnv(t, Services{BucketSettings: func(_ context.Context, name string, patch json.RawMessage) error {
		if name != "photos" {
			t.Errorf("name = %q", name)
		}
		got = patch
		return nil
	}}, nil)
	rr := serve(t, env, http.MethodPut, "/buckets/photos/settings", `{"auditReads":true,"reflinkRetention":3}`)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rr.Code)
	}
	if !strings.Contains(string(got), "reflinkRetention") {
		t.Fatalf("patch lost: %s", got)
	}
}

// TestRoutes_Purge pins POST /purge: the named dataset reaches the service and
// the response echoes it.
func TestRoutes_Purge(t *testing.T) {
	var purged string
	env := newRouteEnv(t, Services{Purge: func(_ context.Context, dataset string) error {
		purged = dataset
		return nil
	}}, nil)
	rr := serve(t, env, http.MethodPost, "/purge", `{"dataset":"pool/ds"}`)
	if rr.Code != http.StatusOK || purged != "pool/ds" {
		t.Fatalf("status=%d purged=%q", rr.Code, purged)
	}
	if rr := serve(t, env, http.MethodPost, "/purge", `{}`); rr.Code != http.StatusBadRequest {
		t.Fatalf("empty dataset: status = %d, want 400", rr.Code)
	}
}

// TestRoutes_Unknown404AndMethod405 pins the JSON 404/405 envelope.
func TestRoutes_Unknown404AndMethod405(t *testing.T) {
	env := newRouteEnv(t, Services{}, nil)
	if rr := serve(t, env, http.MethodGet, "/nope", ""); rr.Code != http.StatusNotFound {
		t.Fatalf("unknown path: status = %d, want 404", rr.Code)
	}
	for _, tc := range []struct{ method, path string }{
		{http.MethodPost, "/status"},
		{http.MethodGet, "/config/save"},
		{http.MethodDelete, "/buckets"},
		{http.MethodGet, "/buckets/x/settings"},
		{http.MethodGet, "/purge"},
		{http.MethodGet, "/buckets/x/y"},
	} {
		rr := serve(t, env, tc.method, tc.path, "")
		if rr.Code != http.StatusMethodNotAllowed && rr.Code != http.StatusNotFound {
			t.Fatalf("%s %s: status = %d, want 405 or 404", tc.method, tc.path, rr.Code)
		}
		if body := mustJSON[errorEnvelope](t, rr); body.Error.Code == "" || body.Error.Message == "" {
			t.Fatalf("%s %s: empty error envelope", tc.method, tc.path)
		}
	}
}

// TestRoutes_EveryRouteRequiresCertificate pins that NO route is reachable
// without a verified client certificate, for every method of every route.
func TestRoutes_EveryRouteRequiresCertificate(t *testing.T) {
	env := newRouteEnv(t, Services{}, nil)
	requests := []struct{ method, path string }{
		{http.MethodGet, "/status"},
		{http.MethodGet, "/config"},
		{http.MethodPut, "/config"},
		{http.MethodPost, "/config/save"},
		{http.MethodPost, "/auth/reload"},
		{http.MethodGet, "/buckets"},
		{http.MethodPost, "/buckets"},
		{http.MethodGet, "/buckets/x"},
		{http.MethodDelete, "/buckets/x"},
		{http.MethodPut, "/buckets/x/settings"},
		{http.MethodPost, "/purge"},
		{http.MethodGet, "/nope"},
	}
	for _, tc := range requests {
		rr := httptest.NewRecorder()
		env.frontend.Handler().ServeHTTP(rr, httptest.NewRequest(tc.method, tc.path, nil))
		if rr.Code != http.StatusUnauthorized {
			t.Fatalf("%s %s without a certificate: status = %d, want 401", tc.method, tc.path, rr.Code)
		}
	}
}

// TestRoutes_AuditExactlyOnePerRequest pins Contract 6: exactly one audit
// record per authenticated request, op = admin, principal = CN, and the
// bucket/key coordinates and denied flag the route implies.
func TestRoutes_AuditExactlyOnePerRequest(t *testing.T) {
	svc := Services{
		Status: func(context.Context) (StatusReport, error) { return StatusReport{}, nil },
		CreateBucket: func(context.Context, string) error {
			return &ServiceError{Status: http.StatusConflict, Code: "BucketAlreadyOwnedByYou", Message: "exists"}
		},
		Purge: func(context.Context, string) error { return nil },
	}
	var calls []auditCall
	env := newRouteEnv(t, svc, &calls)

	steps := []struct {
		method, path, body  string
		wantStatus          int
		wantBucket, wantKey string
	}{
		{http.MethodGet, "/status", "", 200, "", ""},
		{http.MethodPost, "/buckets", `{"name":"photos"}`, 409, "photos", ""},
		{http.MethodPost, "/purge", `{"dataset":"pool/ds"}`, 200, "", "pool/ds"},
		{http.MethodGet, "/nope", "", 404, "", ""},
		{http.MethodPost, "/status", "", 405, "", ""},
	}
	for i, s := range steps {
		rr := serve(t, env, s.method, s.path, s.body)
		if rr.Code != s.wantStatus {
			t.Fatalf("step %d: status = %d, want %d", i, rr.Code, s.wantStatus)
		}
	}
	if len(calls) != len(steps) {
		t.Fatalf("audit calls = %d, want exactly %d (one per request)", len(calls), len(steps))
	}
	for i, s := range steps {
		c := calls[i]
		if c.principal != "tester" {
			t.Fatalf("step %d principal = %q, want the certificate CN", i, c.principal)
		}
		if c.op != "admin" {
			t.Fatalf("step %d op = %q, want admin", i, c.op)
		}
		if c.method != s.method || c.bucket != s.wantBucket || c.key != s.wantKey {
			t.Fatalf("step %d coords = %+v, want method=%s bucket=%q key=%q", i, c, s.method, s.wantBucket, s.wantKey)
		}
		if c.status != s.wantStatus {
			t.Fatalf("step %d status = %d, want %d", i, c.status, s.wantStatus)
		}
		if c.denied != (s.wantStatus >= 400) {
			t.Fatalf("step %d denied = %v, want %v", i, c.denied, s.wantStatus >= 400)
		}
	}
}

// TestRoutes_NilServices503 pins that a missing service is a 503, not a panic.
func TestRoutes_NilServices503(t *testing.T) {
	env := newRouteEnv(t, Services{}, nil)
	for _, tc := range []struct{ method, path, body string }{
		{http.MethodGet, "/status", ""},
		{http.MethodGet, "/config", ""},
		{http.MethodPut, "/config", `{}`},
		{http.MethodPost, "/config/save", ""},
		{http.MethodPost, "/auth/reload", ""},
		{http.MethodGet, "/buckets", ""},
		{http.MethodPost, "/buckets", `{"name":"x"}`},
		{http.MethodGet, "/buckets/x", ""},
		{http.MethodDelete, "/buckets/x", ""},
		{http.MethodPut, "/buckets/x/settings", `{}`},
		{http.MethodPost, "/purge", `{"dataset":"d"}`},
	} {
		rr := serve(t, env, tc.method, tc.path, tc.body)
		if rr.Code != http.StatusServiceUnavailable {
			t.Fatalf("%s %s: status = %d, want 503", tc.method, tc.path, rr.Code)
		}
	}
}
