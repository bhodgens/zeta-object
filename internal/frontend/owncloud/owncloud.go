// Package owncloud implements the ownCloud protocol frontend: the OCS
// negotiation surface (/ocs/v1.php, /ocs/v2.php) layered over the landed
// webdav data plane (internal/frontend/webdav).
//
// Scope (adapted leaf-01 decision, decision.md): the minimal
// WebDAV+OCS subset — OCS capabilities negotiation, cloud/user, and the
// wrapped WebDAV handler for all data transfer. Shares/provisioning and
// every other classic-API endpoint answer with a DOCUMENTED OCS error
// envelope (404, message naming the degradation) — never silent
// emulation, never an invented success.
//
// Composition (master Contract 3): Frontend wraps the webdav frontend.
// OCS paths (/ocs/) are routed to the OCS handlers; every other path —
// including /remote.php/webdav/** — delegates to the wrapped webdav
// Handler() UNCHANGED, so the data plane, its auth pipeline, and its
// authorization behavior are the already-verified webdav code. Auth is
// the same Basic authenticator shape webdav uses, constructed by main's
// factory over the identity registry.
//
// This package performs no storage access of its own: the webdav
// frontend holds the Backend reference; OCS responses derive from the
// frontend's true state only.
package owncloud

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"github.com/bhodgens/zeta-object/internal/auth"
	"github.com/bhodgens/zeta-object/internal/frontend"
	"github.com/bhodgens/zeta-object/internal/frontend/webdav"
)

// webdavFrontend is the slice of the wrapped webdav frontend the owncloud
// composition depends on (interface segregation: the package sees only
// the delegation surface, not webdav internals).
type webdavFrontend interface {
	FrontendName() string
	Handler() http.Handler
	Authenticator() auth.Authenticator
	Capabilities() frontend.ProtocolCaps
}

// webdavAdapter adapts *webdav.Frontend to webdavFrontend.
type webdavAdapter struct{ inner *webdav.Frontend }

func (a webdavAdapter) FrontendName() string                { return a.inner.Name() }
func (a webdavAdapter) Handler() http.Handler               { return a.inner.Handler() }
func (a webdavAdapter) Authenticator() auth.Authenticator   { return a.inner.Authenticator() }
func (a webdavAdapter) Capabilities() frontend.ProtocolCaps { return a.inner.Capabilities() }

// Frontend implements frontend.Frontend with Name() == "owncloud"
// (master Contract 3). Construct with New; the zero value is not usable.
type Frontend struct {
	wrapped    webdavFrontend
	authnr     auth.Authenticator
	pathPrefix string // non-empty: stripped from request paths before delegation
}

// Compile-time assertion: *Frontend satisfies the frozen frontend seam.
var _ frontend.Frontend = (*Frontend)(nil)

// New constructs the ownCloud frontend wrapping the given webdav
// frontend. The wrapped frontend is the ONLY construction input: it
// carries the Backend and the authenticator, and the OCS surface itself
// never touches storage — the capabilities document is derived from the
// frontend's true state (contract 1), not from a second Backend handle.
// No /remote.php/webdav path rewriting (paths delegate as-is).
func New(wd *webdav.Frontend) (*Frontend, error) {
	return NewWithPathPrefix(wd, "")
}

// NewWithPathPrefix constructs the ownCloud frontend and additionally
// strips pathPrefix (e.g. "/remote.php/webdav/<bucket>") from every
// non-OCS request path before delegation. The classic ownCloud client
// speaks /remote.php/webdav/<bucket>/<path>; the wrapped webdav frontend
// has no idea that prefix exists — without the strip, a mode-B MKCOL of
// "<prefix>/dir" saw a non-empty parent prefix and 409'd (surfaced when
// e2e case 24's asserts ran for the first time; they had been passing on
// the harness's stale tally). An empty prefix delegates every path as-is.
func NewWithPathPrefix(wd *webdav.Frontend, pathPrefix string) (*Frontend, error) {
	if wd == nil {
		return nil, errors.New("owncloud: wrapped webdav frontend is required")
	}
	adapted := webdavAdapter{inner: wd}
	if adapted.Authenticator() == nil {
		return nil, errors.New("owncloud: wrapped webdav frontend has no authenticator (use webdav.WithAuthenticator)")
	}
	return &Frontend{wrapped: adapted, authnr: adapted.Authenticator(), pathPrefix: pathPrefix}, nil
}

