// xattr.go — principal breadcrumb stamps (design
// docs/design/zfs-principal-metadata.md section 2a, adopted variant):
// additive, per-principal `user.zeta.*` xattrs written BEST-EFFORT on the
// object data file. Xattrs are object metadata (exactly like mode bits) —
// annotation only, zero effect on permissions (design 2b) — and an xattr
// failure NEVER fails the data operation: one WARN line with the xattr
// NAME (the AccessKeyID is log-safe; it already appears in dispatch logs)
// and continue.
//
// Names (user.* namespace):
//
//	user.zeta.owner              creator's AccessKeyID — set ONCE at
//	                             object create, never overwritten
//	user.zeta.writer.<keyID>     op@RFC3339-UTC (op: put|multipart|copy)
//	user.zeta.reader.<keyID>     last-read RFC3339 — opt-in per bucket,
//	                             first read per principal only
//
// Per-principal NAMES, not a shared list value: distinct names are
// independent writes — no read-modify-write, no lock, idempotent per
// principal (design 2a). Xattr names cap at 255 bytes; the registry
// rejects access keys that would reach the cap (auth registry build,
// design 2a "validate at config load that access keys fit the prefix
// budget").
package fsbackend

import (
	"log"
	"strings"
	"time"
)

// Xattr owner/writer/reader name roots. The per-principal suffixes are
// appended verbatim (AccessKeyIDs are registry-validated against the
// 255-byte name budget before they can ever reach here).
const (
	xattrOwner        = "user.zeta.owner"
	xattrWriterPrefix = "user.zeta.writer."
	xattrReaderPrefix = "user.zeta.reader."
	// XattrNameBudget is the kernel xattr-name cap (255 bytes). The
	// longest name this package builds is writer/reader prefix +
	// AccessKeyID; the registry's boundary check uses the same constant
	// arithmetic (duplicated in internal/auth to avoid the import cycle —
	// a comment there pins the pairing).
	XattrNameBudget = 255
	// XattrMaxKeyIDLen is the largest AccessKeyID that keeps every
	// breadcrumb name within XattrNameBudget. Exported so internal/auth
	// documents the same boundary without importing fsbackend.
	XattrMaxKeyIDLen = XattrNameBudget - len(xattrWriterPrefix)
)

// xattrOpPut / xattrOpMultipart are the breadcrumb op families. Copy goes
// through Put (Plan Contract 2c: PutOptions carries only Principal; the
// op string records the write family, attribution is unaffected).
const (
	xattrOpPut       = "put"
	xattrOpMultipart = "multipart"
)

// stampOwnerIfAbsentBreadcrumb sets user.zeta.owner = principal ONLY when
// the object does not already carry the owner xattr — the creator is
// recorded once and NEVER overwritten afterward (design 2a). Callers run
// this on the post-write inode (the atomic write replaces the file, so a
// fresh inode is absent-clean). Fail-open by contract.
func stampOwnerIfAbsentBreadcrumb(path, principal string) {
	if principal == "" {
		return
	}
	if err := setXattrIfAbsent(path, xattrOwner, principal); err != nil {
		if !isErrXattrNotFound(err) {
			log.Printf("WARNING: fsbackend: xattr stamp skipped (%s): %v", xattrOwner, err)
		}
	}
}

// stampWriterBreadcrumb sets user.zeta.writer.<principal> =
// "<op>@<RFC3339 UTC>" — the per-principal breadcrumb of the latest
// mutating write. Fail-open by contract.
func stampWriterBreadcrumb(path, principal, op string) {
	if principal == "" {
		return
	}
	name := xattrWriterPrefix + principal
	value := op + "@" + time.Now().UTC().Format(time.RFC3339)
	if err := setXattr(path, name, value); err != nil {
		log.Printf("WARNING: fsbackend: xattr stamp skipped (%s): %v", name, err)
	}
}

// StampReaderFirstRead stamps user.zeta.reader.<principal> with the
// last-read RFC3339 timestamp, FIRST READ PER PRINCIPAL ONLY: the presence
// check (fgetxattr) skips principals already stamped, keeping GET write
// amplification at one xattr write per reader per object — never per read
// (design 2a read path, gated by the per-bucket auditReads tunable).
// Fail-open: on read-only mounts or any xattr error the GET still
// succeeds — the stamp is silently skipped after one WARN.
func StampReaderFirstRead(path, principal string) {
	if principal == "" {
		return
	}
	name := xattrReaderPrefix + principal
	if _, err := getXattr(path, name); err == nil {
		return // already stamped: one write per reader per object
	} else if !isErrXattrNotFound(err) {
		log.Printf("WARNING: fsbackend: xattr read-stamp skipped (%s): %v", name, err)
		return
	}
	if err := setXattr(path, name, time.Now().UTC().Format(time.RFC3339)); err != nil {
		log.Printf("WARNING: fsbackend: xattr read-stamp skipped (%s): %v", name, err)
	}
}

