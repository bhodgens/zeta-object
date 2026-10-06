// zfssurface_bridge_shared_test.go — bughunt L4: the webdav bridge
// re-inlined resolveEventsContext's validation in THREE places instead of
// calling the ONE implementation the s3 capability endpoints use.
//
// zfssurface_bridge.go's file header claims the provider resolution and
// the wire forms are "ONE implementation - never duplicated across
// frontends", but the bucket-existence/validation block was copy-pasted
// into HandleBucketEventsForBucket, HandleObjectEventsForBucket and
// HandleObjectVersionsForBucket. Byte-equivalent today, so every suite
// stayed green - which is exactly the drift risk: the day
// resolveEventsContext changes, three frozen copies silently keep the old
// behavior and webdav stops matching s3.
//
// These tests pin the SHARING, not the bytes:
//
//  1. A structural pin: the validation body exists exactly ONCE in the
//     package (a grep-equivalent over the package's own sources, so a
//     future re-inline fails here even though the bytes still match).
//  2. A behavioral table: all FOUR call sites (the s3 handlers and the
//     three bridge entry points) agree on the validation verdict for the
//     same inputs - including the traversal bucket that motivated
//     resolveEventsContext in the first place.
package s3

import (
	"bytes"
	"context"
	"go/ast"
	"go/parser"
	"go/token"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/bhodgens/zeta-object/internal/backend"
	"github.com/bhodgens/zeta-object/internal/backend/fsbackend"
	"github.com/bhodgens/zeta-object/internal/metadata"
)

// sharedEventsEnv is newEventsTestServer's shape for the bridge tests: a
// real fs backend over a temp root, the process seams installed against
// it, and an attached provider for one bucket.
func newSharedEventsEnv(t *testing.T, bucket string) string {
	t.Helper()
	root := t.TempDir()
	f, err := fsbackend.New(root)
	if err != nil {
		t.Fatal(err)
	}
	installServerConfigView(serverConfigView{DataDir: root + "/"})
	installFSRootResolver(func(b string) string { return filepath.Join(root, b) })
	installBackendLookup(func(string) (backend.Backend, error) { return f, nil })
	t.Cleanup(func() {
		installBackendLookup(nil)
		installFSRootResolver(nil)
	})

	bucketPath := filepath.Join(root, bucket)
	if err := os.MkdirAll(filepath.Join(bucketPath, ".metadata"), 0o755); err != nil {
		t.Fatalf("mkdir bucket: %v", err)
	}
	if err := os.WriteFile(filepath.Join(bucketPath, ".events-provider"), nil, 0o644); err != nil {
		t.Fatalf("attach marker: %v", err)
	}
	return root
}

// bridgeProvider is the provider double the shared-validation tests
// attach (metadata.Provider behavior the handlers need: History +
// LastDetail).
type bridgeProvider struct{}

func (bridgeProvider) Name() string { return "zfs-events" }

func (bridgeProvider) Probe(_ context.Context, _ string) (metadata.ProbeResult, error) {
	return metadata.ProbeResult{Available: true, Dataset: "stub/shared"}, nil
}

func (bridgeProvider) History(_ context.Context, _, _ string, _ metadata.HistoryQuery) ([]metadata.ObjectEvent, error) {
	return []metadata.ObjectEvent{{Op: "create", Key: "k.txt", Txg: 5}}, nil
}

func (bridgeProvider) Purge(_ context.Context, _ string) error { return nil }

func (bridgeProvider) LastDetail() metadata.HistoryDetail {
	return metadata.HistoryDetail{Dataset: "stub/shared"}
}

// eventsValidationSurfaces are the FIVE call sites of the ?events /
// ?versions bucket validation: the two s3 capability handlers and the
// three webdav bridge entry points. Every one of them must REACH the
// shared implementation; none may contain the validation itself.
var eventsValidationSurfaces = []string{
	"handleBucketEvents",
	"handleObjectEvents",
	"HandleBucketEventsForBucket",
	"HandleObjectEventsForBucket",
	"HandleObjectVersionsForBucket",
}

