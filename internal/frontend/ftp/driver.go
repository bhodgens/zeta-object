// driver.go — ftpserverlib MainDriver (server-level) + ClientDriver
// (per-session afero.Fs over Backend). This file is the Contract C mapping:
// LIST→List(Delimiter "/"), STOR→Put, RETR→Get, DELE→Delete, RMD→Delete of
// the "dir/" marker, MKD→Put of the zero-byte "dir/" marker,
// SIZE/MDTM/STAT→Stat. Path model: /bucket/path/to/key; CWD/CDUP are pure
// string resolution. Rename is Put-copy + Delete (no atomic-rename
// guarantee — Contract C); REST/seek is rejected (no Range mapping).
package ftp

import (
	"bytes"
	"context"
	"crypto/tls"
	"errors"
	"io"
	"os"
	"path"
	"strings"
	"time"

	ftpserver "github.com/fclairamb/ftpserverlib"
	afero "github.com/spf13/afero"

	"github.com/bhodgens/zeta-object/internal/auth"
	"github.com/bhodgens/zeta-object/internal/objectmodel"
)

// dirMarkerSuffix terminates directory marker keys (fs-layout convention).
const dirMarkerSuffix = "/"

// mainDriver implements ftpserver.MainDriver.
type mainDriver struct {
	f *Frontend
}

// GetSettings advertises the listener handed in via Serve, the optional
// passive-port range/public IP, and the TLS config for AUTH TLS (explicit).
func (m *mainDriver) GetSettings() (*ftpserver.Settings, error) {
	s := &ftpserver.Settings{
		Listener:   m.f.listener,
		ListenAddr: m.f.cfg.ListenAddr,
		// Explicit TLS only (AUTH TLS): control channel upgrades on demand;
		// never implicit TLS on the listener.
		TLSRequired: ftpserver.ClearOrEncrypted,
		// SITE has no object-model meaning (502-class degradation) and
		// active mode is disabled: passive-only data channels (the e2e and
		// interop clients all speak PASV/EPSV).
		DisableSite:       true,
		DisableActiveMode: true,
		Banner:            "zeta-object FTP",
	}
	if m.f.cfg.PassivePortMin > 0 {
		s.PassiveTransferPortRange = ftpserver.PortRange{
			Start: m.f.cfg.PassivePortMin,
			End:   m.f.cfg.PassivePortMax,
		}
	}
	if m.f.cfg.PublicIP != "" {
		s.PublicHost = m.f.cfg.PublicIP
	}
	return s, nil
}

// ClientConnected sends the welcome line.
func (m *mainDriver) ClientConnected(cc ftpserver.ClientContext) (string, error) {
	return "zeta-object FTP ready", nil
}

// ClientDisconnected is a no-op (per-session driver is garbage-collected).
func (m *mainDriver) ClientDisconnected(cc ftpserver.ClientContext) {}

// AuthUser authenticates USER/PASS through the PasswordVerifier seam. Any
// failure is ftpserverlib's 530. The resolved identity rides the returned
// ClientDriver for grant checks (leaf 05).
func (m *mainDriver) AuthUser(cc ftpserver.ClientContext, user, pass string) (ftpserver.ClientDriver, error) {
	id, ok := m.f.cfg.Verifier.Verify(user, pass)
	if !ok {
		return nil, errAuthFailed
	}
	return &clientDriver{f: m.f, id: id}, nil
}

// GetTLSConfig returns the explicit-TLS config (AUTH TLS). Nil when no cert
// is configured: ftpserverlib then answers AUTH TLS negatively and plain
// FTP keeps working (explicit, not implicit).
func (m *mainDriver) GetTLSConfig() (*tls.Config, error) {
	return m.f.cfg.TLSConfig, nil
}

// clientDriver implements ftpserver.ClientDriver (afero.Fs) per session,
// plus the ftpserverlib convenience extensions (file list, transfer
// handles, dir/file remove distinction). The identity resolved at login is
// carried for grant enforcement.
type clientDriver struct {
	f  *Frontend
	id auth.Identity
}

