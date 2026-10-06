// h3probe - the HTTP/3 wire probe for e2e case 38 (quic-h3-2026-10 leaf 03).
//
// One binary, two transports:
//
//	-mode h3  - HTTP/3 over QUIC to the h3 frontend, presenting the client
//	            certificate from -cert/-key. Exercises the mTLS webdav data
//	            plane over QUIC: PUT, GET round-trip, Range (206/416),
//	            PROPFIND Depth 1 and on-file (size + etag), and the
//	            handshake-rejection contract (a client WITHOUT a cert or with
//	            a wrong-CA cert fails the TLS handshake - there is no HTTP
//	            401 over h3 for certificate failures).
//	-mode tcp - plain HTTPS (net/http default transport) to a TCP webdav
//	            frontend with Basic auth: 401 without credentials, the same
//	            Range semantics (transport-independent), and the Alt-Svc
//	            advertisement (h3="<port>"; persist=1) on every response.
//	-mode fetch - ONE arbitrary request (default GET; -method/-body) over
//	            HTTP/3 with the configured client certificate; prints
//	            "STATUS <code>" then the raw body (quic-h3-2026-10 leaf 04):
//	            the machine-readable form the ZFS validation harness parses
//	            to fetch ?events / ?events&versions and POST ?batch over
//	            the h3 transport. Additive — the e2e case's usage is
//	            unchanged.
//
// Content is deterministic (byte i = i&0xff), so every Range assert compares
// exact bytes, never lengths.
//
// Output: one PASS/FAIL line per assert, then the tally line
// "h3probe: N/M PASS". Exit code 0 only when N == M.
//
// quic-go is pinned to the SAME version the server's go.mod pins (leaf 02);
// see scripts/e2e/h3probe/go.mod.
package main

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"flag"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/quic-go/quic-go"
	"github.com/quic-go/quic-go/http3"
)

// check records one assert outcome and prints its PASS/FAIL line.
type check struct {
	pass int
	fail int
}

func (c *check) ok(label string, cond bool, detail string) {
	if cond {
		c.pass++
		fmt.Printf("PASS %s\n", label)
		return
	}
	c.fail++
	if detail != "" {
		fmt.Printf("FAIL %s (%s)\n", label, detail)
		return
	}
	fmt.Printf("FAIL %s\n", label)
}

// failErr records a FAIL line for an unexpected transport-level error.
func (c *check) failErr(label string, err error) {
	c.fail++
	fmt.Printf("FAIL %s (%v)\n", label, err)
}

// content builds the deterministic payload: byte i = i&0xff ("iota buffer").
// Every range assert compares against content[lo:hi] slices of this buffer.
func content(n int) []byte {
	b := make([]byte, n)
	for i := range b {
		b[i] = byte(i & 0xff)
	}
	return b
}

func main() {
	mode := flag.String("mode", "h3", "transport mode: h3 (HTTP/3 + client cert) or tcp (plain HTTPS + Basic auth)")
	url := flag.String("url", "", "base URL, e.g. https://127.0.0.1:9443")
	path := flag.String("path", "/e2e38/probe.bin", "object path under -url the probe PUTs and reads")
	user := flag.String("user", "", "Basic auth user (tcp mode)")
	pass := flag.String("pass", "", "Basic auth password (tcp mode)")
	portH3 := flag.String("port-h3", "", "UDP port expected in the tcp-mode alt-svc assert")
	certFile := flag.String("cert", "", "client certificate PEM (h3 mode)")
	keyFile := flag.String("key", "", "client private key PEM (h3 mode)")
	method := flag.String("method", "GET", "HTTP method for -mode fetch (default GET)")
	body := flag.String("body", "", "request body for -mode fetch (non-GET)")
	flag.Parse()

	if *url == "" {
		fmt.Println("FAIL -url is required")
		os.Exit(2)
	}
	base := strings.TrimSuffix(*url, "/")
	objURL := base + *path

	var c check
	var exitCode int
	switch *mode {
	case "h3":
		exitCode = probeH3(&c, base, objURL, *certFile, *keyFile)
	case "tcp":
		exitCode = probeTCP(&c, base, objURL, *user, *pass, *portH3)
	case "fetch":
		exitCode = probeFetch(&c, base, objURL, *certFile, *keyFile, *method, *body)
	default:
		fmt.Printf("FAIL unknown -mode %q (want h3|tcp)\n", *mode)
		os.Exit(2)
	}

	fmt.Printf("h3probe: %d/%d PASS\n", c.pass, c.pass+c.fail)
	os.Exit(exitCode)
}

