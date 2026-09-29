## Summary

Add FTP/FTPS and SFTP protocol frontends to zeta-object, behind the pluggable Frontend seam. This targets the large ecosystem of tools that speak file-transfer protocols but not object APIs.

## Context

zeta-object is gaining a two-axis plugin architecture (tracked in plan trees under `docs/plans/`):

- `docs/plans/object-model-2026-09/` - neutral object model
- `docs/plans/backend-interface-2026-09/` - pluggable storage backends
- `docs/plans/frontend-interface-2026-09/` - pluggable protocol frontends (S3 first)

FTP and SFTP are file-semantics protocols that map cleanly onto the mutable-object model. SFTP in particular is the common automation path (scripts, rclone, WinSCP). Note the distributed ZFS plan already contemplates SSH/SFTP as a backend transport; this issue is the reverse direction: clients speaking (S)FTP to zeta-object as a server.

## Scope

- FTP/FTPS frontend: likely built on github.com/fclairamb/ftpserverlib (license check required - reject GPL/AGPL linkage per project policy; vendoring constraints apply)
- SFTP frontend: likely built on github.com/pkg/sftp (same license gate)
- Directory listing maps to List with prefix/delimiter; STOR/RETR map to Put/Get; DELE/RMD map to Delete
- FTPS: explicit TLS (AUTH TLS) reusing the existing certs/cert.pem config
- SFTP: SSH key auth maps to the identity model (system-level key association is the natural fit here - see auth issue); password auth via the same adapter
- Capability degradation at the seam: no conditional reads, no multipart; reject or no-op with protocol-appropriate responses

## Acceptance criteria

- [ ] Both frontends registered under internal/frontend/, call only internal/backend.Backend
- [ ] Pass the Frontend conformance test suite
- [ ] rclone and an off-the-shelf FTP client can round-trip objects through both frontends
- [ ] License audit documented for any third-party dependency (LICENSE read from module cache, recorded in the PR)

## Dependencies

- Plans: object-model-2026-09, backend-interface-2026-09, frontend-interface-2026-09
- Issue: auth: pluggable authentication architecture for multi-protocol frontends

## Effort estimate

~1 week for both (SFTP slightly less; pkg/sftp does the protocol work).
