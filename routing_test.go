package main

// routing_test.go — leaf 4.3: dispatch routing matrix through rootHandler.
//
// Every production request flows through rootHandler → service/bucket/object
// dispatch, yet handler-level unit tests bypass that layer. These tests drive
// REAL, fully SigV4-signed requests through rootHandler and assert, per
// matrix cell, the status code plus a distinguishing marker (XML root
// element, S3 error code, or dispatch-layer 405 text) that proves WHICH
// handler answered.
//
// No production code is touched. A cell that 405s unexpectedly or routes to
// the wrong handler is a finding, not something these tests paper over.

import (
	"encoding/hex"
	"encoding/xml"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// ---- signed-request plumbing (header-auth SigV4, same-package helpers) ----

// routingAuth signs req with header auth (SigV4, UNSIGNED-free: real payload
// hash) using the same canonical rules as authenticateRequest, and sets the
// Authorization header. Extra headers on the req are NOT signed (only
// host/x-amz-content-sha256/x-amz-date are), matching the AWS CLI default.
func routingAuth(t *testing.T, req *http.Request, body []byte) {
	t.Helper()

	now := time.Now().UTC()
	amzDate := now.Format(iso8601Format)
	dateStamp := now.Format(shortDateFormat)
	payloadHash := hashSHA256(body)

	req.Host = "localhost:8443"
	req.Header.Set("x-amz-date", amzDate)
	req.Header.Set("x-amz-content-sha256", payloadHash)

	signedHeaders := "host;x-amz-content-sha256;x-amz-date"
	canonicalHeaders := fmt.Sprintf("host:%s\nx-amz-content-sha256:%s\nx-amz-date:%s\n",
		req.Host, payloadHash, amzDate)
	canonicalRequest := strings.Join([]string{
		req.Method,
		getCanonicalURI(req),
		getCanonicalQueryString(req),
		canonicalHeaders,
		signedHeaders,
		payloadHash,
	}, "\n")
	credentialScope := fmt.Sprintf("%s/%s/%s/aws4_request", dateStamp, defaultRegion, serviceName)
	stringToSign := strings.Join([]string{
		awsAlgorithm,
		amzDate,
		credentialScope,
		hashSHA256([]byte(canonicalRequest)),
	}, "\n")
	signingKey := getSigningKey(serverCredentials.SecretAccessKey, dateStamp, defaultRegion, serviceName)
	signature := hex.EncodeToString(hmacSHA256(signingKey, stringToSign))

	req.Header.Set("Authorization", fmt.Sprintf(
		"AWS4-HMAC-SHA256 Credential=%s/%s/%s/%s/aws4_request, SignedHeaders=%s, Signature=%s",
		serverCredentials.AccessKeyID, dateStamp, defaultRegion, serviceName, signedHeaders, signature))
}

// routingRequest builds a fully-signed request for method/target (target may
// carry a query string), runs it through rootHandler, and returns the
// recorder.
func routingRequest(t *testing.T, method, target string, body []byte, header map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, target, strings.NewReader(string(body)))
	for k, v := range header {
		req.Header.Set(k, v)
	}
	routingAuth(t, req, body)
	w := httptest.NewRecorder()
	rootHandler(w, req)
	return w
}

// ---- response-marker helpers ----

// xmlRootName returns the local name of the first XML start element.
func xmlRootName(t *testing.T, body string) string {
	t.Helper()
	dec := xml.NewDecoder(strings.NewReader(body))
	for {
		tok, err := dec.Token()
		if err != nil {
			t.Fatalf("no XML root element in body: %q", body)
		}
		if se, ok := tok.(xml.StartElement); ok {
			return se.Name.Local
		}
	}
}

// s3ErrCode extracts the <Code> from an S3 error document.
func s3ErrCode(t *testing.T, body string) string {
	t.Helper()
	var e S3Error
	if err := xml.Unmarshal([]byte(body), &e); err != nil {
		t.Fatalf("unmarshal error body %q: %v", body, err)
	}
	return e.Code
}

