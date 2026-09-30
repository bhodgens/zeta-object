# SFTP Public-Key Adapter Hook (Interface-Only) - Implementation Leaf

> **For the implementing agent:** You are the implementer for this leaf.
> Implement ALL tasks below using TDD. Do NOT commit — the orchestrator
> handles all git operations after review. Do NOT use read_file on existing
> source files — explore with search_files or terminal cat. After writing
> a file, do NOT read it back to verify — write once and stop. After
> completing, report what you built, what files you touched, and any
> deviations from the spec.

## Meta

- **Parent:** [master.md](../master.md)
- **Scope:** The `PublicKeyAuthenticator` seam (Contract 5): canonicalization
  of SSH public-key strings, fingerprint-safe logging, and registry wiring —
  INTERFACE-ONLY. No SFTP frontend is built; the future
  `frontend: (S)FTP` issue consumes this hook.
- **Dependencies:** leaf 01 (`LookupByPublicKey` + `IdentityConfig.SSHPublicKeys`).
- **Estimated Context:** 40K
- **Concurrency Group:** B

## Goal

SFTP clients authenticate with SSH keys, not passwords or SigV4. Leaf 01
stores `sshPublicKeys` per identity and exposes a raw
`LookupByPublicKey(authorizedKey)` string lookup. This leaf hardens that raw
lookup into a usable adapter seam:

1. `auth.PublicKeyAuthenticator` interface — what the SFTP frontend will call.
2. Canonicalization: `authorized_keys`-format lines (options, keytype,
   base64 blob, comment), whitespace tolerance, keytype case — so an admin
   pasting a line from `~/.ssh/id_ed25519.pub` works without preprocessing.
3. Fingerprint-based logging: auth successes/failures log the key's SHA-256
   fingerprint (RFC 4716 / OpenSSH style), NEVER the key material.

Deliberately NOT here: no SSH server, no golang.org/x/crypto/ssh dependency
(stdlib-only repo rule), no wire protocol. Parsing stays line-format-level so
the SFTP frontend (which will have real key blobs from its SSH handshake)
can normalize through the same seam.

## Context

Key facts:

- Repo rule: Go stdlib only. `golang.org/x/crypto/ssh` is NOT vendored
  (check `vendor/`). Canonicalization must therefore be lexical/format-level:
  split into fields, recognize the `<keytype> <base64> [comment]` shape,
  normalize whitespace and case of the keytype, and compare the DECODED base64
  blob bytes (so padding/whitespace differences don't matter). We do NOT parse
  the wire-format blob's inner fields — fingerprint = SHA-256 over the decoded
  blob bytes.
- Leaf 01's registry stores normalized keys and matches by exact normalized
  equality; `LookupByPublicKey` miss ⇒ false.
- Duplicate public keys across identities are rejected at registry build
  (leaf 01 Task 2).

Key files:
- [master.md](../master.md) — Contract 5
- internal/auth/registry.go — the lookups this wraps
- docs/plans/issue-sftp-ftp-frontend.md — the future consumer (read for shape,
  do not implement)

## Interface Contracts (From Parent)

```go
// File: internal/auth/publickey.go (new)
package auth

// PublicKeyAuthenticator is the seam the future SFTP frontend consumes:
// normalize an authorized_keys-style line (or bare keytype+blob string) and
// resolve it to an identity. Implemented by MultiRegistry.
type PublicKeyAuthenticator interface {
    AuthenticatePublicKey(presentedKey string) (Identity, error)
}

// CanonicalizePublicKey normalizes an authorized_keys-format line to the
// canonical form stored/compared by the registry: "<lowercased-keytype> <base64-blob>"
// (comment stripped, options stripped, whitespace collapsed). Returns an
// error for empty/garbage input. Exported so the SFTP frontend can
// pre-normalize handshake keys through the SAME path.
func CanonicalizePublicKey(presentedKey string) (canonical string, fingerprint string, err error)

// Typed errors: ErrKeyMalformed (unparseable), ErrKeyUnknown (valid but not registered).
```

## Tasks

### Task 1: Canonicalization + fingerprint

**Files:**
- Create: `internal/auth/publickey.go`
- Test: `internal/auth/publickey_test.go`

**Step 1: Write failing tests** (table-driven, using REAL-format fake keys —
generate two throwaway ed25519/rsa public blobs by hand-embedding base64 of
obviously-fake byte blobs; never real private material):

- Full line with comment + trailing newline → canonical `type blob`, comment gone.
- Options prefix (`environment="X" ssh-ed25519 AAA... comment`) → stripped.
- Whitespace/tab variance, trailing spaces → same canonical form.
- Keytype case (`SSH-ED25519` vs `ssh-ed25519`) → normalized to lowercase.
- Same blob with different comments → SAME canonical string (dedupe target).
- Different blob, same comment → different canonical string.
- Fingerprint: `SHA256:<base64std-nopad-of-blob-hash>` for a known input
  (precomputed constant in the test).
- Garbage (``, `not-a-key`, `ssh-ed25519` with no blob, non-base64 blob) →
  `ErrKeyMalformed`.

**Step 2:** `go test ./internal/auth/ -run TestPublicKey -v` → FAIL.

**Step 3: Implement** `CanonicalizePublicKey` (lexical parse + base64 decode,
no external deps).

**Step 4:** same command → PASS.

### Task 2: `MultiRegistry` implements `PublicKeyAuthenticator`

**Files:**
- Modify: `internal/auth/registry.go` (add method + canonicalize at both
  build time and lookup time)
- Test: `internal/auth/registry_publickey_test.go`

**Step 1: Write failing tests:**

- Identity registered with a commented, option-prefixed key line; lookup with
  the bare `type blob` form → HIT with that identity's grants.
- Unknown-but-valid key → `(Identity{}, ErrKeyUnknown)`.
- Malformed presented key → `ErrKeyMalformed` (never a silent miss).
- Two identities with keys differing only in comment → registry BUILD
  rejected as duplicate (canonical collision).
- Compile-time assertion `var _ PublicKeyAuthenticator = (*MultiRegistry)(nil)`.

**Step 2:** FAIL → **Step 3:** implement (canonicalize on insert and on
lookup; store fingerprints alongside for logging) → **Step 4:** PASS.

### Task 3: Fingerprint-safe logging helper

**Files:**
- Modify: `internal/auth/publickey.go`
- Test: covered in Task 1/2 tests

Implement one exported helper the SFTP frontend will use for audit lines:

```go
// LogSafeKey returns "accessKeyID=<akid> fingerprint=<fp>" for log lines.
// Key material and comments never appear. Unit test asserts no base64
// substring of the key leaks into the returned string.
func LogSafeKey(accessKeyID, fingerprint string) string
```

## Self-Verification Checklist

- [ ] `go test ./internal/auth/ -v` green (all tasks)
- [ ] Stdlib only — no new imports outside stdlib
- [ ] No key material in any log path or error message (tests assert)
- [ ] No SFTP server/protocol code — interface + canonicalization only
- [ ] Frozen shapes untouched; leaf-01 registry behavior unchanged for
      access-key/Basic lookups
- [ ] `make fmt`, `make vet`, `make lint`, `make test-cover-enforce` clean
      (re-measure floors per AGENTS.md if aggregate moved)

**DO NOT COMMIT.**

**Deviations from spec:** [none / list any with rationale]

## Review Checklist (For Review Agent)

- [ ] Tasks 1–3 done, tests first, green
- [ ] Contract 5 satisfied exactly (names, file paths)
- [ ] Canonicalization is format-level only (no crypto/ssh dependency)
- [ ] Scope held: nothing in `internal/frontend/`, no listener changes

Output: APPROVED or list of specific gaps with file + line references.

## Notes

- The e2e/wire-level coverage rule (AGENTS.md) does NOT bite this leaf:
  an interface-only hook is not a user-facing feature. The user-facing
  features (multi-identity config, grant enforcement, dev mode) get their e2e
  case in leaf 06.
- When the SFTP frontend lands, its handshake presents decoded key blobs; it
  should call `CanonicalizePublicKey` then `AuthenticatePublicKey` — same
  path as config-time, one canonical form everywhere.
