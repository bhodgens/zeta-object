# Third-Party License Audit — FTP/SFTP frontend tree (2026-09)

Policy (user hard rule): GPL/AGPL dependencies are acceptable ONLY as
subprocesses — never vendored, never linked into shipped binaries.
Classification: MIT / BSD / ISC / Apache-2.0 ⇒ approved for linkage;
GPL/AGPL/LGPL ⇒ REJECTED as a linked dep, escalate to the user; a MISSING or
non-standard LICENSE file ⇒ treated as rejected until clarified.

Source of truth: the LICENSE file read from the local Go module cache
(GOMODCACHE), never memory or online reconnaissance. The online check in the
planning notes (master.md) was reconnaissance; the reads below are the record.

## Verdict: ALL PERMISSIVE — gate PASSED. No GPL/AGPL/LGPL anywhere in the
## dependency closure; leaves 03 (FTP) and 04 (SFTP) are cleared to proceed.

| Module | Version (go.mod) | License (from cache) | SPDX | Copyright | Checked |
|---|---|---|---|---|---|
| github.com/fclairamb/ftpserverlib | v0.32.4 | MIT (`license.txt`) | MIT | (c) 2016 Andrew Arrow; (c) 2016 Florent Clairambault | 2026-09-30 |
| github.com/pkg/sftp | v1.13.10 | BSD 2-clause (`LICENSE`) | BSD-2-Clause | (c) 2013 Dave Cheney | 2026-09-30 |
| golang.org/x/crypto | v0.44.0 | BSD 3-clause (`LICENSE`) | BSD-3-Clause | Copyright 2009 The Go Authors | 2026-09-30 |
| github.com/jlaffaye/ftp | v0.2.4 | ISC (`LICENSE`) | ISC | (c) 2011-2013 Julien Laffaye | 2026-09-30 |
| github.com/kr/fs (transitive, pkg/sftp) | v0.1.0 | BSD 3-clause (`LICENSE`) | BSD-3-Clause | (c) 2012 The Go Authors | 2026-09-30 |
| github.com/spf13/afero (transitive, ftpserverlib) | v1.15.0 | Apache 2.0 (`LICENSE.txt`) | Apache-2.0 | (c) Steve Francia / spf13 | 2026-09-30 |

Version note: pkg/sftp pinned at v1.13.10 and x/crypto at v0.44.0 (not the
latest) because the repo targets `go 1.25.6` and newer lines require
`go 1.26` — same licenses at both versions (re-read from cache).

Roles: ftpserverlib = FTP/FTPS server protocol (linked into binary);
pkg/sftp = SFTP subsystem (linked into binary); golang.org/x/crypto = SSH
transport under SFTP (linked, transitive); jlaffaye/ftp = FTP CLIENT for
package tests ONLY (test-only, not shipped — gated anyway, ISC is permissive).

Note on repo `vendor/`: the project's `vendor/` directory holds test tooling
(boto3 venv, mc, ceph s3-tests), NOT Go modules — none of the four modules
above is hand-vendored; all flow through the module cache per policy.

Reproduce:

    go list -m -f '{{.Path}}@{{.Version}}' <module>
    cat "$(go env GOMODCACHE)/$(go list -m -f '{{.Path}}@{{.Version}}' <module> | tr ':' '!')"/LICENSE

(the ftpserverlib file is `license.txt`, afero's is `LICENSE.txt` — same
command with that filename).

Reviewed-by: sftp-ftp-2026-09 leaf 02 execution (2026-09-30); versions pinned
at go.mod when the frontend code lands (`go mod tidy` re-verifies).