// ---- service-level matrix ----

func TestRouting_ServiceLevel(t *testing.T) {
	setupTestEnv(t)

	t.Run("GET / lists buckets", func(t *testing.T) {
		w := routingRequest(t, "GET", "/", nil, nil)
		if w.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200: %s", w.Code, w.Body.String())
		}
		if root := xmlRootName(t, w.Body.String()); root != "ListAllMyBucketsResult" {
			t.Errorf("XML root = %q, want ListAllMyBucketsResult (listBucketsHandler)", root)
		}
	})

	for _, method := range []string{"PUT", "POST", "DELETE", "HEAD"} {
		t.Run(method+" / is 405", func(t *testing.T) {
			w := routingRequest(t, method, "/", nil, nil)
			if w.Code != http.StatusMethodNotAllowed {
				t.Fatalf("status = %d, want 405: %s", w.Code, w.Body.String())
			}
			// Distinguishing marker: the service-level dispatch's own text,
			// not the bucket- or object-level one.
			if want := "Method Not Allowed at service level"; !strings.Contains(w.Body.String(), want) {
				t.Errorf("body %q missing marker %q", w.Body.String(), want)
			}
		})
	}
}

// ---- bucket-level matrix ----

func TestRouting_BucketLevel(t *testing.T) {
	env := setupTestEnv(t)
	const bkt = "route-bucket"

	t.Run("PUT /bkt creates bucket (200)", func(t *testing.T) {
		w := routingRequest(t, "PUT", "/"+bkt, nil, nil)
		if w.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200: %s", w.Code, w.Body.String())
		}
		if info, err := os.Stat(filepath.Join(env.dataDir, bkt)); err != nil || !info.IsDir() {
			t.Error("bucket directory was not created (createBucketHandler did not answer)")
		}
	})

	t.Run("PUT /bkt again is 409 BucketAlreadyOwnedByYou", func(t *testing.T) {
		w := routingRequest(t, "PUT", "/"+bkt, nil, nil)
		if w.Code != http.StatusConflict {
			t.Fatalf("status = %d, want 409: %s", w.Code, w.Body.String())
		}
		if code := s3ErrCode(t, w.Body.String()); code != "BucketAlreadyOwnedByYou" {
			t.Errorf("code = %q, want BucketAlreadyOwnedByYou", code)
		}
	})

	t.Run("HEAD /bkt existing is 200", func(t *testing.T) {
		w := routingRequest(t, "HEAD", "/"+bkt, nil, nil)
		if w.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200", w.Code)
		}
	})

	t.Run("HEAD /bkt missing is 404", func(t *testing.T) {
		w := routingRequest(t, "HEAD", "/route-nope", nil, nil)
		if w.Code != http.StatusNotFound {
			t.Fatalf("status = %d, want 404", w.Code)
		}
	})

	t.Run("GET /bkt (no params) is ListBucketResult", func(t *testing.T) {
		w := routingRequest(t, "GET", "/"+bkt, nil, nil)
		if w.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200: %s", w.Code, w.Body.String())
		}
		if root := xmlRootName(t, w.Body.String()); root != "ListBucketResult" {
			t.Errorf("XML root = %q, want ListBucketResult (listObjectsV2Handler default route)", root)
		}
	})

	t.Run("GET /bkt missing is 404 NoSuchBucket", func(t *testing.T) {
		w := routingRequest(t, "GET", "/route-nope", nil, nil)
		if w.Code != http.StatusNotFound {
			t.Fatalf("status = %d, want 404: %s", w.Code, w.Body.String())
		}
		if code := s3ErrCode(t, w.Body.String()); code != "NoSuchBucket" {
			t.Errorf("code = %q, want NoSuchBucket", code)
		}
	})

	t.Run("GET /bkt?list-type=2 is ListBucketResult", func(t *testing.T) {
		w := routingRequest(t, "GET", "/"+bkt+"?list-type=2", nil, nil)
		if w.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200: %s", w.Code, w.Body.String())
		}
		if root := xmlRootName(t, w.Body.String()); root != "ListBucketResult" {
			t.Errorf("XML root = %q, want ListBucketResult", root)
		}
	})

	t.Run("GET /bkt?location is LocationConstraint", func(t *testing.T) {
		w := routingRequest(t, "GET", "/"+bkt+"?location", nil, nil)
		if w.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200: %s", w.Code, w.Body.String())
		}
		if root := xmlRootName(t, w.Body.String()); root != "LocationConstraint" {
			t.Errorf("XML root = %q, want LocationConstraint (getBucketLocationHandler)", root)
		}
	})

	t.Run("GET /bkt?uploads is ListMultipartUploadsResult", func(t *testing.T) {
		w := routingRequest(t, "GET", "/"+bkt+"?uploads", nil, nil)
		if w.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200: %s", w.Code, w.Body.String())
		}
		if root := xmlRootName(t, w.Body.String()); root != "ListMultipartUploadsResult" {
			t.Errorf("XML root = %q, want ListMultipartUploadsResult (listMultipartUploadsHandler)", root)
		}
	})

	t.Run("GET /bkt?acl is 501 NotImplemented (handleACL stub)", func(t *testing.T) {
		w := routingRequest(t, "GET", "/"+bkt+"?acl", nil, nil)
		if w.Code != http.StatusNotImplemented {
			t.Fatalf("status = %d, want 501: %s", w.Code, w.Body.String())
		}
		if code := s3ErrCode(t, w.Body.String()); code != "NotImplemented" {
			t.Errorf("code = %q, want NotImplemented (handleACL)", code)
		}
	})

	t.Run("POST /bkt is 405", func(t *testing.T) {
		w := routingRequest(t, "POST", "/"+bkt, nil, nil)
		if w.Code != http.StatusMethodNotAllowed {
			t.Fatalf("status = %d, want 405: %s", w.Code, w.Body.String())
		}
		if want := "Method Not Allowed for bucket"; !strings.Contains(w.Body.String(), want) {
			t.Errorf("body %q missing marker %q", w.Body.String(), want)
		}
	})

	t.Run("POST /bkt?delete is DeleteResult", func(t *testing.T) {
		// Seed one object, then batch-delete it through the sub-resource.
		env.writeTestObject(t, bkt, "doomed.txt", "delete me")
		body := `<Delete><Object><Key>doomed.txt</Key></Object></Delete>`
		w := routingRequest(t, "POST", "/"+bkt+"?delete", []byte(body), nil)
		if w.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200: %s", w.Code, w.Body.String())
		}
		if root := xmlRootName(t, w.Body.String()); root != "DeleteResult" {
			t.Errorf("XML root = %q, want DeleteResult (deleteObjectsHandler)", root)
		}
		if !strings.Contains(w.Body.String(), "<Key>doomed.txt</Key>") {
			t.Errorf("DeleteResult does not report deleted key: %s", w.Body.String())
		}
	})

	t.Run("DELETE /bkt with objects is 409 BucketNotEmpty", func(t *testing.T) {
		env.writeTestObject(t, bkt, "keep.txt", "blocker")
		w := routingRequest(t, "DELETE", "/"+bkt, nil, nil)
		if w.Code != http.StatusConflict {
			t.Fatalf("status = %d, want 409: %s", w.Code, w.Body.String())
		}
		if code := s3ErrCode(t, w.Body.String()); code != "BucketNotEmpty" {
			t.Errorf("code = %q, want BucketNotEmpty", code)
		}
	})

	t.Run("DELETE /bkt empty is 204", func(t *testing.T) {
		// Remove the blocker through the object route first (also proves it).
		if w := routingRequest(t, "DELETE", "/"+bkt+"/keep.txt", nil, nil); w.Code != http.StatusNoContent {
			t.Fatalf("setup: DELETE object status = %d, want 204: %s", w.Code, w.Body.String())
		}
		w := routingRequest(t, "DELETE", "/"+bkt, nil, nil)
		if w.Code != http.StatusNoContent {
			t.Fatalf("status = %d, want 204: %s", w.Code, w.Body.String())
		}
		if _, err := os.Stat(filepath.Join(env.dataDir, bkt)); !os.IsNotExist(err) {
			t.Error("bucket should be gone (deleteBucketHandler did not answer)")
		}
	})
}