// Name returns the frontend's registry name (config "type" value).
func (f *Frontend) Name() string { return "owncloud" }

// Handler returns the routing handler: /ocs/ paths go to the OCS router,
// everything else (including /remote.php/webdav/**) to the wrapped webdav
// data plane unchanged.
func (f *Frontend) Handler() http.Handler {
	return http.HandlerFunc(f.serveHTTP)
}

// Authenticator returns the wrapped webdav frontend's Basic adapter.
func (f *Frontend) Authenticator() auth.Authenticator { return f.authnr }

// Capabilities returns the composed caps: the webdav data plane's caps
// verbatim (Versioning stays false — no metadata provider exists in v1,
// so the capabilities document never advertises it either).
func (f *Frontend) Capabilities() frontend.ProtocolCaps {
	return f.wrapped.Capabilities()
}

// serveHTTP is the routing entry (master Contract 3 path-prefix switch).
func (f *Frontend) serveHTTP(w http.ResponseWriter, r *http.Request) {
	// /status.php: the classic-server capability ping (issue #5 acceptance
	// — the 6.x client probes it before anything else and aborts on 404).
	// The 6.x client probes <sync-root>/status.php, so any path ENDING in
	// /status.php answers. It is unauthenticated by design (the real
	// server's status.php is too).
	if strings.HasSuffix(r.URL.Path, "/status.php") {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"installed":      true,
			"maintenance":    false,
			"needsDbUpgrade": false,
			"version":        "10.11.0",
			"versionstring":  "10.11.0",
			"productname":    "zeta-object",
			"edition":        "",
		})
		return
	}
	// The 6.x client resolves OCS/status endpoints relative to the sync
	// root (<root>/ocs/v1.php/..., <root>/status.php), so match them at
	// ANY path depth, not just the server root.
	if v, ok := ocsVersionOf(r.URL.Path); ok {
		f.routeOCS(w, r, v)
		return
	}
	if idx := strings.LastIndex(r.URL.Path, "/ocs/"); idx >= 0 {
		if v, ok := ocsVersionOf(r.URL.Path[idx:]); ok {
			f.routeOCS(w, r, v)
			return
		}
	}
	if idx := strings.LastIndex(r.URL.Path, "/ocs/"); idx >= 0 {
		if v, ok := ocsVersionOf(r.URL.Path[idx:]); ok {
			f.routeOCS(w, r, v)
			return
		}
	}
	// oCIS-style data path: /remote.php/dav/files/<user>/... where <user>
	// is the authenticated identity's accessKey (the client derives the
	// dav root from its login user). The bucket-pinned frontend exposes
	// exactly one bucket, so strip the static dav/files prefix at its
	// FIRST occurrence and re-root at the bucket root; repeated
	// occurrences (the client re-appends its derived dav root to an
	// already-prefixed path) collapse into the same bucket root, matching
	// the /remote.php/webdav/ mapping.
	// Classic OC10 dav root: /remote.php/webdav/** - strip the prefix so
	// the sync root resolves to the bucket root even when its virtual
	// collection is empty (an empty-prefix 404 breaks every real client's
	// first sync).
	if strings.Contains(r.URL.Path, "/remote.php/webdav") {
		idx := strings.Index(r.URL.Path, "/remote.php/webdav")
		stripped := r.URL.Path[idx+len("/remote.php/webdav"):]
		r2 := r.Clone(r.Context())
		r2.URL.Path = "/" + strings.TrimPrefix(stripped, "/")
		r = r2
	}
	if idx := strings.Index(r.URL.Path, "/remote.php/dav/files/"); idx >= 0 {
		// Strip "/remote.php/dav/files/<user>" (the dav root is the
		// authenticated user's bucket root) - the remaining segment after
		// the prefix is the user name; drop it along with the prefix.
		rest := r.URL.Path[idx+len("/remote.php/dav/files/"):]
		if slash := strings.Index(rest, "/"); slash >= 0 {
			rest = rest[slash:] // keep "/<key...>"
		} else {
			rest = "/" // the dav root itself
		}
		r2 := r.Clone(r.Context())
		r2.URL.Path = rest
		r = r2
	}
	if f.pathPrefix != "" && strings.HasPrefix(r.URL.Path, f.pathPrefix) {
		stripped := strings.TrimPrefix(r.URL.Path, f.pathPrefix)
		if stripped == "" {
			stripped = "/"
		}
		r2 := r.Clone(r.Context())
		r2.URL.Path = stripped
		r = r2
	}
	f.wrapped.Handler().ServeHTTP(w, r)
}