// compile-time: the driver is a full afero.Fs and the ftpserverlib
// extensions the wire commands need.
var (
	_ afero.Fs                                    = (*clientDriver)(nil)
	_ ftpserver.ClientDriverExtensionFileList     = (*clientDriver)(nil)
	_ ftpserver.ClientDriverExtentionFileTransfer = (*clientDriver)(nil)
	_ ftpserver.ClientDriverExtensionRemoveDir    = (*clientDriver)(nil)
)

// resolve maps an FTP path onto (bucket, key). Empty bucket = root.
func (d *clientDriver) resolve(p string) (bucket, key string) {
	return bucketOf(p)
}

// authorizeBucket applies the session identity's grants (leaf 05): zero
// Backend calls on denied paths — this runs before every storage touch.
func (d *clientDriver) authorizeBucket(bucket string, write bool) error {
	return frontendAuthorize(d.id, bucket, write)
}

// --- afero.Fs mapping ------------------------------------------------------

// Create is part of afero.Fs; ftpserverlib transfers go through GetHandle,
// but the interface requires it. Delegates to OpenFile.
func (d *clientDriver) Create(name string) (afero.File, error) {
	return d.OpenFile(name, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o644)
}

// Mkdir creates the zero-byte "dir/" marker (Contract C MKD row). The flat
// object namespace needs no parents.
func (d *clientDriver) Mkdir(name string, perm os.FileMode) error {
	bucket, key := d.resolve(name)
	if bucket == "" || key == "" {
		return &ftpError{code: 550, message: "Cannot create directory here (buckets are created via the S3 API)"}
	}
	if err := d.authorizeBucket(bucket, true); err != nil {
		return mapBackendError(err)
	}
	_, err := d.f.be.Put(context.Background(), bucket, key+dirMarkerSuffix,
		bytes.NewReader(nil), 0, objectmodel.PutOptions{})
	if err != nil {
		return mapBackendError(err)
	}
	return nil
}

// MkdirAll: flat namespace — identical to Mkdir of the final segment.
func (d *clientDriver) MkdirAll(p string, perm os.FileMode) error {
	return d.Mkdir(p, perm)
}

// Open opens a file for reading (RETR).
func (d *clientDriver) Open(name string) (afero.File, error) {
	return d.OpenFile(name, os.O_RDONLY, 0)
}

// OpenFile is the storage-touching core for RETR/STOR directions; REST
// (offset) arrives through GetHandle and is rejected there.
func (d *clientDriver) OpenFile(name string, flag int, perm os.FileMode) (afero.File, error) {
	bucket, key := d.resolve(name)
	if bucket == "" || key == "" || strings.HasSuffix(key, dirMarkerSuffix) {
		return nil, &ftpError{code: 550, message: "Not a regular file"}
	}
	write := flag&(os.O_WRONLY|os.O_CREATE|os.O_RDWR) != 0
	if err := d.authorizeBucket(bucket, write); err != nil {
		return nil, mapBackendError(err)
	}
	if write {
		wf := &writeFile{
			d: d, bucket: bucket, key: key,
			append: flag&os.O_APPEND != 0,
		}
		wf.name = path.Base("/" + key)
		return wf, nil
	}
	ctx := context.Background()
	rc, obj, err := d.f.be.Get(ctx, bucket, key, objectmodel.GetOptions{})
	if err != nil {
		return nil, mapBackendError(err)
	}
	rf := &readFile{rc: rc, obj: obj}
	rf.name = path.Base("/" + key)
	return rf, nil
}

// Remove deletes an object (DELE). A "dir/" marker key is NOT removable via
// DELE (RMD owns directories — ftpserverlib routes by extension).
func (d *clientDriver) Remove(name string) error {
	bucket, key := d.resolve(name)
	if bucket == "" || key == "" {
		return &ftpError{code: 550, message: "No such file"}
	}
	if err := d.authorizeBucket(bucket, true); err != nil {
		return mapBackendError(err)
	}
	if err := d.f.be.Delete(context.Background(), bucket, key); err != nil {
		return mapBackendError(err)
	}
	return nil
}

