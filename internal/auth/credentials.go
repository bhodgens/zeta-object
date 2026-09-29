package auth

// CredentialSource supplies the shared secret for an access key ID.
// v1 has exactly one pair (package main's serverCredentials); the auth GH
// issue ("auth: pluggable authentication architecture") replaces this with
// real identity lookup.
type CredentialSource interface {
	SecretKey(accessKeyID string) (string, bool)
}
