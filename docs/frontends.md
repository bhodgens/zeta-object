# Protocol Frontends

mini-s3 speaks multiple wire protocols through a single pluggable seam. A
**frontend** decodes a wire protocol into the neutral object model
(`internal/backend.Backend`) and encodes protocol-appropriate responses.
Today only `s3` is registered; WebDAV, (S)FTP, and ownCloud frontends are
planned (see their GH issues).

## The `frontend.Frontend` contract

```go
type Frontend interface {
    Name() string                      // "s3", "webdav", "ftp", ...
    Handler() http.Handler             // mounts itself under the server mux
    Authenticator() auth.Authenticator // per-frontend auth adapter (placeholder model; auth GH issue will replace it)
    Capabilities() ProtocolCaps        // what this protocol can express
}
```

### Capability rule

A frontend that cannot express an operation returns a **protocol-appropriate
error at the seam** — it never silently emulates (e.g. WebDAV answers a
multipart request with a WebDAV error, not by faking S3 multipart
semantics).

## Configuring frontends

The optional `frontends` array in `config.json` selects what runs:

```jsonc
"frontends": [
  { "type": "s3" },
  { "type": "webdav", "listenAddr": ":8444" }
]
```

- **Absent/empty array** = `[{ "type": "s3" }]` on the default listener —
  exact backward compatibility with configs that predate the seam.
- **No `listenAddr`** → the frontend's `Handler()` is mounted on the default
  listener's mux (the `listenAddr` from the top-level config, overridable by
  `MINIS3_LISTEN_ADDR`).
- **With `listenAddr`** → the frontend gets its own dedicated TLS listener on
  that address, using the same shared `certFile`/`keyFile` pair.
- `MINIS3_LISTEN_ADDR` overrides **only the default listener**; per-frontend
  `listenAddr` values are never touched by it.
- **Unknown or duplicate `type` values abort startup loudly** with the list
  of known types — there is no silent fallback.

### Multi-listener decision

Any number of dedicated listeners is supported (a plain loop starts one
`http.Server` per entry with its own `listenAddr`); in practice one extra is
expected in the near term — a second port for a second frontend. Graceful
shutdown drains the default listener first, then every dedicated listener,
all within the same 30-second `serverShutdownTimeout` window.

## Adding a new frontend

1. Implement `frontend.Frontend` (decode to `backend.Backend`, encode
   protocol responses, honor the capability rule above).
2. Run `frontend.RunConformanceSuite` in your package's tests.
3. Add exactly one factory entry to `frontendFactories` in `frontends.go` —
   nothing else in `main` changes.
4. Document any new config keys in `config.json.example`.

## Related work

- WebDAV, (S)FTP, ownCloud frontends: tracked in their individual GH issues.
- The `auth.Authenticator`/`auth.Identity` shape is a placeholder; the open
  "auth: pluggable authentication architecture" GH issue replaces it with a
  real identity model (per-frontend/per-user credentials).
