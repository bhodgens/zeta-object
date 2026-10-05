package adminserver

import (
	"embed"
	"io/fs"
	"mime"
	"net/http"
	"path"
	"strings"
)

// static.go — the embedded UI assets and the unauthenticated static surface.
//
// The UI ships via go:embed so the console has no runtime asset path and makes
// no external request. The unauthenticated surface is exactly this asset set
// plus the sign-in page (Contract 2).

//go:embed web
var webFS embed.FS

// webRoot is the directory inside the embed that holds the UI. The embedded
// paths are therefore web/index.html, web/login.html, web/assets/<file>.
const webRoot = "web"

// staticHandler serves the console's static assets from an fs.FS. The
// production instance wraps the embedded web/ tree; tests inject a MapFS whose
// keys carry the same web/ prefix.
type staticHandler struct {
	fsys fs.FS
}

// newStaticHandler returns a handler over fsys.
func newStaticHandler(fsys fs.FS) *staticHandler {
	return &staticHandler{fsys: fsys}
}

// ServeHTTP serves GET / (the shell), GET /assets/{file}, and 404s anything
// else. It is mounted at "/" so an unknown path never falls through.
func (h *staticHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch {
	case r.URL.Path == "/":
		h.serveFile(w, r, "index.html")
	case strings.HasPrefix(r.URL.Path, "/assets/"):
		name := strings.TrimPrefix(r.URL.Path, "/assets/")
		if name == "" || strings.Contains(name, "..") {
			http.NotFound(w, r)
			return
		}
		h.serveFile(w, r, path.Join("assets", name))
	default:
		http.NotFound(w, r)
	}
}

// serveLogin serves the sign-in page. It needs no session.
func (h *staticHandler) serveLogin(w http.ResponseWriter, r *http.Request) {
	h.serveFile(w, r, "login.html")
}

// serveFile writes an embedded file with a content type from its extension.
func (h *staticHandler) serveFile(w http.ResponseWriter, r *http.Request, name string) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		writeError(w, http.StatusMethodNotAllowed, "method_not_allowed", "method not allowed")
		return
	}
	data, err := fs.ReadFile(h.fsys, path.Join(webRoot, name))
	if err != nil {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", contentTypeFor(name))
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(data) // #nosec G705 -- data is the embedded asset set (go:embed), never user input
}

// contentTypeFor maps a file name to a Content-Type, defaulting to
// application/octet-stream for unknown extensions.
func contentTypeFor(name string) string {
	if ct := mime.TypeByExtension(path.Ext(name)); ct != "" {
		return ct
	}
	return "application/octet-stream"
}
