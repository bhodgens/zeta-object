// driver.go — the sftp.Handlers triple (FileGet/FilePut/FileCmd/FileList)
// over Backend: Contract C. Path model /bucket/path/to/key; "/" lists
// buckets as directories; mkdir/rmdir are "dir/" marker Put/Delete; stat →
// Stat (+ marker fallback); rename = Get+Put+Delete (no atomicity
// guarantee); symlink/readlink → SSH_FX_OP_UNSUPPORTED; setstat/fsetstat →
// SSH_FX_PERMISSION_DENIED (no object-model meaning).
package sftp

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path"
	"strings"
	"time"

	"github.com/pkg/sftp"

	"github.com/bhodgens/zeta-object/internal/auth"
	"github.com/bhodgens/zeta-object/internal/objectmodel"
)

// dirMarkerSuffix terminates directory marker keys (fs-layout convention).
const dirMarkerSuffix = "/"

// maxUploadBufferBytes bounds the in-memory upload/download buffers (T7):
// the fs backend caps a Put at 5 GiB, but that check runs at Close — the
// whole body is buffered HERE first, so without this cap an authenticated
// client can OOM the process before the backend ever sees the bytes.
// Matches the backend default (maxPutBytesDefault).
const maxUploadBufferBytes = 5 << 30

// handlers builds the sftp.Handlers triple bound to one session identity
// (leaf 05: grant checks run before every storage touch — zero Backend
// calls on denied paths).
func (f *Frontend) handlers(id auth.Identity) sftp.Handlers {
	return sftp.Handlers{
		FileGet:  &fileGet{f: f, id: id},
		FilePut:  &filePut{f: f, id: id},
		FileCmd:  &fileCmd{f: f, id: id},
		FileList: &fileList{f: f, id: id},
	}
}

// authorize applies grants before I/O (shared helper shape with the ftp
// frontend; frontend.AuthorizeRequest is THE decision point).
func authorize(id auth.Identity, bucket string, write bool) error {
	return frontendAuthorize(id, bucket, write)
}

// --- FileGet (Get / reads) --------------------------------------------------

type fileGet struct {
	f  *Frontend
	id auth.Identity
}

// Fileread returns an io.ReaderAt for the path. SFTP reads are ReaderAt-
// shaped; the object is small-buffered whole (v1: Put/Get are single-shot)
// and served from memory — byte-exact, never size-lying. Conditional reads
// have no SFTP expression (caps: ConditionalReads false).
func (g *fileGet) Fileread(req *sftp.Request) (io.ReaderAt, error) {
	bucket, key := splitPath(req.Filepath)
	if bucket == "" || key == "" {
		return nil, errNoSuchFile
	}
	if err := authorize(g.id, bucket, false); err != nil {
		return nil, errPermission
	}
	rc, _, err := g.f.be.Get(context.Background(), bucket, key, objectmodel.GetOptions{})
	if err != nil {
		return nil, mapBackendError(err)
	}
	defer rc.Close()
	// T7: cap the materialized read (download path mirrors the upload cap).
	data, err := io.ReadAll(io.LimitReader(rc, maxUploadBufferBytes+1))
	if err != nil {
		return nil, mapBackendError(err)
	}
	if int64(len(data)) > maxUploadBufferBytes {
		return nil, sftp.ErrSSHFxFailure
	}
	return bytes.NewReader(data), nil
}

// --- FilePut (Put / writes) -------------------------------------------------

type filePut struct {
	f  *Frontend
	id auth.Identity
}

// Filewrite returns an io.WriterAt. SFTP clients write at arbitrary
// offsets without announcing size, so writes buffer (bounded by the
// client's transfer) and commit as ONE Put with the exact final size on
// Close — the backend's size parameter always matches the payload
// (streaming contract; never lie about size).
func (p *filePut) Filewrite(req *sftp.Request) (io.WriterAt, error) {
	bucket, key := splitPath(req.Filepath)
	if bucket == "" || key == "" || strings.HasSuffix(key, dirMarkerSuffix) {
		return nil, errNoSuchFile
	}
	if err := authorize(p.id, bucket, true); err != nil {
		return nil, errPermission
	}
	return &putWriter{f: p.f, bucket: bucket, key: key}, nil
}

