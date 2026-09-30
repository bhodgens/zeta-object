# Bughunt Round 2: post-F2 arc (3b0a7cf..53e1fb0) — 2026-09-29

5 read-only auditors, disjoint scopes. Baseline at audit time: build/vet/full
suite/race/e2e 213/213/coverage 86% all green (live tree includes the
in-flight zeta-object rename by a parallel session). All HIGH findings
parent-verified (probe or mechanical trace). Prior waves (7f7ce35..3b0a7cf)
already audited + fixed; this wave covers everything after.

## HIGH (6)

- H1 (E1): Put(bucket, ".") bricks an unmaterialized bucket. The 9094993
  '.' exemption + Join(bucket,".")==bucketPath (mechanically verified) makes
  a fresh-bucket Put write the data file AT the bucket directory path;
  sidecar mkdir fails, residual rule keeps the file, bucket disappears from
  ListBuckets and all subsequent ops fail. Pre-9094993 this was
  InvalidArgument. Fix: reject '.' again (it aliases the bucket dir).
  internal/backend/fsbackend/paths.go:141.
- H2 (E6/C2): CI coverage floors silently dead since the rename.
  check.yml case labels still 'mini-s3/...'; go list now yields
  'github.com/bhodgens/zeta-object/...' -> no arm matches -> FLOOR unset ->
  awk floor 0 -> floors can never fail. The rename must carry the label
  update (AGENTS.md rule: floors move with code in the same change).
- H3 (C1): MINIS3_LISTEN_ADDR renamed with NO legacy fallback
  (frontends.go:134 reads ZETAOBJECT_LISTEN_ADDR raw). Operators with the
  old var in unit files silently lose the override (port conflict / wrong
  interface). Credentials + CONFIG got fallbacks; LISTEN_ADDR and
  DEBUG_AUTH did not.
- H4 (A1): root-voting mis-election in zfs_events.go detectRoot. Raw
  frequency voting: a LOST mid-chain directory referenced by many children
  out-votes the true root; resolvePath then returns fabricated EXACT keys
  that drop path components (mechanically traced). Worse than the
  documented partial fallback because it never matches the true key.
- H5 (A2): any in-window record for the root dir itself breaks resolution
  two ways: parent=0 -> root lands in byID, zero votes, detection refused,
  all events degrade to partial; nonzero parent (SETATTR on dataset dir) ->
  elects the pool root, every resolved key gets a phantom prefix and
  queries for true keys return 0 events (F-live-1 resurrected). Fixtures
  omit exactly this record; needs one live-host capture to size frequency.
- H6 (A3): single objid->name mapping rewrites history. Objid reuse
  (delete + recreate) or an in-window directory RENAME makes pre-event
  records resolve to WRONG exact keys under the new name, and queries for
  the true old path return 0 events. Needs per-record historical mappings
  (txg-scoped), not newest-wins-for-all-time.

## MEDIUM (9)

- M1 (D1): production wiring re-opens the A2/C2 detail race through the
  singleton provider: s3_wiring installs metadata.Lookup("zfs-events")
  (one shared instance) for every bucket; concurrent ?events on different
  buckets can report the other bucket's dataset/recordsLost. Tests miss it
  (stubbed resolver).
- M2 (D2): boot-relative hrtime serialized as wall-clock RFC3339 when the
  time field is present - fabricated timestamps + silently broken Since
  filter for boot-relative platforms.
- M3 (D3): bucket-name traversal reaches ?events: dispatch has no
  validBucket on GET paths; /..%2f..%2fdir?events passes bucketExists via
  Join and runs Probe + zfs get against directories outside dataDir,
  disclosing dataset name + key history (SigV4-gated). Fix: validBucket in
  resolveEventsContext.
- M4 (E5): NextKeyMarker encode fix half-done - response encodes but
  incoming key-marker is never decoded -> resumption compares encoded vs
  raw, can skip/repeat keys for encodable characters.
- M5 (A5/E2): conservative suffix match cross-directory bleed + returned
  Key not the queried key + no partial flag/objid on the wire for clients
  to detect it. Also hits mixed healthy/lost windows, not just fully
  degraded ones (E2 probe).
- M6 (E3): cross-directory RENAME with one lost endpoint reconstructs the
  move wrong (old side bare, new side exact) and the half-event
  suffix-matches other directories.
- M7 (B1): TestRecordActivityResetOnFilter is vacuous - the ResetOn filter
  it names cannot make it fail (mutation-probed). Real behavior unpinned.
- M8 (A4): fixtures are oldest-first while first-seen-wins assumes
  newest-first - internally inconsistent; needs one real id-reuse capture
  to determine which is true.
- M9 (C3): config.json.example documents only the dead MINIS3_ names.

## LOW / INFO (12)