// RemoveDir removes a directory: Delete on the "dir/" marker key; a missing
// marker is NoSuchKey → 550 (Contract C RMD row). ftpserverlib routes RMD
// here and DELE to Remove via the RemoveDir extension.
func (d *clientDriver) RemoveDir(name string) error {
	bucket, key := d.resolve(name)
	if bucket == "" || key == "" {
		return &ftpError{code: 550, message: "Cannot remove that directory"}
	}
	if err := d.authorizeBucket(bucket, true); err != nil {
		return mapBackendError(err)
	}
	if err := d.f.be.Delete(context.Background(), bucket, key+dirMarkerSuffix); err != nil {
		return mapBackendError(err)
	}
	return nil
}

// RemoveAll: recursive delete is NOT expressible (Delete is per-key and
// List-delete loops would be silent emulation of a directory semantic).
// Rejected — clients delete file-by-file (matches the WebDAV frontend's
// no-silent-emulation rule; plain RFC 959 clients use DELE per file).
func (d *clientDriver) RemoveAll(p string) error {
	return &ftpError{code: 502, message: "Recursive delete not implemented: delete files individually"}
}

// Rename implements RNFR/RNTO as Get + Put + Delete (no atomic-rename
// guarantee — Contract C). Directories (dir/ markers) rename their marker.
func (d *clientDriver) Rename(oldname, newname string) error {
	oldB, oldK := d.resolve(oldname)
	newB, newK := d.resolve(newname)
	if oldB == "" || oldK == "" || newB == "" || newK == "" {
		return &ftpError{code: 550, message: "Rename needs both source and destination inside a bucket"}
	}
	if err := d.authorizeBucket(oldB, true); err != nil {
		return mapBackendError(err)
	}
	if err := d.authorizeBucket(newB, true); err != nil {
		return mapBackendError(err)
	}
	ctx := context.Background()
	srcKey := oldK
	if strings.HasSuffix(oldK, dirMarkerSuffix) {
		srcKey = oldK // already marker form via RMD-style paths
	}
	rc, _, err := d.f.be.Get(ctx, oldB, srcKey, objectmodel.GetOptions{})
	if err != nil {
		return mapBackendError(err)
	}
	defer rc.Close()
	data, err := io.ReadAll(rc)
	if err != nil {
		return mapBackendError(err)
	}
	if _, err := d.f.be.Put(ctx, newB, newK+dirMarkerSuffixIfDir(oldK), bytes.NewReader(data), int64(len(data)), objectmodel.PutOptions{}); err != nil {
		return mapBackendError(err)
	}
	if err := d.f.be.Delete(ctx, oldB, srcKey); err != nil {
		return mapBackendError(err)
	}
	return nil
}

// dirMarkerSuffixIfDir appends the marker suffix when the source key was a
// directory marker (RNTO of a directory renames the marker).
func dirMarkerSuffixIfDir(key string) string {
	if strings.HasSuffix(key, dirMarkerSuffix) {
		return dirMarkerSuffix
	}
	return ""
}

