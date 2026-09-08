// This file is INBOUND attachment ingestion: when a human attaches a
// photo or a file to a Zulip message, the relay downloads it into the
// conversation's working directory and puts the local path — and,
// where the agent can take one, the image itself — in front of the
// agent.
//
// # Why it exists
//
// Zulip does not deliver a file with a message. It delivers markdown
// containing `![name.jpg](/user_uploads/2/20/HASH/name.jpg)`, and the
// bytes sit behind an authenticated endpoint. Handing that text
// straight to the agent means handing it a path it cannot open: the
// only way through was for the agent to curl the Zulip API with the
// bot's credentials, which is both absurd and a credential it should
// never need. The symmetric outbound half — `outbox/` uploaded at the
// end of a turn — has worked since the beginning; this is its missing
// counterpart.
//
// # Where the files go, and for how long
//
// `<session cwd>/inbox/`, rooted exactly like `outbox/`. They PERSIST:
// the conversation's state directory is the conversation's memory, and
// an agent that answered about a screenshot last Tuesday should still
// be able to look at it. Nothing here ever deletes a file.
//
// `!new` does not clear the directory and must not: it retires the
// conv-id and mints a fresh one (see journal.Retire), so the new
// conversation gets a NEW working directory and therefore an empty
// inbox, while the old files stay on disk under the retired id. That
// is the same rule the rest of the relay follows — ending a
// conversation never destroys its files.
//
// # Bounds, all of them mandatory
//
// Anyone who can post in a served channel can attach a file, so every
// limit here is a limit on what a stranger can make the relay do:
//
//   - maxAttachments per message, so a wall of links cannot cost a
//     hundred authenticated round-trips.
//   - A per-FILE byte cap and a per-MESSAGE total, both configurable.
//     A file over cap is skipped and NAMED in the prompt; it never
//     fails the turn.
//   - inlineImageMax bounds what may be base64'd into the prompt
//     itself. A 20 MB image is a legal attachment and an illegal
//     prompt.
//   - attachTimeout bounds the whole ingestion, so a wedged download
//     cannot eat a turn's budget.
//
// EVERY failure degrades to the plain Zulip link plus a note. A turn
// must never fail because a download did.
package handler

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"mime"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	acp "github.com/coder/acp-go-sdk"

	"github.com/kfet/zulip-acp/internal/zulipproto"
)

// InboxDir is the per-conversation directory inbound attachments are
// downloaded into, relative to the session cwd. The mirror image of
// OutboxDir.
const InboxDir = "inbox"

// DefaultMaxAttachmentBytes caps ONE inbound attachment and
// DefaultMaxAttachmentTotalBytes caps a whole message's worth.
//
// 20 MB per file is comfortably above a phone photo and comfortably
// below anything that belongs in a chat message; the message total is
// three of them, so a burst of photos still arrives while a dump of
// ten large files does not. Both are operator-configurable
// (`max_attachment_bytes`, `max_attachment_total_bytes`).
const (
	DefaultMaxAttachmentBytes      int64 = 20 << 20
	DefaultMaxAttachmentTotalBytes int64 = 60 << 20
)

const (
	// maxAttachments bounds how many files one message can pull in.
	maxAttachments = 10
	// inlineImageMax bounds an image sent as a ContentBlock::Image, in
	// bytes before base64. Anything bigger is still downloaded and
	// still named — the agent reads it from disk instead of carrying
	// ~1.4x its size through the prompt.
	inlineImageMax = 5 << 20
	// inlineTotalMax bounds what ONE message may inline in total. The
	// per-image cap alone is not enough: ten images each just under it
	// would still assemble a prompt an order of magnitude larger than
	// anything an agent can take. Images past the budget are on disk
	// and named, like any other file.
	inlineTotalMax = 10 << 20
	// attachTimeout bounds the downloads one message costs.
	attachTimeout = 60 * time.Second
	// maxCollisionSuffix bounds the search for a free filename. Two
	// different files of the same name in one conversation is normal;
	// a hundred is someone trying something.
	maxCollisionSuffix = 100
)

// uploadRef is one `/user_uploads/…` reference found in a message.
type uploadRef struct {
	// Path is the relay-relative upload path, query and fragment
	// stripped, exactly as it must be handed to DownloadUpload.
	Path string
	// Name is the human filename the path ends in, percent-decoded.
	Name string
}

// urlDelims terminate a URL token in Zulip markdown. Comma, semicolon
// and colon are deliberately NOT here: they are legal in a filename,
// and trailing sentence punctuation is trimmed separately.
const urlDelims = " \t\r\n()[]<>\"'`*|"

