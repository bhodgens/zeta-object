// bucket_name_gate_test.go — the bughunt 2026-10-05 C1 pin.
//
// C1: a traversal-shaped BUCKET name (".." / ".") reached the data plane
// through the s3 frontend. getBucketPath is filepath.Join(dataDir, name) and
// filepath.Join CLEANS "..", so ".." resolved to the data root's PARENT and
// the request read, wrote, listed and DELETED outside the data root. The wave
// that fixed H3 validated the name at the bucketmanager layer, but the s3
// FRONTEND's own object plane never reaches bucketmanager for these surfaces
// — it goes straight to the backend seam — so the hole survived one layer up.
//
// Why this file exists as a UNIT pin and not only as an e2e case: the defect
// class that let C1 through was "every test injects a dependency or calls the
// handler with a shape the wire cannot produce". So this pin drives the REAL
// dispatch switch with the PRODUCTION backend shape (a dataDir-rooted
// fsbackend, which is what buildBackendLookup constructs for a name not
// declared in cfg.Buckets — backend_lookup.go:138) and asserts on the FILESYSTEM
// after each request, not just on a status code: a status-only assertion is
// satisfied by a 500, which is not the contract.
//
// The gate is asserted on EVERY surface that resolves a bucket path, plus a
// positive control per surface so the case cannot pass by refusing everything.

package s3

import (
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/bhodgens/zeta-object/internal/backend"
	"github.com/bhodgens/zeta-object/internal/backend/fsbackend"
)

// installC1Probe installs the production-shaped backend seam over dataDir:
// an undeclared bucket name falls through buildBackendLookup's default branch
// to constructor(fs, dataDir) — root=dataDir, no single-bucket opt — and
// fsbackend.bucketPath then does filepath.Join(root, bucket).
func installC1Probe(t *testing.T, dataDir string) {
	t.Helper()
	installServerConfigView(serverConfigView{DataDir: dataDir})
	installBackendLookup(func(string) (backend.Backend, error) {
		return fsbackend.New(dataDir)
	})
	t.Cleanup(func() { installBackendLookup(nil) })
}

// c1TraversalNames are the names that must never resolve. ".." and "." are
// the whole class: filepath.Join(dataDir, "..") is dataDir's parent.
var c1TraversalNames = []string{"..", "."}

