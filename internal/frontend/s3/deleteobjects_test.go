// deleteobjects_test.go — quic-h3-2026-10 leaf 07 Task 2: DeleteObjects
// (POST /{bucket}?delete) wire shapes pinned against the AWS documented
// forms, with the execution mapped onto the internal/batchops core.
// Fixtures are inline (the AWS REST-reference manifest and result
// documents); Quiet mode suppression is pinned; the 400 classes
// (malformed XML, empty list, >1000 keys) answer MalformedXML with the
// execution core untouched (nothing deleted).
package s3

import (
	"encoding/xml"
	"net/http"
	"strings"
	"testing"

	"github.com/bhodgens/zeta-object/internal/batchops"
)

// TestDeleteObjects_AWSManifestFixtureParses: the AWS documented
// request form (namespaced, Quiet, Object/Key entries) parses into the
// handler's request type — including the BARE (namespace-less) form real
// clients emit.
func TestDeleteObjects_AWSManifestFixtureParses(t *testing.T) {
	const namespaced = `<?xml version="1.0" encoding="UTF-8"?>
<Delete xmlns="http://s3.amazonaws.com/doc/2006-03-01/">
  <Object><Key>sample1.txt</Key></Object>
  <Object><Key>sample2.txt</Key></Object>
  <Quiet>true</Quiet>
</Delete>`
	var req DeleteRequest
	if err := xml.Unmarshal([]byte(namespaced), &req); err != nil {
		t.Fatalf("namespaced manifest did not parse: %v", err)
	}
	if len(req.Objects) != 2 || req.Objects[0].Key != "sample1.txt" || req.Objects[1].Key != "sample2.txt" {
		t.Fatalf("objects = %+v, want sample1.txt+sample2.txt", req.Objects)
	}
	if !req.Quiet {
		t.Errorf("Quiet = false, want true")
	}

	// Bare form (Go's XMLName requires an exact namespace match, so the
	// handler's decode must accept what real clients emit without one).
	const bare = `<Delete><Object><Key>k</Key></Object></Delete>`
	req = DeleteRequest{}
	if err := xml.Unmarshal([]byte(bare), &req); err != nil {
		t.Fatalf("bare manifest did not parse: %v", err)
	}
	if len(req.Objects) != 1 || req.Objects[0].Key != "k" {
		t.Fatalf("bare objects = %+v, want [k]", req.Objects)
	}
}

// TestDeleteObjects_AWSResultFixtureRoundTrip: the AWS documented result
// form (namespaced DeleteResult with Deleted and Error entries) round-
// trips through the handler's response type.
func TestDeleteObjects_AWSResultFixtureRoundTrip(t *testing.T) {
	const fixture = `<?xml version="1.0" encoding="UTF-8"?>
<DeleteResult xmlns="http://s3.amazonaws.com/doc/2006-03-01/">
  <Deleted><Key>sample1.txt</Key></Deleted>
  <Error><Key>sample2.txt</Key><Code>AccessDenied</Code><Message>Access Denied</Message></Error>
</DeleteResult>`
	var result DeleteResult
	if err := xml.Unmarshal([]byte(fixture), &result); err != nil {
		t.Fatalf("AWS result fixture did not parse into DeleteResult: %v", err)
	}
	if len(result.Deleted) != 1 || result.Deleted[0].Key != "sample1.txt" {
		t.Fatalf("Deleted = %+v, want [sample1.txt]", result.Deleted)
	}
	if len(result.Error) != 1 || result.Error[0].Key != "sample2.txt" || result.Error[0].Code != "AccessDenied" {
		t.Fatalf("Error = %+v, want sample2.txt/AccessDenied", result.Error)
	}
}

// TestDeleteObjects_MappedOntoBatchCore: a manifest of 2 keys executes
// through the batchops core (the dispatch routes to the same handler;
// both keys land Deleted) and the response carries the AWS wire shape.
func TestDeleteObjects_MappedOntoBatchCore(t *testing.T) {
	env := setupS3TestEnv(t)
	env.setupBucket(t, "core-bkt")
	env.writeTestObject(t, "core-bkt", "sample1.txt", "one")
	env.writeTestObject(t, "core-bkt", "sample2.txt", "two")

	body := `<Delete xmlns="http://s3.amazonaws.com/doc/2006-03-01/"><Object><Key>sample1.txt</Key></Object><Object><Key>sample2.txt</Key></Object></Delete>`
	w := deleteObjects(t, "core-bkt", body)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", w.Code, w.Body.String())
	}
	var result DeleteResult
	if err := xml.Unmarshal(w.Body.Bytes(), &result); err != nil {
		t.Fatalf("unmarshal DeleteResult: %v (%s)", err, w.Body.String())
	}
	if len(result.Deleted) != 2 {
		t.Fatalf("Deleted = %+v, want 2 entries", result.Deleted)
	}
	if len(result.Error) != 0 {
		t.Fatalf("Error = %+v, want none", result.Error)
	}
	for _, k := range []string{"sample1.txt", "sample2.txt"} {
		if _, err := env.b.Stat(t.Context(), "core-bkt", k); err == nil {
			t.Errorf("%s should be deleted", k)
		}
	}
}