// uploadRefs extracts the attachment references in a raw markdown
// body, in order of appearance and deduped.
//
// It handles the three spellings that actually occur:
//
//   - `![alt](/user_uploads/…)`, the image the composer writes;
//   - `[name](/user_uploads/…)`, the file link it writes for anything
//     that is not an image;
//   - `https://<our realm>/user_uploads/…`, what "Copy link" puts on
//     the clipboard, pasted bare or inside a markdown link.
//
// An absolute URL pointing at ANY OTHER host is ignored. hosts are the
// names this realm answers to — Site's host plus any configured
// aliases; when the list is empty only relative paths are accepted,
// which is the safe direction to fail — a relay that does not know its
// own name must not go fetching other people's URLs with its
// credentials.
func uploadRefs(text string, hosts []string) []uploadRef {
	var out []uploadRef
	seen := map[string]bool{}
	for base := 0; ; {
		i := strings.Index(text[base:], zulipproto.UploadPrefix)
		if i < 0 {
			return out
		}
		at := base + i
		// Widen to the whole URL token: left to whatever opened it (a
		// markdown '(', a space, the start of the body), right to
		// whatever closes it.
		s := at
		for s > 0 && !strings.ContainsRune(urlDelims, rune(text[s-1])) {
			s--
		}
		e := at
		for e < len(text) && !strings.ContainsRune(urlDelims, rune(text[e])) {
			e++
		}
		base = e
		tok := strings.TrimRight(text[s:e], ".,;:!?")
		if ref, ok := parseUploadRef(tok, hosts); ok && !seen[ref.Path] {
			seen[ref.Path] = true
			out = append(out, ref)
			if len(out) >= maxAttachments {
				return out
			}
		}
	}
}

// parseUploadRef validates one candidate token and reduces it to the
// path DownloadUpload takes.
func parseUploadRef(tok string, hosts []string) (uploadRef, bool) {
	// Query and fragment are Zulip UI state ("?...", "#narrow/..."),
	// never part of the stored file.
	if i := strings.IndexAny(tok, "?#"); i >= 0 {
		tok = tok[:i]
	}
	if !strings.HasPrefix(tok, zulipproto.UploadPrefix) {
		// The only other accepted spelling is an absolute URL on a
		// name this realm answers to.
		u, err := url.Parse(tok)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") {
			return uploadRef{}, false
		}
		if !hostMatches(u.Host, hosts) {
			return uploadRef{}, false
		}
		tok = u.EscapedPath()
		if !strings.HasPrefix(tok, zulipproto.UploadPrefix) {
			return uploadRef{}, false
		}
	}
	rest, ok := zulipproto.UploadRest(tok)
	if !ok {
		return uploadRef{}, false
	}
	// UploadRest has already established that every segment decodes to
	// a non-empty name, so UploadName cannot come back empty here.
	return uploadRef{Path: zulipproto.UploadPrefix + rest, Name: zulipproto.UploadName(tok)}, true
}

// hostMatches reports whether h is one of the names this realm answers
// to. Case-insensitive, because a host name is. An empty h cannot
// match: siteHosts never yields an empty entry, so a URL without a
// host ("https:///user_uploads/…") falls through as not ours.
func hostMatches(h string, hosts []string) bool {
	for _, want := range hosts {
		if strings.EqualFold(h, want) {
			return true
		}
	}
	return false
}

// siteHosts is every name this realm answers to: Site's host plus the
// operator's configured aliases, empty entries dropped. It is empty
// when none of them is known, and ingestion then accepts relative
// paths only.
//
// Aliases exist because a realm is routinely reachable under more than
// one name — a Tailscale name for the relay and a public vanity domain
// for the humans is the common shape. Site is the name the RELAY uses
// to talk to the server; "Copy link" in someone's browser hands out
// the name THEY used, and without aliases that link is silently not an
// attachment.
func (h *Handler) siteHosts() []string {
	var out []string
	add := func(s string) {
		if s == "" {
			return
		}
		// An alias may be written as a bare host ("zulip.example.com")
		// or as a URL, because both are things an operator will
		// reasonably put in a config file.
		if strings.Contains(s, "//") {
			u, err := url.Parse(s)
			if err != nil || u.Host == "" {
				return
			}
			s = u.Host
		}
		out = append(out, strings.TrimSuffix(s, "/"))
	}
	add(h.cfg.Site)
	for _, a := range h.cfg.SiteAliases {
		add(strings.TrimSpace(a))
	}
	return out
}

// ingested is one attachment's outcome: either a local file or a
// stated reason there is not one.
type ingested struct {
	ref uploadRef
	// name is the SANITISED filename, and the only spelling that ever
	// reaches the agent's prompt or the operator's log. ref.Name is
	// percent-DECODED attacker-controlled text — see safeName.
	name string
	// local is the absolute path the bytes landed at, "" when skipped.
	local string
	mime  string
	size  int
	// skip is the human reason this file is not on disk, "" on
	// success. Exactly one of local and skip is set.
	skip string
	// data is retained only long enough to build an inline image
	// block, and only when one is going to be built.
	data []byte
}

