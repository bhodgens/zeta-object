package auth

// grants.go is part of auth.go's frozen Identity type: the grant query
// methods are ADDED by the pluggable-authentication tree (leaf 01) without
// changing the struct shape. The grant decision lives in exactly one place —
// these two methods — and every frontend (S3, Basic, future SFTP) asks them.

// CanRead reports whether the identity may read bucket. A "*" grant covers
// every bucket; a per-bucket grant covers that bucket only. Write implies
// read. A nil or empty grant map grants nothing.
func (id Identity) CanRead(bucket string) bool {
	if g, ok := id.BucketGrants[bucket]; ok && g.Read {
		return true
	}
	g, ok := id.BucketGrants["*"]
	return ok && g.Read
}

// CanWrite reports whether the identity may write bucket. A "*" grant covers
// every bucket; a per-bucket grant covers that bucket only. A nil or empty
// grant map grants nothing.
func (id Identity) CanWrite(bucket string) bool {
	if g, ok := id.BucketGrants[bucket]; ok && g.Write {
		return true
	}
	g, ok := id.BucketGrants["*"]
	return ok && g.Write
}

// WildcardIdentity returns an identity with wildcard readwrite grants — the
// historical "one omnipotent credential pair" semantic. Used by the legacy
// CredentialSource fallback path in the S3 adapter and by dev mode's
// anonymous principal; it is the ONE place that shape is synthesized.
func WildcardIdentity(accessKeyID string) Identity {
	return Identity{
		AccessKeyID:  accessKeyID,
		BucketGrants: map[string]Grant{"*": {Read: true, Write: true}},
	}
}
