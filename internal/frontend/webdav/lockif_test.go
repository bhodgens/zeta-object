// lockif_test.go — leaf 02 (webdav-locking-2026-10): Contract 2 tests.
// Table-driven ParseIfHeader cases (single token, token list, tagged
// lists, malformed input) and CheckWriteLock enforcement (unlocked,
// matching token, missing/wrong token).
package webdav

import (
	"errors"
	"testing"
)

func TestParseIfHeader(t *testing.T) {
	const (
		tokA = "opaquelocktoken:11111111-2222-3333-4444-555555555555"
		tokB = "opaquelocktoken:aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee"
	)
	tests := []struct {
		name string
		hdr  string
		want []string
	}{
		{name: "empty header", hdr: "", want: nil},
		{name: "whitespace only", hdr: "   ", want: nil},
		{
			name: "single token",
			hdr:  "(<" + tokA + ">)",
			want: []string{tokA},
		},
		{
			name: "single token with inner spaces",
			hdr:  "( <" + tokA + "> )",
			want: []string{tokA},
		},
		{
			name: "token list",
			hdr:  "(<" + tokA + ">) (<" + tokB + ">)",
			want: []string{tokA, tokB},
		},
		{
			name: "token list comma separated",
			hdr:  "(<" + tokA + ">), (<" + tokB + ">)",
			want: []string{tokA, tokB},
		},
		{
			name: "tagged list single",
			hdr:  "<http://example.com/doc.txt> (<" + tokA + ">)",
			want: []string{tokA},
		},
		{
			name: "tagged list with multiple lists",
			hdr:  "<http://example.com/doc.txt> (<" + tokA + ">) (<" + tokB + ">)",
			want: []string{tokA, tokB},
		},
		{
			name: "two tagged productions",
			hdr:  "<http://a/f> (<" + tokA + ">), <http://b/g> (<" + tokB + ">)",
			want: []string{tokA, tokB},
		},
		{
			name: "not prefix tolerated",
			hdr:  "(Not <" + tokA + ">) <" + tokB + ">",
			want: []string{tokA, tokB},
		},
		{
			name: "etag condition ignored token extracted",
			hdr:  "(<\"strong-etag\"> <" + tokA + ">)",
			want: []string{tokA},
		},
		{
			name: "no tokens in valid list",
			hdr:  "(<\"strong-etag\">)",
			want: nil,
		},
		{name: "malformed unbalanced open paren", hdr: "(<" + tokA + ">", want: nil},
		{name: "malformed unterminated tag", hdr: "<" + tokA + ">)", want: nil},
		{name: "malformed bare word", hdr: "garbage", want: nil},
		{name: "malformed stray closing paren", hdr: ")", want: nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := ParseIfHeader(tt.hdr)
			if len(got) != len(tt.want) {
				t.Fatalf("ParseIfHeader(%q) = %v, want %v", tt.hdr, got, tt.want)
			}
			for i := range tt.want {
				if got[i] != tt.want[i] {
					t.Fatalf("ParseIfHeader(%q)[%d] = %q, want %q", tt.hdr, i, got[i], tt.want[i])
				}
			}
		})
	}
}

func TestCheckWriteLock(t *testing.T) {
	s, _ := newTestLockStore(t)
	held, err := s.Acquire("/b/doc.txt", LockInfo{Owner: "mailto:a@example.com", Depth: "0", Timeout: 600})
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	other, err := s.Acquire("/b/other.txt", LockInfo{Depth: "0", Timeout: 600})
	if err != nil {
		t.Fatalf("Acquire other: %v", err)
	}

	tests := []struct {
		name     string
		key      string
		ifHeader string
		wantErr  error
	}{
		{
			name:     "unlocked resource",
			key:      "/b/free.txt",
			ifHeader: "",
			wantErr:  nil,
		},
		{
			name:     "locked matching token single",
			key:      "/b/doc.txt",
			ifHeader: "(<" + held.Token + ">)",
			wantErr:  nil,
		},
		{
			name:     "locked matching token in list",
			key:      "/b/doc.txt",
			ifHeader: "(<opaquelocktoken:deadbeef-0000-0000-0000-000000000000>) (<" + held.Token + ">)",
			wantErr:  nil,
		},
		{
			name:     "locked matching tagged list",
			key:      "/b/doc.txt",
			ifHeader: "<http://example.com/doc.txt> (<" + held.Token + ">)",
			wantErr:  nil,
		},
		{
			name:     "locked no token submitted",
			key:      "/b/doc.txt",
			ifHeader: "",
			wantErr:  ErrLocked,
		},
		{
			name:     "locked wrong token",
			key:      "/b/doc.txt",
			ifHeader: "(<" + other.Token + ">)",
			wantErr:  ErrLocked,
		},
		{
			name:     "locked malformed header",
			key:      "/b/doc.txt",
			ifHeader: "(<" + held.Token,
			wantErr:  ErrLocked,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := CheckWriteLock(s, tt.key, tt.ifHeader)
			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("CheckWriteLock(%q, %q) = %v, want %v", tt.key, tt.ifHeader, err, tt.wantErr)
			}
		})
	}
}