// TestEventsValidationIsSharedNotReInlined is the STRUCTURAL L4 pin.
//
// The ?events / ?versions bucket validation must be implemented ONCE in
// the package, and every surface must reach it through the shared chain.
// Byte-equivalence is not the pin: the three bridge copies were
// byte-identical today and every suite stayed green, which IS the drift
// risk. This parses the package and asserts:
//
//   - exactly ONE function body implements the validation (calls
//     validBucket, consults bucketExists, writes NoSuchBucket);
//   - each of the FIVE ?events surfaces CALLS that function (directly or
//     through package-local helpers), so changing the one implementation
//     changes all five.
//
// Functions outside this set (batch, bucket location, HEAD, CopyObject,
// ?versioning) legitimately do their own bucket checks; they are not
// re-inlines of this validation, and scoping to the surfaces keeps the pin
// honest instead of grep-flailing on unrelated callers.
func TestEventsValidationIsSharedNotReInlined(t *testing.T) {
	calls, bodies := parseCallGraph(t)

	// (1) No surface may implement the validation itself.
	for _, surface := range eventsValidationSurfaces {
		body, ok := bodies[surface]
		if !ok {
			t.Fatalf("surface %s not found in package s3", surface)
		}
		if callsFunction(body, "validBucket") || callsFunction(body, "bucketExists") {
			t.Fatalf("%s re-inlines the ?events bucket validation instead of calling the shared implementation — the drift this file's header claims cannot happen:\n%s",
				surface, body)
		}
	}

	// (2) All FIVE surfaces must reach ONE common implementation. The
	// shared implementation is whichever function in the intersection of
	// their reachable sets performs the validation (calls validBucket,
	// consults bucketExists, writes NoSuchBucket). Asserting the
	// INTERSECTION rather than "exactly one such function in the package"
	// is deliberate: unrelated handlers outside this set (batch, bucket
	// location, HEAD, CopyObject, ?versioning) legitimately do their own
	// bucket checks, and their presence is not a re-inline of this
	// validation.
	reachable := map[string]map[string]bool{}
	for _, surface := range eventsValidationSurfaces {
		reachable[surface] = reachSet(calls, surface)
	}
	var shared []string
	for name := range reachable[eventsValidationSurfaces[0]] {
		inAll := true
		for _, surface := range eventsValidationSurfaces[1:] {
			if !reachable[surface][name] {
				inAll = false
				break
			}
		}
		if !inAll {
			continue
		}
		body := bodies[name]
		if callsFunction(body, "validBucket") && strings.Contains(body, "bucketExists") && strings.Contains(body, "NoSuchBucket") {
			shared = append(shared, name)
		}
	}
	if len(shared) != 1 {
		sort.Strings(shared)
		t.Fatalf("all %d ?events surfaces must reach ONE shared bucket validation; found %v. Without a common implementation a change to one surface's check silently diverges from the others.",
			len(eventsValidationSurfaces), shared)
	}
	t.Logf("shared ?events validation reached by all %d surfaces: %s", len(eventsValidationSurfaces), shared[0])
}

// reachSet returns every package-local function reachable from start
// (including start itself) through package-local calls.
func reachSet(calls map[string]map[string]bool, start string) map[string]bool {
	seen := map[string]bool{}
	queue := []string{start}
	for len(queue) > 0 {
		name := queue[0]
		queue = queue[1:]
		if seen[name] {
			continue
		}
		seen[name] = true
		for callee := range calls[name] {
			if _, known := calls[callee]; known {
				queue = append(queue, callee)
			}
		}
	}
	return seen
}

// parseCallGraph returns, for package s3, the map of function name to the
// package-local functions it calls plus the source text of its body.
// Methods are excluded: this chain is package-local free functions.
//
// It parses the package's own .go files directly (no go/packages
// dependency): the pin only needs the non-test sources of ONE directory,
// which is exactly this package's.
func parseCallGraph(t *testing.T) (map[string]map[string]bool, map[string]string) {
	t.Helper()
	fset := token.NewFileSet()
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("reading package s3 dir: %v", err)
	}
	calls := map[string]map[string]bool{}
	bodies := map[string]string{}
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		file, perr := parser.ParseFile(fset, name, nil, 0)
		if perr != nil {
			t.Fatalf("parsing %s: %v", name, perr)
		}
		raw, rerr := os.ReadFile(name)
		if rerr != nil {
			t.Fatalf("reading %s: %v", name, rerr)
		}
		for _, decl := range file.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Body == nil || fn.Recv != nil {
				continue
			}
			fnName := fn.Name.Name
			callees := map[string]bool{}
			ast.Inspect(fn.Body, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok {
					return true
				}
				if ident, ok := call.Fun.(*ast.Ident); ok {
					callees[ident.Name] = true
				}
				return true
			})
			calls[fnName] = callees
			lo := fset.Position(fn.Body.Lbrace).Offset
			hi := min(fset.Position(fn.Body.Rbrace).Offset, len(raw))
			bodies[fnName] = string(raw[lo:hi])
		}
	}
	return calls, bodies
}

// callsFunction reports whether a function body's source contains a CALL
// to name (an identifier occurrence in call position).
func callsFunction(body, name string) bool {
	return strings.Contains(body, name+"(")
}

