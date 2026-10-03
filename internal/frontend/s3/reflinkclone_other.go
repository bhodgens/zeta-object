//go:build !linux

// reflinkclone_other.go — the non-linux arm of the reflink clone op
// (s3-versioning leaf 06). darwin has NO FICLONE; every other GOOS is
// unsupported too. The typed error (reflinkCloneUnsupportedError, in
// reflinkversions.go) is the fail-soft trigger: callers log one WARN
// and skip the version record (never break the PUT).
package s3

// reflinkClone is unsupported off linux; the dev host (darwin) exercises
// the fail-soft path through this arm.
func reflinkClone(dstPath, srcPath string) error {
	return &reflinkCloneUnsupportedError{}
}
