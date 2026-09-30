// env.go — the legacy env-pair bridge (pluggable-authentication tree leaf
// 01). The migration contract: the env pair (ZETAOBJECT_ACCESS_KEY /
// ZETAOBJECT_SECRET_KEY, MINIS3_* fallback, default minioadmin) is ALWAYS
// present in the registry as identity name "env" with wildcard readwrite
// grants — byte-identical to the pre-tree single-pair behavior.
//
// Production wiring calls EnvPair from package main (config.go) AFTER
// loadCredentials has resolved the values; nothing in this package reads the
// environment itself (the credential values arrive as arguments so tests can
// drive every branch deterministically).
package auth

// EnvPair returns the env-derived identity config: name "env", wildcard
// readwrite grants. accessKey/secretKey are the values package main's
// loadCredentials resolved (including its default and warn-on-empty rules,
// which stay in package main — this function does not duplicate them).
func EnvPair(accessKey, secretKey string) IdentityConfig {
	return IdentityConfig{
		Name:      "env",
		AccessKey: accessKey,
		SecretKey: secretKey,
		Grants:    map[string]string{"*": GrantReadWrite},
	}
}
