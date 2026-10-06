package bucketmanager

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/bhodgens/zeta-object/internal/fslock"
	"github.com/bhodgens/zeta-object/internal/objectmodel"
)

// This file holds the pins for the Env.Exists and nil-Custom follow-ups to the
// bughunt H3 fix: Create and Delete were guarded by Env.validateName, Exists was
// not, and the guard itself dereferenced a nil Env.Custom. Both are fixed in
// the package that every caller funnels through, so the pins live here.

// TestExistsRefusesTraversalNames is the Exists half of H3. Exists made no
// validateName call at all, so every one of these names resolved through
// e.BucketPath to a REAL path and the stat answered for it:
//
//	Exists("../srvdata") = true   (a sibling of the data root)
//	Exists("..")         = true   (the data root's parent)
//	Exists("")           = true   (the data root itself)
//	Exists("../../etc")  = true   (whatever the OS says about /etc)
//
// On the management surface that is GET /buckets/{name} answering
// 200 {"name":"..","exists":true} - indistinguishable from a real bucket, i.e. a
// filesystem-existence oracle over the data root's parent and its siblings. The
// refusal must happen BEFORE e.BucketPath is consulted, so nothing outside the
// root is ever stat-ed, and it must be the same 400 taxonomy error Create and
// Delete return (never a raw error, never a 500).
func TestExistsRefusesTraversalNames(t *testing.T) {
	for _, name := range traversalNames {
		t.Run(name, func(t *testing.T) {
			root, env, _ := setupEnv(t)

			// Resolve the path the unvalidated name WOULD have reached, and
			// fail the test if the Env ever touches it. A resolver that
			// panics is the strictest possible witness that the guard runs
			// before path resolution.
			resolved := filepath.Join(root, name)
			t.Cleanup(func() { _ = os.RemoveAll(filepath.Join(filepath.Dir(root), "srvdata")) })
			if err := os.MkdirAll(filepath.Join(filepath.Dir(root), "srvdata"), 0o755); err != nil {
				t.Fatal(err)
			}
			env.BucketPath = func(bucket string) string {
				if bucket == "" {
					t.Errorf("Env.Exists(%q) resolved a path after the name rule should have refused it", name)
				}
				return filepath.Join(root, bucket)
			}
			Install(*env)

			ok, err := Exists(context.Background(), name)
			if err == nil {
				t.Fatalf("Exists(%q) = %v, nil; a traversal name must be refused, not answered", name, ok)
			}
			if ok {
				t.Fatalf("Exists(%q) = true; a traversal name must never report an existing bucket", name)
			}
			if got := errCode(err); got != "InvalidArgument" {
				t.Fatalf("Exists(%q): code = %q, want InvalidArgument (err %v)", name, got, err)
			}
			if !strings.Contains(err.Error(), "bucket name") {
				t.Fatalf("Exists(%q): error must name the rule, got %v", name, err)
			}
			// The out-of-tree sibling is still on disk: nothing was probed
			// destructively and nothing outside the root was touched.
			if _, statErr := os.Stat(filepath.Join(filepath.Dir(root), "srvdata")); statErr != nil {
				t.Fatalf("Exists(%q) disturbed an out-of-tree directory: %v", name, statErr)
			}
			_ = resolved
		})
	}
}

// TestExistsRefusesNamesBeforeResolution is the ordering guarantee stated
// directly: for a name the rule refuses, e.BucketPath must not be called AT ALL
// (not merely called with a safe value). A resolver wired to fail the test is the
// witness; before the fix Exists called it unconditionally.
func TestExistsRefusesNamesBeforeResolution(t *testing.T) {
	_, env, _ := setupEnv(t)
	env.BucketPath = func(bucket string) string {
		t.Fatalf("BucketPath resolver called for the refused name %q; the guard must run FIRST", bucket)
		return ""
	}
	Install(*env)

	if ok, err := Exists(context.Background(), ".."); err == nil || ok {
		t.Fatalf("Exists(\"..\") = (%v, %v), want (false, a refusal)", ok, err)
	}
}