// putWriter buffers an upload; Close commits one Put.
//
// Commit guard: Close only Puts when at least one WriteAt SUCCEEDED and the
// transfer was NOT aborted. A zero-write close (aborted APPE / new upload —
// the client opened the handle and closed it without transferring) must NOT
// store an empty object or clobber the previous one, so it is a silent no-op.
//
// Abort detection (T2, closes the partial-commit residual): the writer
// implements sftp.TransferError. pkg/sftp v1.13.11 RequestServer.Serve
// (request-server.go:204-217) sweeps every still-open handle when the
// connection drops: it calls TransferError(err) — io.ErrUnexpectedEOF for a
// dropped peer, the underlying transport error otherwise — BEFORE calling
// Close. A CLEAN close (SSH_FXP_CLOSE) removes the handle from the open
// table first (closeRequest), so TransferError never fires on the success
// path. Abort → Close commits NOTHING: the previous object keeps its bytes.
type putWriter struct {
	f       *Frontend
	bucket  string
	key     string
	buf     bytes.Buffer
	writes  bool  // at least one WriteAt succeeded
	aborted error // non-nil once TransferError fired (nil = clean)
}

// compile-time: the writer receives pkg/sftp's transfer-error notification.
var _ sftp.TransferError = (*putWriter)(nil)

// TransferError records the connection-drop cause. pkg/sftp calls this
// exactly once per open handle during Serve's end-of-stream sweep, before
// Close, with all worker goroutines already joined (wg.Wait precedes the
// sweep), so no extra synchronization against WriteAt is needed.
func (w *putWriter) TransferError(err error) {
	w.aborted = err
}

func (w *putWriter) WriteAt(p []byte, off int64) (int, error) {
	if off != int64(w.buf.Len()) {
		return 0, sftp.ErrSSHFxOpUnsupported // random-access writes are not representable
	}
	if int64(w.buf.Len())+int64(len(p)) > maxUploadBufferBytes {
		// T7: refuse the growth instead of OOMing the process; the close
		// then commits nothing for a fresh handle (writes flag is set only
		// on success) — an existing object keeps its bytes.
		return 0, sftp.ErrSSHFxFailure
	}
	n, err := w.buf.Write(p)
	if err == nil {
		w.writes = true
	}
	return n, err
}

func (w *putWriter) Close() error {
	if w.aborted != nil {
		// T2: the connection dropped mid-transfer. Commit NOTHING —
		// an existing object keeps its exact bytes (a dropped overwrite
		// no longer stores the buffered prefix).
		return mapBackendError(w.aborted)
	}
	if !w.writes {
		// Abort semantics: no write ever succeeded, so commit nothing —
		// an existing object keeps its exact bytes and no empty object
		// is created.
		return nil
	}
	payload := w.buf.Bytes()
	_, err := w.f.be.Put(context.Background(), w.bucket, w.key,
		bytes.NewReader(payload), int64(len(payload)), objectmodel.PutOptions{})
	return mapBackendError(err)
}

// --- FileCmd (Setstat/Rename/Rmdir/Mkdir/Link/Symlink/Remove) ---------------

type fileCmd struct {
	f  *Frontend
	id auth.Identity
}

// Filecmd routes the command methods (Contract C rows).
func (c *fileCmd) Filecmd(req *sftp.Request) error {
	switch req.Method {
	case "Setstat":
		// chmod/chown/utimes have no object-model meaning → PERMISSION_DENIED
		// (Contract D: rejection, never silent success).
		return errPermission
	case "Rename":
		return c.rename(req.Filepath, req.Target)
	case "Rmdir":
		return c.rmdir(req.Filepath)
	case "Mkdir":
		return c.mkdir(req.Filepath)
	case "Link":
		return errUnsupported // hard links: no object-model meaning
	case "Symlink":
		return errUnsupported // Contract C: object model has no links
	case "Remove":
		return c.remove(req.Filepath)
	default:
		return errUnsupported
	}
}

