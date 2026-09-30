# 03 - FTP/FTPS Frontend - Implementation Leaf

> **For the implementing agent:** You are the implementer for this leaf.
> Implement ALL tasks below using TDD. Do NOT commit — the orchestrator
> handles all git operations after review.

## Meta

- **Parent:** [master.md](master.md)
- **Scope:** `internal/frontend/ftp` — FTP and FTPS (explicit TLS, AUTH TLS)
  on `github.com/fclairamb/ftpserverlib`, driver mapping commands to
  `internal/backend.Backend` per master Contract C.
- **Dependencies:** 01-nonhttp-listener-adapter.md (NonHTTPFrontend, options),
  02-dependency-license-audit.md (MIT gate PASSED for ftpserverlib).
- **Estimated Context:** 90K
- **Concurrency Group:** B (parallel with leaf 04 after 01+02 land)

## Goal

After this leaf, config entry `{"type":"ftp","listenAddr":":2121","options":{...}}`
starts a working FTP server on that port. Plain FTP and explicit FTPS both
work; objects round-trip through `Backend`. Grant enforcement details are
leaf 05's — this leaf wires a local `PasswordVerifier` interface so the auth
adapter can be swapped in without protocol-code churn.

## Config surface (owner of these option keys)

```json
{ "type": "ftp", "listenAddr": ":2121",
  "options": {
    "passivePortMin": "50000",   // default: kernel-assigned (0)
    "passivePortMax": "50100",
    "publicIP": "127.0.0.1"      // optional, PASV reply address; default: listener IP
  } }
```

FTPS reuses the top-level `certFile`/`keyFile` (no new keys): the factory
signature already receives the server's TLS material via construction; pass
it as `tls.Config{MinVersion: TLS1.2}` into ftpserverlib (AUTH TLS explicit —
control channel upgrades on demand, data channel PROT P).

Unknown `options` keys: factory returns a loud error (fail startup), same
contract as unknown frontend/backend types.

## Exact files

- New: `internal/frontend/ftp/frontend.go` — `ftp.New(b backend.Backend, tlsCfg *tls.Config, opts ...)`,
  implements `frontend.Frontend` + `frontend.NonHTTPFrontend`; `Handler()`
  returns a stub that always writes `501 Not Implemented` (never mounted;
  documents the seam's http-shaped legacy).
- New: `internal/frontend/ftp/driver.go` — ftpserverlib `MainDriver` +
  `ClientDriver` mapping commands to Backend (Contract C).
- New: `internal/frontend/ftp/auth.go` — local `PasswordVerifier`
  interface: `Verify(user, password string) (Identity, bool)` + a
  `StaticVerifier` over the existing env credential pair (temporary until
  leaf 05's adapter).
- New: `internal/frontend/ftp/errors.go` — objectmodel.Error → FTP reply
  code/suffix mapping (5xx permanent, 4xx transient for ctx cancel/timeouts).
- New: `internal/frontend/ftp/factory.go` — option validation; registered in
  `frontends.go`'s `frontendFactories` as `"ftp"` (one-line edit + known-types
  test update).
- Tests: `internal/frontend/ftp/{frontend_test.go,driver_test.go,conformance_test.go,ftps_test.go}`.
- Modify: `frontends.go` — factory entry `"ftp"`.
- Modify: `frontends_test.go` — known-types list includes `ftp`.

## Driver mapping details (binding)

- ftpserverlib's ClientDriver is a filesystem-like interface; DO NOT fake a
  filesystem — implement it directly over Backend calls per Contract C.
- Path model: `/bucket/path/to/key`. CWD/CDUP are driver-local string
  resolution; only storage-touching ops hit Backend.
- Listing: `LIST`/`NLST`/`MLSD` → `List` with `Delimiter:"/"`;
  `CommonPrefixes` render as directories. `MLSD` facts: size/mtype/modify
  from `objectmodel.Object`.
- STOR: single-shot `Put` (no multipart — FTP has none anyway; the
  degradation contract is about never emulating S3 multipart). On
  `ctx` cancel mid-transfer: 426 response.
- RETR: `Get`; FTP REST (restart) is REJECTED (`502`/`551`-style reply) —
  no Range mapping per the caps (`ConditionalReads:false`).
- DELE → `Delete`; RMD → Delete on the `"dir/"` marker key, missing marker →
  550. MKD → zero-byte `"dir/"` marker Put; MDTM/SIZE → `Stat`.
- Login: USER/PASS through `PasswordVerifier`; on failure ftpserverlib's
  530. Anonymous disabled. TLS-required mode: if `options` later grows a
  `requireTLS` key it belongs here — NOT in this leaf (record as follow-up).

## Test-first steps

1. Failing test: `TestConformance` —
   `frontend.RunConformanceSuite(t, f, frontend.ConformanceOptions{})` with a
   temp-dir fs backend; proves Frontend shape before any wire test.
2. Failing test: `TestFTPRoundTrip` — start `ftp.New` on `127.0.0.1:0` with
   a fake Backend (record calls); connect with `github.com/jlaffaye/ftp`
   (test-only dep, MIT-gated in leaf 02); login; STOR a payload; RETR and
   compare bytes; DELE; assert fake Backend saw
   `Put(bucket,key)`/`Get`/`Delete` with exact keys. Also assert the
   size parameter passed to Put matches the payload (streaming contract).
3. Failing test: `TestListingMapsToListDelimiter` — fake Backend captures
   ListParams: LIST `/bkt/` issues `Prefix:"bkt/", Delimiter:"/"`;
   common prefixes render as `dir NLIST` entries.
4. Failing test: `TestAuthRejectsBadPassword` — 530, no Backend call.
5. Failing test: `TestFTPSExplicitTLS` — same server with tls.Config from
   generated test cert; jlaffaye client with `FTPS` explicit mode +
   `InsecureSkipVerify` (test cert); round-trip succeeds; plaintext on the
   same port still works (explicit, not implicit).
6. Failing test: `TestPasvPortRange` — options min/max honored (connect via
   loopback, assert data-channel port within range).
7. Failing test (factory): unknown option key → construction error naming
   the key; `{"type":"ftp"}` without listenAddr → construction error
   (shared-mux rejection from leaf 01).
8. Implement until green; then `make test`, `make lint`.

## Done-when

- All tests green including `frontend.RunConformanceSuite` for the ftp frontend.
- Registered as `"ftp"`; `make build` binary starts with an ftp-only config
  (manually or via the leaf 06 e2e case pattern in a scratch run).
- No FTP code calls anything below `backend.Backend` (grep: no `internal/backend/fsbackend`
  or `os.` file IO outside test fakes).
