package admin

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"

	"github.com/bhodgens/zeta-object/internal/auth"
)

// maxManagementBodyBytes bounds a management request body (config patches are
// small JSON objects); an oversized body is a 400, never an unbounded read.
const maxManagementBodyBytes = 1 << 20 // 1 MiB

// errorEnvelope is the JSON error shape for every management response:
// {"error":{"code":"...","message":"..."}}.
type errorEnvelope struct {
	Error errorBody `json:"error"`
}

type errorBody struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// ServiceError lets an injected service choose the response status and the
// JSON error code (e.g. a bucketmanager error mapped to 409
// DatasetBucketNotDeletable, or a config validator error mapped to 400). A
// service returning any other error maps to a generic 500.
type ServiceError struct {
	Status  int
	Code    string
	Message string
}

func (e *ServiceError) Error() string { return e.Message }

// writeJSON encodes body as application/json with the given status.
func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(body); err != nil {
		// The status line and headers are already committed; there is no
		// recovery, and the error carries nothing actionable.
		return
	}
}

// writeRawJSON writes an already-rendered JSON document. A nil/empty document
// becomes JSON null.
func writeRawJSON(w http.ResponseWriter, status int, raw json.RawMessage) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if len(raw) == 0 {
		raw = json.RawMessage("null")
	}
	if err := json.NewEncoder(w).Encode(raw); err != nil {
		return
	}
}

// writeError writes the standard JSON error envelope.
func writeError(w http.ResponseWriter, status int, code, message string) {
	writeJSON(w, status, errorEnvelope{Error: errorBody{Code: code, Message: message}})
}

// writeServiceFailure maps a service error onto the JSON envelope: a
// *ServiceError carries the status and code the service chose; anything else
// is a generic 500 that leaks no internal detail.
func writeServiceFailure(w http.ResponseWriter, action string, err error) {
	if se, ok := errors.AsType[*ServiceError](err); ok {
		writeError(w, se.Status, se.Code, se.Message)
		return
	}
	writeError(w, http.StatusInternalServerError, "InternalError", action+" failed")
}

// serviceUnavailable is the 503 for a route whose service was never wired (a
// construction bug the wiring test prevents from shipping).
func serviceUnavailable(w http.ResponseWriter, route string) {
	writeError(w, http.StatusServiceUnavailable, "NotImplemented", route+" is not implemented")
}

// statusRecorder wraps http.ResponseWriter to capture the handler's response
// status for the audit record (WriteHeader defaulting to 200 via the implicit
// Write path).
type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (s *statusRecorder) WriteHeader(code int) {
	s.status = code
	s.ResponseWriter.WriteHeader(code)
}

func (s *statusRecorder) Write(b []byte) (int, error) {
	if s.status == 0 {
		s.status = http.StatusOK
	}
	return s.ResponseWriter.Write(b)
}

// serveHTTP is the auth-wrapped entry point. Every route, including /status,
// requires a verified client certificate; there is no unauthenticated route.
// Every AUTHENTICATED request appends EXACTLY ONE audit record (principal =
// certificate CN, op = admin); a rejected handshake writes no record because
// there is no principal to attribute it to.
func (f *adminFrontend) serveHTTP(w http.ResponseWriter, r *http.Request) {
	principal, status, code := f.authenticate(r)
	if status != 0 {
		writeError(w, status, code, messageForCode(code))
		return
	}
	rec := &statusRecorder{ResponseWriter: w}
	bucket, key := f.dispatch(rec, r)
	if rec.status == 0 {
		rec.status = http.StatusOK
	}
	f.auditRequest(principal, r.Method, bucket, key, rec.status)
}

// auditRequest appends the single per-request audit record (no-op when no
// audit seam is injected).
func (f *adminFrontend) auditRequest(principal, method, bucket, key string, status int) {
	if f.opts.Audit == nil {
		return
	}
	f.opts.Audit(principal, method, bucket, key, string(auth.OpAdmin), status, status >= 400)
}

