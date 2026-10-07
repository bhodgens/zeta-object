package scheduler

// platform.go - the pure-Go OS probes: free device space (statfs via
// golang.org/x/sys/unix - no cgo) and the macOS battery deferral probe
// (`pmset -g batt` output parsing; IOKit would be cgo, which the leaf
// explicitly rules out, so the leaf's documented shelling-out option is
// what ships).

import (
	"fmt"
	"os/exec"
	"runtime"
	"strings"

	"golang.org/x/sys/unix"
)

// statfsDeviceFree returns free bytes available to this (non-root) user
// on the device holding dir. Pure Go via x/sys/unix; errors surface (the
// caller decides the fail-open/fail-closed behavior).
func statfsDeviceFree(dir string) (uint64, error) {
	var st unix.Statfs_t
	if err := unix.Statfs(dir, &st); err != nil {
		return 0, fmt.Errorf("statfs %s: %w", dir, err)
	}
	// Bavail = blocks available to non-root; Bsize is in bytes on both
	// BSD and Linux statfs structs as exposed by x/sys.
	return st.Bavail * uint64(st.Bsize), nil
}

// BatteryDischarging reports whether the machine is on battery AND
// discharging (macOS only; on any other GOOS it is always false). It
// parses `pmset -g batt` output - no cgo (IOKit would need cgo; the leaf
// documents the shelling-out option as the acceptable fallback).
func BatteryDischarging() bool {
	if runtime.GOOS != "darwin" {
		return false
	}
	out, err := exec.Command("pmset", "-g", "batt").Output()
	if err != nil {
		return false // no pmset / desktop Mac: never defer
	}
	return pmsetDischarging(string(out))
}

// pmsetDischarging parses pmset output. Shapes seen in the wild:
//
//	Now drawing from 'AC Power'                    -> false
//	Now drawing from 'Internal Battery'            -> true
//	Now drawing from 'Internal Battery' ... 34%; discharging; ... -> true
//	Now drawing from 'AC Power' ... charged; ...   -> false
func pmsetDischarging(out string) bool {
	for line := range strings.SplitSeq(out, "\n") {
		if !strings.Contains(line, "Now drawing from") {
			continue
		}
		if strings.Contains(line, "AC Power") {
			return false
		}
		if strings.Contains(line, "Internal Battery") || strings.Contains(line, "Battery Power") {
			// "finishing charge"/"charged" lines still say AC-adjacent
			// states; treat only explicit discharge states as true.
			if strings.Contains(line, "discharging") || !strings.Contains(line, "charged") {
				return true
			}
			return false
		}
	}
	return false
}
