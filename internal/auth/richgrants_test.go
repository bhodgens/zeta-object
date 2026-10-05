// richgrants_test.go — rich grant expressions (auth extensions leaf 09):
// ParseGrantValue (both forms, every failure mode), MatchesObject (prefix
// boundary cases — 'photos/2024/*' must NOT match 'photos/20240/' or
// 'photos/2024'), ActiveAt (inclusive edges), AuthorizeOp (nil-table ⇒ v1
// equivalence golden table vs CanRead/CanWrite; positive/negative/union),
// and the floor-union derivation.
package auth_test

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/bhodgens/zeta-object/internal/auth"
)

// str turns a Go string into the raw-JSON carrier the dual-form config
// uses (legacy shorthand form).
func str(s string) json.RawMessage { return json.RawMessage(`"` + s + `"`) }

// obj turns a JSON object literal into the raw-JSON carrier (rich form).
func obj(s string) json.RawMessage { return json.RawMessage(s) }

// TestParseGrantValueLegacyForm pins the frozen string vocabulary: same
// parsing, same error message shape as the pre-leaf parser.
func TestParseGrantValueLegacyForm(t *testing.T) {
	e, err := auth.ParseGrantValue("id", "photos", str("readonly"))
	if err != nil {
		t.Fatalf("readonly: %v", err)
	}
	if !e.Allows(auth.OpRead) || e.Allows(auth.OpWrite) {
		t.Errorf("readonly ops wrong: %v", e.Ops)
	}
	if e.Pattern != "photos" {
		t.Errorf("pattern = %q", e.Pattern)
	}
	e, err = auth.ParseGrantValue("id", "*", str("readwrite"))
	if err != nil {
		t.Fatalf("readwrite: %v", err)
	}
	if !e.Allows(auth.OpRead) || !e.Allows(auth.OpWrite) {
		t.Errorf("readwrite ops wrong: %v", e.Ops)
	}
	// Unknown value: the frozen error message (registry_test.go pins it
	// end-to-end; here the parser itself).
	_, err = auth.ParseGrantValue("id", "b", str("admin"))
	if err == nil || !strings.Contains(err.Error(), `unknown grant value "admin"`) {
		t.Errorf("err = %v, want unknown grant value", err)
	}
}

// TestParseGrantValueRichForm pins the object form's happy path.
func TestParseGrantValueRichForm(t *testing.T) {
	e, err := auth.ParseGrantValue("ci", "photos/2024/*",
		obj(`{"ops":["read","write","list"],"not-before":"2026-09-01T00:00:00Z","not-after":"2026-12-31T23:59:59Z"}`))
	if err != nil {
		t.Fatalf("rich parse: %v", err)
	}
	if e.Pattern != "photos/2024/*" {
		t.Errorf("pattern = %q", e.Pattern)
	}
	for _, op := range []auth.Op{auth.OpRead, auth.OpWrite, auth.OpList} {
		if !e.Allows(op) {
			t.Errorf("op %s missing", op)
		}
	}
	if e.Allows(auth.OpDelete) || e.Allows(auth.OpCreate) {
		t.Errorf("unexpected ops: %v", e.Ops)
	}
	if e.NotBefore.IsZero() || e.NotAfter.IsZero() {
		t.Errorf("window lost: %+v", e)
	}
	// Timeless rich entry: both bounds zero.
	e, err = auth.ParseGrantValue("ci", "b/*", obj(`{"ops":["delete"]}`))
	if err != nil {
		t.Fatalf("rich parse: %v", err)
	}
	if !e.NotBefore.IsZero() || !e.NotAfter.IsZero() {
		t.Errorf("bounds should be zero: %+v", e)
	}
}

