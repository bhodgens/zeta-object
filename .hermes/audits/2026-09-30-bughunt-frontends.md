# Bughunt: auth and protocol frontends (53e1fb0..HEAD) — 2026-09-30

Report only. No fixes applied.

Wave anchor: git rev 53e1fb0. The prior bughunt closed there
(.hermes/audits/bughunt-postF2-2026-09-29.md). This wave covers the
unaudited tree after that rev: pluggable auth, WebDAV, FTP, SFTP,
ownCloud, and the S3 grant wiring. HEAD at report time: 2314aa3.

7 read-only auditors. Parent read every HIGH site in the current tree
before it entered this file. One auditor HIGH was regraded (W3).

## Baseline

`go test -p 2 -count=1 ./...` exit 0. All 13 packages reported ok
(root, auth, backend, conformance, fsbackend, frontend, ftp, owncloud,
s3, sftp, webdav, metadata, objectmodel).

Not run this wave: `go test -race`, lint, govulncheck (the fix wave ran
them; see the disposition note at the end of this file). Case
scripts/e2e/cases/19-webdav.sh starts the real binary with two webdav
entries. At audit time current startup could not do that (C1 and C2
below); the fix wave repaired both and the full suite re-ran green.

## HIGH — parent-verified

Live on a configured FTP or SFTP listener, or on the S3 listener:

- A1. A per-bucket readonly grant does not override wildcard readwrite.
  internal/auth/grants.go:22. CanWrite returns true when the specific
  grant has Write:false and "*" has Write:true. The fallthrough is the
  same in CanRead (grants.go:11). registry.go:92 says per-bucket beats
  "*". README.md:105 shows the example grants {"*":"readwrite",
  "photos":"readonly"}. That identity can write "photos". parseGrants
  never emits Read:false, so the shipped vocabulary fails open on write
  only.

- S1. CopyObject reads the source bucket with no grant check.
  internal/frontend/s3/object_handlers.go:1337. authorizeS3Request
  (dispatch.go:88) checks only the URL-path bucket. putObjectHandler
  (object_handlers.go:44) sends a PUT with x-amz-copy-source to
  copyObjectHandler. That handler Get()s srcBucket. An identity with
  write on one bucket and no grant on another can copy the other
  object's bytes into the writable bucket. The source name is not
  passed through validBucket.

- F1. A failed STOR still replaces the object.
  internal/frontend/ftp/driver.go:524. writeFile.Close always Put()s
  the buffer. writeFile does not implement ftpserver.FileTransferError.
  ftpserverlib v0.32.4 handle_files.go:108 calls closeUnchecked when
  TransferOpen fails, and handle_files.go:115 calls Close after
  doFileTransfer even when that call returned an error. STOR without
  PASV, a dropped data connection, or ABOR replaces a good object with
  an empty or truncated body. The client is told the transfer failed.

- T1. SFTP rename deletes the object when source and destination are
  the same key. internal/frontend/sftp/driver.go:224. rename Put()s
  then Delete()s. There is no same-key guard and no destination
  existence check. path.Clean can map two paths onto one key. Put
  rewrites that key. Delete then removes it. A distinct existing
  destination is also overwritten, then the source is deleted. No
  versioning. Both cases lose bytes.

- T2. SFTP upload Close always commits the buffer.
  internal/frontend/sftp/driver.go:114. There is no abort flag. Close
  Put()s whatever is in the buffer, including an empty buffer. A
  dropped overwrite, or a close after WriteAt rejects a non-tail
  offset, replaces the previous object. The auditor also said
  pkg/sftp RequestServer closes every open handle on disconnect. That
  library path was not re-read this wave. The handler defect does not
  depend on it: any Close commits.

Not live on the production binary today. The handler bug is real.
startupPlan passes a nil backend (C1), so webdav.New rejects the
frontend and main fatals before listen. Do not fix C1 alone. A wiring
fix that passes the dataDir FS backend makes these live.

- W1. Mode A accepts "." and ".." as the bucket segment.
  internal/frontend/webdav/paths.go:59. parseResource does not reject
  those segments. dispatch.go:36 feeds r.URL.Path into that mapper.
  COPY/MOVE Destination is parsed with url.Parse (copymove.go:142) and
  the same mapper. The mux never sees that header. fsbackend
  bucketPath is filepath.Join(root, bucket) with no bucket-name check
  (object.go:31). Join(root, "..") is the parent of dataDir. Put writes
  there. Delete os.Removes that path. A "*" write grant matches the
  name "..". validateKey rejects ".." in the key, so this is one
  directory up, not an open walk. Mode B is not this bug: the same
  segment becomes a key, and validateKey rejects it. S3 validBucket
  (bucket_handlers.go:362) rejects names shorter than 3 characters.
  WebDAV does not call it.