func TestBucketNameGate_TraversalNameNeverResolvesToTheParentDirectory(t *testing.T) {
	root := t.TempDir()
	dataDir := filepath.Join(root, "data")
	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		t.Fatal(err)
	}
	// Canaries OUTSIDE the data root: a sibling file and a sibling directory.
	outsideFile := filepath.Join(root, "victim.txt")
	if err := os.WriteFile(outsideFile, []byte("canary"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, "srvdata"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "srvdata", "thing.txt"), []byte("two"), 0o644); err != nil {
		t.Fatal(err)
	}
	installC1Probe(t, dataDir)

	// A real bucket, created through the real handler, so the positive
	// controls below exercise the same wiring the traversal arms do.
	if w := httptest.NewRecorder(); true {
		rec := w
		defaultTestFrontend().bucketLevelDispatch(rec, httptest.NewRequest("PUT", "/good", nil), "good")
		if rec.Code != 200 {
			t.Fatalf("setup CreateBucket good: status %d", rec.Code)
		}
		rec = httptest.NewRecorder()
		defaultTestFrontend().objectLevelDispatch(rec, httptest.NewRequest("PUT", "/good/inside.txt",
			strings.NewReader("inside")), "good", "inside.txt")
		if rec.Code != 200 {
			t.Fatalf("setup PutObject good/inside.txt: status %d", rec.Code)
		}
	}

	// Every bucket-name-bearing surface, in one table. write drives the
	// request; the assertion is on the STATUS plus (for the mutating arms)
	// the canary files still being there.
	surfaces := []struct {
		name string
		do   func(bucket string) *httptest.ResponseRecorder
		want int
	}{
		{"DeleteObjects ?delete", func(b string) *httptest.ResponseRecorder {
			r := httptest.NewRecorder()
			defaultTestFrontend().bucketLevelDispatch(r, httptest.NewRequest("POST", "/"+b+"?delete",
				strings.NewReader(`<Delete><Object><Key>victim.txt</Key></Object></Delete>`)), b)
			return r
		}, 404},
		{"PutObject", func(b string) *httptest.ResponseRecorder {
			r := httptest.NewRecorder()
			defaultTestFrontend().objectLevelDispatch(r, httptest.NewRequest("PUT", "/"+b+"/planted.txt",
				strings.NewReader("planted by a traversal name")), b, "planted.txt")
			return r
		}, 404},
		{"GetObject", func(b string) *httptest.ResponseRecorder {
			r := httptest.NewRecorder()
			defaultTestFrontend().objectLevelDispatch(r, httptest.NewRequest("GET", "/"+b+"/victim.txt", nil), b, "victim.txt")
			return r
		}, 404},
		{"DeleteObject", func(b string) *httptest.ResponseRecorder {
			r := httptest.NewRecorder()
			defaultTestFrontend().objectLevelDispatch(r, httptest.NewRequest("DELETE", "/"+b+"/secret2.txt", nil), b, "secret2.txt")
			return r
		}, 404},
		{"HeadObject", func(b string) *httptest.ResponseRecorder {
			r := httptest.NewRecorder()
			defaultTestFrontend().objectLevelDispatch(r, httptest.NewRequest("HEAD", "/"+b+"/victim.txt", nil), b, "victim.txt")
			return r
		}, 404},
		{"ListObjectsV2 ?list-type=2", func(b string) *httptest.ResponseRecorder {
			r := httptest.NewRecorder()
			defaultTestFrontend().bucketLevelDispatch(r, httptest.NewRequest("GET", "/"+b+"?list-type=2", nil), b)
			return r
		}, 404},
		{"ListObjectVersions ?versions", func(b string) *httptest.ResponseRecorder {
			r := httptest.NewRecorder()
			defaultTestFrontend().bucketLevelDispatch(r, httptest.NewRequest("GET", "/"+b+"?versions", nil), b)
			return r
		}, 404},
		{"ListMultipartUploads ?uploads", func(b string) *httptest.ResponseRecorder {
			r := httptest.NewRecorder()
			defaultTestFrontend().bucketLevelDispatch(r, httptest.NewRequest("GET", "/"+b+"?uploads", nil), b)
			return r
		}, 404},
		{"Batch ?batch", func(b string) *httptest.ResponseRecorder {
			r := httptest.NewRecorder()
			defaultTestFrontend().bucketLevelDispatch(r, httptest.NewRequest("POST", "/"+b+"?batch",
				strings.NewReader(`{"operations":[{"op":"delete","from":"victim.txt"}]}`)), b)
			return r
		}, 404},
		// ?location carries its OWN handler-side validBucket gate
		// (bucket_handlers.go:233) and answers 400, where the surfaces above
		// answer 404. Both are refusals; the contract is "never serves the
		// traversal name", not one exact code per surface.
		{"GetBucketLocation ?location", func(b string) *httptest.ResponseRecorder {
			r := httptest.NewRecorder()
			defaultTestFrontend().bucketLevelDispatch(r, httptest.NewRequest("GET", "/"+b+"?location", nil), b)
			return r
		}, 400},
	}

	for _, name := range c1TraversalNames {
		for _, s := range surfaces {
			rec := s.do(name)
			// REFUSED, not SERVED: any 4xx is a refusal of the traversal
			// name. Asserting one exact code per surface would break the next
			// time a surface adds its own handler-side gate (as ?location did);
			// asserting only "not 2xx" would let a 3xx redirect through. The
			// want column is documentation of today's status, not the
			// assertion - the assertion is the 4xx class plus the canary.
			if rec.Code != s.want {
				t.Logf("note: bucket %q on %s answered %d, not the documented %d - still a refusal",
					name, s.name, rec.Code, s.want)
			}
			if rec.Code < 400 || rec.Code >= 500 {
				t.Errorf("bucket %q on %s: status %d - the traversal name was SERVED, not refused (body %s)",
					name, s.name, rec.Code, strings.TrimSpace(rec.Body.String()))
			}
		}
		// The filesystem is the contract: nothing outside the data root may
		// have been created, removed or read through a traversal bucket name.
		if _, err := os.Stat(outsideFile); err != nil {
			t.Errorf("bucket %q: the canary OUTSIDE the data root was DELETED", name)
		}
		if _, err := os.Stat(filepath.Join(root, "planted.txt")); err == nil {
			t.Errorf("bucket %q: a file was WRITTEN outside the data root", name)
		}
	}
}

