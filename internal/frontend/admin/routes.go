package admin

import (
	"encoding/json"
	"net/http"
)

// errorEnvelope is the JSON error shape for every management response:
// {"error":{"code":"...","message":"..."}}.
type errorEnvelope struct {
	Error errorBody `json:"error"`
}

type errorBody struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

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

// writeError writes the standard JSON error envelope.
func writeError(w http.ResponseWriter, status int, code, message string) {
	writeJSON(w, status, errorEnvelope{Error: errorBody{Code: code, Message: message}})
}

// serveHTTP is the auth-wrapped entry point. Every route, including /status,
// requires a verified client certificate; there is no unauthenticated route.
func (f *adminFrontend) serveHTTP(w http.ResponseWriter, r *http.Request) {
	principal, status, code := f.authenticate(r)
	if status != 0 {
		writeError(w, status, code, messageForCode(code))
		return
	}
	f.dispatch(w, r, principal)
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

// dispatch is the route table. This leaf serves GET /status only; leaf 04
// adds the configuration and bucket routes. Every other path and method is a
// JSON 404.
func (f *adminFrontend) dispatch(w http.ResponseWriter, r *http.Request, _ string) {
	if r.Method == http.MethodGet && r.URL.Path == "/status" {
		f.handleStatus(w, r)
		return
	}
	writeError(w, http.StatusNotFound, "NotFound", "no such route")
}

// handleStatus serves GET /status. A nil Status service is a 503 (a
// construction bug the wiring test prevents from shipping), never a 500 or an
// empty body.
func (f *adminFrontend) handleStatus(w http.ResponseWriter, r *http.Request) {
	if f.opts.Services.Status == nil {
		writeError(w, http.StatusServiceUnavailable, "NotImplemented", "GET /status is not implemented")
		return
	}
	report, err := f.opts.Services.Status(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "InternalError", "status unavailable")
		return
	}
	writeJSON(w, http.StatusOK, report)
}
