package zulipproto

import (
	"mime"
	"net/http"
	"path/filepath"
	"strings"
)

// DefaultContentType is what an upload gets when nothing better can be
// established. Zulip serves it back as a download.
const DefaultContentType = "application/octet-stream"

// textContentType is the ONLY type Zulip renders inline for text-ish
// files. Declaring text/markdown or text/x-diff is more accurate and
// strictly less useful: Zulip's inline allowlist, not the truth about
// the bytes, decides whether the reader gets a preview or a download.
const textContentType = "text/plain; charset=utf-8"

// textExt pins the extensions an agent actually writes into outbox to
// text/plain. The point is DETERMINISM: without it the answer comes
// from the host's /etc/mime.types, so .json, .sh and .yaml resolve to
// application/* on one distro and not another, and the same file would
// preview on one relay and download on the next.
var textExt = map[string]bool{
	".diff": true, ".go": true, ".json": true, ".jsonl": true,
	".log": true, ".md": true, ".patch": true, ".sh": true,
	".toml": true, ".txt": true, ".yaml": true, ".yml": true,
}

// ContentType decides the MIME type an upload is declared with.
//
// The order is deliberate: the explicit text table first, because it
// overrules correct-but-useless answers from the two detectors; then
// the extension database; then content sniffing over head (the first
// 512 bytes are enough for net/http's sniffer, and it is what covers
// an extensionless file); then octet-stream.
//
// Any text/* result is normalised to textContentType so a sniffed
// "text/plain; charset=utf-8" and a tabled .log agree exactly.
func ContentType(filename string, head []byte) string {
	if textExt[strings.ToLower(filepath.Ext(filename))] {
		return textContentType
	}
	ct := mime.TypeByExtension(strings.ToLower(filepath.Ext(filename)))
	if ct == "" {
		ct = http.DetectContentType(head)
	}
	if strings.HasPrefix(ct, "text/") {
		return textContentType
	}
	return ct
}

// validContentType normalises the type a caller declares, because a
// type reaches Upload from ContentType (which reads the host's
// /etc/mime.types via mime.TypeByExtension) or straight from a caller,
// and neither is ours to trust: a bare CR or LF would let it inject
// headers or close the part boundary early.
//
// Parsing alone is not enough — ParseMediaType("text/plain\r\n")
// succeeds by trimming — so the value written into the header is the
// re-FORMATTED one, never the caller's string. Sanitising once, at the
// only place that writes the header, is why ContentType itself can
// stay a pure lookup.
func validContentType(ct string) string {
	mt, params, err := mime.ParseMediaType(ct)
	if err != nil {
		return DefaultContentType
	}
	return mustFormatMediaType(mt, params)
}