// Stat implements SIZE/MDTM/STAT facts: bucket root and common prefixes
// render as directories; objects via backend.Stat (missing → NoSuchKey →
// 550 by the reply mapping).
func (d *clientDriver) Stat(name string) (os.FileInfo, error) {
	name = strings.TrimSuffix(name, dirMarkerSuffix)
	if name == "" || name == "/" || name == "." {
		return dirFileInfo{name: "/"}, nil
	}
	bucket, key := d.resolve(name)
	if bucket == "" {
		return nil, &ftpError{code: 550, message: "No such file or directory"}
	}
	// Grant gate BEFORE any storage touch (leaf 05: zero Backend calls on
	// denied paths — existence probing would leak bucket existence).
	if err := d.authorizeBucket(bucket, false); err != nil {
		return nil, mapBackendError(err)
	}
	if key == "" {
		// Bucket root: exists iff the bucket lists (or Stats its marker).
		if _, err := d.f.be.Stat(context.Background(), bucket, dirMarkerSuffix); err == nil {
			return dirFileInfo{name: bucket}, nil
		}
		if _, err := d.f.be.List(context.Background(), bucket, objectmodel.ListParams{Delimiter: dirMarkerSuffix, MaxKeys: 1}); err == nil {
			return dirFileInfo{name: bucket}, nil
		}
		return nil, mapBackendError(objectmodel.ErrNoSuchBucket(bucket))
	}
	if obj, err := d.statMarkerFirst(context.Background(), bucket, key); err == nil {
		fi := objectFileInfo{obj: obj, name: key}
		if strings.HasSuffix(obj.Key, dirMarkerSuffix) || obj.Size == 0 && strings.HasSuffix(obj.Key, dirMarkerSuffix) {
			return dirFileInfo{name: key}, nil
		}
		return fi, nil
	}
	// Virtual directory: exists iff anything lists under prefix.
	page, err := d.f.be.List(context.Background(), bucket, objectmodel.ListParams{
		Prefix:    key + dirMarkerSuffix,
		Delimiter: dirMarkerSuffix,
		MaxKeys:   1,
	})
	if err == nil && (len(page.Objects) > 0 || len(page.CommonPrefixes) > 0) {
		return dirFileInfo{name: key}, nil
	}
	return nil, &ftpError{code: 550, message: "No such file or directory"}
}

// statMarkerFirst tries the exact key, then the dir-marker form (a Stat of
// "/bkt/dir" must find the "dir/" marker).
func (d *clientDriver) statMarkerFirst(ctx context.Context, bucket, key string) (objectmodel.Object, error) {
	obj, err := d.f.be.Stat(ctx, bucket, key)
	if err == nil {
		return obj, nil
	}
	return d.f.be.Stat(ctx, bucket, key+dirMarkerSuffix)
}

// Name names this filesystem (afero.Fs boilerplate).
func (d *clientDriver) Name() string { return "zeta-object-ftp" }

// Chmod/Chown/Chtimes have no object-model meaning (caps: no versioning,
// no metadata writes) → 502-class rejection, never silent success.
func (d *clientDriver) Chmod(name string, mode os.FileMode) error {
	return &ftpError{code: 502, message: "Command not implemented for this capability set"}
}
func (d *clientDriver) Chown(name string, uid, gid int) error {
	return &ftpError{code: 502, message: "Command not implemented for this capability set"}
}
func (d *clientDriver) Chtimes(name string, atime, mtime time.Time) error {
	return &ftpError{code: 502, message: "Command not implemented for this capability set"}
}

// ReadDir implements ftpserver.ClientDriverExtensionFileList: LIST/NLST/MLSD
// entries. Root lists buckets; inside a bucket, List(Prefix, Delimiter "/")
// renders objects as files and common prefixes as directories.
func (d *clientDriver) ReadDir(name string) ([]os.FileInfo, error) {
	name = strings.TrimSuffix(name, dirMarkerSuffix)
	if name == "" || name == "/" || name == "." {
		buckets, err := d.f.be.Buckets(context.Background())
		if err != nil {
			return nil, mapBackendError(err)
		}
		out := make([]os.FileInfo, 0, len(buckets))
		for _, b := range buckets {
			// Grant-filtered bucket listing (leaf 05: read visibility).
			if !d.id.CanRead(b.Name) {
				continue
			}
			out = append(out, dirFileInfo{name: b.Name})
		}
		return out, nil
	}
	bucket, key := d.resolve(name)
	if bucket == "" {
		return nil, &ftpError{code: 550, message: "No such directory"}
	}
	if err := d.authorizeBucket(bucket, false); err != nil {
		return nil, mapBackendError(err)
	}
	prefix := ""
	if key != "" {
		prefix = key + dirMarkerSuffix
	}
	page, err := d.f.be.List(context.Background(), bucket, objectmodel.ListParams{
		Prefix:    prefix,
		Delimiter: dirMarkerSuffix,
	})
	if err != nil {
		return nil, mapBackendError(err)
	}
	out := make([]os.FileInfo, 0, len(page.Objects)+len(page.CommonPrefixes))
	for _, obj := range page.Objects {
		trimmed := strings.TrimPrefix(obj.Key, prefix)
		if trimmed == "" {
			continue // the directory's own marker
		}
		if strings.HasSuffix(trimmed, dirMarkerSuffix) {
			continue // dir markers render via common prefixes
		}
		out = append(out, objectFileInfo{obj: obj, name: trimmed})
	}
	for _, cp := range page.CommonPrefixes {
		trimmed := strings.TrimSuffix(strings.TrimPrefix(cp, prefix), dirMarkerSuffix)
		if trimmed == "" {
			continue
		}
		out = append(out, dirFileInfo{name: trimmed})
	}
	return out, nil
}

