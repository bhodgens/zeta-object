package main

// concurrency_stress_test.go — leaf 4.8: Go-native concurrency stress suite.
//
// The e2e suite has one 20-iteration bash PUT/GET loop; these tests prove the
// locking contracts (lockObject, getMultipartLock, writeFileAtomic) at higher
// concurrency through the REAL signed-request path (rootHandler + SigV4).
//
// Invariants under -race:
//   1. ConcurrentSameKeyPutGetDelete — GET never observes a torn/empty body
//      and never 500s; every 200 body is byte-exact from SOME complete PUT.
//   2. ConcurrentDifferentKeys      — all writes succeed; final list shows
//      every key (no lost writes).
//   3. ConcurrentUploadPartAbort    — terminal session state is consistent:
//      session gone, or session present with ALL parts and part files.
//   4. ConcurrentCreateDeleteBucket — ListBuckets always 200 + parseable XML.
//   5. ConcurrentCompleteMultipart  — complete racing the final uploadPart
//      yields success or NoSuchUpload, never a torn final object.
//   6. ConcurrentListDuringWrite    — every list parses; KeyCount <= total.
//
// Goroutines must not use t.Fatal/t.Helper failures directly: every failure
// funnels through errCollector, checked on the test goroutine after join.

import (
	"crypto/md5"
	"encoding/hex"
	"encoding/json"
	"encoding/xml"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// ---- t-free signed-request plumbing (mirrors routing_test.go helpers) ----

// stressSign signs req with header-auth SigV4 exactly like routingAuth, but
// without any *testing.T dependency so it is safe inside goroutines.
func stressSign(req *http.Request, body []byte) {
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

// stressDo runs one fully-signed request through rootHandler and returns the
// status code and body. Never touches *testing.T.
func stressDo(method, target string, body []byte) (int, []byte) {
	req := httptest.NewRequest(method, target, strings.NewReader(string(body)))
	stressSign(req, body)
	w := httptest.NewRecorder()
	rootHandler(w, req)
	return w.Code, w.Body.Bytes()
}

// ---- error collector (every goroutine failure funnels here) ----

type errCollector struct {
	mu   sync.Mutex
	errs []string
}

func (c *errCollector) errorf(format string, args ...any) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.errs = append(c.errs, fmt.Sprintf(format, args...))
}

func (c *errCollector) len() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.errs)
}

// report drains the collector onto t, capped so a systemic failure does not
// flood the log.
func (c *errCollector) report(t *testing.T) {
	t.Helper()
	c.mu.Lock()
	defer c.mu.Unlock()
	const max = 20
	for i, e := range c.errs {
		if i >= max {
			t.Errorf("...and %d more errors", len(c.errs)-max)
			break
		}
		t.Error(e)
	}
}

// stressCreateBucket creates the test bucket through the real handler path.
func stressCreateBucket(t *testing.T, bkt string) {
	t.Helper()
	code, body := stressDo(http.MethodPut, "/"+bkt, nil)
	if code != http.StatusOK {
		t.Fatalf("create bucket %q: status = %d: %s", bkt, code, body)
	}
}

// stressInitUpload initiates a multipart upload and returns the UploadID.
func stressInitUpload(t *testing.T, bkt, obj string) string {
	t.Helper()
	code, body := stressDo(http.MethodPost, "/"+bkt+"/"+obj+"?uploads", nil)
	if code != http.StatusOK {
		t.Fatalf("init multipart %s/%s: status = %d: %s", bkt, obj, code, body)
	}
	var res InitiateMultipartUploadResult
	if err := xml.Unmarshal(body, &res); err != nil {
		t.Fatalf("init multipart: bad XML: %v", err)
	}
	if res.UploadID == "" {
		t.Fatalf("init multipart: empty UploadID in %s", body)
	}
	return res.UploadID
}

// stressETag computes the MD5 hex ETag the server stores for content.
func stressETag(content string) string {
	sum := md5.Sum([]byte(content))
	return hex.EncodeToString(sum[:])
}

// parseListV2 unmarshals a ListObjectsV2 response body.
func parseListV2(t *testing.T, body []byte) ListBucketResult {
	t.Helper()
	var res ListBucketResult
	if err := xml.Unmarshal(body, &res); err != nil {
		t.Fatalf("list body does not parse as ListBucketResult: %v (%q)", err, body)
	}
	return res
}

// ====================================================================
// 1. ConcurrentSameKeyPutGetDelete
// ====================================================================

