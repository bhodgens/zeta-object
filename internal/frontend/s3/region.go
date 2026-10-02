package s3

import "strings"

// region.go — SigV4 verification region accessor (region-config-2026-10
// leaf 01, Contract 1). The region comes from the server `region` config
// key, wired at startup by the config loader; tests set it via SetRegion.

// region returns the SigV4 region this server verifies against.
// Wires from the server config at startup; tests set it via
// SetRegion. Default "us-east-1".
func regionOf() string {
	if region == "" {
		return defaultRegion
	}
	return region
}

// SetRegion sets the verification region (config wiring entry;
// empty restores the default). Not safe for concurrent use with
// in-flight requests: call at startup only.
func SetRegion(r string) {
	// Belt and suspenders: config load already lowercases (SigV4
	// regions are lowercase), but direct callers get the same
	// normalization.
	region = strings.ToLower(r)
}

// region is the package-level verification region. Startup-only: written
// by SetRegion before request traffic, read-only afterwards.
var region = ""
