package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"mini-s3/internal/backend"
	"mini-s3/internal/objectmodel"
)

// TestBuildBackendLookup pins leaf 03's startup installer: registry-driven
// construction, per-bucket backend selection, memoization per (name, root),
// and fail-loud on unknown backend names.
func TestBuildBackendLookup(t *testing.T) {
	cases := []struct {
		name    string
		cfg     ServerConfig
		verify  func(t *testing.T, lookup func(string) (backend.Backend, error))
		wantErr string // empty = expect success
	}{
		{
			name: "absent keys → all fs, default root",
			cfg:  ServerConfig{DataDir: t.TempDir()},
			verify: func(t *testing.T, lookup func(string) (backend.Backend, error)) {
				b, err := lookup("anybucket")
				if err != nil || b == nil {
					t.Fatalf("default fs lookup failed: %v", err)
				}
			},
		},
		{
			name: "explicit fs per bucket",
			cfg: func() ServerConfig {
				root := t.TempDir()
				return ServerConfig{
					DataDir: t.TempDir(),
					Buckets: map[string]string{"photos": root}, // legacy form
				}
			}(),
			verify: func(t *testing.T, lookup func(string) (backend.Backend, error)) {
				if _, err := lookup("photos"); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "unknown backend type → startup error, no fallback",
			cfg: func() ServerConfig {
				return ServerConfig{
					DataDir: t.TempDir(),
					Buckets: map[string]string{"photos": t.TempDir()},
					// object-form selection of an unregistered name
					BucketBackends: map[string]string{"photos": "nope"},
				}
			}(),
			wantErr: `unknown backend type "nope"`,
		},
		{
			name: "two buckets different roots → separate FS instances",
			cfg: func() ServerConfig {
				rootA, rootB := t.TempDir(), t.TempDir()
				placedRoots["alpha"] = rootA
				placedRoots["beta"] = rootB
				return ServerConfig{
					DataDir: t.TempDir(),
					Buckets: map[string]string{"alpha": rootA, "beta": rootB},
				}
			}(),
			verify: func(t *testing.T, lookup func(string) (backend.Backend, error)) {
				// Memoization: repeated lookups share one instance.
				a1, err := lookup("alpha")
				if err != nil {
					t.Fatal(err)
				}
				a2, err := lookup("alpha")
				if err != nil {
					t.Fatal(err)
				}
				if a1 != a2 {
					t.Fatal("same (name, root) must memoize to one Backend instance")
				}
				// Placement proof: write through the Backend interface and
				// stat the expected path under the configured root.
				provePlacement(t, lookup, "alpha")
				provePlacement(t, lookup, "beta")
			},
		},
		{
			name: "two buckets same root → one shared instance",
			cfg: func() ServerConfig {
				shared := t.TempDir()
				return ServerConfig{
					DataDir: t.TempDir(),
					Buckets: map[string]string{"gamma1": shared, "gamma2": shared},
				}
			}(),
			verify: func(t *testing.T, lookup func(string) (backend.Backend, error)) {
				g1, err := lookup("gamma1")
				if err != nil {
					t.Fatal(err)
				}
				g2, err := lookup("gamma2")
				if err != nil {
					t.Fatal(err)
				}
				if g1 != g2 {
					t.Fatal("buckets sharing (name, root) must share one Backend instance")
				}
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			lookup, err := buildBackendLookup(tc.cfg)
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("want err %q, got %v", tc.wantErr, err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			tc.verify(t, lookup)
		})
	}
}

// provePlacement writes a object via the resolved Backend and asserts the
// bytes landed under the bucket's configured root (consumed via the
// interface only — no type assertion).
func provePlacement(t *testing.T, lookup func(string) (backend.Backend, error), bucket string) {
	t.Helper()
	b, err := lookup(bucket)
	if err != nil {
		t.Fatalf("lookup(%s): %v", bucket, err)
	}
	root := placedRoots[bucket]
	key := "proof/object.txt"
	if _, err := b.Put(context.Background(), bucket, key, strings.NewReader("placed"), 6, objectmodel.PutOptions{}); err != nil {
		t.Fatalf("Put via backend: %v", err)
	}
	dataPath := filepath.Join(root, bucket, key)
	if _, err := os.Stat(dataPath); err != nil {
		t.Fatalf("object not placed at %s: %v", dataPath, err)
	}
}

// placedRoots is filled by the two-roots case before provePlacement runs.
var placedRoots = map[string]string{}