// messageForCode is deliberately generic: authentication failures must leak
// nothing about WHY the request was rejected (missing certificate, wrong
// issuer, expiry).
func messageForCode(code string) string {
	switch code {
	case "Unauthorized":
		return "client certificate authentication required"
	case "Forbidden":
		return "client certificate is not authorized"
	default:
		return "request rejected"
	}
}

// routeKind identifies a management route independently of its method.
type routeKind int

const (
	routeUnknown routeKind = iota
	routeStatus
	routeConfig
	routeConfigSave
	routeAuthReload
	routeBuckets
	routeBucket
	routeBucketSettings
	routePurge
)

// classify maps (method, path) to a route kind, its {name} path parameter and
// whether the method is allowed for that route. An unknown path yields
// routeUnknown; a known path with a wrong method yields a known kind with
// methodOK == false (a 405).
func classify(method, path string) (kind routeKind, name string, methodOK bool) {
	switch path {
	case "/status":
		return routeStatus, "", method == http.MethodGet
	case "/config":
		return routeConfig, "", method == http.MethodGet || method == http.MethodPut
	case "/config/save":
		return routeConfigSave, "", method == http.MethodPost
	case "/auth/reload":
		return routeAuthReload, "", method == http.MethodPost
	case "/buckets":
		return routeBuckets, "", method == http.MethodGet || method == http.MethodPost
	case "/purge":
		return routePurge, "", method == http.MethodPost
	}
	if !strings.HasPrefix(path, "/buckets/") {
		return routeUnknown, "", false
	}
	rest := strings.TrimPrefix(path, "/buckets/")
	if n, ok := strings.CutSuffix(rest, "/settings"); ok {
		if n == "" || strings.Contains(n, "/") {
			return routeUnknown, "", false
		}
		return routeBucketSettings, n, method == http.MethodPut
	}
	if rest == "" || strings.Contains(rest, "/") {
		return routeUnknown, "", false
	}
	return routeBucket, rest, method == http.MethodGet || method == http.MethodDelete
}

// dispatch is the route table. It returns the audit coordinates (bucket, key)
// for the request; the caller records them. Unknown paths are a JSON 404 and a
// wrong method on a known path is a JSON 405.
func (f *adminFrontend) dispatch(w http.ResponseWriter, r *http.Request) (bucket, key string) {
	kind, name, methodOK := classify(r.Method, r.URL.Path)
	if kind == routeUnknown {
		writeError(w, http.StatusNotFound, "NotFound", "no such route")
		return "", ""
	}
	if !methodOK {
		writeError(w, http.StatusMethodNotAllowed, "MethodNotAllowed", "method not allowed for this route")
		return "", ""
	}
	switch kind {
	case routeStatus:
		f.handleStatus(w, r)
		return "", ""
	case routeConfig:
		if r.Method == http.MethodGet {
			f.handleGetConfig(w, r)
		} else {
			f.handlePutConfig(w, r)
		}
		return "", ""
	case routeConfigSave:
		f.handleSaveConfig(w, r)
		return "", ""
	case routeAuthReload:
		f.handleAuthReload(w, r)
		return "", ""
	case routeBuckets:
		if r.Method == http.MethodGet {
			f.handleListBuckets(w, r)
		} else {
			return f.handleCreateBucket(w, r)
		}
	case routeBucket:
		if r.Method == http.MethodGet {
			f.handleBucketDetail(w, r, name)
		} else {
			f.handleDeleteBucket(w, r, name)
		}
		return name, ""
	case routeBucketSettings:
		f.handleBucketSettings(w, r, name)
		return name, ""
	case routePurge:
		return f.handlePurge(w, r)
	}
	return "", ""
}

