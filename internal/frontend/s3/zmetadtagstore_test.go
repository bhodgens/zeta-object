package s3

// zmetadtagstore_test.go — the zmetadTagStore tests (zfs-metadata#13
// consumer leaf). NO real zmetad: the zmetadTagRunner seam is scripted
// per test (the zfsdatasets_test.go pattern) and the object-id resolver
// is a canned func. Covers: argv shapes for --tag-get/--tag-set/
// --tag-clear, first-= output parsing (happy + values containing '=' +
// blank lines + non-pair lines), the ErrNoSuchKey mapping of an
// unresolvable object id, the unavailable-error mapping of runner
// failures (NEVER a sidecar fallback), the empty-set Put -> --tag-clear
// contract, and the parseZmetadTagGetOutput unit cases.

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/bhodgens/zeta-object/internal/metadata"
	"github.com/bhodgens/zeta-object/internal/objectmodel"
)

// scriptZmetadTagRunner replaces the zmetadTagRunner seam with a fake
// recording every argv and returning the configured stdout/stderr/error.
func scriptZmetadTagRunner(t *testing.T, stdout, stderr string, err error) *[][]string {
	t.Helper()
	got := &[][]string{}
	orig := zmetadTagRunner
	zmetadTagRunner = func(ctx context.Context, binary string, args ...string) ([]byte, string, error) {
		*got = append(*got, append([]string{binary}, args...))
		return []byte(stdout), stderr, err
	}
	t.Cleanup(func() { zmetadTagRunner = orig })
	return got
}

// testZmetadTagStore builds a store with a canned object-id resolver
// (key -> 42) so no DB is touched.
func testZmetadTagStore() *zmetadTagStore {
	return &zmetadTagStore{
		binary:  "zmetad",
		dbPath:  "/tmp/unused.db",
		dataset: "testpool/zval/bkt",
		resolveObjectID: func(context.Context, string, string) (uint64, error) {
			return 42, nil
		},
	}
}

func TestParseZmetadTagGetOutput(t *testing.T) {
	cases := []struct {
		name   string
		stdout string
		want   map[string]string
	}{
		{"empty output is empty non-nil map", "", map[string]string{}},
		{"single pair", "team=infra\n", map[string]string{"team": "infra"}},
		{"multiple pairs", "a=1\nb=2\n", map[string]string{"a": "1", "b": "2"}},
		{"first-= splits, value keeps =", "k=v=with=equals\n", map[string]string{"k": "v=with=equals"}},
		{"blank lines skipped", "\na=1\n\n", map[string]string{"a": "1"}},
		{"pair without = skipped", "nonsense\na=1\n", map[string]string{"a": "1"}},
		{"empty value", "k=\n", map[string]string{"k": ""}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := parseZmetadTagGetOutput([]byte(tc.stdout))
			if got == nil {
				t.Fatal("parse must never return nil")
			}
			if len(got) != len(tc.want) {
				t.Fatalf("got %v, want %v", got, tc.want)
			}
			for k, v := range tc.want {
				if got[k] != v {
					t.Errorf("tag %q: got %q want %q", k, got[k], v)
				}
			}
		})
	}
}

func TestZmetadTagStore_GetArgvAndParse(t *testing.T) {
	store := testZmetadTagStore()
	argvs := scriptZmetadTagRunner(t, "env=prod\nteam=core\n", "", nil)

	tags, err := store.Get("obj.txt")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if len(tags) != 2 || tags["env"] != "prod" || tags["team"] != "core" {
		t.Fatalf("tags = %v", tags)
	}
	if len(*argvs) != 1 {
		t.Fatalf("expected 1 exec, got %v", *argvs)
	}
	argv := (*argvs)[0]
	// The -d <dbPath> pair rides every tag exec (without it the CLI opens
	// the compiled default /var/lib/zfs/zmetad.db — found live on zfs-meta).
	want := []string{"zmetad", "--tag-get", "testpool/zval/bkt", "--tag-object", "42", "-d", "/tmp/unused.db"}
	if len(argv) != len(want) {
		t.Fatalf("argv = %v, want %v", argv, want)
	}
	for i := range want {
		if argv[i] != want[i] {
			t.Fatalf("argv = %v, want %v", argv, want)
		}
	}
}