// TestParseGrantValueFailureModes pins every fail-loud branch, each naming
// the offender.
func TestParseGrantValueFailureModes(t *testing.T) {
	cases := []struct {
		name string
		key  string
		raw  json.RawMessage
		want string
	}{
		{"unknown op", "b", obj(`{"ops":["admin"]}`), `unknown op "admin"`},
		{"empty ops", "b", obj(`{"ops":[]}`), "non-empty"},
		{"ops not array", "b", obj(`{"ops":"read"}`), "ops"},
		{"bad not-before", "b", obj(`{"ops":["read"],"not-before":"yesterday"}`), "not-before"},
		{"bad not-after", "b", obj(`{"ops":["read"],"not-after":"next week"}`), "not-after"},
		{"not-after <= not-before", "b", obj(`{"ops":["read"],"not-before":"2026-10-02T00:00:00Z","not-after":"2026-10-01T00:00:00Z"}`), "must be after"},
		{"not-after == not-before", "b", obj(`{"ops":["read"],"not-before":"2026-10-01T00:00:00Z","not-after":"2026-10-01T00:00:00Z"}`), "must be after"},
		{"unknown json key", "b", obj(`{"ops":["read"],"prefix":"x"}`), "unknown field"},
		{"non-string non-object", "b", obj(`42`), "string"},
		{"empty bucket key", "", str("readonly"), "must not be empty"},
		{"mid-string glob", "photos/*/2024", str("readwrite"), "trailing"},
		{"double star", "b/**", str("readwrite"), "only wildcard"},
		{"star in bucket name", "ph*tos", str("readwrite"), "trailing star only"},
		{"prefix without star", "photos/2024/", str("readwrite"), "trailing"},
		{"empty bucket component", "/x", str("readwrite"), "bucket component"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := auth.ParseGrantValue("id", tc.key, tc.raw)
			if err == nil {
				t.Fatalf("expected error, got nil")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error %q missing %q", err.Error(), tc.want)
			}
			if !strings.Contains(err.Error(), `identity "id"`) {
				t.Errorf("error must name the identity: %v", err)
			}
		})
	}
	// ,= in the bucket component (SFTP round-trip safety).
	_, err := auth.ParseGrantValue("id", "a,b/x*", str("readwrite"))
	if err == nil || !strings.Contains(err.Error(), `must not contain`) {
		t.Errorf("comma bucket err = %v", err)
	}
}

// TestMatchesObject pins the pattern grammar and the '/'-boundary rule.
func TestMatchesObject(t *testing.T) {
	mk := func(pattern string) auth.GrantExpr {
		e, err := auth.ParseGrantValue("id", pattern, str("readwrite"))
		if err != nil {
			t.Fatalf("pattern %q: %v", pattern, err)
		}
		return e
	}
	cases := []struct {
		pattern string
		bucket  string
		key     string
		want    bool
	}{
		{"*", "any", "k/x", true},
		{"*", "any", "", true},
		{"photos", "photos", "a.jpg", true},
		{"photos", "photos", "", true},
		{"photos", "other", "a.jpg", false},
		{"photos/2024/*", "photos", "2024/a.jpg", true},
		{"photos/2024/*", "photos", "2024/2025/deep.jpg", true},
		// THE boundary cases: "2024" must not match "20240/" or bare "2024".
		{"photos/2024/*", "photos", "20240/x.jpg", false},
		{"photos/2024/*", "photos", "2024", false},
		{"photos/2024/*", "photos", "20245/y.jpg", false},
		// Exact key prefix without slash IS allowed when followed by '/'.
		{"photos/2024/*", "photos", "2024/", true},
		{"photos/2024/*", "other", "2024/a.jpg", false},
		{"photos/*", "photos", "anything", true},
		{"photos/*", "photos", "", true},
		{"photos/*", "photos2", "x", false},
	}
	for _, tc := range cases {
		t.Run(tc.pattern+"|"+tc.bucket+"/"+tc.key, func(t *testing.T) {
			if got := mk(tc.pattern).MatchesObject(tc.bucket, tc.key); got != tc.want {
				t.Fatalf("MatchesObject(%q, %q) = %v, want %v", tc.bucket, tc.key, got, tc.want)
			}
		})
	}
}

// TestActiveAtEdges pins inclusivity at BOTH ends: not-before ≤ now ≤
// not-after.
func TestActiveAtEdges(t *testing.T) {
	nb := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	na := time.Date(2026, 12, 31, 23, 59, 59, 0, time.UTC)
	e := auth.GrantExpr{NotBefore: nb, NotAfter: na}
	cases := []struct {
		name string
		t    time.Time
		want bool
	}{
		{"exactly not-before", nb, true},
		{"exactly not-after", na, true},
		{"inside", nb.Add(24 * time.Hour), true},
		{"one ns before", nb.Add(-time.Nanosecond), false},
		{"one ns after", na.Add(time.Nanosecond), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := e.ActiveAt(tc.t); got != tc.want {
				t.Fatalf("ActiveAt = %v, want %v", got, tc.want)
			}
		})
	}
	// Zero bounds are unbounded.
	open := auth.GrantExpr{}
	if !open.ActiveAt(time.Date(1999, 1, 1, 0, 0, 0, 0, time.UTC)) ||
		!open.ActiveAt(time.Date(2999, 1, 1, 0, 0, 0, 0, time.UTC)) {
		t.Error("zero bounds must be unbounded")
	}
}

