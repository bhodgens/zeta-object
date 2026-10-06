package bucketmanager

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// traversalNames are the names that must never reach the filesystem: each one
// escapes (or aliases) the data root once filepath.Join cleans it.
var traversalNames = []string{
	"..",
	"../x",
	"../../etc",
	".",
	"./x",
	"",
	"a/b",
	"a\\b",
	"/abs",
	"a/../..",
}

// TestDeleteRefusesTraversalNames is the regression guard for H3: the
// bucketmanager Delete entry point validated nothing, so
// Delete(ctx, "../srvdata") resolved to filepath.Join(dataDir, "../srvdata") —
// a directory OUTSIDE the data root — and RemoveAll removed it. The refusal
// must happen before any stat/read/remove and the out-of-tree directory must
// still be there afterwards.
func TestDeleteRefusesTraversalNames(t *testing.T) {
	for _, name := range traversalNames {
		t.Run(name, func(t *testing.T) {
			root, env, prov := setupEnv(t)
			Install(*env)

			// A sibling directory of the data root: exactly what "../x"
			// resolved to before the fix.
			outside := filepath.Join(filepath.Dir(root), "srvdata")
			if err := os.MkdirAll(outside, 0o755); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = os.RemoveAll(outside) })

			err := Delete(context.Background(), name, DeleteOptions{AllowDatasetDestroy: true})
			if err == nil {
				t.Fatalf("Delete(%q) succeeded; a traversal name must be refused", name)
			}
			if got := errCode(err); got != "InvalidArgument" {
				t.Fatalf("Delete(%q): code = %q, want InvalidArgument (err %v)", name, got, err)
			}
			if !strings.Contains(err.Error(), "bucket name") {
				t.Fatalf("Delete(%q): error must name the rule, got %v", name, err)
			}
			if _, statErr := os.Stat(outside); statErr != nil {
				t.Fatalf("Delete(%q) removed an out-of-tree directory: %v", name, statErr)
			}
			// "" and "." resolve to the data root itself; removing it would be
			// a whole-root wipe, so its survival is part of the same guard.
			if _, statErr := os.Stat(root); statErr != nil {
				t.Fatalf("Delete(%q) removed the data root itself: %v", name, statErr)
			}
			if len(prov.existsCalls)+len(prov.destroyCalls) != 0 {
				t.Fatalf("Delete(%q) reached the provisioner: %v", name, prov)
			}
		})
	}
}

// TestCreateRefusesTraversalNames is the Create half of H3: Create had the
// same gap, so a traversal name created directories outside the data root.
func TestCreateRefusesTraversalNames(t *testing.T) {
	for _, name := range traversalNames {
		t.Run(name, func(t *testing.T) {
			root, env, prov := setupEnv(t)
			Install(*env)

			parent := filepath.Dir(root)
			before, beforeErr := os.ReadDir(parent)
			if beforeErr != nil {
				t.Fatal(beforeErr)
			}

			err := Create(context.Background(), name)
			if err == nil {
				t.Fatalf("Create(%q) succeeded; a traversal name must be refused", name)
			}
			if got := errCode(err); got != "InvalidArgument" {
				t.Fatalf("Create(%q): code = %q, want InvalidArgument (err %v)", name, got, err)
			}
			if len(prov.createCalls) != 0 {
				t.Fatalf("Create(%q) reached the provisioner: %v", name, prov.createCalls)
			}
			after, afterErr := os.ReadDir(parent)
			if afterErr != nil {
				t.Fatal(afterErr)
			}
			if len(after) != len(before) {
				t.Fatalf("Create(%q) changed the data root's parent: %v -> %v", name, before, after)
			}
		})
	}
}