func TestZmetadTagStore_PutArgvReplaceSet(t *testing.T) {
	store := testZmetadTagStore()
	argvs := scriptZmetadTagRunner(t, "2 tag(s) set for testpool/zval/bkt object 42\n", "", nil)

	if err := store.Put("obj.txt", map[string]string{"a": "1", "b": "x=y"}); err != nil {
		t.Fatalf("Put: %v", err)
	}
	if len(*argvs) != 1 {
		t.Fatalf("expected 1 exec, got %v", *argvs)
	}
	argv := (*argvs)[0]
	joined := strings.Join(argv, " ")
	// Fixed head (mode, dataset, object), then one repeatable --tag per pair.
	if argv[0] != "zmetad" || argv[1] != "--tag-set" || argv[2] != "testpool/zval/bkt" || argv[3] != "--tag-object" || argv[4] != "42" {
		t.Fatalf("argv head = %v", argv)
	}
	if !strings.Contains(joined, "--tag a=1") || !strings.Contains(joined, "--tag b=x=y") {
		t.Fatalf("tag pairs missing from argv: %v", argv)
	}
}

func TestZmetadTagStore_PutEmptySetClears(t *testing.T) {
	store := testZmetadTagStore()
	argvs := scriptZmetadTagRunner(t, "tags cleared for testpool/zval/bkt object 42\n", "", nil)

	if err := store.Put("obj.txt", nil); err != nil {
		t.Fatalf("Put(empty): %v", err)
	}
	argv := (*argvs)[0]
	if argv[1] != "--tag-clear" { // argv[0] is the binary
		t.Fatalf("empty Put must exec --tag-clear, got %v", argv)
	}
}

func TestZmetadTagStore_DeleteArgv(t *testing.T) {
	store := testZmetadTagStore()
	argvs := scriptZmetadTagRunner(t, "tags cleared for testpool/zval/bkt object 42\n", "", nil)

	if err := store.Delete("obj.txt"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	argv := (*argvs)[0]
	want := []string{"zmetad", "--tag-clear", "testpool/zval/bkt", "--tag-object", "42"}
	for i := range want {
		if argv[i] != want[i] {
			t.Fatalf("argv = %v, want %v", argv, want)
		}
	}
}

func TestZmetadTagStore_UnresolvableObjectIDIsNoSuchKey(t *testing.T) {
	store := testZmetadTagStore()
	store.resolveObjectID = func(context.Context, string, string) (uint64, error) {
		return 0, &metadata.ObjectIDNotFoundError{Dataset: "testpool/zval/bkt", Path: "ghost.txt"}
	}
	scriptZmetadTagRunner(t, "", "", nil)

	for name, fn := range map[string]func() error{
		"Get":    func() error { _, err := store.Get("ghost.txt"); return err },
		"Put":    func() error { return store.Put("ghost.txt", map[string]string{"k": "v"}) },
		"Delete": func() error { return store.Delete("ghost.txt") },
	} {
		t.Run(name, func(t *testing.T) {
			err := fn()
			var omErr *objectmodel.Error
			if !errors.As(err, &omErr) || omErr.Code != objectmodel.CodeNoSuchKey {
				t.Fatalf("expected NoSuchKey, got %v", err)
			}
		})
	}
}

func TestZmetadTagStore_ExecFailureIsUnavailableNeverSilent(t *testing.T) {
	store := testZmetadTagStore()
	scriptZmetadTagRunner(t, "", "cannot get tags: database is locked", errors.New("exit status 1"))

	if _, err := store.Get("obj.txt"); err == nil {
		t.Fatal("Get must fail on a zmetad failure")
	} else {
		if _, ok := errors.AsType[*zmetadTagUnavailableError](err); !ok {
			t.Fatalf("expected zmetadTagUnavailableError, got %v", err)
		}
		if !strings.Contains(err.Error(), "database is locked") {
			t.Errorf("stderr must ride in the error: %v", err)
		}
	}
	if err := store.Put("obj.txt", map[string]string{"k": "v"}); err == nil {
		t.Fatal("Put must fail on a zmetad failure")
	}
	if err := store.Delete("obj.txt"); err == nil {
		t.Fatal("Delete must fail on a zmetad failure")
	}
}

func TestZmetadTagStore_BadKeyIsNoSuchKeyWithoutExec(t *testing.T) {
	store := testZmetadTagStore()
	argvs := scriptZmetadTagRunner(t, "", "", nil)

	if _, err := store.Get("../escape"); err == nil {
		t.Fatal("invalid key must be rejected")
	}
	if len(*argvs) != 0 {
		t.Fatalf("no exec may happen on a rejected key, got %v", *argvs)
	}
}

func TestFailingTagStore_FailsEveryOp(t *testing.T) {
	store := failingTagStore{cause: errors.New("probe timed out")}
	if _, err := store.Get("k"); err == nil {
		t.Error("Get must fail")
	}
	if err := store.Put("k", nil); err == nil {
		t.Error("Put must fail")
	}
	if err := store.Delete("k"); err == nil {
		t.Error("Delete must fail")
	}
}