// GetHandle implements ftpserver.ClientDriverExtentionFileTransfer: the
// upload/download fast path. offset > 0 is a REST (restart) — rejected
// (551-style): no Range mapping per the capability set (Contract C).
func (d *clientDriver) GetHandle(name string, flags int, offset int64) (ftpserver.FileTransfer, error) {
	if offset != 0 {
		return nil, &ftpError{code: 551, message: "Restart (REST) not supported: no partial transfer mapping"}
	}
	return d.OpenFile(name, flags, 0o644)
}

// --- file handles -----------------------------------------------------------

// fileBase carries the afero.File boilerplate shared by both directions.
type fileBase struct{ name string }

func (f *fileBase) Name() string              { return f.name }
func (f *fileBase) Sync() error               { return nil }
func (f *fileBase) Truncate(size int64) error { return errNotImplemented }
func (f *fileBase) WriteString(s string) (int, error) {
	return 0, errNotImplemented
}
func (f *fileBase) Readdir(int) ([]os.FileInfo, error) {
	return nil, errNotImplemented
}
func (f *fileBase) Readdirnames(int) ([]string, error) {
	return nil, errNotImplemented
}

// errNotImplemented is the shared 502-class degradation error.
var errNotImplemented = &ftpError{code: 502, message: "Command not implemented for this capability set"}

// readFile wraps a Get reader as an afero.File (RETR).
type readFile struct {
	fileBase
	rc  io.ReadCloser
	obj objectmodel.Object
}

func (f *readFile) Read(p []byte) (int, error) { return f.rc.Read(p) }
func (f *readFile) ReadAt(p []byte, off int64) (int, error) {
	return 0, errNotImplemented
}

// Seek is rejected: RETR has no Range mapping (ConditionalReads: false).
func (f *readFile) Seek(offset int64, whence int) (int64, error) {
	return 0, &ftpError{code: 551, message: "Seek/resume not supported"}
}
func (f *readFile) Stat() (os.FileInfo, error) { return objectFileInfo{f.obj, f.name}, nil }
func (f *readFile) Write([]byte) (int, error)  { return 0, errNotImplemented }
func (f *readFile) WriteAt([]byte, int64) (int, error) {
	return 0, errNotImplemented
}
func (f *readFile) Close() error { return f.rc.Close() }

// writeFile buffers an STOR upload and issues ONE Put on Close with the
// exact byte count (single-shot; no multipart emulation — Contract C).
// The buffer is bounded by the transfer itself: FTP carries no size hint,
// so an in-memory buffer is the honest v1 tradeoff (documented; object
// sizes beyond memory will fail with a resource error, never a truncated
// silent success).
type writeFile struct {
	fileBase
	d      *clientDriver
	bucket string
	key    string
	buf    bytes.Buffer
	append bool // APPE: existing bytes are fetched first (still one Put)
	filled bool
	// aborted records an interrupted transfer (ftpserver.FileTransferError):
	// Close must then refuse to Put the partial buffer over the stored object.
	aborted     bool
	transferErr error // first TransferError cause (nil → generic 451)
}