// decodeBody reads + unmarshals a bounded JSON request body into v, answering
// a 400 with the fix on any failure. It returns false when it already
// answered.
func decodeBody(w http.ResponseWriter, r *http.Request, v any) bool {
	r.Body = http.MaxBytesReader(w, r.Body, maxManagementBodyBytes)
	raw, err := io.ReadAll(r.Body)
	if err != nil {
		writeError(w, http.StatusBadRequest, "InvalidArgument", "reading request body: "+err.Error())
		return false
	}
	if err := json.Unmarshal(raw, v); err != nil {
		writeError(w, http.StatusBadRequest, "InvalidArgument", "request body must be a JSON object: "+err.Error())
		return false
	}
	return true
}

// handleStatus serves GET /status. A nil Status service is a 503 (a
// construction bug the wiring test prevents from shipping).
func (f *adminFrontend) handleStatus(w http.ResponseWriter, r *http.Request) {
	if f.opts.Services.Status == nil {
		serviceUnavailable(w, "GET /status")
		return
	}
	report, err := f.opts.Services.Status(r.Context())
	if err != nil {
		writeServiceFailure(w, "status", err)
		return
	}
	writeJSON(w, http.StatusOK, report)
}

// handleGetConfig serves GET /config: the effective configuration (secrets
// masked by the store) plus the restart-required list.
func (f *adminFrontend) handleGetConfig(w http.ResponseWriter, r *http.Request) {
	if f.opts.Services.GetConfig == nil {
		serviceUnavailable(w, "GET /config")
		return
	}
	raw, err := f.opts.Services.GetConfig(r.Context())
	if err != nil {
		writeServiceFailure(w, "config read", err)
		return
	}
	writeRawJSON(w, http.StatusOK, raw)
}

// handlePutConfig serves PUT /config: a partial update through the store. On
// success the body is {applied:[...], restartRequired:[...]}; an invalid patch
// is a 400 carrying the validator message and nothing changed.
func (f *adminFrontend) handlePutConfig(w http.ResponseWriter, r *http.Request) {
	if f.opts.Services.PutConfig == nil {
		serviceUnavailable(w, "PUT /config")
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxManagementBodyBytes)
	raw, err := io.ReadAll(r.Body)
	if err != nil {
		writeError(w, http.StatusBadRequest, "InvalidArgument", "reading request body: "+err.Error())
		return
	}
	result, err := f.opts.Services.PutConfig(r.Context(), json.RawMessage(raw))
	if err != nil {
		writeServiceFailure(w, "config update", err)
		return
	}
	writeJSON(w, http.StatusOK, result)
}

// handleSaveConfig serves POST /config/save: persist the live configuration to
// the config file atomically.
func (f *adminFrontend) handleSaveConfig(w http.ResponseWriter, r *http.Request) {
	if f.opts.Services.SaveConfig == nil {
		serviceUnavailable(w, "POST /config/save")
		return
	}
	if err := f.opts.Services.SaveConfig(r.Context()); err != nil {
		writeServiceFailure(w, "config save", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"saved": true})
}

// handleAuthReload serves POST /auth/reload: re-run the identity reload path
// and refresh the admin listener's trusted client CA.
func (f *adminFrontend) handleAuthReload(w http.ResponseWriter, r *http.Request) {
	if f.opts.Services.ReloadAuth == nil {
		serviceUnavailable(w, "POST /auth/reload")
		return
	}
	if err := f.opts.Services.ReloadAuth(r.Context()); err != nil {
		writeServiceFailure(w, "auth reload", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"reloaded": true})
}

// handleListBuckets serves GET /buckets.
func (f *adminFrontend) handleListBuckets(w http.ResponseWriter, r *http.Request) {
	if f.opts.Services.ListBuckets == nil {
		serviceUnavailable(w, "GET /buckets")
		return
	}
	raw, err := f.opts.Services.ListBuckets(r.Context())
	if err != nil {
		writeServiceFailure(w, "bucket list", err)
		return
	}
	writeRawJSON(w, http.StatusOK, raw)
}

