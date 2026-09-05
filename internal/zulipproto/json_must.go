package zulipproto

import (
	"encoding/json"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"net/textproto"
)

// mustJSON marshals v, panicking on failure.
//
// Every call site passes a closed, hand-built shape — []string,
// []int64, [][]string, the zform widget document, or a slice of
// map[string]any holding only strings and int64 — none of which
// encoding/json can fail on. There
// is no input a caller can supply that reaches the error branch, so
// returning the error would leave an `if err != nil` that no test can
// ever cover.
func mustJSON(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		panic("zulipproto: unmarshalable value: " + err.Error())
	}
	return string(b)
}

// mustFormFile creates the single multipart part of an upload.
//
// It sets the part's Content-Type explicitly because multipart's own
// CreateFormFile hardcodes application/octet-stream, and Zulip stores
// the declared type and serves the file back with it — every upload
// was download-only, with no inline preview, because of that default.
//
// multipart.Writer only ever fails here by propagating a write error
// from its underlying io.Writer, and the underlying writer is a
// *bytes.Buffer whose Write never returns an error. Unreachable from
// any caller, so it panics rather than leaving an uncoverable branch.
func mustFormFile(mw *multipart.Writer, filename, contentType string) io.Writer {
	h := make(textproto.MIMEHeader)
	h.Set("Content-Disposition", fmt.Sprintf(`form-data; name="file"; filename=%q`, filename))
	h.Set("Content-Type", contentType)
	w, err := mw.CreatePart(h)
	if err != nil {
		panic("zulipproto: multipart CreatePart on a bytes.Buffer: " + err.Error())
	}
	return w
}

// mustCloseWriter finalises the multipart body. Same reasoning as
// mustFormFile: Close only writes the closing boundary to the
// *bytes.Buffer, which cannot fail.
func mustCloseWriter(mw *multipart.Writer) {
	if err := mw.Close(); err != nil {
		panic("zulipproto: multipart Close on a bytes.Buffer: " + err.Error())
	}
}

// mustFormatMediaType re-formats a media type for the multipart header.
//
// FormatMediaType returns "" only for a type or parameter name it
// cannot represent, and its input here comes straight out of
// ParseMediaType, which has already rejected exactly those. No caller
// can reach the empty return, so it panics rather than leaving the
// caller an uncoverable fallback branch. Round-tripping through both
// is what neutralises a hostile value: a CR/LF smuggled in via RFC2231
// comes back out percent-encoded (measured: charset*=utf-8”a%0D%0Ab).
func mustFormatMediaType(mt string, params map[string]string) string {
	ct := mime.FormatMediaType(mt, params)
	if ct == "" {
		panic("zulipproto: FormatMediaType rejected a ParseMediaType result: " + mt)
	}
	return ct
}