A6 depth-cap silent degradation; A7 hrtime wall-clock (pre-existing, =
M2's root); A8 window-orphaned dirs (inherent, doc); B2 runCommand smoke
tests log-blind; B3 double-register unassertable as written; B4 4 of 18
detectors never probe-proven; B5 detection false-positive channel; B6 dead
import silencers; C4 set-but-empty legacy warning names the wrong var;
C5 getEnvOrDefaultLegacy string surgery footgun; C6 validation-doc name
stale (rename scope); D4 ?events&versions ignores prefix (key leak within
bucket); D5 unbounded concurrent zfs execs (authenticated DoS bound);
D6 internal dataset naming on wire. (Counted: 13 - D4/D5/D6 included.)

## Verified clean (highlights)

Exec injection surface (argv-only, no shell, fail-closed); XML escaping in
ext wire (probe-verified); SigV4-before-dispatch ordering intact;
mountpoint containment incl. prefix-boundary trap; purge not
network-reachable; uid/gid still absent; IsLatest/delete-marker derivation
correct; coverage floors honest at audit-time (86.9/84 etc., achieved-2
exactly); detection harness genuinely revert-provable (3 live mutations);
root_coverage_wiring_test.go ~95% substantive, detection_test.go ~90%,
actions_coverage_test.go ~80% (one vacuous core, M7).

## Gate-claim verification

| Claim | Verified |
|---|---|
| test-cover-enforce runs, floor 47 | yes; but 47 vs ~86% = stale-weak |
| check.yml floors = achieved-2 | yes at 2af1175; DEAD post-rename (H2) |
| fixtures unchanged claim (df168f7) | verified via git log -p |
| e2e 213/213 | re-run this session, green |

## Context notes

- The zeta-object rename (a04dbbb..0b7c5ae) landed mid-audit from a
  parallel session; H2/M9 are rename-scope semantics (silent gate death,
  doc drift) flagged under the same-change rule.
- One live-host capture (dir rename + heavy activity incl. root-dir
  records + an objid-reuse instance) would confirm/kill H5, H6, M8 - the
  three graph-assumption findings. Host zfs-meta remains deployable.


---

## Fix-wave round 2 disposition (2026-09-29, commits 8710e20 / 69e2d2f / 14310d4)

| Finding | Disposition |
|---|---|
| H1 '.' bricking | FIXED 8710e20 (reverted exemption; pins incl. bucket-not-bricked) |
| H2 CI floors dead | FIXED by rename session 39cdf9b (labels -> zeta-object) |
| H3 LISTEN_ADDR/DEBUG_AUTH fallback | DEFERRED: fix lives in frontends.go/sigv4.go - owned by the in-flight auth-tree session; queue behind its landing |
| H4 root mis-election | FIXED 8710e20 (chain-terminus unanimity) |
| H5 root-dir record | FIXED 8710e20 (nameless records never map/vote; parent==root resolves) |
| H6 objid reuse rewrite | FIXED 8710e20 (txg-scoped mappings; live capture shows fresh-objid allocation per mount session - reuse is a long-lived-dataset hazard) |
| M1 singleton detail race | DEFERRED: fix is in s3_wiring.go (auth session's file); queue |
| M2 hrtime wall-clock | FIXED 8710e20 (plausibility gate, zero = unknown) |
| M3 events traversal | FIXED 69e2d2f (validBucket in resolveEventsContext) |
| M4 marker decode | FIXED 69e2d2f (+round-trip walk test; orchestrator fixed the test's decode-vs-encoded comparison the fixer left red) |
| M5/M6 suffix bleed + rename half-reconstruction | ADDRESSED by H4/H6: exact-vs-partial now anchored to chain-terminus unanimity + txg-scoped mappings; residual partial-match breadth documented as contract |
| M7 vacuous ResetOn test | FIXED 14310d4 (timer-pointer identity; mutation-verified) |
| M8 ordering assumption | FIXED 8710e20 (oldest-first proven by live capture e6de259) |
| M9 example docs | FIXED by rename session |
| D4 prefix | FIXED 69e2d2f (+documented delimiter/encoding-type limitation) |
| D5 exec amplification | FIXED 8710e20 (semaphore cap 4) |
| D6 topology on wire | WONTFIX-by-design: dataset name in envelope is a documented contract; noted for shared-credential tenants |
| B2/B3/B4/B5/B6, C4, C5, A6, A7, A8 | LOW/INFO: accepted or deferred with the owner files; recorded in the report |

Live re-validation of the reconstruction fixes on zfs-meta: pending the
auth-tree session's landing (the fix commits are in; the host capture that
grounded them is committed as a fixture). Order-of-operations note: the
test fixer's failing TestVersionsMarkerRoundTripEncodableKeys (compared
decoded got vs encoded want) was corrected by the orchestrator before
commit; gates all green after.


## Final sweep against post-auth-tree HEAD (2026-09-30)

The auth/frontends session landed its full tree (GH #4 auth, #1 WebDAV,
#2 FTP/SFTP, #3 ownCloud; e2e now 349/349). Disposition updates:

- H3 LISTEN_ADDR/DEBUG_AUTH fallback: FIXED 992af99 (this session; the
  files freed up when the auth tree landed).
- M1 singleton detail race: FIXED - landed via the sibling's sweep of
  s3_wiring.go (verified in HEAD: per-bucket fresh provider instance in
  the wiring resolver, content identical to this session's fix).
- C4 warning-name: RESOLVED by the auth session's config refactor
  (warning names the actually-set variable; set-but-empty does not
  inherit legacy - honest-empty semantics verified).
- C5 string-surgery legacy derivation: still present (config.go:414) -
  INFO-latent, documented footgun, acceptable.
- gosec G706 in errors_to_s3.go: belongs to the auth session's uncommitted
  logSafe work (their finding is arguably a false positive - logSafe
  strips control characters before logging).

Verification at sweep time: build clean, full suite 0 failures, race
green (metadata/frontend), lint findings confined to the auth session's
uncommitted files, e2e 349/349.