// mkdir writes the zero-byte "dir/" marker.
func (c *fileCmd) mkdir(p string) error {
	bucket, key := splitPath(p)
	if bucket == "" || key == "" {
		return errPermission // bucket creation is S3-API-only
	}
	if err := authorize(c.id, bucket, true); err != nil {
		return errPermission
	}
	_, err := c.f.be.Put(context.Background(), bucket, key+dirMarkerSuffix,
		bytes.NewReader(nil), 0, objectmodel.PutOptions{})
	return mapBackendError(err)
}

// rmdir deletes the "dir/" marker; missing marker → SSH_FX_NO_SUCH_FILE.
func (c *fileCmd) rmdir(p string) error {
	bucket, key := splitPath(p)
	if bucket == "" || key == "" {
		return errPermission
	}
	if err := authorize(c.id, bucket, true); err != nil {
		return errPermission
	}
	if err := c.f.be.Delete(context.Background(), bucket, key+dirMarkerSuffix); err != nil {
		return mapBackendError(err)
	}
	return nil
}

// remove deletes an object key.
func (c *fileCmd) remove(p string) error {
	bucket, key := splitPath(p)
	if bucket == "" || key == "" {
		return errNoSuchFile
	}
	if err := authorize(c.id, bucket, true); err != nil {
		return errPermission
	}
	if err := c.f.be.Delete(context.Background(), bucket, key); err != nil {
		return mapBackendError(err)
	}
	return nil
}

// rename = Get + Put + Delete (Contract C RNTO row: no atomic-rename
// guarantee; directory markers rename their marker form).
//
// Safety guards (SFTP v3 rename semantics — RENAME must not clobber):
//   - same key: source and destination resolve to the same bucket+key →
//     idempotent success WITHOUT touching storage (no Put, no Delete).
//     Without this guard the Put-then-Delete sequence would delete the
//     object outright.
//   - existing destination: Stat first; an existing destination key fails
//     with SSH_FX_FAILURE and NOTHING is touched (no overwrite, SFTP v3
//     has no implicit clobber — clients use posix-rename@openssh.com for
//     that, which this server does not offer).
func (c *fileCmd) rename(oldPath, newPath string) error {
	oldB, oldK := splitPath(oldPath)
	newB, newK := splitPath(newPath)
	if oldB == "" || oldK == "" || newB == "" || newK == "" {
		return errPermission
	}
	if err := authorize(c.id, oldB, true); err != nil {
		return errPermission
	}
	if err := authorize(c.id, newB, true); err != nil {
		return errPermission
	}
	// F2 twin: a directory source's marker key ("d/") is what must move.
	// The client's path never carries the trailing slash (SFTP normalizes
	// before this handler sees it), so detect directory-ness from STORAGE
	// like the FTP driver does. A marker-only directory renames as an
	// empty body plus the marker suffix on the destination.
	srcKey := oldK
	ctx := context.Background()
	if _, err := c.f.be.Stat(ctx, oldB, oldK+dirMarkerSuffix); err == nil {
		srcKey = oldK + dirMarkerSuffix
	}
	dstKey := newK + dirMarkerSuffixIfDir(srcKey)
	if oldB == newB && srcKey == dstKey {
		// Same bucket+key: a no-op. Must precede the destination Stat so
		// the source's own existence does not read as "clobber attempt".
		return nil
	}
	if _, err := c.f.be.Stat(ctx, newB, dstKey); err == nil {
		// Destination exists: refuse without touching anything (no
		// overwrite — SFTP v3 rename must not clobber).
		return errFailure
	} else if !isNoSuchKey(err) {
		return mapBackendError(err)
	}
	var data []byte
	rc, _, err := c.f.be.Get(ctx, oldB, srcKey, objectmodel.GetOptions{})
	if err != nil {
		if !strings.HasSuffix(srcKey, dirMarkerSuffix) {
			return mapBackendError(err)
		}
		// Marker-only directory: empty body, marker rename.
		data = nil
	} else {
		data, err = io.ReadAll(rc)
		if cerr := rc.Close(); cerr != nil {
			return mapBackendError(cerr)
		}
		if err != nil {
			return mapBackendError(err)
		}
	}
	if _, err := c.f.be.Put(ctx, newB, dstKey, bytes.NewReader(data), int64(len(data)), objectmodel.PutOptions{}); err != nil {
		return mapBackendError(err)
	}
	if err := c.f.be.Delete(ctx, oldB, srcKey); err != nil {
		return mapBackendError(err)
	}
	return nil
}

