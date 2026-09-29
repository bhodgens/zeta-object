package s3_test

// bucket_versions_test.go — pin tests for the GET /bucket?versions
// sub-resource fixes (bughunt-gateway-2026-09-29):
//   - D5: max-keys=0 → empty listing, IsTruncated=false, MaxKeys=0
//     (V2 leaf-2.4 fix 13 parity; previously returned ALL versions).
//   - D6: truncated pages carry NextKeyMarker/NextVersionIdMarker =
//     the last emitted key/versionId.
//   - D7: encoding-type=url percent-encodes keys and reports
//     EncodingType=url (mirrors ListObjectsV2).

import (
	"encoding/xml"
	"io"
	"net/http"
	"testing"
)

// versionsResponse is the decoded shape of a ListVersionsResult for pins.
type versionsResponse struct {
	XMLName             xml.Name `xml:"ListVersionsResult"`
	Name                string   `xml:"Name"`
	NextKeyMarker       string   `xml:"NextKeyMarker"`
	NextVersionIdMarker string   `xml:"NextVersionIdMarker"`
	EncodingType        string   `xml:"EncodingType"`
	MaxKeys             int      `xml:"MaxKeys"`
	IsTruncated         bool     `xml:"IsTruncated"`
	Versions            []struct {
		Key       string `xml:"Key"`
		VersionId string `xml:"VersionId"`
	} `xml:"Version"`
}

func decodeVersionsResponse(t *testing.T, body string) versionsResponse {
	t.Helper()
	var resp versionsResponse
	if err := xml.Unmarshal([]byte(body), &resp); err != nil {
		t.Fatalf("unmarshal versions XML %q: %v", body, err)
	}
	return resp
}

// TestVersionsMaxKeysZeroEmpty pins D5: max-keys=0 must return an empty,
// non-truncated listing instead of every version.
func TestVersionsMaxKeysZeroEmpty(t *testing.T) {
	srv := newTestServer(t)

	if resp := doSigned(t, srv, "PUT", "/vzero-bkt", ""); resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		t.Fatalf("create bucket: got %d", resp.StatusCode)
	} else {
		resp.Body.Close()
	}
	for _, k := range []string{"a.txt", "b.txt", "c.txt"} {
		if resp := doSigned(t, srv, "PUT", "/vzero-bkt/"+k, "x"); resp.StatusCode != http.StatusOK {
			resp.Body.Close()
			t.Fatalf("put %s: got %d", k, resp.StatusCode)
		} else {
			resp.Body.Close()
		}
	}

	resp := doSigned(t, srv, "GET", "/vzero-bkt?versions&max-keys=0", "")
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d (body %s), want 200", resp.StatusCode, body)
	}
	got := decodeVersionsResponse(t, string(body))
	if got.MaxKeys != 0 {
		t.Fatalf("MaxKeys = %d, want 0 (body %s)", got.MaxKeys, body)
	}
	if got.IsTruncated {
		t.Fatalf("IsTruncated = true, want false (body %s)", body)
	}
	if len(got.Versions) != 0 {
		t.Fatalf("got %d Version entries, want 0 (body %s)", len(got.Versions), body)
	}
}

// TestVersionsTruncationMarkers pins D6: a truncated page carries
// NextKeyMarker/NextVersionIdMarker of the last emitted entry, and the
// markers resume the listing; a complete page carries neither.
func TestVersionsTruncationMarkers(t *testing.T) {
	srv := newTestServer(t)

	if resp := doSigned(t, srv, "PUT", "/vmark-bkt", ""); resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		t.Fatalf("create bucket: got %d", resp.StatusCode)
	} else {
		resp.Body.Close()
	}
	for _, k := range []string{"a.txt", "b.txt", "c.txt"} {
		if resp := doSigned(t, srv, "PUT", "/vmark-bkt/"+k, "x"); resp.StatusCode != http.StatusOK {
			resp.Body.Close()
			t.Fatalf("put %s: got %d", k, resp.StatusCode)
		} else {
			resp.Body.Close()
		}
	}

	// Page 1: max-keys=2 → truncated with markers = last emitted entry.
	resp := doSigned(t, srv, "GET", "/vmark-bkt?versions&max-keys=2", "")
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("page1 status = %d (body %s)", resp.StatusCode, body)
	}
	page1 := decodeVersionsResponse(t, string(body))
	if !page1.IsTruncated {
		t.Fatalf("page1 IsTruncated = false, want true (body %s)", body)
	}
	if len(page1.Versions) != 2 {
		t.Fatalf("page1 versions = %d, want 2 (body %s)", len(page1.Versions), body)
	}
	if page1.NextKeyMarker != "b.txt" {
		t.Fatalf("NextKeyMarker = %q, want b.txt (body %s)", page1.NextKeyMarker, body)
	}
	if page1.NextVersionIdMarker != "null" {
		t.Fatalf("NextVersionIdMarker = %q, want null (body %s)", page1.NextVersionIdMarker, body)
	}

	// Resume from the marker: only the remaining key comes back, complete.
	resp2 := doSigned(t, srv, "GET", "/vmark-bkt?versions&max-keys=2&key-marker="+page1.NextKeyMarker+"&version-id-marker="+page1.NextVersionIdMarker, "")
	body2, _ := io.ReadAll(resp2.Body)
	resp2.Body.Close()
	if resp2.StatusCode != http.StatusOK {
		t.Fatalf("page2 status = %d (body %s)", resp2.StatusCode, body2)
	}
	page2 := decodeVersionsResponse(t, string(body2))
	if page2.IsTruncated {
		t.Fatalf("page2 IsTruncated = true, want false (body %s)", body2)
	}
	if len(page2.Versions) != 1 || page2.Versions[0].Key != "c.txt" {
		t.Fatalf("page2 versions = %+v, want [c.txt] (body %s)", page2.Versions, body2)
	}
	if page2.NextKeyMarker != "" || page2.NextVersionIdMarker != "" {
		t.Fatalf("complete page must omit Next markers (body %s)", body2)
	}
}