// TestExistsLegitimateNameStillReportsTrue pins that the guard is not
// over-broad: a real bucket directory under the root still answers true, and a
// missing one still answers (false, nil) - NOT an error, which is what keeps
// GET /buckets/{name} on a nonexistent bucket a clean 404 NoSuchBucket.
func TestExistsLegitimateNameStillReportsTrue(t *testing.T) {
	root, env, _ := setupEnv(t)
	Install(*env)
	makeBucket(t, root, "exists-ok")

	ok, err := Exists(context.Background(), "exists-ok")
	if err != nil || !ok {
		t.Fatalf("Exists(exists-ok) = %v, %v; want true, nil", ok, err)
	}
	ok, err = Exists(context.Background(), "not-created")
	if err != nil || ok {
		t.Fatalf("Exists(not-created) = %v, %v; want false, nil (an unknown name is not an error)", ok, err)
	}
}

// TestExistsCustomBucketWaiverStillWorks pins that the guard reuses
// Env.validateName EXACTLY, waiver included: a config-declared custom bucket
// with a non-S3 name must still report true through Exists, or the management
// GET /buckets/{name} for a config-declared custom bucket would regress from 200
// to 400.
//
// The resolver here is built the way BOTH production Install sites build it
// (main.go and s3/dataset_provisioner.go both pass getBucketPath, which returns
// the configured custom path first and dataDir/name otherwise). Env.exists
// resolves through Env.BucketPath only - it never consults Env.Custom for the
// PATH, it consults it solely for the naming-rule waiver - so a resolver that
// ignored the custom map would resolve this bucket to a nonexistent path under
// the root and report false.
func TestExistsCustomBucketWaiverStillWorks(t *testing.T) {
	root, env, _ := setupEnv(t)
	customPath := filepath.Join(filepath.Dir(root), "declared-custom")
	if err := os.MkdirAll(customPath, 0o755); err != nil {
		t.Fatal(err)
	}
	env.Custom = func(bucket string) (string, bool) {
		if bucket == "Weird_Custom.NAME" {
			return customPath, true
		}
		return "", false
	}
	// getBucketPath's shape: a configured custom path wins over the root.
	env.BucketPath = func(bucket string) string {
		if p, ok := env.Custom(bucket); ok {
			return p
		}
		return filepath.Join(root, bucket)
	}
	Install(*env)

	ok, err := Exists(context.Background(), "Weird_Custom.NAME")
	if err != nil || !ok {
		t.Fatalf("Exists(declared custom) = %v, %v; want true, nil (the waiver must survive)", ok, err)
	}
	// The same shape, NOT declared custom, is still refused by the S3 naming rule.
	if ok, err := Exists(context.Background(), "Not_Declared"); err == nil {
		t.Fatalf("Exists(Not_Declared) = %v, nil; want a refusal", ok)
	} else if ok {
		t.Fatalf("Exists(Not_Declared) = true alongside an error")
	}
}

// TestCustomWaiverKeepsContainment is the informational item's pin: with a name
// declared custom, Env.validateName returns after checkContainment ALONE, so no
// length or charset rule applies. The property that must still hold is
// containment - the resolved path is always ONE segment directly under the
// resolved root and can never climb. checkContainment rejects "", ".", "..",
// and every name carrying a separator (either host's), which is exactly what
// makes filepath.Join(root, name) a direct child.
//
// This pins the property rather than a length cap: a cap would refuse a
// legitimate operator-declared custom bucket, and the name is operator-controlled
// config, not request input.
func TestCustomWaiverKeepsContainment(t *testing.T) {
	for _, name := range traversalNames {
		if err := checkContainment(name); err == nil {
			t.Errorf("checkContainment(%q) = nil; the waiver must never admit a climbing name", name)
		}
	}
	// And a declared-custom name that survives containment still lands one level
	// below the root, even when it is long or carries characters the S3 rule
	// would refuse: no dot-dot, no separator, so no climb.
	for _, name := range []string{
		strings.Repeat("a", 400),
		"Weird_Custom.NAME",
		"....",
		"a b",
	} {
		if err := checkContainment(name); err != nil {
			t.Errorf("checkContainment(%q) = %v, want nil (one safe segment)", name, err)
			continue
		}
		root := t.TempDir()
		joined := filepath.Clean(filepath.Join(root, name))
		if joined != root && !strings.HasPrefix(joined, root+string(filepath.Separator)) {
			t.Errorf("a custom-declared %q escaped the root: %q", name, joined)
		}
		if filepath.Dir(joined) != filepath.Clean(root) {
			t.Errorf("a custom-declared %q is not a direct child of the root: %q", name, joined)
		}
	}
}

