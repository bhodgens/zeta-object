package transport

// memfs.go is the in-memory Transport stub for unit tests (leaf-02
// constraint: the real HTTP client is leaf 06). It mirrors the server
// facts the plan tree locks in:
//   - ETag = MD5 of the stored body (master.md "server facts").
//   - Put with a non-empty If-Match etag answers ErrConflict on a stale
//     value (HTTP 412), and Put with a missing parent collection fails
//     (HTTP 409, mapped to ErrNoParent).
//   - Move enforces the destination If-Match (412 on stale).
//   - Delete on a non-empty collection fails (ErrNotEmpty), on missing
//     keys fails (ErrNotExist).
//   - PROPFIND listings are sorted, non-recursive by default, and the
//     reserved prefixes (.metadata/, .zfs, .uploads) never surface.
//   - A collection tombstone (rmdir'd while remote children exist) is
//     modeled as a marker key "d/__zeta_collection__" listing as a dir
//     entry - NOT needed: the stub keeps directories as real entries,
//     and Delete of a non-empty dir fails like the server does.

import (
	"context"
	"crypto/md5" // #nosec G401 -- the wire ETag IS the body MD5 (server contract); not a security use
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"
	"sync"
	"time"
)

// memNode is one stored key: either a file (body set) or a collection
// (dir set). Collections are materialized implicitly by Put/Mkdir.
type memNode struct {
	dir      bool
	body     []byte
	modTime  time.Time
	tomb     bool // rmdir'd but children may still exist server-side
	children map[string]bool
}

// MemFS is an in-memory Transport. Create with NewMemFS.
type MemFS struct {
	mu          sync.Mutex
	nodes       map[string]*memNode
	putErr      error // injected Put failure (tests)
	getErr      error // injected Get failure (tests)
	allErr      error // injected failure for every verb (tests)
	propfindErr error // injected Propfind-only failure (tests, via FailPropfinds)
}

// NewMemFS builds an empty stub.
func NewMemFS() *MemFS {
	return &MemFS{nodes: map[string]*memNode{"": {dir: true, children: map[string]bool{}}}}
}

// Keys returns the sorted list of live (non-tombstone) keys, files only
// by default - test helper.
func (m *MemFS) Keys() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []string
	for k, n := range m.nodes {
		if !n.tomb && !n.dir {
			out = append(out, k)
		}
	}
	sort.Strings(out)
	return out
}

// PutRaw seeds a key bypassing the reserved-name gate - test helper for
// seeding server-side state the CLIENT must never see through the mount.
func (m *MemFS) PutRaw(key string, body string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	p, _ := parentOf(key)
	if p != "" {
		m.ensureDirs(p)
	}
	m.nodes[key] = &memNode{body: []byte(body), modTime: time.Now()}
}

// ServerBody returns the stored body for key - test helper.
func (m *MemFS) ServerBody(key string) (string, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	n, ok := m.nodes[key]
	if !ok || n.tomb || n.dir {
		return "", false
	}
	return string(n.body), true
}

// ServerBodyOrEmpty is ServerBody with a quiet false - test helper.
func (m *MemFS) ServerBodyOrEmpty(key string) string {
	b, _ := m.ServerBody(key)
	return b
}

// MD5Hex is the ETag contract helper (body MD5) - test helper.
func MD5Hex(b []byte) string { return md5hex(b) }

// FailPuts makes every Put fail with err until cleared (nil clears).
// FailAll is the stronger form (every verb fails). Test helpers.
func (m *MemFS) FailPuts(err error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.putErr = err
}

func (m *MemFS) FailAll(err error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.putErr, m.getErr, m.allErr = err, err, err
}

// HasDir reports whether a collection exists (test helper).
func (m *MemFS) HasDir(key string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	n, ok := m.nodes[key]
	return ok && n.dir && !n.tomb
}

// pickErr surfaces the injected failure (allErr wins).
func (m *MemFS) pickErr(specific error) error {
	if m.allErr != nil {
		return m.allErr
	}
	return specific
}

func md5hex(b []byte) string {
	return fmt.Sprintf("%x", md5.Sum(b)) // #nosec G401 -- ETag contract, see file header
}

// parentOf splits "a/b/c" -> ("a/b", "c"); "" parent for top level.
func parentOf(key string) (string, string) {
	parent, leaf, found := strings.CutLast(key, "/")
	if !found {
		return "", key
	}
	return parent, leaf
}