// probeH3 runs the h3-mode asserts. It ALWAYS also exercises the
// handshake-rejection contract (no-cert and wrong-CA clients) - the case
// passes only when the positive asserts AND both rejections are green.
func probeH3(c *check, base, objURL, certFile, keyFile string) int {
	const size = 4096
	body := content(size)
	key := strings.TrimPrefix(objURL, base)

	// --- positive path: client presents the configured certificate -------
	withCert, errOpen := loadClientCert(certFile, keyFile)
	tlsOK := &tls.Config{
		MinVersion:         tls.VersionTLS13,
		InsecureSkipVerify: true, //nolint:gosec // e2e: the server cert is self-signed
		ServerName:         "localhost",
	}
	if errOpen == nil {
		tlsOK.Certificates = []tls.Certificate{withCert}
	}
	trOK := &http3.Transport{TLSClientConfig: tlsOK}
	defer trOK.Close()
	client := &http.Client{Transport: trOK, Timeout: 30 * time.Second}

	if errOpen != nil {
		c.failErr("h3 PUT with client cert", errOpen)
		c.failErr("h3 GET round-trips exact bytes", errOpen)
		c.failErr("h3 Range bytes=0-99 -> 206 + Content-Range + first 100 bytes", errOpen)
		c.failErr("h3 suffix Range bytes=-50 -> 206 + last 50 bytes", errOpen)
		c.failErr("h3 unsatisfiable Range -> 416", errOpen)
		c.failErr("h3 PROPFIND Depth 1 lists the directory", errOpen)
		c.failErr("h3 PROPFIND on the file shows size + etag", errOpen)
	} else {
		// PUT with the client cert: 201 (create) or 204 (overwrite).
		req, err := http.NewRequest(http.MethodPut, objURL, strings.NewReader(string(body)))
		if err != nil {
			c.failErr("h3 PUT with client cert", err)
		} else {
			req.ContentLength = int64(len(body))
			req.Header.Set("Content-Type", "application/octet-stream")
			resp, err := client.Do(req)
			if err != nil {
				c.failErr("h3 PUT with client cert", err)
			} else {
				drain(resp)
				c.ok("h3 PUT with client cert", resp.StatusCode == 201 || resp.StatusCode == 204,
					fmt.Sprintf("status=%d", resp.StatusCode))

				// GET round-trips the exact bytes.
				getResp, err := client.Get(objURL)
				if err != nil {
					c.failErr("h3 GET round-trips exact bytes", err)
				} else {
					got, err := io.ReadAll(getResp.Body)
					getResp.Body.Close()
					c.ok("h3 GET round-trips exact bytes",
						err == nil && getResp.StatusCode == 200 && string(got) == string(body),
						fmt.Sprintf("status=%d len=%d err=%v", getResp.StatusCode, len(got), err))
				}

				// Range bytes=0-99: 206 + Content-Range + the first 100 bytes.
				assertRange(c, client, objURL, "bytes=0-99", 206,
					fmt.Sprintf("bytes 0-99/%d", size), body[0:100],
					"h3 Range bytes=0-99 -> 206 + Content-Range + first 100 bytes")

				// Suffix range bytes=-50: 206 + the last 50 bytes.
				assertRange(c, client, objURL, "bytes=-50", 206,
					fmt.Sprintf("bytes %d-%d/%d", size-50, size-1, size), body[size-50:],
					"h3 suffix Range bytes=-50 -> 206 + last 50 bytes")

				// Unsatisfiable: 416.
				reqR, _ := http.NewRequest(http.MethodGet, objURL, nil)
				reqR.Header.Set("Range", fmt.Sprintf("bytes=%d-", size+1000))
				r416, err := client.Do(reqR)
				if err != nil {
					c.failErr("h3 unsatisfiable Range -> 416", err)
				} else {
					drain(r416)
					c.ok("h3 unsatisfiable Range -> 416", r416.StatusCode == 416,
						fmt.Sprintf("status=%d", r416.StatusCode))
				}

				// PROPFIND Depth 1 on the parent dir: 207 listing it.
				dirURL := base + strings.TrimSuffix(key, "/"+baseLeaf(key))
				assertPropfind(c, client, dirURL, 1, 207, 2,
					"h3 PROPFIND Depth 1 lists the directory")

				// PROPFIND Depth 0 on the file: size + etag properties.
				assertPropfindFile(c, client, objURL, size)
			}
		}
	}

	// --- negative path: NO client certificate -> TLS HANDSHAKE FAILS -----
	assertHandshakeReject(c, base, "h3 no client cert -> handshake failure (no HTTP 401 over h3)",
		&tls.Config{MinVersion: tls.VersionTLS13, InsecureSkipVerify: true, ServerName: "localhost"}) //nolint:gosec // e2e probe: self-signed server cert

	// --- negative path: WRONG-CA certificate -> same handshake failure ---
	if errOpen == nil {
		roguePEM, rogueKey, err := selfSignedPair("wrong-ca-device")
		if err != nil {
			c.failErr("h3 wrong-CA cert -> handshake failure", err)
		} else {
			rogue, lerr := tls.X509KeyPair(roguePEM, rogueKey)
			if lerr != nil {
				c.failErr("h3 wrong-CA cert -> handshake failure", lerr)
			} else {
				assertHandshakeReject(c, base, "h3 wrong-CA cert -> handshake failure",
					&tls.Config{
						MinVersion:         tls.VersionTLS13,
						InsecureSkipVerify: true, //nolint:gosec // e2e probe: self-signed server cert
						ServerName:         "localhost",
						Certificates:       []tls.Certificate{rogue},
					})
			}
		}
	} else {
		c.failErr("h3 wrong-CA cert -> handshake failure", errOpen)
	}

	if c.fail > 0 {
		return 1
	}
	return 0
}

