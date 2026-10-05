// audit_seam.go — the EXPORTED audit append entry point (management-api-2026-10
// leaf 04, master Contract 6). The admin frontend must not import package s3
// (seams are injected from package main), so this file exposes one function
// over the same writer-only sink audit_log.go owns. It changes NOTHING about
// the record: the eight-key shape (ts, principal, method, bucket, key, op,
// status, denied) is unchanged, and a management request simply writes
// op = "admin".
//
// Charter discipline is preserved: this is a WRITE path only. Nothing here
// reads the file back, and the management frontend holds no audit state — it
// calls this function once per authenticated request and forgets.
package s3

// AppendAudit appends ONE audit record for an authenticated request to the
// installed writer-only sink. A nil (disabled) writer is a no-op, so an
// operator without an audit log configured sees no cost. principal is the
// authenticated identity (the certificate Common Name for a management
// request), op is the internal/auth Op value ("admin" for management
// requests), and denied should be true when status is >= 400.
//
// This is the injection point package main hands to the admin frontend
// (admin.Options.Audit); the signature matches admin.AuditFunc exactly.
func AppendAudit(principal, method, bucket, key, op string, status int, denied bool) {
	recordAudit(principal, method, bucket, key, op, status, denied)
}