// ocsVersionOf extracts the OCS protocol version from the request path.
// Only the exact prefixes /ocs/v1.php and /ocs/v2.php are OCS; both
// MUST answer identically apart from the version header and the v1/v2
// HTTP-status mapping (master Contract 2).
func ocsVersionOf(urlPath string) (int, bool) {
	switch {
	case strings.HasPrefix(urlPath, "/ocs/v1.php"):
		return 1, true
	case strings.HasPrefix(urlPath, "/ocs/v2.php"):
		return 2, true
	default:
		return 0, false
	}
}

// ocsSubpath is the endpoint path inside the version prefix:
// "/ocs/v2.php/cloud/user" → "/cloud/user" (always slash-led).
func ocsSubpath(urlPath string, version int) string {
	var prefix string
	if version == 1 {
		prefix = "/ocs/v1.php"
	} else {
		prefix = "/ocs/v2.php"
	}
	return strings.TrimPrefix(urlPath, prefix)
}

// routeOCS dispatches an OCS request by (method, subpath). Implemented
// endpoints are the adapted leaf-01 subset; everything else gets the
// documented OCS 404 envelope naming the degradation (never a silent
// empty-success).
func (f *Frontend) routeOCS(w http.ResponseWriter, r *http.Request, version int) {
	// Every OCS request is authenticated first (the webdav rule extended
	// to the negotiation surface): anonymous requests get the OCS 997
	// envelope, not the document.
	if _, ok := f.authenticateOCS(w, version, r); !ok {
		return
	}
	sub := ocsSubpath(r.URL.Path, version)
	switch sub {
	case "/config", "/cloud/capabilities":
		if r.Method != http.MethodGet {
			f.methodNotAllowed(w, version)
			return
		}
		writeOCS(w, version, http.StatusOK, ocsStatusOK, "OK", BuildCapabilities(false).ocPayload())
	case "/cloud/user":
		if r.Method != http.MethodGet {
			f.methodNotAllowed(w, version)
			return
		}
		f.handleUser(w, version, r)
	default:
		writeOCS(w, version, http.StatusNotFound, ocsStatusNotFound,
			"endpoint not implemented: this server serves the WebDAV+OCS minimal subset only (capabilities, cloud/user); see docs/owncloud-compatibility.md", nil)
	}
}

// methodNotAllowed answers a wrong-method request on a known endpoint.
// v1 answers HTTP 200 with the 405 statuscode in the envelope; v2 maps
// the statuscode to the HTTP status (mapping rule, xml.go).
func (f *Frontend) methodNotAllowed(w http.ResponseWriter, version int) {
	httpStatus := http.StatusMethodNotAllowed
	writeOCS(w, version, httpStatus, ocsStatusNotAllowed, "method not allowed", nil)
}

// authenticateOCS runs the wrapped frontend's authenticator for OCS
// requests. Missing or invalid credentials get the OCS 997 envelope
// (classic convention: no valid credentials) — HTTP 401 on v2, HTTP 200
// on v1 with the statuscode in the envelope. Authenticator-internal
// failures are a wiring bug: 500 with the generic message.
func (f *Frontend) authenticateOCS(w http.ResponseWriter, version int, r *http.Request) (auth.Identity, bool) {
	id, err := f.authnr.Authenticate(r)
	if err == nil {
		return id, true
	}
	if errors.Is(err, auth.ErrBasicMissing) || errors.Is(err, auth.ErrBasicMalformed) ||
		errors.Is(err, auth.ErrBadCredentials) || errors.Is(err, auth.ErrBasicUnsupported) {
		writeOCS(w, version, http.StatusUnauthorized, ocsStatusUnauthorised, "Unauthorised", nil)
		return auth.Identity{}, false
	}
	writeOCS(w, version, http.StatusInternalServerError, ocsStatusInternal, "Internal Server Error", nil)
	return auth.Identity{}, false
}