// TestConcurrentSameKeyPutGetDelete hammers ONE key with 8 goroutines x 50
// iterations of mixed PUT/GET/DELETE. Invariants: GET returns 200 with a
// byte-exact body from some complete PUT, or 404 — never a torn or empty
// body, never a 5xx.
func TestConcurrentSameKeyPutGetDelete(t *testing.T) {
	env := setupTestEnv(t)
	const bkt, key = "stress-same", "hot/key"
	stressCreateBucket(t, bkt)

	// Values are unique per (goroutine, iteration) with unique prefixes, so
	// a torn/truncated body can never collide with a legitimate value.
	valueFor := func(g, i int) string {
		return fmt.Sprintf("g%02d-i%03d-%s", g, i, strings.Repeat("p", g+1))
	}

	var (
		col         errCollector
		knownMu     sync.Mutex
		known       = map[string]struct{}{}
		wg          sync.WaitGroup
		workers     = 8
		iters       = 50
		getOK       atomicCounter
		getNotFound atomicCounter
	)
	known[""] = struct{}{} // never legal: guards against empty-body passes

	for g := range workers {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for i := range iters {
				val := valueFor(g, i)
				// Register the value BEFORE the PUT: the atomic rename means
				// the file only ever appears complete, so pre-registering can
				// only widen the legal set, never mask a torn read.
				knownMu.Lock()
				known[val] = struct{}{}
				knownMu.Unlock()

				switch i % 3 {
				case 0, 1: // PUT then GET
					code, _ := stressDo(http.MethodPut, "/"+bkt+"/"+key, []byte(val))
					if code != http.StatusOK {
						// Leaf 4.8 invariants govern GET behavior; a PUT 500
						// under concurrent DELETE is nonetheless logged as a
						// production finding (see deleteObjectCore races).
						col.errorf("PUT g%d i%d: status = %d", g, i, code)
						continue
					}
					stressGetSameKey(&col, &knownMu, known, bkt, key, &getOK, &getNotFound)
				case 2: // GET then DELETE
					stressGetSameKey(&col, &knownMu, known, bkt, key, &getOK, &getNotFound)
					code, _ := stressDo(http.MethodDelete, "/"+bkt+"/"+key, nil)
					if code != http.StatusNoContent && code != http.StatusNotFound {
						col.errorf("DELETE g%d i%d: status = %d, want 204 or 404", g, i, code)
					}
				}
			}
		}(g)
	}
	wg.Wait()

	col.report(t)
	if col.len() > 0 {
		t.Fatalf("ConcurrentSameKeyPutGetDelete: %d invariant failures (dataDir %s)", col.len(), env.dataDir)
	}
	t.Logf("GETs: %d x 200, %d x 404", getOK.load(), getNotFound.load())
}

// stressGetSameKey issues one GET and asserts the torn-body/5xx invariants.
func stressGetSameKey(col *errCollector, knownMu *sync.Mutex, known map[string]struct{},
	bkt, key string, getOK, getNotFound *atomicCounter) {

	code, body := stressDo(http.MethodGet, "/"+bkt+"/"+key, nil)
	switch code {
	case http.StatusOK:
		if len(body) == 0 {
			col.errorf("GET returned 200 with EMPTY body")
			return
		}
		knownMu.Lock()
		_, ok := known[string(body)]
		knownMu.Unlock()
		if !ok {
			col.errorf("TORN BODY: GET returned %d bytes not matching any complete PUT: %q", len(body), truncateForLog(body))
			return
		}
		getOK.inc()
	case http.StatusNotFound:
		getNotFound.inc()
	default:
		col.errorf("GET status = %d (body %q); want 200 or 404, never 5xx", code, truncateForLog(body))
	}
}

func truncateForLog(b []byte) string {
	if len(b) > 64 {
		return string(b[:64]) + "..."
	}
	return string(b)
}

// atomicCounter is a minimal shared counter (stdlib only).
type atomicCounter struct {
	mu sync.Mutex
	n  int
}

func (c *atomicCounter) inc() { c.mu.Lock(); c.n++; c.mu.Unlock() }
func (c *atomicCounter) load() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.n
}

// ====================================================================
// 2. ConcurrentDifferentKeys
// ====================================================================