// assertHandshakeReject drives one h3 GET with the given client TLS config
// and asserts the CONNECTION fails with a QUIC crypto TransportError -
// with RequireAndVerifyClientCert there is no HTTP status on the wire for
// certificate failures.
func assertHandshakeReject(c *check, base, label string, tlsCfg *tls.Config) {
	tr := &http3.Transport{TLSClientConfig: tlsCfg}
	defer tr.Close()
	client := &http.Client{Transport: tr, Timeout: 15 * time.Second}
	resp, err := client.Get(base + "/e2e38-should-reject.bin")
	if err == nil {
		drain(resp)
		c.ok(label, false, fmt.Sprintf("got HTTP %d, want a handshake failure", resp.StatusCode))
		return
	}
	var terr *quic.TransportError
	isCrypto := errors.As(err, &terr) && terr.ErrorCode.IsCryptoError()
	c.ok(label, isCrypto, fmt.Sprintf("err=%v type=%T", err, err))
}

// assertRange runs one Range GET and asserts status, Content-Range, and the
// exact body span.
func assertRange(c *check, client *http.Client, objURL, rng string, wantStatus int, wantCR string, wantBody []byte, label string) {
	req, err := http.NewRequest(http.MethodGet, objURL, nil)
	if err != nil {
		c.failErr(label, err)
		return
	}
	req.Header.Set("Range", rng)
	resp, err := client.Do(req)
	if err != nil {
		c.failErr(label, err)
		return
	}
	defer resp.Body.Close()
	got, err := io.ReadAll(resp.Body)
	crOK := resp.Header.Get("Content-Range") == wantCR
	bodyOK := err == nil && string(got) == string(wantBody)
	c.ok(label, resp.StatusCode == wantStatus && crOK && bodyOK,
		fmt.Sprintf("status=%d content-range=%q len=%d err=%v", resp.StatusCode, resp.Header.Get("Content-Range"), len(got), err))
}