// TestAuthorizeOpNilTableV1Equivalence is the golden table: with no rich
// table, AuthorizeOp answers exactly what CanRead/CanWrite would.
func TestAuthorizeOpNilTableV1Equivalence(t *testing.T) {
	now := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	ids := []struct {
		name string
		id   auth.Identity
	}{
		{"wildcard rw", auth.Identity{BucketGrants: map[string]auth.Grant{"*": {Read: true, Write: true}}}},
		{"readonly photos", auth.Identity{BucketGrants: map[string]auth.Grant{"photos": {Read: true}}}},
		{"wildcard ro + photos none", auth.Identity{BucketGrants: map[string]auth.Grant{"*": {Read: true}, "photos": {}}}},
		{"nil grants", auth.Identity{}},
	}
	ops := []auth.Op{auth.OpRead, auth.OpWrite, auth.OpList, auth.OpDelete, auth.OpCreate}
	buckets := []string{"photos", "other"}
	for _, tc := range ids {
		for _, op := range ops {
			for _, bucket := range buckets {
				t.Run(tc.name+"|"+string(op)+"|"+bucket, func(t *testing.T) {
					var wantV1 bool
					switch op {
					case auth.OpWrite, auth.OpDelete, auth.OpCreate:
						wantV1 = tc.id.CanWrite(bucket)
					default:
						wantV1 = tc.id.CanRead(bucket)
					}
					got := auth.AuthorizeOp(tc.id, op, bucket, "k", now)
					if got != wantV1 {
						t.Fatalf("AuthorizeOp = %v, v1 answer = %v", got, wantV1)
					}
				})
			}
		}
	}
}

// TestAuthorizeOpRichTable pins the rich-table decision: allow iff SOME
// entry matches && active && allows; v1 false on covered buckets does not
// leak through the zero-bit marker.
func TestAuthorizeOpRichTable(t *testing.T) {
	now := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	past := now.Add(-24 * time.Hour)
	future := now.Add(24 * time.Hour)

	withRich := func(exprs []auth.GrantExpr) auth.Identity {
		return auth.Identity{
			AccessKeyID: "ak",
			BucketGrants: map[string]auth.Grant{
				"photos": {}, // zero-bit marker (time-scoped floor)
			},
		}.WithRichGrants(exprs)
	}
	active := func() auth.GrantExpr {
		return auth.GrantExpr{
			Pattern:   "photos/2024/*",
			Ops:       map[auth.Op]bool{auth.OpRead: true, auth.OpWrite: true},
			NotBefore: past,
			NotAfter:  future,
		}
	}

	id := withRich([]auth.GrantExpr{active()})
	if !auth.AuthorizeOp(id, auth.OpRead, "photos", "2024/a.jpg", now) {
		t.Error("in-window read under prefix denied")
	}
	if !auth.AuthorizeOp(id, auth.OpWrite, "photos", "2024/b.jpg", now) {
		t.Error("in-window write under prefix denied")
	}
	if auth.AuthorizeOp(id, auth.OpRead, "photos", "20240/x.jpg", now) {
		t.Error("boundary miss must deny")
	}
	if auth.AuthorizeOp(id, auth.OpWrite, "photos", "2023/old.jpg", now) {
		t.Error("outside prefix must deny")
	}
	if auth.AuthorizeOp(id, auth.OpDelete, "photos", "2024/a.jpg", now) {
		t.Error("op not in entry must deny")
	}
	// Time windows: expired and not-yet-active entries never match.
	expired := active()
	expired.NotBefore, expired.NotAfter = now.Add(-48*time.Hour), past
	if auth.AuthorizeOp(withRich([]auth.GrantExpr{expired}), auth.OpRead, "photos", "2024/a.jpg", now) {
		t.Error("expired window must deny")
	}
	staged := active()
	staged.NotBefore, staged.NotAfter = future, future.Add(24*time.Hour)
	if auth.AuthorizeOp(withRich([]auth.GrantExpr{staged}), auth.OpRead, "photos", "2024/a.jpg", now) {
		t.Error("future window must deny")
	}
	// A rich entry does NOT extend to buckets it doesn't cover: the v1
	// pre-filter stays authoritative there.
	if auth.AuthorizeOp(id, auth.OpRead, "other", "k", now) {
		t.Error("uncovered bucket must follow the v1 floor (marker/absent denies)")
	}
	// Union: any matching+active+allowing entry authorizes.
	listOnly := auth.GrantExpr{Pattern: "photos/*", Ops: map[auth.Op]bool{auth.OpList: true}}
	if !auth.AuthorizeOp(withRich([]auth.GrantExpr{listOnly}), auth.OpList, "photos", "k", now) {
		t.Error("list-only entry should allow list")
	}
	if auth.AuthorizeOp(withRich([]auth.GrantExpr{listOnly}), auth.OpRead, "photos", "k", now) {
		t.Error("list-only entry must not allow read")
	}
}