// TestConcurrentDifferentKeys has 8 goroutines write 50 distinct keys each.
// Every PUT must succeed and the final ListObjectsV2 must contain all 400
// keys — no lost writes.
func TestConcurrentDifferentKeys(t *testing.T) {
	setupTestEnv(t)
	const bkt = "stress-distinct"
	stressCreateBucket(t, bkt)

	const (
		workers = 8
		iters   = 50
	)
	var (
		col errCollector
		wg  sync.WaitGroup
	)
	for g := range workers {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for i := range iters {
				key := fmt.Sprintf("dist/g%d/k%03d", g, i)
				val := fmt.Sprintf("payload-%d-%d", g, i)
				code, body := stressDo(http.MethodPut, "/"+bkt+"/"+key, []byte(val))
				if code != http.StatusOK {
					col.errorf("PUT %s: status = %d: %s", key, code, body)
				}
			}
		}(g)
	}
	wg.Wait()

	code, body := stressDo(http.MethodGet, "/"+bkt+"?list-type=2", nil)
	if code != http.StatusOK {
		t.Fatalf("final list: status = %d: %s", code, body)
	}
	res := parseListV2(t, body)
	if res.KeyCount != workers*iters || len(res.Contents) != workers*iters {
		t.Errorf("final list KeyCount = %d, Contents = %d; want %d (lost writes?)",
			res.KeyCount, len(res.Contents), workers*iters)
	}
	seen := map[string]bool{}
	for _, o := range res.Contents {
		seen[o.Key] = true
	}
	for g := range workers {
		for i := range iters {
			key := fmt.Sprintf("dist/g%d/k%03d", g, i)
			if !seen[key] {
				t.Errorf("LOST WRITE: key %q missing from final list", key)
			}
		}
	}
}

// ====================================================================
// 3. ConcurrentUploadPartAbort
// ====================================================================

// TestConcurrentUploadPartAbort races 5 part uploads against an abort on one
// session. getMultipartLock must serialize them: the terminal state is
// either "session gone (meta AND _parts dir removed)" or "session present
// with all 5 parts recorded and their part files on disk" — never partial
// meta, never a missing _parts dir under live meta, never a panic.
func TestConcurrentUploadPartAbort(t *testing.T) {
	env := setupTestEnv(t)
	const bkt, obj = "stress-abort", "abort.bin"
	stressCreateBucket(t, bkt)

	uploadID := stressInitUpload(t, bkt, obj)

	const parts = 5
	var (
		col       errCollector
		wg        sync.WaitGroup
		abortDone = make(chan struct{})
	)

	// Part uploaders: distinct parts 1..5, fire as soon as the session is live.
	for p := 1; p <= parts; p++ {
		wg.Add(1)
		go func(p int) {
			defer wg.Done()
			body := fmt.Sprintf("part-%d-data", p)
			code, resp := stressDo(http.MethodPut,
				fmt.Sprintf("/%s/%s?partNumber=%d&uploadId=%s", bkt, obj, p, uploadID),
				[]byte(body))
			switch code {
			case http.StatusOK:
			case http.StatusNotFound: // abort won the race for this part
			default:
				col.errorf("uploadPart %d: status = %d: %s", p, code, resp)
			}
		}(p)
	}

	// Aborter runs concurrently.
	wg.Go(func() {
		code, resp := stressDo(http.MethodDelete,
			fmt.Sprintf("/%s/%s?uploadId=%s", bkt, obj, uploadID), nil)
		switch code {
		case http.StatusNoContent: // abort won
		case http.StatusNotFound: // every part beat the abort
		default:
			col.errorf("abort: status = %d: %s", code, resp)
		}
		close(abortDone)
	})

	wg.Wait()
	<-abortDone

	// Terminal-state audit straight from the filesystem.
	uploadsDir := filepath.Join(env.dataDir, bkt, ".metadata", ".uploads")
	metaPath := filepath.Join(uploadsDir, uploadID+".json")
	partsDir := filepath.Join(uploadsDir, uploadID+"_parts")

	_, metaErr := os.Stat(metaPath)
	_, partsDirErr := os.Stat(partsDir)

	if os.IsNotExist(metaErr) {
		// Session gone: the parts dir must be gone too.
		if !os.IsNotExist(partsDirErr) {
			t.Errorf("INCONSISTENT: session meta gone but _parts dir remains at %s", partsDir)
		}
		return
	}
	if metaErr != nil {
		t.Fatalf("stat session meta: %v", metaErr)
	}
	// Session survived: it must hold ALL parts and their files.
	data, err := os.ReadFile(metaPath) //nolint:gosec // uploadID is 32-hex from the handler.
	if err != nil {
		t.Fatalf("read session meta: %v", err)
	}
	var session MultipartUpload
	if err := json.Unmarshal(data, &session); err != nil {
		t.Fatalf("session meta does not parse: %v", err)
	}
	if len(session.Parts) != parts {
		t.Errorf("PARTIAL META: session has %d parts recorded, want %d (abort tore the session?)", len(session.Parts), parts)
	}
	if os.IsNotExist(partsDirErr) {
		t.Errorf("INCONSISTENT: session meta live but _parts dir missing at %s", partsDir)
		return
	}
	for p := 1; p <= parts; p++ {
		partFile := filepath.Join(partsDir, fmt.Sprintf("part-%d", p))
		if _, err := os.Stat(partFile); err != nil {
			t.Errorf("part %d recorded in meta but file missing: %v", p, err)
		}
	}
}