// handleCreateBucket serves POST /buckets; the name is in the body.
func (f *adminFrontend) handleCreateBucket(w http.ResponseWriter, r *http.Request) (bucket, key string) {
	if f.opts.Services.CreateBucket == nil {
		serviceUnavailable(w, "POST /buckets")
		return "", ""
	}
	var body struct {
		Name string `json:"name"`
	}
	if !decodeBody(w, r, &body) {
		return "", ""
	}
	if strings.TrimSpace(body.Name) == "" {
		writeError(w, http.StatusBadRequest, "InvalidArgument", `body must carry a non-empty "name"`)
		return "", ""
	}
	if err := f.opts.Services.CreateBucket(r.Context(), body.Name); err != nil {
		writeServiceFailure(w, "bucket create", err)
		return body.Name, ""
	}
	writeJSON(w, http.StatusOK, map[string]any{"created": true, "name": body.Name})
	return body.Name, ""
}

// handleBucketDetail serves GET /buckets/{name}.
func (f *adminFrontend) handleBucketDetail(w http.ResponseWriter, r *http.Request, name string) {
	if f.opts.Services.BucketDetail == nil {
		serviceUnavailable(w, "GET /buckets/{name}")
		return
	}
	raw, err := f.opts.Services.BucketDetail(r.Context(), name)
	if err != nil {
		writeServiceFailure(w, "bucket detail", err)
		return
	}
	writeRawJSON(w, http.StatusOK, raw)
}

// handleDeleteBucket serves DELETE /buckets/{name}: plain-directory buckets
// only; a dataset-backed bucket is refused by the service with 409
// DatasetBucketNotDeletable.
func (f *adminFrontend) handleDeleteBucket(w http.ResponseWriter, r *http.Request, name string) {
	if f.opts.Services.DeleteBucket == nil {
		serviceUnavailable(w, "DELETE /buckets/{name}")
		return
	}
	if err := f.opts.Services.DeleteBucket(r.Context(), name); err != nil {
		writeServiceFailure(w, "bucket delete", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"deleted": true})
}

// handleBucketSettings serves PUT /buckets/{name}/settings: per-bucket
// auditReads and reflinkRetention through the store's hot-apply path.
func (f *adminFrontend) handleBucketSettings(w http.ResponseWriter, r *http.Request, name string) {
	if f.opts.Services.BucketSettings == nil {
		serviceUnavailable(w, "PUT /buckets/{name}/settings")
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxManagementBodyBytes)
	raw, err := io.ReadAll(r.Body)
	if err != nil {
		writeError(w, http.StatusBadRequest, "InvalidArgument", "reading request body: "+err.Error())
		return
	}
	if !json.Valid(raw) {
		writeError(w, http.StatusBadRequest, "InvalidArgument", "request body must be valid JSON")
		return
	}
	if err := f.opts.Services.BucketSettings(r.Context(), name, json.RawMessage(raw)); err != nil {
		writeServiceFailure(w, "bucket settings", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"updated": true})
}

// handlePurge serves POST /purge: metadata history purge for the named
// dataset. This is the only irreversibly destructive route (it clears the
// event history AND the permanent gap/loss record). The dataset is the audit
// key.
func (f *adminFrontend) handlePurge(w http.ResponseWriter, r *http.Request) (bucket, key string) {
	if f.opts.Services.Purge == nil {
		serviceUnavailable(w, "POST /purge")
		return "", ""
	}
	var body struct {
		Dataset string `json:"dataset"`
	}
	if !decodeBody(w, r, &body) {
		return "", ""
	}
	if strings.TrimSpace(body.Dataset) == "" {
		writeError(w, http.StatusBadRequest, "InvalidArgument", `body must carry a non-empty "dataset"`)
		return "", ""
	}
	if err := f.opts.Services.Purge(r.Context(), body.Dataset); err != nil {
		writeServiceFailure(w, "purge", err)
		return "", body.Dataset
	}
	writeJSON(w, http.StatusOK, map[string]any{"purged": true, "dataset": body.Dataset})
	return "", body.Dataset
}
