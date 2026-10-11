// tempcreds.go — ZSTSID- temporary-credential acceptance in the SigV4
// path (accessstor appliance tree leaf 02, Task 2).
//
// The branch point is credentialSecret (auth_adapter.go): a key id
// carrying the sts.AccessKeyPrefix resolves through the injected
// TempCredentialSource — secret re-derived statelessly, expiry enforced,
// grants re-resolved from the FederationRegistry AT REQUEST TIME — while
// every other key keeps the existing registry/env chain untouched. A
// ZSTSID- key with NO source installed (or a failed resolution) is an
// unknown access key, exactly the pre-leaf behavior for a foreign key.
//
// Charter: resolution is pure derivation + registry lookup; nothing is
// stored, nothing is cached across requests.
package s3

import (
	"log"

	"github.com/bhodgens/zeta-object/internal/auth"
	"github.com/bhodgens/zeta-object/internal/frontend/sts"
)

// tempCredsHook is the installed resolver (package main wiring). nil =
// temporary credentials are not recognized (every ZSTSID- key is just an
// unknown access key). Guarded by hookMu (seam.go) like every
// runtime-writable hook in this package.
var tempCredsHook sts.TempCredentialSource

// InstallTempCredentialSource installs the ZSTSID- resolver (exported
// wiring entry). nil restores the disabled default (tests).
func InstallTempCredentialSource(src sts.TempCredentialSource) {
	hookMu.Lock()
	defer hookMu.Unlock()
	tempCredsHook = src
}

// tempCredsFor returns the installed resolver under a read lock.
func tempCredsFor() sts.TempCredentialSource {
	hookMu.RLock()
	defer hookMu.RUnlock()
	return tempCredsHook
}

// isTempAccessKey reports whether the key id carries the ZSTSID- prefix.
func isTempAccessKey(accessKeyID string) bool {
	return len(accessKeyID) >= len(sts.AccessKeyPrefix) && accessKeyID[:len(sts.AccessKeyPrefix)] == sts.AccessKeyPrefix
}

// tempCredentialSecret resolves a ZSTSID- key through the installed
// source. found=false keeps the caller's unknown-key behavior; the
// resolution failure is logged WITHOUT the key's payload segments (the
// principal/nonce ride in the key id — log only the prefix and length).
func (f *Frontend) tempCredentialSecret(accessKeyID string) (string, auth.Identity, bool) {
	src := tempCredsFor()
	if src == nil {
		return "", auth.Identity{}, false
	}
	secret, id, ok := src.Resolve(accessKeyID)
	if !ok {
		log.Printf("Authentication Error: temporary credential %s… (%d bytes) rejected (expired, tampered, or revoked)",
			sts.AccessKeyPrefix, len(accessKeyID))
		return "", auth.Identity{}, false
	}
	return secret, id, true
}