// ingestAttachments downloads every attachment referenced in text into
// <cwd>/inbox and returns the prompt suffix describing them plus any
// extra ACP content blocks to send alongside the text.
//
// It is deliberately total: every error becomes a line in the note.
func (h *Handler) ingestAttachments(ctx context.Context, cwd, text string) (string, []acp.ContentBlock) {
	if !h.cfg.InboundAttachments {
		return "", nil
	}
	refs := uploadRefs(text, h.siteHosts())
	if len(refs) == 0 {
		return "", nil
	}
	// The timeout is layered ON the turn's context rather than
	// detached from it: a superseded turn must stop downloading, and
	// the 60s bound is there so a wedged server cannot spend the
	// turn's whole budget before the agent sees anything.
	ctx, cancel := context.WithTimeout(ctx, attachTimeout)
	defer cancel()

	perFile := h.cfg.MaxAttachmentBytes
	remaining := h.cfg.MaxAttachmentTotalBytes
	dir := filepath.Join(cwd, InboxDir)

	results := make([]ingested, 0, len(refs))
	for _, ref := range refs {
		results = append(results, h.fetchOne(ctx, dir, ref, perFile, &remaining))
	}
	return h.renderAttachments(results)
}

// fetchOne downloads and stores a single attachment, charging its size
// against the message-wide budget.
func (h *Handler) fetchOne(ctx context.Context, dir string, ref uploadRef, perFile int64, remaining *int64) ingested {
	limit := perFile
	if *remaining < limit {
		limit = *remaining
	}
	name := safeName(ref.Name)
	if limit <= 0 {
		return ingested{ref: ref, name: name, skip: fmt.Sprintf("not downloaded: this message's %s attachment budget is used up", humanBytes(h.cfg.MaxAttachmentTotalBytes))}
	}
	data, ctype, err := h.cfg.Client.DownloadUpload(ctx, ref.Path, limit)
	if err != nil {
		h.cfg.Logf("handler: downloading attachment %s: %v", ref.Path, err)
		if errors.Is(err, zulipproto.ErrUploadTooLarge) {
			return ingested{ref: ref, name: name, skip: fmt.Sprintf("not downloaded: larger than the %s still available for this message", humanBytes(limit))}
		}
		return ingested{ref: ref, name: name, skip: fmt.Sprintf("not downloaded: %v", err)}
	}
	*remaining -= int64(len(data))
	path, err := storeAttachment(dir, name, data)
	if err != nil {
		h.cfg.Logf("handler: storing attachment %s: %v", ref.Path, err)
		return ingested{ref: ref, name: name, skip: fmt.Sprintf("not saved: %v", err)}
	}
	return ingested{ref: ref, name: name, local: path, mime: attachMIME(ctype, name, data), size: len(data), data: data}
}

// renderAttachments turns the outcomes into the prompt suffix and the
// content blocks.
func (h *Handler) renderAttachments(results []ingested) (string, []acp.ContentBlock) {
	inline := h.cfg.Agent.Caps().Image
	inlineLeft := inlineTotalMax
	var (
		lines  []string
		blocks []acp.ContentBlock
	)
	for _, r := range results {
		if r.local == "" {
			lines = append(lines, fmt.Sprintf("- %s — %s. Zulip link: %s", r.name, r.skip, r.ref.Path))
			continue
		}
		lines = append(lines, fmt.Sprintf("- %s — local path: %s (%s, %s). Zulip link: %s",
			r.name, r.local, r.mime, humanBytes(int64(r.size)), r.ref.Path))
		if inline && r.size <= inlineLeft && inlineable(r.mime, r.size, r.data) {
			inlineLeft -= r.size
			blocks = append(blocks, acp.ImageBlock(base64.StdEncoding.EncodeToString(r.data), r.mime))
		}
	}
	note := "\n\n[relay] This message has attachments. They have been downloaded into this " +
		"conversation's working directory — read them from the local paths below, do not fetch the Zulip links.\n" +
		strings.Join(lines, "\n")
	return note, blocks
}

