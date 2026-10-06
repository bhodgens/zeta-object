## Summary

Design and implement pluggable authentication that works across ALL frontend protocols (S3, WebDAV, FTP/FTPS, SFTP, ownCloud). Today auth is S3-only and single-identity. We need to decide the identity model: system-level key association, OAuth/OIDC, per-frontend adapters, or a combination.

## Current state (reviewed 2026-09)

- ONE shared credential pair for the whole server: `config.go:109` (struct `serverCredentials`), loaded from env `ZETAOBJECT_ACCESS_KEY` / `ZETAOBJECT_SECRET_KEY`, default `zetaadmin`/`zetaadmin`
- Verification is SigV4-only: `sigv4.go:438` `authenticateRequest` (Authorization header) plus presigned URL verification (~`sigv4.go:666`), 15-minute clock skew window
- No multi-identity, no per-bucket ACL, no token/session concept, no OAuth anywhere
- There is no per-user anything: every request is the same principal

## Requirements

- Every frontend maps its wire protocol's credentials to a COMMON internal identity check. S3 signs with SigV4; WebDAV/FTP/ownCloud use Basic auth; SFTP uses SSH keys. The internal identity + authorization decision must be shared.
- Multiple identities (today's single pair cannot distinguish clients)
- Per-identity bucket grants (read-only vs read-write is the minimum split)
- Zero-auth dev mode must stay available (current default behavior) but must be loudly logged

## Options to decide (the actual purpose of this issue)

1. System-level key association (recommended starting point): config declares multiple access-key/secret-key pairs, each with bucket grants. Frontend adapters map: SigV4 access key ID -> identity; Basic auth username/password -> identity; SFTP public key -> identity. No new protocols to learn; works offline; ~2-3 days.
2. OAuth2/OIDC: needed only if browser-based or third-party-ecosystem clients must obtain tokens dynamically. Heavy for zeta-object's self-hosted niche; would make S3 SigV4 awkward (S3 has no OAuth). Recommend defer.
3. Plugin/subprocess authenticator (hashicorp go-plugin style): maximal extensibility, real operational cost. Recommend only if an external auth source (e.g. LDAP) becomes a requirement.

Recommendation summary: internal Identity model + per-frontend Authenticator adapters (option 1 now, interfaces shaped so option 2/3 can slot in later). The `frontend-interface-2026-09` plan already pins the v1 adapter shape: `Authenticator.Authenticate(r) (Identity, error)` with `Identity{AccessKeyID string; BucketGrants map[string]Grant}`.

## Acceptance criteria

- [ ] Decision recorded in this issue with rationale
- [ ] Multi-identity config format designed (config.json extension, backward compatible with the single env pair)
- [ ] Identity + grant enforcement tested from at least two different frontends proving the model is protocol-neutral
- [ ] Migration path: existing ZETAOBJECT_ACCESS_KEY/SECRET_KEY deployments keep working

## Dependencies

- Plan: frontend-interface-2026-09 (pins the Authenticator adapter interface)
- This issue BLOCKS the WebDAV, (S)FTP, and ownCloud frontend issues' auth acceptance criteria

## Effort estimate

Decision + design: half a day. Option 1 implementation: 2-3 days.
