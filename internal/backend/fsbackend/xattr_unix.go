//go:build unix

// xattr_unix.go — the xattr syscall layer behind xattr.go. Linux and
// darwin both expose fsetxattr/fgetxattr through golang.org/x/sys/unix
// (already pinned in go.mod via the sftp dependency; now a direct require
// — no new third-party code). A build-tag split per the fsdetect_* pattern
// is not needed: one `unix` build tag covers both dev (darwin) and the
// validation host (linux).
package fsbackend

import (
	"errors"
	"os"
	"strings"

	"golang.org/x/sys/unix"
)

// setXattr sets name=value on the open-by-path file.
func setXattr(path, name, value string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	return unix.Fsetxattr(int(f.Fd()), name, []byte(value), 0)
}

// setXattrIfAbsent sets name=value ONLY when the attribute is absent —
// the owner stamp's set-once discipline (a racing second creator loses
// the check and the first writer's owner stays; both writers already
// serialize on the per-key lock in Put).
func setXattrIfAbsent(path, name, value string) error {
	if _, err := getXattr(path, name); err == nil {
		return nil // present: set-once discipline, leave untouched
	} else if !isErrXattrNotFound(err) {
		return err // unreadable (vs absent): report, fail-open at caller
	}
	return setXattr(path, name, value)
}

// getXattr reads name from path, returning the value. ENODATA/ENOATTR
// (attribute absent) is reported as a *xattrNotFoundError so callers can
// distinguish "absent" from "unreadable".
func getXattr(path, name string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	size, err := unix.Fgetxattr(int(f.Fd()), name, nil)
	if err != nil {
		return "", err
	}
	buf := make([]byte, size)
	n, err := unix.Fgetxattr(int(f.Fd()), name, buf)
	if err != nil {
		return "", err
	}
	return string(buf[:n]), nil
}

// listXattrNames enumerates the extended attribute names on path
// (test-side integrity checks use it).
func listXattrNames(path string) ([]string, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	size := 256
	for {
		buf := make([]byte, size)
		n, err := unix.Flistxattr(int(f.Fd()), buf)
		if err == unix.ERANGE {
			size *= 2
			continue
		}
		if err != nil {
			return nil, err
		}
		if n == 0 {
			return nil, nil
		}
		var names []string
		for part := range strings.SplitSeq(string(buf[:n-1]), "\x00") {
			if part != "" {
				names = append(names, part)
			}
		}
		return names, nil
	}
}

// xattrNotFoundError marks ENODATA/ENOATTR — the attribute-absent class.
type xattrNotFoundError struct{ err error }

func (e *xattrNotFoundError) Error() string { return e.err.Error() }
func (e *xattrNotFoundError) Unwrap() error { return e.err }

// isErrXattrNotFound reports whether err is the attribute-absent class
// (ENODATA on Linux, ENOATTR on darwin — darwin's errno is the same value
// reached through the OSError's wrapped syscall error) or a missing file.
func isErrXattrNotFound(err error) bool {
	if _, ok := errors.AsType[*xattrNotFoundError](err); ok {
		return true
	}
	if errors.Is(err, unix.ENODATA) || errors.Is(err, os.ErrNotExist) {
		return true
	}
	// darwin: Fgetxattr reports ENOATTR ("attribute not found"), which has
	// no shared constant with Linux's ENODATA in x/sys/unix — match the
	// errno value/text to stay portable across both platforms without a
	// per-GOOS split.
	if errno, ok := errors.AsType[unix.Errno](err); ok {
		return errno == unix.ENODATA || errno.Error() == "attribute not found"
	}
	return false
}
