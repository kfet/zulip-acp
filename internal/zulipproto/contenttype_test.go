package zulipproto

import (
	"strings"
	"testing"
)

// TestContentType pins the resolution order. The .md and .patch cases
// are the regression: mime.TypeByExtension and net/http's sniffer both
// answer them "correctly" (text/markdown, text/plain without a charset)
// and Zulip renders neither the way the table does.
func TestContentType(t *testing.T) {
	const text = "text/plain; charset=utf-8"
	png := []byte("\x89PNG\r\n\x1a\n")
	cases := []struct {
		name string
		head []byte
		want string
	}{
		{"report.log", nil, text},
		{"a.patch", nil, text},
		{"notes.md", nil, text},
		{"NOTES.MD", nil, text},        // extension match is case-insensitive
		{"shot.png", nil, "image/png"}, // known extension, not text
		{"doc.pdf", nil, "application/pdf"},
		{"sniffed.unknownext", []byte("plain words\n"), text},       // no extension entry -> sniff -> text
		{"sniffed.unknownext", png, "image/png"},                    // no extension entry -> sniff -> binary
		{"noext", nil, text},                                        // no extension, nothing to sniff -> text
		{"blob.unknownext", []byte{0x00, 0x01}, DefaultContentType}, // NUL bytes sniff as binary
	}
	for _, c := range cases {
		if got := ContentType(c.name, c.head); got != c.want {
			t.Errorf("ContentType(%q) = %q, want %q", c.name, got, c.want)
		}
	}
}

func TestValidContentType(t *testing.T) {
	// ParseMediaType accepts "text/plain\r\n" by trimming, so the guard
	// is the re-format, not the parse.
	if got := validContentType("text/plain\r\nX-Evil: 1"); got != DefaultContentType {
		t.Errorf("injected header: got %q", got)
	}
	if got := validContentType(""); got != DefaultContentType {
		t.Errorf("empty: got %q", got)
	}
	if got := validContentType("text/plain; charset=utf-8"); got != "text/plain; charset=utf-8" {
		t.Errorf("round trip: got %q", got)
	}
	// RFC2231 is the way a CR/LF gets past ParseMediaType; the re-format
	// is what puts it back in percent-encoded form.
	if got := validContentType(`text/plain; charset*=utf-8''a%0D%0Ab`); strings.ContainsAny(got, "\r\n") {
		t.Errorf("raw CRLF survived: %q", got)
	}
}