// dirMarkerSuffixIfDir appends the marker suffix when the source key was a
// directory marker.
func dirMarkerSuffixIfDir(key string) string {
	if strings.HasSuffix(key, dirMarkerSuffix) {
		return dirMarkerSuffix
	}
	return ""
}

// isNoSuchKey reports whether err is the backend's miss error (used by
// rename's destination probe: "not there" is the expected good path).
func isNoSuchKey(err error) bool {
	if err == nil {
		return false
	}
	var oe *objectmodel.Error
	return errors.As(err, &oe) &&
		(oe.Code == objectmodel.CodeNoSuchKey || oe.Code == objectmodel.CodeNoSuchBucket)
}

// --- FileList (List / Stat / Readlink) --------------------------------------

type fileList struct {
	f  *Frontend
	id auth.Identity
}

// Filelist returns a ListerAt for List ("readdir"), Stat, and Readlink
// methods. Readlink is rejected (SSH_FX_OP_UNSUPPORTED — no links).
func (l *fileList) Filelist(req *sftp.Request) (sftp.ListerAt, error) {
	switch req.Method {
	case "List":
		return l.listDir(req.Filepath)
	case "Stat":
		return l.stat(req.Filepath)
	case "Readlink":
		return nil, errUnsupported
	default:
		return nil, errUnsupported
	}
}

// listDir renders a directory listing: root → buckets; inside a bucket →
// List(Prefix, Delimiter "/") with common prefixes as directories.
func (l *fileList) listDir(p string) (sftp.ListerAt, error) {
	clean := strings.TrimSuffix(p, dirMarkerSuffix)
	if clean == "" || clean == "/" || clean == "." {
		if err := authorizeRoot(l.id); err != nil {
			return nil, errPermission
		}
		buckets, err := l.f.be.Buckets(context.Background())
		if err != nil {
			return nil, mapBackendError(err)
		}
		var infos []os.FileInfo
		for _, b := range buckets {
			if !l.id.CanRead(b.Name) {
				continue // grant-filtered visibility (leaf 05)
			}
			infos = append(infos, dirFileInfo{name: b.Name, modTime: b.CreatedAt})
		}
		return &lister{infos: infos}, nil
	}
	bucket, key := splitPath(clean)
	if bucket == "" {
		return nil, errNoSuchFile
	}
	if err := authorize(l.id, bucket, false); err != nil {
		return nil, errPermission
	}
	prefix := ""
	if key != "" {
		prefix = key + dirMarkerSuffix
	}
	page, err := l.f.be.List(context.Background(), bucket, objectmodel.ListParams{
		Prefix:    prefix,
		Delimiter: dirMarkerSuffix,
	})
	if err != nil {
		return nil, mapBackendError(err)
	}
	var infos []os.FileInfo
	for _, obj := range page.Objects {
		trimmed := strings.TrimPrefix(obj.Key, prefix)
		if trimmed == "" || strings.HasSuffix(trimmed, dirMarkerSuffix) {
			continue // the dir's own marker / markers render as prefixes
		}
		infos = append(infos, objectFileInfo{obj: obj, name: trimmed})
	}
	for _, cp := range page.CommonPrefixes {
		trimmed := strings.TrimSuffix(strings.TrimPrefix(cp, prefix), dirMarkerSuffix)
		if trimmed == "" {
			continue
		}
		infos = append(infos, dirFileInfo{name: trimmed})
	}
	return &lister{infos: infos}, nil
}