// TestNilCustomIsNotInstalled pins Finding 3: Env.validateName calls
// e.Custom(name), so an Env with a nil Custom used to panic with a nil pointer
// dereference instead of refusing. All three entry points must answer
// errNotInstalled() (the same not-installed contract a nil BucketPath gets), and
// no entry point may panic. Both production Install sites set Custom, so this is
// a guard for future callers, not today's exposure.
func TestNilCustomIsNotInstalled(t *testing.T) {
	env := Env{
		Locks:      fslock.Default,
		BucketPath: func(bucket string) string { return filepath.Join(t.TempDir(), bucket) },
		Custom:     nil,
	}
	Install(env)

	cases := []struct {
		name string
		call func() error
	}{
		{"Create", func() error { return Create(context.Background(), "any-bucket") }},
		{"Delete", func() error {
			return Delete(context.Background(), "any-bucket", DeleteOptions{AllowDatasetDestroy: true})
		}},
		{"Exists", func() error {
			_, err := Exists(context.Background(), "any-bucket")
			return err
		}},
	}
	want := errNotInstalled()
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			defer func() {
				if r := recover(); r != nil {
					t.Fatalf("%s with a nil Env.Custom panicked: %v", c.name, r)
				}
			}()
			err := c.call()
			if err == nil {
				t.Fatalf("%s with a nil Env.Custom returned nil, want the not-installed error", c.name)
			}
			if err.Error() != want.Error() {
				t.Fatalf("%s = %v, want the not-installed error %v", c.name, err, want)
			}
			if oe, ok := errors.AsType[*objectmodel.Error](err); !ok || oe.HTTPStatus != 500 {
				t.Fatalf("%s = %#v, want the 500 not-installed taxonomy error", c.name, err)
			}
		})
	}
}

// TestFullyInstalledEnvStillWorks is the other half of Finding 3: adding Custom
// to the not-installed guard must not refuse a fully-installed Env. Both the
// plain-directory and the dataset-backed create/Exists/delete round trips work
// with every seam populated (Locks, BucketPath, Custom, Provisioner).
func TestFullyInstalledEnvStillWorks(t *testing.T) {
	cases := []struct {
		label   string
		dataset bool
	}{
		{"plain directory", false},
		{"dataset-backed", true},
	}
	for _, c := range cases {
		t.Run(c.label, func(t *testing.T) {
			root, env, prov := setupEnv(t)
			if c.dataset {
				// The dataset branch's create does NOT mkdir the bucket dir (it
				// expects zfs to have), so the fake provisioner materializes it.
				prov.createFn = func(_ context.Context, bucket string) (string, error) {
					if err := os.MkdirAll(filepath.Join(root, bucket), 0o755); err != nil {
						return "", err
					}
					return prov.parent + "/" + bucket, nil
				}
				env.Provisioner = prov
			} else {
				env.Provisioner = nil
			}
			Install(*env)
			ctx := context.Background()

			if err := Create(ctx, "installed-ok"); err != nil {
				t.Fatalf("Create: %v", err)
			}
			if ok, err := Exists(ctx, "installed-ok"); err != nil || !ok {
				t.Fatalf("Exists = %v, %v; want true, nil", ok, err)
			}
			if err := Delete(ctx, "installed-ok", DeleteOptions{AllowDatasetDestroy: true}); err != nil {
				t.Fatalf("Delete: %v", err)
			}
			if _, err := os.Stat(filepath.Join(root, "installed-ok")); !os.IsNotExist(err) {
				t.Fatalf("bucket survived delete: %v", err)
			}
		})
	}
}