// TestBucketNameGate_PositiveControl is the guard against the gate being
// "fixed" by refusing everything: every surface that answers 404 for ".."
// must still answer 200 for a real bucket. A test suite where every request
// 404s would pass the C1 arms.
func TestBucketNameGate_PositiveControl(t *testing.T) {
	root := t.TempDir()
	dataDir := filepath.Join(root, "data")
	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		t.Fatal(err)
	}
	installC1Probe(t, dataDir)

	rec := httptest.NewRecorder()
	defaultTestFrontend().bucketLevelDispatch(rec, httptest.NewRequest("PUT", "/good", nil), "good")
	if rec.Code != 200 {
		t.Fatalf("CreateBucket good: status %d", rec.Code)
	}
	rec = httptest.NewRecorder()
	defaultTestFrontend().objectLevelDispatch(rec, httptest.NewRequest("PUT", "/good/inside.txt",
		strings.NewReader("inside")), "good", "inside.txt")
	if rec.Code != 200 {
		t.Fatalf("PutObject good/inside.txt: status %d", rec.Code)
	}

	for _, s := range []struct {
		name string
		do   func() *httptest.ResponseRecorder
	}{
		{"GetObject", func() *httptest.ResponseRecorder {
			r := httptest.NewRecorder()
			defaultTestFrontend().objectLevelDispatch(r, httptest.NewRequest("GET", "/good/inside.txt", nil), "good", "inside.txt")
			return r
		}},
		{"HeadObject", func() *httptest.ResponseRecorder {
			r := httptest.NewRecorder()
			defaultTestFrontend().objectLevelDispatch(r, httptest.NewRequest("HEAD", "/good/inside.txt", nil), "good", "inside.txt")
			return r
		}},
		{"ListObjectsV2", func() *httptest.ResponseRecorder {
			r := httptest.NewRecorder()
			defaultTestFrontend().bucketLevelDispatch(r, httptest.NewRequest("GET", "/good?list-type=2", nil), "good")
			return r
		}},
		{"ListObjectVersions", func() *httptest.ResponseRecorder {
			r := httptest.NewRecorder()
			defaultTestFrontend().bucketLevelDispatch(r, httptest.NewRequest("GET", "/good?versions", nil), "good")
			return r
		}},
		{"ListMultipartUploads", func() *httptest.ResponseRecorder {
			r := httptest.NewRecorder()
			defaultTestFrontend().bucketLevelDispatch(r, httptest.NewRequest("GET", "/good?uploads", nil), "good")
			return r
		}},
		{"GetBucketLocation", func() *httptest.ResponseRecorder {
			r := httptest.NewRecorder()
			defaultTestFrontend().bucketLevelDispatch(r, httptest.NewRequest("GET", "/good?location", nil), "good")
			return r
		}},
		// "delete" of a key that does not exist reports per-item success, so
		// the manifest is valid and the surface answers 200. (An unknown op
		// is a 400 by the batchops contract, which is a different assertion.)
		{"Batch", func() *httptest.ResponseRecorder {
			r := httptest.NewRecorder()
			defaultTestFrontend().bucketLevelDispatch(r, httptest.NewRequest("POST", "/good?batch",
				strings.NewReader(`{"operations":[{"op":"delete","from":"absent.txt"}]}`)), "good")
			return r
		}},
		{"DeleteObjects", func() *httptest.ResponseRecorder {
			r := httptest.NewRecorder()
			defaultTestFrontend().bucketLevelDispatch(r, httptest.NewRequest("POST", "/good?delete",
				strings.NewReader(`<Delete><Object><Key>inside.txt</Key></Object></Delete>`)), "good")
			return r
		}},
	} {
		rec := s.do()
		if rec.Code != 200 {
			t.Errorf("positive control %s on a real bucket: status %d, want 200 (body %s)",
				s.name, rec.Code, strings.TrimSpace(rec.Body.String()))
		}
	}
}

// TestBucketNameGate_ExistsIsNotAGate pins WHY the gate cannot be
// bucketExists alone: for a traversal name, getBucketPath resolves to a real
// directory (the data root's parent), so the existence check passes and the
// request proceeds. This is the mechanism C1 rode in on, kept as a pin so a
// future "simplification" back to bucketExists-only is caught at the mechanism
// rather than only at the symptom.
func TestBucketNameGate_ExistsIsNotAGate(t *testing.T) {
	root := t.TempDir()
	dataDir := filepath.Join(root, "data")
	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		t.Fatal(err)
	}
	installC1Probe(t, dataDir)

	// dataDir's parent is root, which exists — so bucketExists("..") is true.
	if !bucketExists("..") {
		t.Skip("data-root parent does not exist in this environment; the mechanism pin needs a real parent")
	}
	if validBucket("..") {
		t.Fatal("validBucket(\"..\") must be false — otherwise the name gate cannot reject the traversal")
	}
	if validBucket(".") {
		t.Fatal("validBucket(\".\") must be false")
	}
	if !validBucket("good") {
		t.Fatal("validBucket(\"good\") must be true")
	}
	_ = filepath.Join(root, "unused")
}