// ---- object-level matrix ----

func TestRouting_ObjectLevel(t *testing.T) {
	env := setupTestEnv(t)
	const bkt, key = "route-bucket", "routed.txt"
	_ = env.setupBucket(t, bkt)

	t.Run("PUT plain stores object", func(t *testing.T) {
		w := routingRequest(t, "PUT", "/"+bkt+"/"+key, []byte("plain put body"), nil)
		if w.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200: %s", w.Code, w.Body.String())
		}
		if w.Header().Get("ETag") == "" {
			t.Error("missing ETag header (putObjectHandler did not answer)")
		}
		data, err := os.ReadFile(filepath.Join(env.dataDir, bkt, key))
		if err != nil || string(data) != "plain put body" {
			t.Errorf("stored object = %q, err %v; want plain put body", data, err)
		}
	})

	t.Run("HEAD object existing is 200", func(t *testing.T) {
		w := routingRequest(t, "HEAD", "/"+bkt+"/"+key, nil, nil)
		if w.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200", w.Code)
		}
		if w.Header().Get("ETag") == "" {
			t.Error("missing ETag header (headObjectHandler did not answer)")
		}
	})

	t.Run("HEAD object missing is 404", func(t *testing.T) {
		w := routingRequest(t, "HEAD", "/"+bkt+"/missing.bin", nil, nil)
		if w.Code != http.StatusNotFound {
			t.Fatalf("status = %d, want 404", w.Code)
		}
	})

	t.Run("GET object returns body", func(t *testing.T) {
		w := routingRequest(t, "GET", "/"+bkt+"/"+key, nil, nil)
		if w.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200: %s", w.Code, w.Body.String())
		}
		if w.Body.String() != "plain put body" {
			t.Errorf("body = %q, want %q", w.Body.String(), "plain put body")
		}
	})

	t.Run("PUT with x-amz-copy-source is CopyObjectResult", func(t *testing.T) {
		w := routingRequest(t, "PUT", "/"+bkt+"/copied.txt", nil,
			map[string]string{"x-amz-copy-source": bkt + "/" + key})
		if w.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200: %s", w.Code, w.Body.String())
		}
		if root := xmlRootName(t, w.Body.String()); root != "CopyObjectResult" {
			t.Errorf("XML root = %q, want CopyObjectResult (copyObjectHandler)", root)
		}
		data, err := os.ReadFile(filepath.Join(env.dataDir, bkt, "copied.txt"))
		if err != nil || string(data) != "plain put body" {
			t.Errorf("copied object = %q, err %v; want copied content", data, err)
		}
	})

	t.Run("POST ?uploads is InitiateMultipartUploadResult", func(t *testing.T) {
		w := routingRequest(t, "POST", "/"+bkt+"/big.bin?uploads", nil, nil)
		if w.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200: %s", w.Code, w.Body.String())
		}
		if root := xmlRootName(t, w.Body.String()); root != "InitiateMultipartUploadResult" {
			t.Errorf("XML root = %q, want InitiateMultipartUploadResult (initiateMultipartUploadHandler)", root)
		}
	})

	// Shared in-flight upload for the uploadId-routed cells below.
	initUpload := func(t *testing.T, obj string) string {
		t.Helper()
		w := routingRequest(t, "POST", "/"+bkt+"/"+obj+"?uploads", nil, nil)
		if w.Code != http.StatusOK {
			t.Fatalf("init multipart: status = %d: %s", w.Code, w.Body.String())
		}
		var res InitiateMultipartUploadResult
		if err := xml.Unmarshal(w.Body.Bytes(), &res); err != nil {
			t.Fatalf("init multipart: bad XML: %v", err)
		}
		return res.UploadID
	}

	t.Run("PUT ?partNumber&uploadId is UploadPart (ETag, no object)", func(t *testing.T) {
		uploadID := initUpload(t, "big.bin")
		w := routingRequest(t, "PUT",
			fmt.Sprintf("/%s/big.bin?partNumber=1&uploadId=%s", bkt, uploadID),
			[]byte("part one data"), nil)
		if w.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200: %s", w.Code, w.Body.String())
		}
		if w.Header().Get("ETag") == "" {
			t.Error("missing ETag header (uploadPartHandler did not answer)")
		}
		// uploadPart must NOT have created a plain object at big.bin.
		if _, err := os.Stat(filepath.Join(env.dataDir, bkt, "big.bin")); !os.IsNotExist(err) {
			t.Error("UploadPart must not create the object file; putObjectHandler answered instead")
		}
	})

	t.Run("GET ?uploadId is ListPartsResult", func(t *testing.T) {
		uploadID := initUpload(t, "parts.bin")
		// one part so the listing is non-trivial
		if w := routingRequest(t, "PUT",
			fmt.Sprintf("/%s/parts.bin?partNumber=1&uploadId=%s", bkt, uploadID),
			[]byte("x"), nil); w.Code != http.StatusOK {
			t.Fatalf("setup upload part: status = %d: %s", w.Code, w.Body.String())
		}
		w := routingRequest(t, "GET",
			fmt.Sprintf("/%s/parts.bin?uploadId=%s", bkt, uploadID), nil, nil)
		if w.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200: %s", w.Code, w.Body.String())
		}
		if root := xmlRootName(t, w.Body.String()); root != "ListPartsResult" {
			t.Errorf("XML root = %q, want ListPartsResult (listPartsHandler)", root)
		}
		if !strings.Contains(w.Body.String(), "<PartNumber>1</PartNumber>") {
			t.Errorf("ListPartsResult missing part 1: %s", w.Body.String())
		}
	})

	t.Run("POST ?uploadId routes to CompleteMultipart (MalformedXML 400 marker)", func(t *testing.T) {
		uploadID := initUpload(t, "fin.bin")
		// Non-XML body: only completeMultipartUploadHandler produces this
		// exact 400 MalformedXML, proving the route fired.
		w := routingRequest(t, "POST",
			fmt.Sprintf("/%s/fin.bin?uploadId=%s", bkt, uploadID),
			[]byte("this is not xml"), nil)
		if w.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400: %s", w.Code, w.Body.String())
		}
		if code := s3ErrCode(t, w.Body.String()); code != "MalformedXML" {
			t.Errorf("code = %q, want MalformedXML (completeMultipartUploadHandler)", code)
		}
	})

	t.Run("DELETE ?uploadId is 204 (abort route)", func(t *testing.T) {
		uploadID := initUpload(t, "gone.bin")
		w := routingRequest(t, "DELETE",
			fmt.Sprintf("/%s/gone.bin?uploadId=%s", bkt, uploadID), nil, nil)
		if w.Code != http.StatusNoContent {
			t.Fatalf("status = %d, want 204: %s", w.Code, w.Body.String())
		}
		uploadMeta := filepath.Join(env.dataDir, bkt, ".metadata", ".uploads", uploadID+".json")
		if _, err := os.Stat(uploadMeta); !os.IsNotExist(err) {
			t.Error("abort should have removed the upload session (abortMultipartUploadHandler)")
		}
	})

	t.Run("DELETE object is 204", func(t *testing.T) {
		w := routingRequest(t, "DELETE", "/"+bkt+"/"+key, nil, nil)
		if w.Code != http.StatusNoContent {
			t.Fatalf("status = %d, want 204: %s", w.Code, w.Body.String())
		}
		if _, err := os.Stat(filepath.Join(env.dataDir, bkt, key)); !os.IsNotExist(err) {
			t.Error("object should be gone (deleteObjectHandler)")
		}
	})

	for _, method := range []string{"PATCH", "OPTIONS"} {
		t.Run(method+" object is 405", func(t *testing.T) {
			w := routingRequest(t, method, "/"+bkt+"/"+key, nil, nil)
			if w.Code != http.StatusMethodNotAllowed {
				t.Fatalf("status = %d, want 405: %s", w.Code, w.Body.String())
			}
			if want := "Method Not Allowed for object"; !strings.Contains(w.Body.String(), want) {
				t.Errorf("body %q missing marker %q", w.Body.String(), want)
			}
		})
	}
}

