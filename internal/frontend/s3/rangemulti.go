package s3

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"sort"
)

// WriteMultipartByteranges streams a 206 multipart/byteranges response.
// fetch(off, end) must return an io.ReadCloser for the byte span
// [off, end) of the object (caller closes). Boundary is generated
// (crypto/rand hex). Content-Type of the object rides in each part
// when contentType != "". Headers already written by the caller:
// none - this function writes everything (status, Content-Type with
// boundary). Never materializes the whole object: each span is fetched
// lazily in ascending order and copied to the client with io.Copy.
//
// Limitation: once the headers and any part bytes have been written the
// response is committed and cannot be retried. A fetch error mid-stream
// therefore aborts the body best-effort (a Flush is attempted and the
// error is returned to the caller for logging/closing) rather than
// producing a clean error response - the client sees a truncated
// multipart body.
func WriteMultipartByteranges(w http.ResponseWriter, key string, size int64,
	contentType string, spans []Span,
	fetch func(off, end int64) (io.ReadCloser, error)) error {

	if len(spans) == 0 {
		return fmt.Errorf("s3: WriteMultipartByteranges called with no ranges for %s", key)
	}

	boundary, err := multipartBoundary()
	if err != nil {
		return fmt.Errorf("s3: boundary generation for %s: %w", key, err)
	}

	w.Header().Set("Content-Type", "multipart/byteranges; boundary="+boundary)
	w.WriteHeader(http.StatusPartialContent)

	// Fetch in ascending offset order regardless of caller ordering.
	ordered := make([]Span, len(spans))
	copy(ordered, spans)
	sort.Slice(ordered, func(i, j int) bool { return ordered[i].Start < ordered[j].Start })

	for _, span := range ordered {
		if _, err := fmt.Fprintf(w, "--%s\r\n", boundary); err != nil {
			return fmt.Errorf("s3: writing part delimiter for %s: %w", key, err)
		}
		if _, err := fmt.Fprintf(w, "Content-Range: bytes %d-%d/%d\r\n", span.Start, span.End-1, size); err != nil {
			return fmt.Errorf("s3: writing Content-Range for %s: %w", key, err)
		}
		if contentType != "" {
			if _, err := fmt.Fprintf(w, "Content-Type: %s\r\n", contentType); err != nil {
				return fmt.Errorf("s3: writing part Content-Type for %s: %w", key, err)
			}
		}
		if _, err := io.WriteString(w, "\r\n"); err != nil {
			return fmt.Errorf("s3: writing part header terminator for %s: %w", key, err)
		}

		rc, err := fetch(span.Start, span.End)
		if err != nil {
			return fmt.Errorf("s3: fetching %s bytes [%d,%d): %w", key, span.Start, span.End, err)
		}
		_, copyErr := io.Copy(w, rc)
		closeErr := rc.Close()
		if copyErr != nil {
			return fmt.Errorf("s3: streaming %s bytes [%d,%d): %w", key, span.Start, span.End, copyErr)
		}
		if closeErr != nil {
			return fmt.Errorf("s3: closing %s range reader [%d,%d): %w", key, span.Start, span.End, closeErr)
		}
		if _, err := io.WriteString(w, "\r\n"); err != nil {
			return fmt.Errorf("s3: writing part trailer for %s: %w", key, err)
		}
	}

	if _, err := fmt.Fprintf(w, "--%s--\r\n", boundary); err != nil {
		return fmt.Errorf("s3: writing closing boundary for %s: %w", key, err)
	}

	// Best-effort flush so a mid-stream failure surfaces to the client
	// promptly rather than sitting in the buffer.
	if rc := http.NewResponseController(w); rc != nil {
		_ = rc.Flush()
	}
	return nil
}

// multipartBoundary generates a 32-char hex boundary from 16 random
// bytes. Hex is CRLF-safe, so the boundary cannot collide with part
// content except with vanishing probability.
func multipartBoundary() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(b[:]), nil
}