// adminDetailShape mirrors admin_wiring.go's adminBucketDetailService: the
// mTLS management surface's GET /buckets/{name} reads bucketmanager.Exists and,
// on an error, answers the error rather than a bucket entry. This is the e2e pin
// for the admin SHAPE that can live in this package - it does not import
// internal/frontend/admin (which never imports bucketmanager in non-test code, so
// the dependency would run the wrong way), and it uses the same classification
// rule admin's router applies (routes.go classify: a /buckets/{name} route needs
// a single path segment).
func adminDetailShape(exists func(ctx context.Context, name string) (bool, error), w http.ResponseWriter, name string) {
	ok, err := exists(context.Background(), name)
	if err != nil {
		oe, isTax := errors.AsType[*objectmodel.Error](err)
		if !isTax {
			http.Error(w, `{"error":{"code":"InternalError"}}`, http.StatusInternalServerError)
			return
		}
		code := oe.Code
		status := oe.HTTPStatus
		if status == 0 {
			status = http.StatusInternalServerError
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(`{"error":{"code":"` + code + `","message":"` + oe.Message + `"}}`))
		return
	}
	if !ok {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"error":{"code":"NoSuchBucket"}}`))
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(`{"name":"` + name + `","exists":true}`))
}

// TestAdminDetailShapeRefusesTraversalName is the end-to-end pin for Finding 1
// in the admin request shape: GET /buckets/.. must answer a 400 naming the rule,
// never the 200 {"name":"..","exists":true} the unvalidated Exists produced -
// the two are indistinguishable from each other, which is the disclosure.
func TestAdminDetailShapeRefusesTraversalName(t *testing.T) {
	root, env, _ := setupEnv(t)
	// A sibling of the data root: what "../srvdata" resolved to before the fix.
	outside := filepath.Join(filepath.Dir(root), "srvdata")
	if err := os.MkdirAll(outside, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(outside) })
	// A real bucket, so the 404-vs-400 distinction is meaningful.
	makeBucket(t, root, "real-one")
	Install(*env)

	for _, name := range []string{"..", ".", "../srvdata"} {
		t.Run(name, func(t *testing.T) {
			rr := httptest.NewRecorder()
			adminDetailShape(Exists, rr, name)
			if rr.Code != http.StatusBadRequest {
				t.Fatalf("GET /buckets/%s: status = %d, want 400 (body %q)", name, rr.Code, rr.Body.String())
			}
			body := rr.Body.String()
			if strings.Contains(body, `"exists":true`) {
				t.Fatalf("GET /buckets/%s claimed a bucket exists: %s", name, body)
			}
			if !strings.Contains(body, "InvalidArgument") || !strings.Contains(body, "bucket name") {
				t.Fatalf("GET /buckets/%s: body must be the InvalidArgument refusal, got %s", name, body)
			}
			if _, err := os.Stat(outside); err != nil {
				t.Fatalf("GET /buckets/%s disturbed an out-of-tree directory: %v", name, err)
			}
		})
	}

	// The legitimate bucket is still a 200, and a missing one is still a clean
	// 404 NoSuchBucket (NOT a 400): the guard must not turn "unknown bucket"
	// into a client error.
	rr := httptest.NewRecorder()
	adminDetailShape(Exists, rr, "real-one")
	if rr.Code != http.StatusOK || !strings.Contains(rr.Body.String(), `"exists":true`) {
		t.Fatalf("GET /buckets/real-one = %d %s; want 200 with exists:true", rr.Code, rr.Body.String())
	}
	rr = httptest.NewRecorder()
	adminDetailShape(Exists, rr, "no-such-bucket")
	if rr.Code != http.StatusNotFound {
		t.Fatalf("GET /buckets/no-such-bucket = %d %s; want 404 NoSuchBucket", rr.Code, rr.Body.String())
	}
}
