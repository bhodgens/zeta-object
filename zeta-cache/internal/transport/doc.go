// Package transport owns the HTTP client: Basic auth over TCP, alt-Svc
// discovery and HTTP/3 upgrade with per-device mTLS, Range GET hydration,
// If-Match PUT, and webdav locking usage. Owned by leaf 06
// (transport-auth); leaf 01 uses a plain http.Client for the startup
// OPTIONS probe only.
package transport