// storeAttachment writes data into dir under name, returning the
// absolute path it landed at.
//
// Collisions are the interesting part, and there are two kinds. The
// SAME file sent twice — a screenshot re-pasted in a follow-up — must
// resolve to the same path, or a conversation accumulates
// photo-1.png, photo-2.png … of identical bytes. A DIFFERENT file of
// the same name must never overwrite the first, because the agent may
// already have been told where the first one is.
func storeAttachment(dir, name string, data []byte) (string, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	base := safeName(name)
	ext := filepath.Ext(base)
	stem := strings.TrimSuffix(base, ext)
	for i := 0; i <= maxCollisionSuffix; i++ {
		candidate := base
		if i > 0 {
			candidate = fmt.Sprintf("%s-%d%s", stem, i, ext)
		}
		path := filepath.Join(dir, candidate)
		// Lstat, not Stat, and IsRegular explicitly: a DANGLING
		// SYMLINK reports ErrNotExist to Stat, and os.WriteFile would
		// then follow it and create the target — outside the inbox,
		// wherever it points. Anything that is not a plain file of our
		// own is "occupied", never a free name.
		//
		// Size first, contents only if it matches. The same file
		// re-pasted must resolve to the same path, but establishing
		// that by reading every candidate would let a message full of
		// large attachments cost a hundred full-file reads each.
		info, err := os.Lstat(path)
		switch {
		case err == nil && info.Mode().IsRegular() && info.Size() == int64(len(data)):
			// A read failure here is not an error to report: it only
			// means this candidate cannot be PROVED identical, and the
			// answer to that is the next suffix, never overwriting it.
			if existing, rerr := os.ReadFile(path); rerr == nil && bytes.Equal(existing, data) {
				return path, nil
			}
			continue
		case err == nil:
			continue // occupied by something else: try the next suffix
		case !errors.Is(err, os.ErrNotExist):
			return "", err
		}
		if err := os.WriteFile(path, data, 0o644); err != nil {
			return "", err
		}
		return path, nil
	}
	return "", fmt.Errorf("%d files already named %q in the inbox", maxCollisionSuffix, base)
}

// safeName reduces a filename from a message to something that can
// only ever name a file INSIDE the inbox, and can only ever be one
// line of prose. It is applied ONCE, in fetchOne, and the result is
// the only spelling anything downstream sees.
//
// zulipproto.UploadRest has already refused a path that walks out of
// the upload namespace, so the path half is the second of two
// independent checks rather than the only one — the name is
// attacker-chosen and ends up in a filesystem call, which is exactly
// where belt and braces is cheap.
//
// Control characters are the other half, and they are not paranoia: the
// name is percent-DECODED, so `%0A` in an upload link is a newline in
// the agent's prompt, and anyone in the realm could use it to forge a
// "[relay]" instruction line.
func safeName(name string) string {
	name = strings.Map(func(r rune) rune {
		if r == utf8.RuneError || unicode.IsControl(r) {
			return -1
		}
		return r
	}, name)
	name = strings.TrimSpace(filepath.Base(filepath.FromSlash(name)))
	// A leading dot hides the file from every default listing, which
	// is the last thing an attachment should do, and ".", ".." and a
	// name that was nothing but spaces are not names at all.
	if trimmed := strings.TrimLeft(name, "."); trimmed != name || trimmed == "" {
		return "attachment" + trimmed
	}
	return name
}

// inlineable reports whether an attachment may be base64'd into the
// prompt as a ContentBlock::Image.
//
// The declared type is not enough on its own. Zulip stores and serves
// back the Content-Type declared at UPLOAD, verbatim — see
// zulipproto's package comment — so "image/png" is a claim made by
// whoever posted the file, and anyone in the realm could use it to put
// megabytes of arbitrary bytes into the agent's context window. The
// bytes must AGREE: net/http's sniffer has to call it an image too.
func inlineable(mimeType string, size int, data []byte) bool {
	if !strings.HasPrefix(mimeType, "image/") || size > inlineImageMax {
		return false
	}
	head := data
	if len(head) > 512 {
		head = head[:512]
	}
	return strings.HasPrefix(http.DetectContentType(head), "image/")
}

// attachMIME decides the type reported to the agent — and, for an
// image, the type the content block is tagged with.
//
// The server's Content-Type is preferred because Zulip serves back the
// type declared at upload, which is what the sender's client actually
// knew. It is stripped of parameters (charset), and an absent or
// deliberately vague answer falls through to the same extension and
// sniffing logic the outbound path uses.
func attachMIME(ctype, name string, data []byte) string {
	if mt, _, err := mime.ParseMediaType(ctype); err == nil && mt != "" && mt != zulipproto.DefaultContentType {
		return mt
	}
	head := data
	if len(head) > 512 {
		head = head[:512]
	}
	return zulipproto.ContentType(name, head)
}

// humanBytes renders a byte count the way a human reads one. Used only
// in prose the agent and the operator see.
func humanBytes(n int64) string {
	switch {
	case n >= 1<<20:
		return fmt.Sprintf("%.1f MB", float64(n)/(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%.1f KB", float64(n)/(1<<10))
	default:
		return fmt.Sprintf("%d bytes", n)
	}
}
