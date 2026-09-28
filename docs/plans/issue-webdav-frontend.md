## Summary

Add a WebDAV protocol frontend to mini-s3, behind the pluggable Frontend seam. This makes every OS treat mini-s3 as a mounted drive: macOS Finder, Windows Explorer, and Linux (gvfs/davfs2) all speak WebDAV natively.

## Context

mini-s3 is gaining a two-axis plugin architecture (tracked in plan trees under `docs/plans/`):

- `docs/plans/object-model-2026-09/` - neutral object model (Object, BucketInfo, ListPage)
- `docs/plans/backend-interface-2026-09/` - pluggable storage backends
- `docs/plans/frontend-interface-2026-09/` - pluggable protocol frontends (the S3 frontend is the first instance)

WebDAV is the highest value-per-effort frontend after S3: small protocol, native OS mounting, ~1 week estimated effort. It is a WebDAV server backed by the neutral model through the Backend interface.

## Scope

- Methods: OPTIONS, PROPFIND (depth 0/1), GET, HEAD, PUT, DELETE, MKCOL, COPY, MOVE
- Map WebDAV collections to S3 bucket/prefix semantics; document the mapping explicitly
- Map WebDAV properties (getcontentlength, getcontenttype, getetag, getlastmodified, resourcetype) from the neutral object model
- Auth via the frontend Authenticator adapter (HTTP Basic); identity model decided in the auth issue
- Capability degradation at the seam: WebDAV has no bucket concept - document how bucket selection works (single-bucket mode via config, or top-level collections = buckets) and reject what cannot be expressed

## Acceptance criteria

- [ ] Frontend registered under internal/frontend/webdav/, calls only internal/backend.Backend
- [ ] Passes the Frontend conformance test suite from frontend-interface-2026-09
- [ ] macOS Finder and Linux davfs2 can mount, read, write, and delete objects
- [ ] Auth: requests without valid credentials rejected with 401

## Dependencies

- Plans: object-model-2026-09, backend-interface-2026-09, frontend-interface-2026-09
- Issue: auth: pluggable authentication architecture for multi-protocol frontends

## Effort estimate

~1 week.