// stat resolves Stat via the exact key, then the dir-marker form, then a
// virtual-directory prefix probe (mirrors the ftp frontend).
func (l *fileList) stat(p string) (sftp.ListerAt, error) {
	clean := strings.TrimSuffix(p, dirMarkerSuffix)
	if clean == "" || clean == "/" || clean == "." {
		return &lister{infos: []os.FileInfo{dirFileInfo{name: "/"}}}, nil
	}
	bucket, key := splitPath(clean)
	if bucket == "" {
		return nil, errNoSuchFile
	}
	if err := authorize(l.id, bucket, false); err != nil {
		return nil, errPermission
	}
	ctx := context.Background()
	if key == "" {
		// Bucket root: exists iff the bucket lists.
		if _, err := l.f.be.List(ctx, bucket, objectmodel.ListParams{Delimiter: dirMarkerSuffix, MaxKeys: 1}); err == nil {
			return &lister{infos: []os.FileInfo{dirFileInfo{name: bucket}}}, nil
		}
		return nil, mapBackendError(objectmodel.ErrNoSuchBucket(bucket))
	}
	if obj, err := l.f.be.Stat(ctx, bucket, key); err == nil {
		return &lister{infos: []os.FileInfo{objectFileInfo{obj: obj, name: key}}}, nil
	}
	if obj, err := l.f.be.Stat(ctx, bucket, key+dirMarkerSuffix); err == nil {
		return &lister{infos: []os.FileInfo{dirFileInfo{name: key, modTime: obj.LastModified}}}, nil
	}
	page, err := l.f.be.List(ctx, bucket, objectmodel.ListParams{
		Prefix:    key + dirMarkerSuffix,
		Delimiter: dirMarkerSuffix,
		MaxKeys:   1,
	})
	if err == nil && (len(page.Objects) > 0 || len(page.CommonPrefixes) > 0) {
		return &lister{infos: []os.FileInfo{dirFileInfo{name: key}}}, nil
	}
	return nil, errNoSuchFile
}

// authorizeRoot: listing buckets requires *some* identity (the session is
// authenticated); per-bucket filtering happens in listDir.
func authorizeRoot(id auth.Identity) error {
	if id.AccessKeyID == "" {
		return errPermission
	}
	return nil
}

// lister adapts []os.FileInfo to sftp.ListerAt.
type lister struct {
	infos []os.FileInfo
	pos   int64
}

// ListAt populates dest; returns io.EOF when exhausted (sftp contract).
func (li *lister) ListAt(dest []os.FileInfo, _ int64) (int, error) {
	n := 0
	for n < len(dest) && int(li.pos) < len(li.infos) {
		dest[n] = li.infos[li.pos]
		li.pos++
		n++
	}
	if n == 0 && int(li.pos) >= len(li.infos) && len(li.infos) > 0 {
		return 0, io.EOF
	}
	if n < len(dest) && len(li.infos) == 0 {
		return n, io.EOF
	}
	if int(li.pos) >= len(li.infos) {
		return n, io.EOF
	}
	return n, nil
}

// splitPath maps an SFTP path onto (bucket, key).
func splitPath(p string) (bucket, key string) { return bucketOf(p) }

// --- FileInfo adapters ------------------------------------------------------

// objectFileInfo adapts objectmodel.Object to os.FileInfo.
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
type dirFileInfo struct {
	name    string
	modTime time.Time
}

func (fi dirFileInfo) Name() string { return path.Base("/" + fi.name) }
func (fi dirFileInfo) Size() int64  { return 0 }
func (fi dirFileInfo) Mode() os.FileMode {
	return os.ModeDir | 0o755
}
func (fi dirFileInfo) ModTime() time.Time { return fi.modTime }
func (fi dirFileInfo) IsDir() bool        { return true }
func (fi dirFileInfo) Sys() any           { return nil }

// compile-time seams.
var (
	_ sftp.FileLister = (*fileList)(nil)
	_ sftp.FileReader = (*fileGet)(nil)
	_ sftp.FileWriter = (*filePut)(nil)
	_ sftp.FileCmder  = (*fileCmd)(nil)
	_ sftp.ListerAt   = (*lister)(nil)
	_ os.FileInfo     = objectFileInfo{}
	_ os.FileInfo     = dirFileInfo{}
)