// TestVersionsEncodingTypeURL pins D7: encoding-type=url percent-encodes
// keys (space → %20, '/' literal) and the response advertises it.
func TestVersionsEncodingTypeURL(t *testing.T) {
	srv := newTestServer(t)

	if resp := doSigned(t, srv, "PUT", "/venc-bkt", ""); resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		t.Fatalf("create bucket: got %d", resp.StatusCode)
	} else {
		resp.Body.Close()
	}
	// A key with a space (and a slash to pin the path-preserving encode).
	if resp := doSigned(t, srv, "PUT", "/venc-bkt/a%20b.txt", "x"); resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		t.Fatalf("put a b.txt: got %d", resp.StatusCode)
	} else {
		resp.Body.Close()
	}
	if resp := doSigned(t, srv, "PUT", "/venc-bkt/dir%2Fkey", "x"); resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		t.Fatalf("put dir/key: got %d", resp.StatusCode)
	} else {
		resp.Body.Close()
	}

	resp := doSigned(t, srv, "GET", "/venc-bkt?versions&encoding-type=url", "")
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d (body %s)", resp.StatusCode, body)
	}
	got := decodeVersionsResponse(t, string(body))
	if got.EncodingType != "url" {
		t.Fatalf("EncodingType = %q, want url (body %s)", got.EncodingType, body)
	}
	seen := map[string]bool{}
	for _, v := range got.Versions {
		seen[v.Key] = true
	}
	if !seen["a%20b.txt"] {
		t.Fatalf("encoded key a%%20b.txt missing (body %s)", body)
	}
	if !seen["dir/key"] {
		t.Fatalf("slash must stay literal: dir/key missing (body %s)", body)
	}
	if seen["a b.txt"] {
		t.Fatalf("raw unencoded key leaked (body %s)", body)
	}
}

// TestVersionsMarkerEncoding pins the regression-review fix: with
// encoding-type=url, NextKeyMarker must be encoded exactly like entry keys.
func TestVersionsMarkerEncoding(t *testing.T) {
	srv := newTestServer(t)

	if resp := doSigned(t, srv, "PUT", "/venc-bkt", ""); resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		t.Fatalf("create bucket: got %d", resp.StatusCode)
	} else {
		resp.Body.Close()
	}
	if resp := doSigned(t, srv, "PUT", "/venc-bkt/a b.txt", "x"); resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		t.Fatalf("put: got %d", resp.StatusCode)
	} else {
		resp.Body.Close()
	}
	if resp := doSigned(t, srv, "PUT", "/venc-bkt/c.txt", "x"); resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		t.Fatalf("put: got %d", resp.StatusCode)
	} else {
		resp.Body.Close()
	}

	resp := doSigned(t, srv, "GET", "/venc-bkt?versions&max-keys=1&encoding-type=url", "")
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d (body %s)", resp.StatusCode, body)
	}
	got := decodeVersionsResponse(t, string(body))
	if !got.IsTruncated {
		t.Fatalf("expected truncated page (body %s)", body)
	}
	if got.NextKeyMarker != "a%20b.txt" {
		t.Fatalf("NextKeyMarker = %q, want encoded a%%20b.txt (body %s)", got.NextKeyMarker, body)
	}
}