// Write accumulates upload bytes.
func (f *writeFile) Write(p []byte) (int, error) {
	if !f.append {
		return f.buf.Write(p)
	}
	if !f.filled {
		// APPE: prepend the existing object (Get + re-Put whole object).
		f.filled = true
		if err := f.prefetch(); err != nil {
			return 0, err
		}
	}
	return f.buf.Write(p)
}

// prefetch loads the existing object into the buffer for APPE.
func (f *writeFile) prefetch() error {
	rc, _, err := f.d.f.be.Get(context.Background(), f.bucket, f.key, objectmodel.GetOptions{})
	if err != nil {
		if errors.Is(err, errNoSuchKeySentinel()) {
			return nil // new file
		}
		var oe *objectmodel.Error
		if asObjectModelError(err, &oe) && oe.Code == objectmodel.CodeNoSuchKey {
			return nil // new file
		}
		return mapBackendError(err)
	}
	defer rc.Close()
	_, err = io.Copy(&f.buf, rc)
	return mapBackendError(err)
}

// WriteAt: ftpserverlib writes sequentially; random-access writes are not
// representable (an out-of-seek write would silently corrupt ordering).
func (f *writeFile) WriteAt(p []byte, off int64) (int, error) {
	if off != int64(f.buf.Len()) {
		return 0, &ftpError{code: 551, message: "Random-access write not supported"}
	}
	return f.Write(p)
}

func (f *writeFile) Read([]byte) (int, error)          { return 0, errNotImplemented }
func (f *writeFile) ReadAt([]byte, int64) (int, error) { return 0, errNotImplemented }
func (f *writeFile) Seek(int64, int) (int64, error) {
	return 0, errNotImplemented
}
func (f *writeFile) Stat() (os.FileInfo, error) {
	return dirFileInfo{name: f.name}, nil // size not final until Close
}

// TransferError implements ftpserver.FileTransferError: ftpserverlib calls
// it BEFORE Close whenever a STOR did not complete (ABOR, dropped data
// connection, mid-copy I/O error — handle_files.go:103 and :162 of
// v0.32.4). The first error and the aborted flag are recorded so Close can
// refuse to commit a partial body over a good object.
func (f *writeFile) TransferError(err error) {
	if f.aborted {
		return // keep the first (root-cause) error
	}
	f.aborted = true
	f.transferErr = err
}

// Close commits the buffered upload as ONE Put with the exact size
// (streaming contract: the size parameter always matches the payload).
// An ABORTED transfer never commits: no Put is issued, so the previously
// stored object (if any) stays byte-intact, and the stored transfer error
// (as an ftpError so the reply mapping keeps its 4xx/5xx class) is returned
// so ftpserverlib reports failure. A clean Close still commits.
func (f *writeFile) Close() error {
	if f.aborted {
		if f.transferErr != nil {
			if fe, ok := errors.AsType[*ftpError](f.transferErr); ok {
				return fe
			}
			return &ftpError{code: 451, message: "Transfer aborted: local error in processing (" + f.transferErr.Error() + ")"}
		}
		return &ftpError{code: 451, message: "Transfer aborted"}
	}
	payload := f.buf.Bytes()
	_, err := f.d.f.be.Put(context.Background(), f.bucket, f.key,
		bytes.NewReader(payload), int64(len(payload)), objectmodel.PutOptions{})
	if err != nil {
		return mapBackendError(err)
	}
	return nil
}

// compile-time: file handles satisfy the afero.File shape ftpserverlib
// consumes through GetHandle/OpenFile.
var (
	_ os.FileInfo            = objectFileInfo{}
	_ os.FileInfo            = dirFileInfo{}
	_ afero.File             = (*readFile)(nil)
	_ afero.File             = (*writeFile)(nil)
	_ ftpserver.FileTransfer = (*readFile)(nil)
	_ ftpserver.FileTransfer = (*writeFile)(nil)
	// F1: aborted transfers are notified via TransferError before Close
	// (ftpserverlib v0.32.4 handle_files.go:103/:162), letting Close refuse
	// to commit the partial buffer over the stored object.
	_ ftpserver.FileTransferError = (*writeFile)(nil)
)