// assertPropfind issues a Depth-N PROPFIND with an empty body and asserts
// 207 plus a minimum count of d:response entries (the directory itself plus
// the probe object).
func assertPropfind(c *check, client *http.Client, url string, depth int, wantStatus, minResponses int, label string) {
	req, err := http.NewRequest("PROPFIND", url, nil)
	if err != nil {
		c.failErr(label, err)
		return
	}
	req.Header.Set("Depth", fmt.Sprint(depth))
	resp, err := client.Do(req)
	if err != nil {
		c.failErr(label, err)
		return
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	n := strings.Count(string(b), "<d:response>") + strings.Count(string(b), "<d:response ")
	c.ok(label, resp.StatusCode == wantStatus && n >= minResponses,
		fmt.Sprintf("status=%d d:response=%d", resp.StatusCode, n))
}

// assertPropfindFile PROPFINDs the file (Depth 0) and asserts the 207 body
// carries getcontentlength with the exact size and a getetag.
func assertPropfindFile(c *check, client *http.Client, objURL string, size int) {
	label := "h3 PROPFIND on the file shows size + etag"
	req, err := http.NewRequest("PROPFIND", objURL, nil)
	if err != nil {
		c.failErr(label, err)
		return
	}
	req.Header.Set("Depth", "0")
	resp, err := client.Do(req)
	if err != nil {
		c.failErr(label, err)
		return
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	body := string(b)
	hasSize := strings.Contains(body, "<d:getcontentlength>"+itoa(size)+"</d:getcontentlength>")
	hasETag := strings.Contains(body, "<d:getetag>")
	c.ok(label, resp.StatusCode == 207 && hasSize && hasETag,
		fmt.Sprintf("status=%d size-prop=%v etag-prop=%v", resp.StatusCode, hasSize, hasETag))
}

// probeTCP runs the tcp-mode asserts over plain HTTPS with Basic auth.
func probeTCP(c *check, base, objURL, user, pass, portH3 string) int {
	const size = 4096
	body := content(size)
	seedURL := base + "/e2e38/tcp-seed.bin"

	// e2e: the server certificate is self-signed; skip ITS verification
	// (the client-certificate assert lives in h3 mode, not here). One
	// explicit transport for EVERY tcp-mode request so the config applies
	// uniformly (the lazy default transport would verify and fail).
	tcpClient := &http.Client{
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{
				InsecureSkipVerify: true, //nolint:gosec // e2e loopback probe
				ServerName:         "localhost",
			},
		},
		Timeout: 30 * time.Second,
	}

	// Seed the object over TCP with Basic auth (tcp-mode write; the Range
	// asserts below then read the same bytes through the same transport).
	req, err := http.NewRequest(http.MethodPut, seedURL, strings.NewReader(string(body)))
	if err != nil {
		c.failErr("tcp PUT with Basic auth", err)
	} else {
		req.ContentLength = int64(len(body))
		req.SetBasicAuth(user, pass)
		resp, err := tcpClient.Do(req)
		if err != nil {
			c.failErr("tcp PUT with Basic auth", err)
		} else {
			drain(resp)
			c.ok("tcp PUT with Basic auth", resp.StatusCode == 201 || resp.StatusCode == 204,
				fmt.Sprintf("status=%d", resp.StatusCode))
		}
	}

	// 401 on unauthenticated GET.
	resp401, err := tcpClient.Get(objURL)
	if err != nil {
		c.failErr("tcp GET without credentials -> 401", err)
	} else {
		drain(resp401)
		c.ok("tcp GET without credentials -> 401", resp401.StatusCode == 401,
			fmt.Sprintf("status=%d", resp401.StatusCode))
	}

	// Authenticated requests ride the SAME skip-verify transport.
	client := tcpClient
	authedGet := func(rawURL string, headers map[string]string) (*http.Response, error) {
		req, err := http.NewRequest(http.MethodGet, rawURL, nil)
		if err != nil {
			return nil, err
		}
		req.SetBasicAuth(user, pass)
		for k, v := range headers {
			req.Header.Set(k, v)
		}
		return client.Do(req)
	}

	// GET round-trips the exact bytes.
	gr, err := authedGet(seedURL, nil)
	if err != nil {
		c.failErr("tcp GET with Basic auth round-trips exact bytes", err)
	} else {
		got, rerr := io.ReadAll(gr.Body)
		gr.Body.Close()
		c.ok("tcp GET with Basic auth round-trips exact bytes",
			rerr == nil && gr.StatusCode == 200 && string(got) == string(body),
			fmt.Sprintf("status=%d len=%d err=%v", gr.StatusCode, len(got), rerr))
	}

	// Range parity over TCP: the SAME spans the h3 asserts use.
	tcpRange(c, client, authedGet, seedURL, "bytes=0-99", 206,
		fmt.Sprintf("bytes 0-99/%d", size), body[0:100],
		"tcp Range bytes=0-99 -> 206 + Content-Range + first 100 bytes")
	tcpRange(c, client, authedGet, seedURL, "bytes=-50", 206,
		fmt.Sprintf("bytes %d-%d/%d", size-50, size-1, size), body[size-50:],
		"tcp suffix Range bytes=-50 -> 206 + last 50 bytes")

	// Unsatisfiable over TCP: 416.
	ru, err := authedGet(seedURL, map[string]string{"Range": fmt.Sprintf("bytes=%d-", size+1000)})
	if err != nil {
		c.failErr("tcp unsatisfiable Range -> 416", err)
	} else {
		drain(ru)
		c.ok("tcp unsatisfiable Range -> 416", ru.StatusCode == 416,
			fmt.Sprintf("status=%d", ru.StatusCode))
	}

	// Alt-Svc: EVERY response carries alt-svc: h3="<port>"; persist=1.
	want := fmt.Sprintf(`h3=":%s"; persist=1`, portH3)
	altOK := true
	var altSeen []string
	for _, r := range []*http.Response{gr, ru} {
		if r == nil {
			continue
		}
		v := r.Header.Get("Alt-Svc")
		altSeen = append(altSeen, v)
		if v != want {
			altOK = false
		}
	}
	c.ok(fmt.Sprintf("tcp every response carries alt-svc %q", want), altOK,
		fmt.Sprintf("seen=%q", altSeen))

	if c.fail > 0 {
		return 1
	}
	return 0
}

// tcpRange is assertRange over the Basic-auth GET closure (tcp mode).
func tcpRange(c *check, _ *http.Client, authedGet func(string, map[string]string) (*http.Response, error),
	rawURL, rng string, wantStatus int, wantCR string, wantBody []byte, label string) {
	resp, err := authedGet(rawURL, map[string]string{"Range": rng})
	if err != nil {
		c.failErr(label, err)
		return
	}
	defer resp.Body.Close()
	got, rerr := io.ReadAll(resp.Body)
	crOK := resp.Header.Get("Content-Range") == wantCR
	bodyOK := rerr == nil && string(got) == string(wantBody)
	c.ok(label, resp.StatusCode == wantStatus && crOK && bodyOK,
		fmt.Sprintf("status=%d content-range=%q len=%d err=%v", resp.StatusCode, resp.Header.Get("Content-Range"), len(got), rerr))
}

// probeFetch (quic-h3-2026-10 leaf 04) runs ONE arbitrary request over the
// h3 transport with the configured client certificate and prints
// "STATUS <code>" followed by the raw body — the form the ZFS validation
// harness parses. Additive mode: h3/tcp are untouched.
func probeFetch(c *check, base, objURL, certFile, keyFile, method, body string) int {
	withCert, errOpen := loadClientCert(certFile, keyFile)
	if errOpen != nil {
		fmt.Printf("STATUS 0\n")
		c.failErr("fetch "+method+" "+objURL, errOpen)
		return 1
	}
	tlsCfg := &tls.Config{
		MinVersion:         tls.VersionTLS13,
		InsecureSkipVerify: true, //nolint:gosec // harness: the server cert is self-signed
		ServerName:         "localhost",
		Certificates:       []tls.Certificate{withCert},
	}
	tr := &http3.Transport{TLSClientConfig: tlsCfg}
	defer tr.Close()
	client := &http.Client{Transport: tr, Timeout: 30 * time.Second}
	var rdr io.Reader
	if body != "" {
		rdr = strings.NewReader(body)
	}
	req, err := http.NewRequest(method, objURL, rdr)
	if err != nil {
		fmt.Printf("STATUS 0\n")
		c.failErr("fetch "+method+" "+objURL, err)
		return 1
	}
	if body != "" {
		req.ContentLength = int64(len(body))
	}
	resp, err := client.Do(req)
	if err != nil {
		fmt.Printf("STATUS 0\n")
		c.failErr("fetch "+method+" "+objURL, err)
		return 1
	}
	defer resp.Body.Close()
	b, rerr := io.ReadAll(resp.Body)
	fmt.Printf("STATUS %d\n", resp.StatusCode)
	_, _ = os.Stdout.Write(b)
	ok := rerr == nil && resp.StatusCode >= 200 && resp.StatusCode < 300
	c.ok("fetch "+method+" "+objURL, ok,
		fmt.Sprintf("status=%d err=%v", resp.StatusCode, rerr))
	if !ok {
		return 1
	}
	return 0
}

// baseLeaf returns the last path segment (the object name) of key.
func baseLeaf(key string) string {
	k := strings.TrimSuffix(key, "/")
	if _, leaf, found := strings.CutLast(k, "/"); found {
		return leaf
	}
	return k
}

func itoa(n int) string { return fmt.Sprint(n) }

func drain(resp *http.Response) {
	if resp == nil {
		return
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
}

// loadClientCert loads the -cert/-key pair for the h3 client.
func loadClientCert(certFile, keyFile string) (tls.Certificate, error) {
	if certFile == "" || keyFile == "" {
		return tls.Certificate{}, errors.New("-cert/-key are required in h3 mode")
	}
	return tls.LoadX509KeyPair(certFile, keyFile)
}

// selfSignedPair mints a throwaway self-signed certificate (the "wrong CA"
// client: its issuer is trusted by nobody). The probe generates it so the
// case needs exactly one PKI generation site (the case script's CA).
func selfSignedPair(cn string) (certPEM, keyPEM []byte, err error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, nil, err
	}
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(time.Now().UnixNano()),
		Subject:               pkix.Name{CommonName: cn},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(24 * time.Hour),
		KeyUsage:              x509.KeyUsageDigitalSignature,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
		BasicConstraintsValid: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		return nil, nil, err
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return nil, nil, err
	}
	certPEM = pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyPEM = pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER})
	return certPEM, keyPEM, nil
}