// TestValidateNameIsS3Rule pins that the shared rule is the SAME rule the S3
// frontend applies (not a traversal-only special case): the S3 naming limits
// and the IP-address ban all hold here too.
func TestValidateNameIsS3Rule(t *testing.T) {
	valid := []string{"abc", "a-bucket-1.test-2", strings.Repeat("a", 63), "123-bucket", "bucket-123"}
	for _, name := range valid {
		if err := ValidateName(name); err != nil {
			t.Errorf("ValidateName(%q) = %v, want nil", name, err)
		}
	}
	invalid := map[string]string{
		"":                      "must not be empty",
		".":                     "relative path segment",
		"..":                    "relative path segment",
		"ab":                    "between 3 and 63",
		strings.Repeat("a", 64): "between 3 and 63",
		"Abc":                   "start with a lowercase letter or number",
		"abc-":                  "end with a lowercase letter or number",
		"a_b":                   "can only contain",
		"my..bucket":            "consecutive periods",
		"192.168.1.1":           "IP address",
		"a/b":                   "path separator",
	}
	for name, want := range invalid {
		err := ValidateName(name)
		if err == nil {
			t.Errorf("ValidateName(%q) = nil, want an error", name)
			continue
		}
		if !strings.Contains(err.Error(), want) {
			t.Errorf("ValidateName(%q) = %v, want it to mention %q", name, err, want)
		}
	}
}

// TestValidateNameExemptsConfiguredCustomBucket pins the custom-bucket
// exemption the S3 frontend already has (its validBucket gate). A
// config-declared name is operator-controlled, and s3.deleteBucketHandler
// passes such a name to the manager untouched — so the manager must admit it
// too, or the S3 wire would change from today's 403/404 to a 400. A name that
// is NOT declared custom still gets the full rule.
func TestValidateNameExemptsConfiguredCustomBucket(t *testing.T) {
	_, env, _ := setupEnv(t)
	env.Custom = func(bucket string) (string, bool) {
		if bucket == "Configured_Custom" {
			return "/mnt/elsewhere", true
		}
		return "", false
	}
	Install(*env)

	// The exported rule itself has NO exemption (it is the bare S3 rule).
	if err := ValidateName("Configured_Custom"); err == nil {
		t.Fatal("ValidateName must not apply the custom exemption")
	}
	// The entry-point guard does.
	if err := env.validateName("Configured_Custom"); err != nil {
		t.Fatalf("config-declared custom bucket rejected by the entry guard: %v", err)
	}
	if err := env.validateName("Not_Configured"); err == nil {
		t.Fatal("a non-custom name must NOT be exempt from the naming rule")
	}
}

// TestCreateDeleteRoundTripLegitimate pins that the fix is not over-broad: a
// legitimate name still creates, lists, and deletes.
func TestCreateDeleteRoundTripLegitimate(t *testing.T) {
	root, env, _ := setupEnv(t)
	Install(*env)
	ctx := context.Background()

	if err := Create(ctx, "legit-bucket"); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if ok, err := Exists(ctx, "legit-bucket"); err != nil || !ok {
		t.Fatalf("Exists = %v, %v; want true", ok, err)
	}
	buckets, err := List(ctx)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(buckets) != 1 || buckets[0].Name != "legit-bucket" {
		t.Fatalf("List = %+v, want [legit-bucket]", buckets)
	}
	if err := Delete(ctx, "legit-bucket", DeleteOptions{}); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "legit-bucket")); !os.IsNotExist(err) {
		t.Fatalf("bucket dir survived delete: %v", err)
	}
}

// TestDeleteCustomInvalidNameStillRefused pins the ordering guarantee that
// actually matters for containment: an invalid name is refused BEFORE the
// custom-bucket 403 guard, so a traversal name that happens to be a
// config-declared custom bucket is still rejected on its face and never
// reaches a path-derived branch.
//
// (The custom exemption in Env.validateName is a name-LIST membership check —
// e.Custom(name) — so it cannot make ".." valid; it only spares a name that
// fails the naming rule while being legitimately declared in config.)
func TestDeleteCustomInvalidNameStillRefused(t *testing.T) {
	_, env, prov := setupEnv(t)
	env.Custom = func(name string) (string, bool) {
		if name == ".." {
			return "/mnt/elsewhere", true
		}
		return "", false
	}
	Install(*env)

	err := Delete(context.Background(), "..", DeleteOptions{AllowDatasetDestroy: true})
	if got := errCode(err); got != "InvalidArgument" {
		t.Fatalf("err = %v (code %q), want InvalidArgument", err, got)
	}
	if len(prov.existsCalls)+len(prov.destroyCalls) != 0 {
		t.Fatalf("provisioner touched: %v", prov)
	}
}
