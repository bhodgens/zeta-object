## Summary

Add an ownCloud protocol frontend to mini-s3: ownCloud client applications (desktop sync client, mobile apps) could then sync against a self-hosted mini-s3.

## Context

mini-s3 is gaining a two-axis plugin architecture (tracked in plan trees under `docs/plans/`):

- `docs/plans/object-model-2026-09/` - neutral object model
- `docs/plans/backend-interface-2026-09/` - pluggable storage backends
- `docs/plans/frontend-interface-2026-09/` - pluggable protocol frontends (S3 first)

Protocol reality (verify during design): "ownCloud protocol" is WebDAV plus ownCloud-specific extensions - OCS API endpoints (shares, capabilities, user metadata) at /ocs/, and either the classic server's provisioning API or the newer ownCloud Infinite Scale (oCIS) API surface. The desktop sync client negotiates capabilities via OCS and uses WebDAV for data transfer. So this frontend is a superset of the WebDAV frontend issue.

## Open design decision (do first)

- Target: classic ownCloud server API vs oCIS API vs the WebDAV+OCS subset the sync client actually requires. Recommendation: implement the minimal subset the official desktop client needs (WebDAV core + OCS capabilities + versioning endpoints it probes), not a full server clone. If the client insists on accounts/shares we do not implement, document the degradation and stop.

## Scope

- WebDAV core (shares the WebDAV frontend issue's implementation; this issue extends it)
- OCS capabilities endpoint advertising exactly what mini-s3 supports
- File versions endpoint if the client requires it - natural fit with the ZFS events metadata provider (docs/plans/metadata-zfs-2026-09/) where available; degrade otherwise
- Auth: Basic auth via the frontend Authenticator adapter (identity model per auth issue); app-password style tokens if the client requires them

## Acceptance criteria

- [ ] Decision documented: which client(s) are the compatibility target, which subset implemented
- [ ] Official ownCloud desktop client can connect, sync up, sync down, delete
- [ ] Registered under internal/frontend/, calls only internal/backend.Backend
- [ ] Passes the Frontend conformance test suite

## Dependencies

- Plans: object-model-2026-09, backend-interface-2026-09, frontend-interface-2026-09
- Issue: frontend: add WebDAV protocol frontend (this issue builds on its implementation)
- Issue: auth: pluggable authentication architecture for multi-protocol frontends

## Effort estimate

2-3 weeks including client-compatibility testing (the API subset discovery is the cost, not the code).

## Honest value note

This frontend ranks below WebDAV and (S)FTP. It is worth doing only if there is a concrete ownCloud client deployment to serve. If that consumer does not exist, close this issue rather than building speculatively.
