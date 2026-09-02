package zulipproto

import (
	"encoding/json"
	"io"
	"mime/multipart"
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
// multipart.Writer only ever fails here by propagating a write error
// from its underlying io.Writer, and the underlying writer is a
// *bytes.Buffer whose Write never returns an error. Unreachable from
// any caller, so it panics rather than leaving an uncoverable branch.
func mustFormFile(mw *multipart.Writer, filename string) io.Writer {
	w, err := mw.CreateFormFile("file", filename)
	if err != nil {
		panic("zulipproto: multipart CreateFormFile on a bytes.Buffer: " + err.Error())
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