// ---- presigned vs header auth interplay (auth layer feeding the dispatch) ----

func TestRouting_PresignedVsHeaderAuth(t *testing.T) {
	env := setupTestEnv(t)
	_ = env.setupBucket(t, "bkt")
	env.writeTestObject(t, "bkt", "hello.txt", "routed-body")

	t.Run("presigned GET (no Authorization header) reaches getObjectHandler", func(t *testing.T) {
		req := buildPresignedRequest(t, "GET", "/bkt/hello.txt", "", presignOpts{})
		w := runPresignedThroughRoot(req)
		if w.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200: %s", w.Code, w.Body.String())
		}
		if w.Body.String() != "routed-body" {
			t.Errorf("body = %q, want %q", w.Body.String(), "routed-body")
		}
	})

	t.Run("bogus query signature with Authorization present is header-auth 403 SignatureDoesNotMatch", func(t *testing.T) {
		// X-Amz-* query params would look presigned, but an Authorization
		// header is present → header auth must win (rootHandler leaf-3.2
		// rule). A bogus header signature yields SignatureDoesNotMatch —
		// NOT AuthorizationQueryParametersError, which would prove the
		// presigned path answered instead.
		req := buildPresignedRequest(t, "GET", "/bkt/hello.txt", "", presignOpts{})
		// Drop the presigned X-Amz-* query params and add an x-amz-date
		// header so authenticateRequest gets past its date check; the
		// syntactically valid Authorization block then fails on signature
		// comparison. If the presigned path were chosen instead, the
		// (preserved) query params would yield a DIFFERENT error code.
		q := req.URL.Query()
		for _, p := range requiredPresignedParams {
			q.Del(p.Name)
		}
		req.URL.RawQuery = q.Encode()
		req.Header.Set("x-amz-date", time.Now().UTC().Format(iso8601Format))
		req.Header.Set("Authorization",
			fmt.Sprintf("AWS4-HMAC-SHA256 Credential=%s/%s/us-east-1/s3/aws4_request, SignedHeaders=host, Signature=%s",
				serverCredentials.AccessKeyID, time.Now().UTC().Format(shortDateFormat), strings.Repeat("0", 64)))
		w := runPresignedThroughRoot(req)
		if w.Code != http.StatusForbidden {
			t.Fatalf("status = %d, want 403: %s", w.Code, w.Body.String())
		}
		body := w.Body.String()
		if code := s3ErrCode(t, body); code != "SignatureDoesNotMatch" {
			t.Errorf("code = %q, want SignatureDoesNotMatch (header-auth path)", code)
		}
		if strings.Contains(body, "AuthorizationQueryParametersError") {
			t.Error("got AuthorizationQueryParametersError — presigned path answered; header auth must win")
		}
	})

	t.Run("valid header auth with junk X-Amz-Signature query reaches getObjectHandler (200)", func(t *testing.T) {
		// The junk query param is only survivable via header auth (it is
		// signed as ordinary query data); 200 with the object body proves
		// the header-auth path fed the object dispatch.
		target := "/bkt/hello.txt?X-Amz-Signature=deadbeef"
		w := routingRequest(t, "GET", target, nil, nil)
		if w.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200: %s", w.Code, w.Body.String())
		}
		if w.Body.String() != "routed-body" {
			t.Errorf("body = %q, want %q", w.Body.String(), "routed-body")
		}
	})
}
