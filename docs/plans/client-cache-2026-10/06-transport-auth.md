# Leaf 06 - transport + auth (TCP Basic, h3 mTLS, Alt-Svc)

## Goal

`internal/transport`: the real webdav HTTP client behind the leaf-02
interface - Basic auth over TCP/HTTPS, alt-Svc-driven HTTP/3 upgrade
with per-device mTLS, and the wire-level operations (PROPFIND, GET with
Range, PUT with Content-Length + If-Match, MKCOL, DELETE, MOVE, LOCK/
UNLOCK, JSON ?batch delete).

## Requirements

- TCP path: https to the configured server URL, Basic auth
  (accessKey:secretKey), TLS with a configurable CA file (self-signed
  gateway: point at its cert). Never disable verification except an
  explicit `insecureSkipVerify: true` config key that logs a WARNING
  line at startup.
- h3 path: read `alt-svc: h3="<port>"; persist=1` from any response
  (the gateway stamps it on every response); when present AND the
  config supplies a client cert, dial UDP/QUIC (quic-go, pin the exact
  version the gateway uses - v0.63.0 lineage; coordinate the version
  with the root go.mod lineage) to the same host with the client cert
  (CN = device name; config: clientCert/clientKey, or a keychain
  alias). mTLS is the ONLY auth over h3: there is no HTTP 401 on that
  transport - a missing/wrong cert fails the TLS handshake. The client
  treats handshake failure with IsCryptoError as "auth failed" and
  surfaces it distinctly.
- Fallback: h3 failures (UDP blocked, handshake refused) degrade to TCP
  automatically for the NEXT request (per-request transport choice with
  a sticky-fallback timer; never a half-request retry that could
  double-apply - PUT is only retried when the first attempt failed
  BEFORE the request was sent, i.e. dial/handshake errors only).
- Operations (match leaf-02's interface + leaf-04 needs):
  - Propfind(ctx, path, depth) -> parsed rows (href, etag, size,
    mtime, fileid, collection bool). Handle the gateway's 207 form:
    prefixed elements with xmlns on root (the ownCloud-compat form
    pinned by e2e 19/25). XML parsing must accept both prefixed and
    default-namespace forms for robustness against other webdav
    servers, but only the gateway's form is asserted.
  - Get(ctx, path, range) streaming to a writer; honor If-None-Match
    revalidation (304 = cache still valid).
  - Put(ctx, path, reader, size, ifMatch, ifNoneMatch) -> response
    ETag. Content-Length ALWAYS set (server rejects chunked with 400).
  - Mkcol, Delete, Move(dest, overwrite, ifMatch-on-dest), Lock/Unlock
    (timeout, token, If header) for multi-machine coherence,
    BatchDelete(paths) via POST ?batch JSON manifest (flat, per-item
    results; no server-side expansion).
- Auth model v1 (locked): EITHER Basic (access+secret from config or
  keyring) OR client cert. Cert storage: macOS Keychain / Linux
  keyring via an interface; config may point at PEM files for
  headless setups (document the tradeoff).
- All operations respect ctx cancellation; no retry loops inside the
  transport (leaf 07's scheduler owns retry policy) EXCEPT the
  idempotent-read single retry on connection reset.

## Constraints

- No sync logic, no caching decisions. Transport is stateless except
  the alt-Svc stickiness and the auth material.
- HTTP/3 client library: quic-go pinned EXACT (API churns); record the
  pin in zeta-cache/go.mod and the report.
- The gateway's h3 frontend requires a clientCA-issued cert; document
  in zeta-cache/README.md how an operator issues a per-device cert
  (openssl one-liner against the gateway's clientCA).

## Acceptance

1. Unit tests with httptest: PROPFIND parse (gateway 207 fixture bytes
   copied from the e2e case 19/25 shapes), PUT Content-Length
   assertion, If-Match header assertion, alt-Svc parse, batch request
   body shape.
2. h3 path: reuse the h3probe pattern - a live-server test (build tag)
   against the gateway with client certs; negative path asserts
   handshake failure (IsCryptoError shape), NOT an HTTP status.
3. e2e case 40: switch the sync smoke to the REAL transport (TCP Basic
   at minimum; h3 section gated on client-cert fixtures like case 35's
   CA generation).
4. `go test -race ./internal/transport/` green; CGO_ENABLED=0 build
   still green (quic-go is pure Go).
5. Report: the alt-Svc sticky-fallback state machine and the retry
   safety table (which ops retry on which error classes - the no-
   double-apply rule).
