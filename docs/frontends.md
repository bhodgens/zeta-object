# Protocol Frontends

zeta-object speaks multiple wire protocols through a single pluggable seam. A
**frontend** decodes a wire protocol into the neutral object model
(`internal/backend.Backend`) and encodes protocol-appropriate responses.
Today `s3`, `webdav`, `ftp`, `sftp`, and `owncloud` are registered.

## The `frontend.Frontend` contract

```go
type Frontend interface {
    Name() string                      // "s3", "webdav", "ftp", ...
    Handler() http.Handler             // mounts itself under the server mux
    Authenticator() auth.Authenticator // per-frontend auth adapter (multi-identity registry, GH #4 - shipped)
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
  { "type": "webdav", "listenAddr": ":8444" },
  { "type": "ftp", "listenAddr": ":2121" },
  { "type": "sftp", "listenAddr": ":2022", "options": { "hostKeyFile": "certs/host_ed25519" } },
  { "type": "owncloud", "listenAddr": ":8446" }
]
```

- **Absent/empty array** = `[{ "type": "s3" }]` on the default listener —
  exact backward compatibility with configs that predate the seam.
- **No `listenAddr`** → the frontend's `Handler()` is mounted on the default
  listener's mux (the `listenAddr` from the top-level config, overridable by
  `ZETAOBJECT_LISTEN_ADDR`).
- **With `listenAddr`** → the frontend gets its own dedicated TLS listener on
  that address, using the same shared `certFile`/`keyFile` pair.
- `ZETAOBJECT_LISTEN_ADDR` overrides **only the default listener**; per-frontend
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

- All four additional frontends are SHIPPED: WebDAV (#1), FTP/FTPS + SFTP
  (#2), ownCloud (#3) - each with its own e2e case under
  `scripts/e2e/cases/` and a README section documenting its mapping and
  degradation contract.
- Pluggable authentication (#4) is SHIPPED: multi-identity registry with
  per-bucket grants, HTTP Basic (WebDAV/ownCloud), FTP login, SFTP
  password + public-key auth, and the opt-in zero-auth dev mode. See the
  `identities` / `auth` blocks in `config.json.example` and the README's
  credentials section.