- W2. WebDAV PUT ignores If-Match and If-None-Match.
  internal/frontend/webdav/put.go:45. The handler forwards the headers
  in PutOptions and does not compare them. fsbackend has no IfMatch or
  IfNoneMatch read. A client that sent If-Match or If-None-Match: *
  gets 204 and loses the previous bytes. copymove.go:105 states that
  the frontend must enforce conditionals because the backend ignores
  them.

- O1. ownCloud omits bigfilechunking. Clients that treat a missing
  flag as enabled upload chunks as normal objects.
  internal/frontend/owncloud/capabilities.go:42 and :90. The files
  block is empty. The in-file comment says omit the flag, do not emit
  false. ownCloud client issue 7862 records that removing the flag
  still caused 10MB chunking, and that the off switch is
  bigfilechunking=false. This wave did not read the client source line
  the auditor named (Capabilities::bigfilechunkingEnabled default
  true). The local-delete step in that auditor report was not traced
  here. The server side is confirmed: a chunk PUT is a normal WebDAV
  PUT and returns an ETag (webdav/put.go:51). ownCloud does not start
  on the production binary today (same C1 nil backend).

## Regrade

- W3. Auditor said HIGH. Parent grade: MED.
  internal/frontend/webdav/copymove.go:70 returns 412 before Put when
  Overwrite:F and the destination already exists. The check at line
  109 uses the same dstExists flag captured at line 61. It cannot see
  a destination created after that stat, and it is unreachable when
  that flag was already true. The race is real. The common path is
  not. IfNoneMatch is passed to Put and the fs backend ignores it, so
  the race has no second gate.

## MEDIUM

Parent-read:

- A2. Public-key canonical form is the raw base64 field, not the
  decoded blob. publickey.go:105. Padding variants do not match.
  Fail-closed (lockout), not a remote bypass. The test named "padded
  blob same canonical as raw" uses a fixture whose padded and raw
  forms are identical.
- A4. Basic secret compare re-hashes the raw secret on every check.
  registry.go:61. storedIdentity.secretHash is never read. SHA-256
  time tracks input length. The unknown-user path hashes a fixed
  40-byte dummy. Known-user vs unknown-user timing differs when the
  real secret is not 40 bytes. Content is not byte-leaked by the
  compare. Both misses return the same sentinel.
- C1. webdav and owncloud are built with main's nil backend.
  main.go:124 passes nil into startupPlan. frontends.go:50 passes that
  value to webdav.New. webdav.New rejects a nil backend (frontend.go:72).
  A config that adds type webdav or owncloud fatals before listen.
  FTP and SFTP call backendResolver() and can still start. Unit tests
  pass a non-nil stub, so they do not catch this.
- C2. The loader rejects a second entry of the same type.
  frontends.go:240. README and scripts/e2e/cases/19-webdav.sh show two
  webdav entries (mode A and mode B). That config cannot start.
- T3. A subsystem request with a payload shorter than 4 bytes panics.
  internal/frontend/sftp/sshserver.go:150 slices req.Payload[4:] with
  no length check. Nothing recovers the panic, so the process dies.
  The client must already be authenticated. This wave did not prove
  that x/crypto/ssh delivers a short payload to that callback.

Auditor-reported, not re-run by the parent. Quoted by the auditor.
Treat as leads, not as closed proof:

- A3. authorized_keys parse binds the first keytype field and ignores
  quotes. publickey.go:81. A quoted option that contains a keytype and
  a base64 token can bind the wrong blob at config load. Not a remote
  bypass on the SFTP path.
- W4. PROPFIND Depth 1 lists with MaxKeys unset. propfind.go:255.
  Depth infinity is rejected. One large Depth:1 response can hold the
  whole level.
- F2. FTP directory RNFR/RNTO does not rename the dir/ marker.
  driver.go:238. path.Clean strips the trailing slash, so the marker
  branch never runs.
- T4. A nil KeyChecker panics in PublicKeyCallback. sshserver.go:28.
  The production factory always sets a checker. New allows nil when
  password auth is on. The documented nil shape crashes.
- T5. Grant round-trip splits on comma and equals. sshserver.go:114.
  A grant key that contains those characters can parse as a wider
  grant. A remote client cannot inject grant keys.
- T6. Host key generate is not exclusive and follows a symlink.
  hostkey.go:52. Two first connections can each generate a key.
  WriteFile follows a dangling symlink at hostKeyFile.
- T7. SFTP upload and download buffer the whole object with no cap.
  driver.go:111. The backend size cap runs only after Close.

## Verified clean (highlights)

- Basic password compare uses SHA-256 digests and
  crypto/subtle.ConstantTimeCompare. Unknown user and wrong password
  share one sentinel. A wrong password is not accepted.
- Dev mode is opt-in (auth.mode "none"). It is not the default. Empty
  mode does not install DevAuthenticator. WebDAV, FTP, and SFTP do not
  install it.
- FTP active mode is off (DisableActiveMode: true). PORT bounce is not
  offered by this settings struct.
- FTP and SFTP path split uses path.Clean before the bucket cut. ".."
  cannot leave the virtual root on those two frontends. Object I/O
  goes through Backend, not os.Open.
