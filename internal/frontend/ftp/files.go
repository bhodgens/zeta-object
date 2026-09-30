// files.go — os.FileInfo adapters over the object model and the small
// predicate helpers shared by the driver. The FTP path model, error
// mapping, and grant enforcement entry points live in driver.go/errors.go.
package ftp

import (
	"errors"
	"os"
	"path"
	"time"

	"github.com/bhodgens/zeta-object/internal/auth"
	"github.com/bhodgens/zeta-object/internal/frontend"
	"github.com/bhodgens/zeta-object/internal/objectmodel"
)

// frontendAuthorize aliases the shared grant helper (leaf 05) so the
// driver reads uniformly.
func frontendAuthorize(id auth.Identity, bucket string, write bool) error {
	return frontend.AuthorizeRequest(id, bucket, write)
}

// asObjectModelError is a local errors.As wrapper for *objectmodel.Error.
func asObjectModelError(err error, target **objectmodel.Error) bool {
	return errors.As(err, target)
}

// errNoSuchKeySentinel is compared via errors.Is against the backend's
// NoSuchKey error value when available.
func errNoSuchKeySentinel() error { return objectmodel.ErrNoSuchKey("") }

// objectFileInfo adapts objectmodel.Object to os.FileInfo (LIST/MLSD facts:
// size, modify, regular-file type).
type objectFileInfo struct {
	obj  objectmodel.Object
	name string
}

func (fi objectFileInfo) Name() string { return path.Base(fi.name) }
func (fi objectFileInfo) Size() int64  { return fi.obj.Size }
func (fi objectFileInfo) Mode() os.FileMode {
	return 0o644
}
func (fi objectFileInfo) ModTime() time.Time { return fi.obj.LastModified }
func (fi objectFileInfo) IsDir() bool        { return false }
func (fi objectFileInfo) Sys() any           { return nil }

// dirFileInfo renders a virtual directory (bucket or common prefix).
type dirFileInfo struct{ name string }

func (fi dirFileInfo) Name() string { return path.Base("/" + fi.name) }
func (fi dirFileInfo) Size() int64  { return 0 }
func (fi dirFileInfo) Mode() os.FileMode {
	return os.ModeDir | 0o755
}
func (fi dirFileInfo) ModTime() time.Time { return time.Time{} }
func (fi dirFileInfo) IsDir() bool        { return true }
func (fi dirFileInfo) Sys() any           { return nil }