// stampOwnerCarried implements the set-once owner rule on the atomic
// write path: the previous object version's owner (carryOwner, read
// before the atomic replace destroyed the old inode) rides onto the new
// inode; with no previous owner the writer IS the creator and becomes
// owner. Later writers therefore never change the owner (design 2a).
func stampOwnerCarried(path, principal, carryOwner string) {
	if principal == "" {
		return
	}
	owner := carryOwner
	if owner == "" {
		owner = principal
	}
	if err := setXattr(path, xattrOwner, owner); err != nil {
		log.Printf("WARNING: fsbackend: xattr stamp skipped (%s): %v", xattrOwner, err)
	}
}

// carryWriterBreadcrumbs re-applies the previous object version's
// per-principal writer breadcrumbs onto the freshly written inode. The
// atomic write (temp+rename) replaces the file, which would otherwise
// erase every prior stamp; re-applying the collected names preserves the
// accumulate-per-principal model (design 2a) without any shared-list
// read-modify-write — each name is an independent set. prior is the
// pre-write name list captured by the caller (CollectBreadcrumbNames).
// Fail-open: errors are logged once and never fail the operation.
func carryWriterBreadcrumbs(path string, prior []breadcrumbStamp) {
	for _, st := range prior {
		if err := setXattr(path, st.name, st.value); err != nil {
			log.Printf("WARNING: fsbackend: xattr stamp skipped (%s): %v", st.name, err)
		}
	}
}

// breadcrumbStamp is one carried-over breadcrumb (name+value pair).
type breadcrumbStamp struct {
	name  string
	value string
}

// CollectBreadcrumbNames snapshots an object's current breadcrumbs (owner
// value + per-principal writer names) before the atomic write replaces
// the inode. Empty principal = legacy path: nothing is read (the backend
// never stamps, so nothing needs carrying).
func CollectBreadcrumbNames(path string) (owner string, writers []breadcrumbStamp) {
	if v, err := getXattr(path, xattrOwner); err == nil {
		owner = v
	}
	names, err := listXattrNames(path)
	if err != nil {
		return owner, nil
	}
	for _, n := range names {
		if !strings.HasPrefix(n, xattrWriterPrefix) {
			continue
		}
		if v, err := getXattr(path, n); err == nil {
			writers = append(writers, breadcrumbStamp{name: n, value: v})
		}
	}
	return owner, writers
}

// StampOwnerIfAbsent is the exported set-once owner stamp for a path
// whose object does not already carry an owner xattr.
func StampOwnerIfAbsent(path, principal string) {
	stampOwnerIfAbsentBreadcrumb(path, principal)
}

// StampOwnerCarried is the exported carried-owner stamp for the above-seam
// multipart assembly: priorOwner (the PRE-rename object's owner value) is
// preserved, else principal becomes the owner at create.
func StampOwnerCarried(path, principal, priorOwner string) {
	stampOwnerCarried(path, principal, priorOwner)
}

// CarryWriterBreadcrumbs is the exported carry-over of prior per-principal
// writer breadcrumbs (see carryWriterBreadcrumbs).
func CarryWriterBreadcrumbs(path string, prior []breadcrumbStamp) {
	carryWriterBreadcrumbs(path, prior)
}

// BreadcrumbStamp is the exported alias of breadcrumbStamp (the multipart
// assembly snapshots and re-applies stamps across the seam).
type BreadcrumbStamp = breadcrumbStamp

// StampWriter is the exported writer breadcrumb for the above-seam
// multipart assembly (op "multipart").
func StampWriter(path, principal string) {
	stampWriterBreadcrumb(path, principal, xattrOpMultipart)
}

// OwnerXattr reads the object's user.zeta.owner value when present. The
// (\"\", false) result never fabricates — ?events owner enrichment surfaces
// an owner ONLY when the xattr exists.
func OwnerXattr(path string) (string, bool) {
	v, err := getXattr(path, xattrOwner)
	if err != nil || v == "" {
		return "", false
	}
	return v, true
}

// WriterXattrName returns the writer breadcrumb name for principal —
// exposed for tests and the README contract.
func WriterXattrName(principal string) string {
	return xattrWriterPrefix + principal
}

// ReaderXattrName returns the reader breadcrumb name for principal.
func ReaderXattrName(principal string) string {
	return xattrReaderPrefix + principal
}
