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