// ensureDirs creates every missing collection along key's parent chain.
func (m *MemFS) ensureDirs(key string) {
	for key != "" {
		n, ok := m.nodes[key]
		if !ok {
			n = &memNode{dir: true, children: map[string]bool{}}
			m.nodes[key] = n
		}
		if !n.dir {
			// existing non-dir node at that path: stop here (a real
			// server would 409; Put's caller surfaces that path)
			return
		}
		// Register this collection in its parent's children set.
		p, base := parentOf(key)
		if pn, ok := m.nodes[p]; ok && pn.dir {
			pn.children[base] = true
		}
		key = p
	}
}

func (m *MemFS) Get(_ context.Context, key string, req *RangeRequest) (io.ReadCloser, *ObjectInfo, error) {
	if err := m.pickErr(m.getErr); err != nil {
		return nil, nil, err
	}
	if reservedKey(key) {
		return nil, nil, ErrNotExist
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	n, ok := m.nodes[key]
	if !ok || n.tomb || n.dir {
		return nil, nil, ErrNotExist
	}
	body := n.body
	start, end := 0, len(body)
	if req != nil {
		start, end = int(req.Start), int(req.End)+1
		if start > len(body) {
			start = len(body)
		}
		if end > len(body) {
			end = len(body)
		}
	}
	return io.NopCloser(strings.NewReader(string(body[start:end]))), &ObjectInfo{
		Size:    int64(len(body)),
		ModTime: n.modTime,
		ETag:    md5hex(body),
	}, nil
}

func (m *MemFS) Put(_ context.Context, key string, body io.ReadSeeker, etag string) (string, error) {
	if err := m.pickErr(m.putErr); err != nil {
		// Surgical fault: only the named key fails (tests).
		var ge *GateError
		if errors.As(err, &ge) && ge.OnlyKey != nil && *ge.OnlyKey != key {
			// fall through: this key's Put is allowed
		} else {
			return "", err
		}
	}
	if reservedKey(key) {
		return "", ErrNotExist
	}
	data, err := io.ReadAll(body)
	if err != nil {
		return "", err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	// Precondition model (mirrors the gateway): a non-empty etag is
	// If-Match (412 on mismatch); an EMPTY etag is If-None-Match:* (412
	// when the object already exists - a lost create race). Seeds that
	// must overwrite use PutRaw.
	if n, ok := m.nodes[key]; ok && !n.tomb && !n.dir {
		if etag != "" && md5hex(n.body) != etag {
			return "", ErrConflict
		}
		if etag == "" {
			return "", ErrConflict
		}
	}
	p, _ := parentOf(key)
	// The gateway materializes parent collections on PUT (S3 semantics:
	// "a/b/c.txt" implies collections a/ and a/b/). MKCOL strictness
	// applies only to explicit Mkdir calls.
	if p != "" {
		m.ensureDirs(p)
	}
	n, ok := m.nodes[key]
	if !ok || n.tomb || n.dir {
		n = &memNode{children: map[string]bool{}}
		m.nodes[key] = n
		m.ensureDirs(p)
	}
	n.dir = false
	n.body = data
	n.modTime = time.Now()
	n.tomb = false
	// Register the file in its parent's children set (the non-empty-
	// collection check reads it).
	if p != "" {
		if pn, ok := m.nodes[p]; ok && pn.dir {
			_, base := parentOf(key)
			pn.children[base] = true
		}
	}
	return md5hex(data), nil
}

func (m *MemFS) Mkdir(_ context.Context, key string) error {
	if err := m.pickErr(m.allErr); err != nil {
		return err
	}
	if reservedKey(key) {
		return ErrNotExist
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.nodes[key]; ok {
		return ErrExist
	}
	p, _ := parentOf(key)
	if p != "" {
		pn, ok := m.nodes[p]
		if !ok || pn.tomb || !pn.dir {
			return ErrNoParent
		}
	}
	m.nodes[key] = &memNode{dir: true, children: map[string]bool{}}
	m.ensureDirs(p)
	return nil
}

func (m *MemFS) Delete(_ context.Context, key string, _ string) error {
	if err := m.pickErr(m.allErr); err != nil {
		return err
	}
	if reservedKey(key) {
		return ErrNotExist
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	n, ok := m.nodes[key]
	if !ok || n.tomb {
		return ErrNotExist
	}
	if n.dir {
		if len(n.children) > 0 {
			return ErrNotEmpty
		}
	}
	delete(m.nodes, key)
	if p, base := parentOf(key); p != "" {
		if pn, ok := m.nodes[p]; ok {
			delete(pn.children, base)
		}
	}
	return nil
}

func (m *MemFS) Move(_ context.Context, oldKey, newKey, destETag string) error {
	if err := m.pickErr(m.allErr); err != nil {
		return err
	}
	if reservedKey(oldKey) || reservedKey(newKey) || oldKey == newKey {
		return ErrNotExist
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	n, ok := m.nodes[oldKey]
	if !ok || n.tomb {
		return ErrNotExist
	}
	if dn, ok := m.nodes[newKey]; ok && !dn.tomb && destETag != "" {
		if dn.dir {
			return ErrNotEmpty
		}
		if md5hex(dn.body) != destETag {
			return ErrConflict
		}
	}
	p, _ := parentOf(newKey)
	if p != "" {
		pn, ok := m.nodes[p]
		if !ok || pn.tomb || !pn.dir {
			return ErrNoParent
		}
	}
	delete(m.nodes, oldKey)
	m.nodes[newKey] = n
	if op, obase := parentOf(oldKey); op != "" {
		if opn, ok := m.nodes[op]; ok {
			delete(opn.children, obase)
		}
	}
	m.ensureDirs(p)
	return nil
}

func (m *MemFS) Propfind(_ context.Context, key string, recursive bool) ([]Entry, error) {
	if err := m.pickErr(m.propfindErr); err != nil {
		return nil, err
	}
	if reservedKey(key) {
		return nil, ErrNotExist
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if key != "" {
		n, ok := m.nodes[key]
		if !ok || n.tomb {
			return nil, ErrNotExist
		}
	}
	prefix := ""
	if key != "" {
		prefix = key + "/"
	}
	seen := map[string]bool{}
	var out []Entry
	if key != "" {
		n := m.nodes[key]
		entry := Entry{Key: key, IsDir: n.dir, Size: int64(len(n.body)), ModTime: n.modTime}
		if n.dir {
			entry.ETag = m.dirETagLocked(key)
		} else {
			entry.ETag = m.fileETagLocked(n)
		}
		out = append(out, entry)
	}
	keys := make([]string, 0, len(m.nodes))
	for k := range m.nodes {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		n := m.nodes[k]
		if n.tomb || k == key || !strings.HasPrefix(k, prefix) {
			continue
		}
		if reservedKey(k[len(prefix):]) {
			continue // reserved names never surface in listings
		}
		rel := k[len(prefix):]
		if rel == "" {
			continue
		}
		name := rel
		isDir := n.dir
		if seg, rest, hasSlash := strings.Cut(rel, "/"); hasSlash {
			// nested: surface only the first path segment as a dir
			if !recursive {
				name = seg
				isDir = true
				_ = rest
			} else {
				continue
			}
		}
		seen[name] = true
		entry := Entry{
			Key:     prefix + name, // direct children surface their name; nested collapse to the first segment
			IsDir:   isDir,
			Size:    int64(len(n.body)),
			ModTime: n.modTime,
		}
		switch {
		case !isDir:
			entry.ETag = m.fileETagLocked(n) // file getetag: body MD5
		case rel == name:
			entry.ETag = m.dirETagLocked(k) // real collection row: its own child token
		default:
			entry.ETag = m.dirETagLocked(prefix + name) // nested collapse: derive from the child collection
		}
		out = append(out, entry)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Key < out[j].Key })
	return out, nil
}

// reservedKey mirrors the server's reserved-name policy: keys under
// .metadata/, .zfs, .uploads never surface through the mount or the
// listings (leaf-02 requirement). A key is reserved when the FIRST path
// segment is reserved - the server's own gate is top-level only.
func reservedKey(key string) bool {
	if key == "" {
		return false
	}
	seg := key
	if i := strings.IndexByte(seg, '/'); i >= 0 {
		seg = seg[:i]
	}
	switch seg {
	case ".metadata", ".zfs", ".uploads":
		return true
	}
	return false
}

// ErrExist and ErrNoParent model the server's MKCOL/PUT/MOVE failure
// modes (405 exists, 409 missing parent).
var (
	ErrExist    = fmt.Errorf("transport: resource already exists")
	ErrNoParent = fmt.Errorf("transport: parent collection is missing")
)