// TestFloorUnionDerivation pins the registry build's floor rules end to
// end: read/write unions into the bucket key; time-scoped entries
// contribute the zero-bit marker; non-read/write entries contribute
// nothing; legacy entries build exactly as before.
func TestFloorUnionDerivation(t *testing.T) {
	build := func(grantsJSON string) (auth.Identity, error) {
		var cfg auth.IdentityConfig
		raw := `{"name":"x","accessKey":"AK","secretKey":"sk","grants":` + grantsJSON + `}`
		if err := json.Unmarshal([]byte(raw), &cfg); err != nil {
			return auth.Identity{}, err
		}
		reg, err := auth.NewMultiRegistry([]auth.IdentityConfig{cfg})
		if err != nil {
			return auth.Identity{}, err
		}
		id, ok := reg.LookupByAccessKey("AK")
		if !ok {
			return auth.Identity{}, errors.New("identity missing after build")
		}
		return id, nil
	}
	now := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)

	// Unbounded read/write prefix: floor grants read AND write on photos.
	id, err := build(`{"photos/2024/*":{"ops":["read","write"]}}`)
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if !id.CanRead("photos") || !id.CanWrite("photos") {
		t.Errorf("floor should carry read+write on photos: %+v", id.BucketGrants)
	}
	if !auth.AuthorizeOp(id, auth.OpWrite, "photos", "2024/a", now) {
		t.Error("prefix write denied")
	}
	if auth.AuthorizeOp(id, auth.OpWrite, "photos", "2023/a", now) {
		t.Error("write outside prefix allowed")
	}

	// Time-scoped read grant: zero-bit marker blocks the "*" fall-through.
	id, err = build(`{"*":"readwrite","photos/2024/*":{"ops":["read"],"not-after":"2026-09-01T00:00:00Z"}}`)
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if id.CanRead("photos") {
		t.Error("time-scoped bucket must NOT read at v1 level (marker must be present)")
	}
	if !id.CanRead("anything-else") {
		t.Error("wildcard floor lost for unmarked buckets")
	}
	if !auth.AuthorizeOp(id, auth.OpRead, "anything-else", "k", now) {
		t.Error("wildcard v1 access lost on other buckets")
	}
	if auth.AuthorizeOp(id, auth.OpRead, "photos", "2024/a", now) {
		t.Error("expired rich grant must deny")
	}

	// List-only entry contributes nothing to the floor.
	id, err = build(`{"photos/*":{"ops":["list"]}}`)
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if id.CanRead("photos") || id.CanWrite("photos") {
		t.Error("list-only entry leaked into the floor")
	}
	if !auth.AuthorizeOp(id, auth.OpList, "photos", "", now) {
		t.Error("list-only entry should allow bucket-level list")
	}

	// Future window: marker present, rich grant inactive now.
	id, err = build(`{"photos/*":{"ops":["read"],"not-before":"2099-01-01T00:00:00Z"}}`)
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if id.CanRead("photos") {
		t.Error("not-yet-active window leaked read into the floor")
	}
	if auth.AuthorizeOp(id, auth.OpRead, "photos", "k", now) {
		t.Error("future window must deny now")
	}

	// Legacy coexistence: legacy entries exactly as before, rich beside.
	id, err = build(`{"*":"readwrite","photos":"readonly","photos/2024/*":{"ops":["write","list"]}}`)
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	// Floor = union: legacy readonly contributes read; the unbounded rich
	// write/list entry unions write into the same bucket key.
	if !id.CanRead("photos") || !id.CanWrite("photos") {
		t.Errorf("floor union wrong: read+write expected on photos, map=%+v", id.BucketGrants)
	}
	if !id.CanWrite("other") || !id.CanRead("other") {
		t.Error("legacy wildcard lost")
	}
	if !auth.AuthorizeOp(id, auth.OpWrite, "photos", "2024/x.jpg", now) {
		t.Error("rich write under prefix denied")
	}
	if auth.AuthorizeOp(id, auth.OpWrite, "photos", "2023/x.jpg", now) {
		t.Error("rich write outside prefix allowed")
	}

	// Rich entry with read ops unions read into the legacy map without
	// clobbering the legacy write bit.
	id, err = build(`{"photos":"readwrite","other/*":{"ops":["read"]}}`)
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if !id.CanWrite("photos") {
		t.Error("legacy readwrite on photos clobbered")
	}
	if !id.CanRead("other") {
		t.Error("rich read union missing on other")
	}
}

