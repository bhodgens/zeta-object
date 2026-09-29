# AGENTS.md

Guidance for AI agents and humans working in this repository.

## E2E coverage rule (hard requirement)

Every new user-facing feature MUST ship with e2e or wire-level test coverage in
`scripts/e2e/` in the SAME change. "User-facing" means any of:

- API surface - new S3 operations, query parameters, headers, or response fields
- Config surface - new `config.json` keys, env vars, or backend options
- Backend capability - new behavior in `internal/backend/*` visible to clients
- Frontend capability - new behavior in `internal/frontend/*` visible to clients

A feature merged without a case under `scripts/e2e/cases/` is incomplete.

## Coverage floors move with code

When a change moves code between packages (file split, package extraction), the
coverage floors MUST be re-measured and updated in the SAME change. Set each
floor to the measured value rounded down, with a comment saying when to ratchet
it back up as tests land.

Incident: the frontend-interface split (2026-09) moved code out of package main
and dropped aggregate coverage from above the old floor to 47.9% while
`COVER_MIN` still said 70 - the gate was red for everyone and got ignored. Do
not let floors drift from reality again. See the AGENTS.md rule in the Makefile
and `.github/workflows/check.yml` comments.

## E2E harness

Run the whole suite with `make e2e` (wraps `scripts/e2e/run-e2e.sh`). The
harness builds the server, generates temp certs and a temp dataDir, starts on a
free port, and runs every `scripts/e2e/cases/*.sh` in lexical order. To add a
case: copy an existing numbered case, keep the `BKT=` bucket convention and the
create/cleanup pairing, and use the assert helpers from `scripts/e2e/lib.sh`.
Cases also exist for interop clients (`boto3`, `mc`) - extend those when the
feature is client-visible there too.

## Plan trees

Multi-leaf work lives under `docs/plans/<tree>/master.md` (for example
`docs/plans/hardening-2026-09/master.md`). Each leaf is one file; a leaf is done
only when its code, tests, and docs land together. Start new campaign plans
there, not in ad-hoc notes.
