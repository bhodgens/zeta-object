//go:build !linux && !freebsd && !dragonfly

package metadata

import "os"

// DetectZFS reports false on platforms without a usable statfs ZFS signal.
// macOS dev machines have no ZFS; this keeps `go build` and cross-compiles
// green. Reason strings for Probe come from the caller.
//
// There is no statfs here, but a missing path must still surface as an
// error so callers can distinguish "not ZFS" from "bad bucket path".
func DetectZFS(path string) (bool, error) {
	if _, err := os.Stat(path); err != nil {
		return false, &PathStatError{Path: path, Err: err}
	}
	return false, nil
}