- S3 path-bucket grant check runs before dispatch (dispatch.go:88).
  Presigned auth does not skip that check. ListBuckets filters to
  granted buckets. Capability endpoints (?events, ?versions, ?uploads)
  sit behind the same path-bucket gate. CopyObject is the hole (S1).
- ownCloud OCS routes call authenticateOCS before dispatch. Bad
  credentials get OCS 997. /cloud/user returns only the authenticated
  access key.

## Disclosure ledger

- No fixes. This request was a bughunt, not "fix".
- e2e, race, lint, and govulncheck were not run. The unit baseline is
  green. A green unit suite does not prove case 19 or case 23 starts.
- Metadata reconstruction (zfs_events.go) was not re-audited. The prior
  wave closed it.
- O1 client source was not opened. The client-default claim rests on
  ownCloud client issue 7862, not on a line read in this session.
- T2's "library closes handles on disconnect" claim was not re-read.
- Untracked binaries zeta-object and zeta-object-server, .claude/, and
  testdata/fuzz/ were ignored.
- Fix order if a later pass lands fixes: A1 and S1 are live and
  independent. F1, T1, and T2 are live when those listeners are
  configured. Do not repair C1 (pass a real backend into webdav.New)
  without W1 in the same change. Enabling the listener alone makes the
  dot-bucket write live.

## Next action

Say "fix all" to land fixes. Do not start with C1 alone.

---

## Fix-wave disposition (2026-09-30, same session)

All 8 HIGH findings + the MED items below were fixed by 7 parallel
fixers with disjoint file ownership, then parent-verified and gated:

- A1 fixed (grants.go: present entry authoritative) — pin
  TestPerBucketGrantBeatsWildcard.
- S1 fixed (identity in request context; copyObjectHandler checks
  CanRead(srcBucket) + validBucket) — pins
  TestCopyObjectSourceGrantEnforcement/* + e2e 18g/18h.
- F1 fixed (writeFile implements ftpserver.FileTransferError; aborted
  Close never Puts) — pins + e2e 20e (ABOR-based, deterministic).
- T1 fixed (same-key rename no-op; existing destination refused) —
  pins + e2e 21d.
- T2 partially fixed (zero-write close commits nothing). Residual:
  a dropped connection after >=1 successful write still commits the
  prefix — pkg/sftp has no transfer-error hook. Documented in the
  putWriter comment.
- W1 fixed (parseResource rejects dot-segment buckets and keys; 403) —
  pins + e2e 19 (PUT/DELETE/PROPFIND /%2e%2e → 403, no file above
  dataDir).
- W2 fixed (If-Match/If-None-Match enforced at the frontend for PUT
  and COPY/MOVE destinations) — pins + e2e 19 conditional chain.
- W3: fixed as part of W2's frontend enforcement (the common path now
  412s before Put; the stat-then-Put race remains documented).
- O1 fixed (capabilities emit <bigfilechunking>false</bigfilechunking>;
  golden tests + docs + e2e 23a updated).
- A2 fixed (canonical form re-encodes the decoded blob) — the old
  padding test was a false pin (15-byte fixture); re-fixtured.
- A4 fixed (compare against the precomputed digest; unknown-user path
  hashes the presented password against a fixed dummy).
- T3/T4/T5/T6 fixed (short-payload guard, nil KeyChecker not installed,
  grant keys with ,/= rejected at config load, host key O_EXCL create).
- C1 fixed (main passes backendFor("") into startupPlan).
- C2 fixed (duplicate check keys on (type, listenAddr, bucket); the
  README/case-19 two-webdav shape now starts).

Gate matrix at close: build ok, vet ok, golangci-lint 0 issues, full
unit suite -count=1 ok (13 packages), TWO full -race passes clean (13
ok, 0 DATA RACE each), govulncheck 0 reachable vulns, gitleaks clean,
e2e green (first full run 415/0 with case 21 masked by a stale tally;
case 21 was then repaired standalone 18/0 and the suite re-run).

Fixer-side case repairs (pre-existing breaks, fixed inside the owned
case files): port collisions between the default HTTPS listener and the
dedicated ftp/sftp listener in cases 19/20/21 (the new C2 semantics
made the pre-existing single-port configs fatal at startup — the
non-HTTP listener binds first); awscli rejects `--body /dev/stdin` in
cases 20/21; case 21 never created its bucket (S3-side seed now follows
a create-bucket); case 19's stale DELETE-dir expectation corrected to
404 per Contract 3; case 19 header asserts made case-insensitive.

Deliberately not fixed (documented residuals): A3 (authorized_keys
quoted-option parsing — config-load hardening, not wire-reachable),
W4 (PROPFIND Depth:1 unbounded page), F2 (FTP directory RNFR/RNTO
marker rename), T5's SFTP-side serialization (config load now rejects
the dangerous grant keys; the serializer is untouched), T6's true
two-process host-key race (loser re-reads the winner's file), T7
(SFTP whole-object buffering cap), T2's partial-write residual above.