// ====================================================================
// 4. ConcurrentCreateDeleteBucket
// ====================================================================

// TestConcurrentCreateDeleteBucket alternates create/delete of one bucket
// name 30x while a reader loops ListBuckets. No panic; every list is 200
// with parseable ListAllMyBucketsResult XML.
func TestConcurrentCreateDeleteBucket(t *testing.T) {
	setupTestEnv(t)
	const bkt = "stress-churn"

	var (
		col      errCollector
		workers  sync.WaitGroup
		done     = make(chan struct{})
		readerWG sync.WaitGroup
	)

	// Creator: 30x create (200 fresh / 409 re-create are both legal).
	workers.Go(func() {
		for range 30 {
			code, body := stressDo(http.MethodPut, "/"+bkt, nil)
			if code != http.StatusOK && code != http.StatusConflict {
				col.errorf("create %q: status = %d: %s", bkt, code, body)
			}
		}
	})

	// Deleter: 30x delete (204 gone / 404 never-existed are both legal).
	workers.Go(func() {
		for range 30 {
			code, body := stressDo(http.MethodDelete, "/"+bkt, nil)
			if code != http.StatusNoContent && code != http.StatusNotFound {
				col.errorf("delete %q: status = %d: %s", bkt, code, body)
			}
		}
	})

	// Reader: loop ListBuckets until both workers finish.
	readerWG.Go(func() {
		for {
			select {
			case <-done:
				return
			default:
			}
			code, body := stressDo(http.MethodGet, "/", nil)
			if code != http.StatusOK {
				col.errorf("ListBuckets: status = %d: %s", code, body)
				continue
			}
			var res ListAllMyBucketsResult
			if err := xml.Unmarshal(body, &res); err != nil {
				col.errorf("ListBuckets XML does not parse: %v (%q)", err, truncateForLog(body))
			}
		}
	})

	workers.Wait()
	close(done)
	readerWG.Wait()
	col.report(t)
	if col.len() > 0 {
		t.Fatalf("ConcurrentCreateDeleteBucket: %d failures", col.len())
	}
}

// ====================================================================
// 5. ConcurrentCompleteMultipart
// ====================================================================

// TestConcurrentCompleteMultipart races complete against a final uploadPart
// on the same session. The complete request names part 1 only, so outcomes
// are: complete wins (part 2 gets 404 NoSuchUpload) or part 2 wins (both
// 200). Either way the final object must be byte-exact part-1 content —
// never torn, never sized from a part not in the request.
func TestConcurrentCompleteMultipart(t *testing.T) {
	env := setupTestEnv(t)
	const bkt, obj = "stress-complete", "race.bin"
	stressCreateBucket(t, bkt)

	const rounds = 15
	var col errCollector

	for round := range rounds {
		key := fmt.Sprintf("race-%d.bin", round)
		uploadID := stressInitUpload(t, bkt, key)

		part1 := fmt.Sprintf("final-part-one-round-%d-<%s>", round, strings.Repeat("A", round+1))
		part2 := fmt.Sprintf("late-part-two-round-%d", round)

		// Pre-store part 1 OUTSIDE the race so complete always has a valid
		// part to assemble; the race is complete-vs-late-part-2.
		code1, body1 := stressDo(http.MethodPut,
			fmt.Sprintf("/%s/%s?partNumber=1&uploadId=%s", bkt, key, uploadID),
			[]byte(part1))
		if code1 != http.StatusOK {
			t.Fatalf("round %d: setup uploadPart 1: status = %d: %s", round, code1, body1)
		}

		var wg sync.WaitGroup
		var completeCode, partCode int

		wg.Go(func() {
			completeCode, _ = stressDoComplete(t, bkt, key, uploadID,
				[]PartToUpload{{PartNumber: 1, ETag: stressETag(part1)}})
		})

		wg.Go(func() {
			partCode, _ = stressDo(http.MethodPut,
				fmt.Sprintf("/%s/%s?partNumber=2&uploadId=%s", bkt, key, uploadID),
				[]byte(part2))
		})

		wg.Wait()

		// Outcome legality.
		switch completeCode {
		case http.StatusOK:
		case http.StatusNotFound: // NoSuchUpload — legal only if impossible here (part 1 pre-stored); flag it
			col.errorf("round %d: complete returned 404 NoSuchUpload though part 1 was stored before the race", round)
		default:
			col.errorf("round %d: complete status = %d, want 200 (or 404)", round, completeCode)
		}
		switch partCode {
		case http.StatusOK: // part 2 beat complete
		case http.StatusNotFound: // complete won; session gone
		default:
			col.errorf("round %d: late uploadPart status = %d, want 200 or 404", round, partCode)
		}

		// Final object must be byte-exact part-1 content — the request named
		// only part 1, so ANY part-2 bytes in the body is a torn assembly.
		code, body := stressDo(http.MethodGet, "/"+bkt+"/"+key, nil)
		if code != http.StatusOK {
			col.errorf("round %d: final GET status = %d: %s", round, code, body)
			continue
		}
		if string(body) != part1 {
			col.errorf("round %d: TORN FINAL OBJECT: got %d bytes %q, want part-1 (%d bytes)",
				round, len(body), truncateForLog(body), len(part1))
		}
	}

	col.report(t)
	if col.len() > 0 {
		t.Fatalf("ConcurrentCompleteMultipart: %d failures (dataDir %s)", col.len(), env.dataDir)
	}
}