// TestEventsValidationAgreesAcrossAllFourCallSites is the BEHAVIORAL half:
// the four surfaces must agree on the validation verdict for the same
// inputs, including the traversal bucket that resolveEventsContext exists
// to reject (bughunt D3) - a bucket name that fails validBucket must NOT
// reach the provider, on ANY of the four surfaces.
func TestEventsValidationAgreesAcrossAllFourCallSites(t *testing.T) {
	const bucket = "shared-events-bkt"
	newSharedEventsEnv(t, bucket)
	// Attach the provider for this bucket path so every surface gets past
	// the validation stage and the ONLY variable is the validation verdict.
	bucketPath := getBucketPath(bucket)
	InstallMetadataProvider(func(bp string) metadata.MetadataProvider {
		if bp == bucketPath {
			return bridgeProvider{}
		}
		return nil
	})
	t.Cleanup(func() { InstallMetadataProvider(nil) })

	for _, tc := range []struct {
		name   string
		bucket string
		want   int // 404 = invalid/unknown bucket, 200 = validated through
	}{
		{"known bucket", bucket, http.StatusOK},
		{"traversal bucket (bughunt D3)", "..%2f..%2fetc", http.StatusNotFound},
		{"dot bucket", ".", http.StatusNotFound},
		{"unknown but well-formed bucket", "no-such-bucket-here", http.StatusNotFound},
		{"invalid name (uppercase)", "Invalid_Bucket", http.StatusNotFound},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// The FOUR surfaces. s3 handles object + bucket; the bridge
			// handles bucket + object events + object versions. Each is
			// driven with the SAME bucket name; the verdict must agree.
			surfaces := map[string]func(w http.ResponseWriter, r *http.Request){
				"s3 bucket events": func(w http.ResponseWriter, r *http.Request) {
					handleBucketEvents(w, r, tc.bucket)
				},
				"s3 object events": func(w http.ResponseWriter, r *http.Request) {
					handleObjectEvents(w, r, tc.bucket, "k.txt")
				},
				"bridge bucket events": func(w http.ResponseWriter, r *http.Request) {
					HandleBucketEventsForBucket(w, r, tc.bucket, bucketPath)
				},
				"bridge object events": func(w http.ResponseWriter, r *http.Request) {
					HandleObjectEventsForBucket(w, r, tc.bucket, "k.txt", bucketPath)
				},
				"bridge object versions": func(w http.ResponseWriter, r *http.Request) {
					HandleObjectVersionsForBucket(w, r, tc.bucket, "k.txt", bucketPath)
				},
			}
			for name, drive := range surfaces {
				w := httptest.NewRecorder()
				req := httptest.NewRequest("GET", "/x?events", nil)
				drive(w, req)
				if w.Code != tc.want {
					t.Errorf("%s: status = %d, want %d (the four surfaces must agree on the shared validation verdict)", name, w.Code, tc.want)
				}
				if tc.want == http.StatusNotFound && !strings.Contains(w.Body.String(), "NoSuchBucket") {
					t.Errorf("%s: 404 body must carry NoSuchBucket, got %s", name, w.Body.String())
				}
			}
		})
	}
}

// TestBridgeBucketEventsMatchesS3HandlerByteForByte is the strongest form
// of "one implementation": for the same bucket and the same query, the
// bridge's response must be byte-identical to the s3 handler's. Any future
// divergence in the shared validation shows up as a byte difference here.
func TestBridgeBucketEventsMatchesS3HandlerByteForByte(t *testing.T) {
	const bucket = "shared-byte-bkt"
	newSharedEventsEnv(t, bucket)
	bucketPath := getBucketPath(bucket)
	InstallMetadataProvider(func(bp string) metadata.MetadataProvider {
		if bp == bucketPath {
			return bridgeProvider{}
		}
		return nil
	})
	t.Cleanup(func() { InstallMetadataProvider(nil) })

	for _, tc := range []struct {
		name   string
		bucket string
		query  string
	}{
		{"valid bucket events", bucket, "?events"},
		{"valid bucket events+versions", bucket, "?events&versions"},
		{"invalid bucket events", "..%2f..%2fetc", "?events"},
		{"invalid bucket events+versions", "..%2f..%2fetc", "?events&versions"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s3w := httptest.NewRecorder()
			handleBucketEvents(s3w, httptest.NewRequest("GET", "/"+tc.bucket+tc.query, nil), tc.bucket)

			bridgew := httptest.NewRecorder()
			HandleBucketEventsForBucket(bridgew, httptest.NewRequest("GET", "/"+tc.bucket+tc.query, nil), tc.bucket, bucketPath)

			if s3w.Code != bridgew.Code {
				t.Fatalf("status differs: s3 %d, bridge %d", s3w.Code, bridgew.Code)
			}
			if !bytes.Equal(s3w.Body.Bytes(), bridgew.Body.Bytes()) {
				t.Fatalf("body differs (one implementation should be byte-identical):\n s3:    %s\n bridge: %s", s3w.Body.String(), bridgew.Body.String())
			}
			if s3w.Header().Get("Content-Type") != bridgew.Header().Get("Content-Type") {
				t.Fatalf("content-type differs: s3 %q, bridge %q", s3w.Header().Get("Content-Type"), bridgew.Header().Get("Content-Type"))
			}
		})
	}
}
