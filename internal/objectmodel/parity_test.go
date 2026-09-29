package objectmodel

import "testing"

func TestSnapshotHeaders(t *testing.T) {
	get := func(k string) string {
		m := map[string]string{
			"Content-Type":     "text/plain",
			"Content-Length":   "5",
			"ETag":             `"abc"`,
			"Last-Modified":    "Mon, 02 Jan 2006 15:04:05 GMT",
			"x-amz-meta-color": "red",
		}
		return m[k]
	}
	s := SnapshotHeaders(get, []string{"x-amz-meta-color"})
	if s.ContentType != "text/plain" || s.ContentLength != "5" || s.ETag != `"abc"` {
		t.Errorf("snapshot wrong: %+v", s)
	}
	if s.UserMetadata["color"] != "red" {
		t.Errorf("user metadata keys must be canonical: %v", s.UserMetadata)
	}
}

func TestAssertHeaderParity(t *testing.T) {
	a := HeaderSnapshot{ContentType: "text/plain", ContentLength: "5",
		ETag: `"abc"`, LastModified: "Mon, 02 Jan 2006 15:04:05 GMT",
		UserMetadata: map[string]string{"color": "red"}}
	identical := a
	if diff := AssertHeaderParity(a, identical); diff != "" {
		t.Errorf("identical snapshots must pass, got diff: %s", diff)
	}
	b := a
	b.ETag = `"different"`
	if diff := AssertHeaderParity(a, b); diff == "" {
		t.Error("differing ETag must be reported")
	}
	c := a
	c.UserMetadata = map[string]string{"color": "blue"}
	if diff := AssertHeaderParity(a, c); diff == "" {
		t.Error("differing user metadata must be reported")
	}
}

func TestAssertHeaderParityDefaultPolicy(t *testing.T) {
	lm := "Mon, 02 Jan 2006 15:04:05 GMT"
	lmPlus30 := "Mon, 02 Jan 2006 15:04:35 GMT"
	lmPlus61 := "Mon, 02 Jan 2006 15:05:06 GMT"
	snap := func(lmV, etag string) HeaderSnapshot {
		return HeaderSnapshot{ContentType: "text/plain", ContentLength: "5",
			ETag: etag, LastModified: lmV, UserMetadata: map[string]string{"color": "red"}}
	}

	// DefaultParityPolicy: Last-Modified within 60s (HTTP-date) tolerated;
	// nothing else.
	if diff := AssertHeaderParityWithPolicy(snap(lm, `"a"`), snap(lmPlus30, `"a"`), DefaultParityPolicy); diff != "" {
		t.Errorf("LM within 60s must be tolerated under DefaultParityPolicy, got: %s", diff)
	}
	if diff := AssertHeaderParityWithPolicy(snap(lm, `"a"`), snap(lmPlus61, `"a"`), DefaultParityPolicy); diff == "" {
		t.Error("LM beyond 60s must FAIL under DefaultParityPolicy")
	}
	if diff := AssertHeaderParityWithPolicy(snap(lm, `"a"`), snap(lmPlus30, `"b"`), DefaultParityPolicy); diff == "" {
		t.Error("ETag difference must FAIL even with tolerated LM")
	}
	notDate := snap("not-a-date", `"a"`)
	if diff := AssertHeaderParityWithPolicy(snap(lm, `"a"`), notDate, DefaultParityPolicy); diff == "" {
		t.Error("non-HTTP-date LM must FAIL, not be silently tolerated")
	}

	// Zero policy = exact: AssertHeaderParity delegates to it, so a 30s LM
	// skew IS a diff there (backward compat).
	if diff := AssertHeaderParity(snap(lm, `"a"`), snap(lmPlus30, `"a"`)); diff == "" {
		t.Error("AssertHeaderParity (exact policy) must report a 30s LM skew")
	}
	var zero ParityPolicy
	if diff := AssertHeaderParityWithPolicy(snap(lm, `"a"`), snap(lmPlus30, `"a"`), zero); diff == "" {
		t.Error("zero policy must be exact on Last-Modified")
	}

	// ETag normalization: quoted vs unquoted (and weak forms) must not read
	// as a parity break under any policy, because comparison routes through
	// ETagsMatch.
	if diff := AssertHeaderParity(snap(lm, `"a"`), snap(lm, "a")); diff != "" {
		t.Errorf("quoted vs unquoted ETag must not be a parity break, got: %s", diff)
	}
	if diff := AssertHeaderParityWithPolicy(snap(lm, `W/"a"`), snap(lm, `"a"`), DefaultParityPolicy); diff != "" {
		t.Errorf("weak vs strong ETag must not be a parity break, got: %s", diff)
	}
}
