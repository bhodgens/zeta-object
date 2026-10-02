# Object Tagging - Implementation Orchestrator

> **For the executing agent:** You are the orchestrator for this tree node.
> Dispatch leaf agents, review, commit after review passes. Do NOT
> implement code yourself.

## Meta

- **Role:** Root
- **Parent:** none (root)
- **Children:** 4 leaf documents
- **Scope:** S3 object tagging (zeta-object#8): x-amz-tagging on PUT/COPY,
  ?tagging subresource (GET/PUT/DELETE), TagCount. Dual storage: dotfile
  sidecar (non-ZFS) + zmetad DB table (ZFS, upstream zfs-metadata#13) with
  sidecar fallback until upstream lands.

## Goal

Tags persist on both storage paths; the S3 tagging API surface works;
parity between paths.

## Architecture

- Parse/validate tags once (shared): `x-amz-tagging` is URL-encoded
  `key=value&key2=value2`; limits 10 tags, key <= 128, value <= 256,
  `aws:` prefix reserved (S3 rejects with InvalidTag). Validate at PUT
  and at ?tagging PUT.
- Dotfile path: extend the sidecar struct (`.meta` JSON) with a Tags
  map. CRITICAL: sidecar JSON must stay backward-compatible (old
  readers ignore the new field; old sidecars have no Tags -> empty).
- ZFS path: upstream zfs-metadata#13 asks for a tags table. Until it
  lands, ZFS buckets ALSO use the sidecar (honest, documented). The
  seam: a tagStore interface so the zmetad-backed implementation slots
  in later without re-touching handlers.

## Interface Contracts

### Contract 1: tag parsing/validation (FROZEN)

```go
// File: internal/objectmodel/tags.go
package objectmodel

// ParseTagHeader decodes an x-amz-tagging header value
// (URL-encoded key=value&...). Returns an error naming the violation
// (count, key length, value length, aws: prefix, bad encoding).
func ParseTagHeader(v string) (map[string]string, error)

// ValidateTags applies the S3 limits to a tag map (also used for the
// ?tagging PUT XML body).
func ValidateTags(tags map[string]string) error

// TagsToXML renders the GetObjectTagging XML body; TagsFromXML parses
// the PutObjectTagging body (TagSet/Tag/Key/Value elements).
func TagsToXML(tags map[string]string) []byte
func TagsFromXML(body []byte) (map[string]string, error)
```

### Contract 2: sidecar + tagStore seam (FROZEN)

- `LegacyObjectMetadata`/Object gains `Tags map[string]string`
  (objectmodel), serialized in the sidecar JSON (omit when empty -
  byte-compat with old sidecars). Sidecar read/write sites in
  object_handlers.go gain tag copy on COPY (respect
  x-amz-tagging-directive: COPY default copies tags; REPLACE uses the
  header) and TagCount header on GET/HEAD.
- Handlers resolve storage via `tagStoreFor(bucketPath) tagStore` in a
  new `internal/frontend/s3/tagstore.go`:

```go
type tagStore interface {
    Get(key string) (map[string]string, error)
    Put(key string, tags map[string]string) error
    Delete(key string) error
}
// v1: sidecarTagStore only; zmetadTagStore lands when upstream ships.
```

## Child Document Index

| # | Document | Type | Dependencies | Est. Context |
|---|----------|------|-------------|-------------|
| 01 | 01-tags-parse.md | leaf | none | ~30K |
| 02 | 02-sidecar-tags.md | leaf | 01 | ~40K |
| 03 | 03-tagging-handlers.md | leaf | 01, 02 | ~45K |
| 04 | 04-tagging-e2e.md | leaf | 03 | ~35K |

Sequential. Leaf briefs are written by the orchestrator from this
master's contracts + the conventions below, following the leaf template
(DISPATCH INSTRUCTION header, TDD tasks, self-verification + review
checklists, Do-NOT-commit) - author each brief before dispatching it.

## Brief contents per leaf (mandatory sections)

- 01: tags.go + tags_test.go per Contract 1 (URL-decode rules, limits,
  aws: reserved, XML round-trip; Tagging header on PUT also accepted
  url-encoded with `&`/`=` only).
- 02: objectmodel Tags field + sidecar JSON round-trip + COPY semantics
  + byte-compat test (old sidecar JSON without Tags decodes fine).
- 03: ?tagging subresource routing (GET/PUT/DELETE on object +
  TagCount/ x-amz-tagging on PUT/GET/HEAD/COPY) + tagStore seam +
  sidecarTagStore. Parity: identical behavior for ZFS buckets (same
  sidecar) - note in README that ZFS-native tags await zfs-metadata#13.
- 04: e2e case `<n+1>-tagging.sh` (PUT with header -> GET ?tagging XML
  round-trip -> PUT ?tagging replace -> DELETE ?tagging -> TagCount on
  HEAD; COPY directive both modes) + README Known Limitations row
  removal + final gates.

## Dispatch Protocol

Same as the other trees (leaf text + repo context + no-commit; in-session
review; commit per leaf; tracking table).

## Coding Conventions

Stdlib-first; errors `s3: `/`objectmodel: ` per package; hyphens in
prose; frozen interfaces (MetadataProvider) untouched; sidecar JSON
backward-compatible is a HARD requirement.

## Completion Tracking Table

| Leaf | State | Review | Commit | Notes |
|------|-------|--------|--------|-------|
| 01-tags-parse.md | pending | - | - | |
| 02-sidecar-tags.md | pending | - | - | |
| 03-tagging-handlers.md | pending | - | - | |
| 04-tagging-e2e.md | pending | - | - | |

## Integration Test Plan

make test / lint NEW_FROM_REV / e2e green (pre-existing webdav+owncloud
failures documented as such). Parity: sidecar path is THE path for both
bucket kinds in v1 - parity is trivially satisfied; note in README.

## Review Checklist

- [ ] Contracts verbatim; sidecar byte-compat test exists.
- [ ] aws: prefix rejected; limits enforced at both entry points.
- [ ] TagCount on GET/HEAD; COPY directive honored.
- [ ] README updated (limitation removed, zfs-metadata#13 noted).

## Open Questions

None blocking; upstream #13 answers the ZFS-native storage shape later.