// stressDoComplete POSTs a CompleteMultipartUpload document.
func stressDoComplete(t *testing.T, bkt, obj, uploadID string, parts []PartToUpload) (int, []byte) {
	t.Helper()
	doc, err := xml.Marshal(CompleteMultipartUpload{Parts: parts})
	if err != nil {
		t.Fatalf("marshal complete body: %v", err)
	}
	return stressDo(http.MethodPost, "/"+bkt+"/"+obj+"?uploadId="+uploadID, doc)
}

// ====================================================================
// 6. ConcurrentListDuringWrite
// ====================================================================

// TestConcurrentListDuringWrite runs 1 writer (100 PUTs over distinct keys)
// against 4 reader goroutines looping ListObjectsV2. Every list must parse,
// KeyCount must equal len(Contents) and never exceed the 100 total keys,
// and every listed key must be one the writer could have written.
func TestConcurrentListDuringWrite(t *testing.T) {
	setupTestEnv(t)
	const (
		bkt   = "stress-listwrite"
		total = 100
	)
	stressCreateBucket(t, bkt)

	var (
		col     errCollector
		writer  sync.WaitGroup
		readers sync.WaitGroup
		done    = make(chan struct{})
	)

	writer.Go(func() {
		for i := range total {
			key := fmt.Sprintf("stream/obj-%03d", i)
			code, body := stressDo(http.MethodPut, "/"+bkt+"/"+key, fmt.Appendf(nil, "data-%03d", i))
			if code != http.StatusOK {
				col.errorf("writer PUT %s: status = %d: %s", key, code, body)
			}
		}
	})

	for r := range 4 {
		readers.Add(1)
		go func(r int) {
			defer readers.Done()
			for {
				select {
				case <-done:
					return
				default:
				}
				code, body := stressDo(http.MethodGet, "/"+bkt+"?list-type=2", nil)
				if code != http.StatusOK {
					col.errorf("reader %d: list status = %d: %s", r, code, body)
					continue
				}
				var res ListBucketResult
				if err := xml.Unmarshal(body, &res); err != nil {
					col.errorf("reader %d: list XML does not parse: %v (%q)", r, err, truncateForLog(body))
					continue
				}
				if res.KeyCount != len(res.Contents) {
					col.errorf("reader %d: KeyCount = %d but len(Contents) = %d", r, res.KeyCount, len(res.Contents))
				}
				if res.KeyCount > total {
					col.errorf("reader %d: KeyCount = %d exceeds total written keys %d", r, res.KeyCount, total)
				}
				for _, o := range res.Contents {
					if !strings.HasPrefix(o.Key, "stream/obj-") {
						col.errorf("reader %d: unexpected key %q in listing", r, o.Key)
					}
				}
			}
		}(r)
	}

	writer.Wait()
	close(done)
	readers.Wait()

	// Final consistency: after the writer is done, one more list must show
	// every key (lists never lose settled writes).
	code, body := stressDo(http.MethodGet, "/"+bkt+"?list-type=2", nil)
	if code != http.StatusOK {
		t.Fatalf("final list: status = %d: %s", code, body)
	}
	res := parseListV2(t, body)
	if res.KeyCount != total {
		t.Errorf("final list KeyCount = %d, want %d", res.KeyCount, total)
	}

	col.report(t)
	if col.len() > 0 {
		t.Fatalf("ConcurrentListDuringWrite: %d failures", col.len())
	}
}