// TestLegacyConfigGoldenSemantics is the migration golden test: a legacy
// document (string grants only) parses to IDENTICAL semantics as before
// the leaf — same floor map, nil rich table.
func TestLegacyConfigGoldenSemantics(t *testing.T) {
	var cfg auth.IdentityConfig
	raw := `{"name":"scraper","accessKey":"AKRO","secretKey":"sk","grants":{"*":"readwrite","photos":"readonly"}}`
	if err := json.Unmarshal([]byte(raw), &cfg); err != nil {
		t.Fatalf("legacy parse: %v", err)
	}
	reg, err := auth.NewMultiRegistry([]auth.IdentityConfig{cfg})
	if err != nil {
		t.Fatalf("registry: %v", err)
	}
	id, ok := reg.LookupByAccessKey("AKRO")
	if !ok {
		t.Fatal("identity missing")
	}
	if !id.CanRead("anything") || !id.CanWrite("anything") {
		t.Error("wildcard rw lost")
	}
	if !id.CanRead("photos") || id.CanWrite("photos") {
		t.Error("per-bucket readonly lost (beats wildcard)")
	}
	if id.RichGrants() != nil {
		t.Error("legacy-only identity must have a nil rich table")
	}
}

// TestOpAdminIsNeverGrantable pins Contract 6 (management-api-2026-10 leaf
// 04): OpAdmin exists with value "admin", is a valid Op, but is NEVER
// conferred by a bucket grant. A full wildcard bucket grant — legacy or
// rich — still gets a false from AuthorizeOp, and a grant that names the
// admin op is rejected fail-loud at parse time.
func TestOpAdminIsNeverGrantable(t *testing.T) {
	if got := string(auth.OpAdmin); got != "admin" {
		t.Fatalf("OpAdmin = %q, want admin", got)
	}
	now := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)

	// Full wildcard legacy bucket grant: read/write yes, admin no.
	wildcard := auth.Identity{BucketGrants: map[string]auth.Grant{
		"*": {Read: true, Write: true},
	}}
	if !auth.AuthorizeOp(wildcard, auth.OpRead, "any", "k", now) {
		t.Fatal("sanity: wildcard grant should allow read")
	}
	if auth.AuthorizeOp(wildcard, auth.OpAdmin, "any", "", now) {
		t.Fatal("a full wildcard bucket grant conferred admin authority")
	}
	if auth.AuthorizeOp(wildcard, auth.OpAdmin, "*", "k", now) {
		t.Fatal("wildcard bucket/key still conferred admin authority")
	}

	// Rich wildcard grant beside it: still denied.
	rich := auth.Identity{AccessKeyID: "ak-rich", BucketGrants: map[string]auth.Grant{
		"*": {Read: true, Write: true},
	}}.WithRichGrants([]auth.GrantExpr{{
		Pattern: "*",
		Ops:     map[auth.Op]bool{auth.OpRead: true, auth.OpWrite: true},
	}})
	if auth.AuthorizeOp(rich, auth.OpAdmin, "any", "k", now) {
		t.Fatal("rich wildcard grant conferred admin authority")
	}

	// A grant may not even NAME the admin op.
	if _, err := auth.ParseGrantValue("id", "b", obj(`{"ops":["admin"]}`)); err == nil {
		t.Fatal("a BucketGrants entry naming the admin op must be rejected")
	}
	if _, err := auth.ParseGrantValue("id", "b", str("admin")); err == nil {
		t.Fatal("legacy grant value \"admin\" must be rejected")
	}
}

// an empty table REMOVES any prior table under the access key (a reload
// fully replaces the side table).
func TestWithRichGrantsClears(t *testing.T) {
	id := auth.Identity{AccessKeyID: "rotate-me"}
	id = id.WithRichGrants([]auth.GrantExpr{{Pattern: "b", Ops: map[auth.Op]bool{auth.OpRead: true}}})
	if id.RichGrants() == nil {
		t.Fatal("table missing after register")
	}
	same := auth.Identity{AccessKeyID: "rotate-me"}
	same = same.WithRichGrants(nil)
	if same.RichGrants() != nil {
		t.Fatal("table survived a nil re-register (stale reload grants)")
	}
	// A THIRD identity sharing nothing must be unaffected.
	other := auth.Identity{AccessKeyID: "someone-else"}
	if other.RichGrants() != nil {
		t.Fatal("unrelated identity sees another's rich table")
	}
}