// TestDeleteObjects_QuietModeErrorsOnly: Quiet suppresses the Deleted
// entries; errors (when present) still appear. A quiet manifest deleting
// two keys answers a DeleteResult with NO Deleted elements.
func TestDeleteObjects_QuietModeErrorsOnly(t *testing.T) {
	env := setupS3TestEnv(t)
	env.setupBucket(t, "quiet-core-bkt")
	env.writeTestObject(t, "quiet-core-bkt", "q1.txt", "one")
	env.writeTestObject(t, "quiet-core-bkt", "q2.txt", "two")

	body := `<Delete><Quiet>true</Quiet><Object><Key>q1.txt</Key></Object><Object><Key>q2.txt</Key></Object></Delete>`
	w := deleteObjects(t, "quiet-core-bkt", body)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", w.Code, w.Body.String())
	}
	if strings.Contains(w.Body.String(), "<Deleted>") {
		t.Errorf("Quiet must suppress Deleted entries: %s", w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "DeleteResult") {
		t.Errorf("Quiet response keeps the DeleteResult envelope: %s", w.Body.String())
	}
	var result DeleteResult
	if err := xml.Unmarshal(w.Body.Bytes(), &result); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(result.Deleted) != 0 || len(result.Error) != 0 {
		t.Fatalf("quiet two-key delete = %+v/%+v, want empty", result.Deleted, result.Error)
	}
}

// TestDeleteObjects_QuietModeStillReportsErrors: an invalid key inside a
// quiet manifest surfaces as an Error entry (Quiet hides successes only).
func TestDeleteObjects_QuietModeStillReportsErrors(t *testing.T) {
	env := setupS3TestEnv(t)
	env.setupBucket(t, "quiet-err-bkt")

	body := `<Delete><Quiet>true</Quiet><Object><Key>.metadata/steal</Key></Object></Delete>`
	w := deleteObjects(t, "quiet-err-bkt", body)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (per-key errors, not a request failure): %s", w.Code, w.Body.String())
	}
	var result DeleteResult
	if err := xml.Unmarshal(w.Body.Bytes(), &result); err != nil {
		t.Fatalf("unmarshal: %v (%s)", err, w.Body.String())
	}
	if len(result.Error) != 1 || result.Error[0].Key != ".metadata/steal" {
		t.Fatalf("Error = %+v, want one entry for .metadata/steal", result.Error)
	}
}

// TestDeleteObjects_PerKeyErrorEntry: a non-deletable item produces the
// AWS <Error><Key/><Code/><Message/></Error> entry while the good key
// still lands Deleted (no cross-item atomicity in the delete surface
// either).
func TestDeleteObjects_PerKeyErrorEntry(t *testing.T) {
	env := setupS3TestEnv(t)
	env.setupBucket(t, "mixed-bkt")
	env.writeTestObject(t, "mixed-bkt", "good.txt", "data")

	body := `<Delete><Object><Key>good.txt</Key></Object><Object><Key>../escape</Key></Object></Delete>`
	w := deleteObjects(t, "mixed-bkt", body)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", w.Code, w.Body.String())
	}
	var result DeleteResult
	if err := xml.Unmarshal(w.Body.Bytes(), &result); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(result.Deleted) != 1 || result.Deleted[0].Key != "good.txt" {
		t.Fatalf("Deleted = %+v, want [good.txt]", result.Deleted)
	}
	if len(result.Error) != 1 || result.Error[0].Key != "../escape" || result.Error[0].Code == "" || result.Error[0].Message == "" {
		t.Fatalf("Error = %+v, want one Key/Code/Message entry for ../escape", result.Error)
	}
}

// TestDeleteObjects_400ClassesNothingDeletes: malformed XML, empty
// object list, and >1000 keys are 400 MalformedXML with the execution
// core untouched — a seeded object survives every one of them.
func TestDeleteObjects_400ClassesNothingDeletes(t *testing.T) {
	over := func(n int) string {
		var sb strings.Builder
		sb.WriteString("<Delete>")
		for range n {
			sb.WriteString("<Object><Key>k</Key></Object>")
		}
		sb.WriteString("</Delete>")
		return sb.String()
	}
	cases := []struct{ name, body string }{
		{"malformed xml", "this is not xml"},
		{"empty list", "<Delete></Delete>"},
		{"over 1000", over(maxBatchDeleteKeys + 1)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			env := setupS3TestEnv(t)
			env.setupBucket(t, "d400-bkt")
			env.writeTestObject(t, "d400-bkt", "survivor.txt", "stay")

			w := deleteObjects(t, "d400-bkt", tc.body)
			if w.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400: %s", w.Code, w.Body.String())
			}
			if !strings.Contains(w.Body.String(), "MalformedXML") {
				t.Errorf("want MalformedXML, got: %s", w.Body.String())
			}
			if _, err := env.b.Stat(t.Context(), "d400-bkt", "survivor.txt"); err != nil {
				t.Errorf("400 must execute nothing — survivor.txt deleted: %v", err)
			}
		})
	}
}

// TestDeleteObjects_Exactly1000IsLegal pins the constant against the S3
// cap: 1000 keys is a 200.
func TestDeleteObjects_Exactly1000IsLegal(t *testing.T) {
	env := setupS3TestEnv(t)
	env.setupBucket(t, "cap1000-bkt")

	var sb strings.Builder
	sb.WriteString("<Delete>")
	for range maxBatchDeleteKeys {
		sb.WriteString("<Object><Key>k</Key></Object>")
	}
	sb.WriteString("</Delete>")
	w := deleteObjects(t, "cap1000-bkt", sb.String())
	if w.Code != http.StatusOK {
		t.Fatalf("exactly 1000 keys: status = %d, want 200: %s", w.Code, w.Body.String())
	}
	if maxBatchDeleteKeys != batchops.MaxOperations {
		t.Fatalf("maxBatchDeleteKeys = %d, want the batchops limit %d (one cap, both surfaces)", maxBatchDeleteKeys, batchops.MaxOperations)
	}
}
