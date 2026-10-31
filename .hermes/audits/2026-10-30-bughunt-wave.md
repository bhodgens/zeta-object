# mini-s3 bughunt wave 2026-10-30 — window 2026-10-14..HEAD

Method: 2 read-only auditors (new-surface audit: admin password rotation,
HTTP/3 header budget, appliance merge, zfs-validate scripts; adversarial
regression review of the 7 fix commits that closed the 2026-10-14 wave)
+ parent verification.

## Parent-verified findings

1. [MEDIUM -> ACCEPTED DESIGN] admin_credential.go:53-68 — rotated admin
   secret persists as a RAW secret (0600, atomic write), not a hash.
   Parent verified: this matches the repo's S3 secret-key model (raw
   secrets are the auth credential; hashing would break SigV4). mTLS gate
   verified (TestAdminPassword_RequiresCertificate: missing/wrong-CA/
   expired all 401). Mutex serializes writes. No fix.
2. [PASS] HTTP/3 decoded-header budget (dc2164c): quic-go enforces
   MaxHeaderBytes at the decode boundary; the amp probe asserts rejection
   (431 / stream reset) AND server survival. Correct.
3. [PASS] Appliance-2026-10 merge (c6ee380): Go changes are dead-code
   cleanup only; no contract drift.
4. [PASS] zfs-validate script fixes: quoting, temp-file races, curl
   masking all properly fixed.
5. [PASS] All 7 prior-wave fix commits (0b001b9, d9de9e1, e36b20d,
   7a193c1, f16315b, 50f5775, 1c0f29f): fixes correct, pin tests feed the
   production caller shape, no new bugs introduced.

## New bugs found: none actionable this window.
The repo's 2026-10-14 wave already consumed the bug surface; the new
commits since are fixes + docs + the appliance merge.

## DISCLOSURE LEDGER (auditor noise)
- s3-a1 #3 "no timing-safe compare": parent verified there is no
  compare in the rotation path (write/read only) — non-finding.
- s3-a1 #2 "race on concurrent rotation": mutex present — non-finding.

## Fix disposition
- No code fixes required. Report-only wave.
