// This file is the Zulip side of the MCP files tools (acp-kit
// relaytool: `send_file`, `list_attachments`, `fetch_attachment`).
//
//   - FilePoster: a small file arrives inline in the tool call. The
//     relay uploads it to /user_uploads with its own credentials and
//     posts the link into the calling conversation.
//   - UploadTargeter (Uploads): a large file goes from the agent's
//     client straight to a one-shot relay endpoint (internal/fileshare),
//     which forwards it to /user_uploads. Only with `files.public_url`.
//   - DownloadLinker: the attachments linked in the conversation's
//     recent messages, handed out as Zulip's temporary signed URLs. The
//     bot's credentials never leave the relay.
//
// Every method takes the broker's conversation token, which relaytool
// resolves from the MCP connection. There is no way to name another
// conversation.
package handler

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"mime"
	"path"
	"strings"
	"time"

	"github.com/kfet/acp-kit/command"
	"github.com/kfet/acp-kit/mcphost"
	"github.com/kfet/zulip-acp/internal/fileshare"
	"github.com/kfet/zulip-acp/internal/journal"
	"github.com/kfet/zulip-acp/internal/zulipproto"
)

const (
	// attachmentScan is how many recent messages AttachmentsIn reads.
	attachmentScan = 100
	// maxListed bounds the attachments AttachmentsIn returns.
	maxListed = 50
	// downloadLinkTTL is how long a temporary URL is promised to work.
	// Zulip signs it for 60 s; a margin covers the HEAD and the hop.
	downloadLinkTTL = 45 * time.Second
)

// PostFileTo satisfies command.FilePoster: it uploads f and posts a link
// to it, with the caption, into the conversation.
func (h *Handler) PostFileTo(token string, f command.OutFile) ([]string, error) {
	if _, err := journal.ParseToken(token); err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), h.cfg.ZulipCallTimeout)
	defer cancel()
	p, err := h.cfg.Client.Upload(ctx, f.Name, zulipproto.ContentType(f.Name, f.Data), bytes.NewReader(f.Data))
	if err != nil {
		return nil, fmt.Errorf("upload %s: %w", f.Name, err)
	}
	return h.postFileLink(token, f.Name, p, f.Caption)
}

// postFileLink posts `[name](path)` and the caption through the
// splitter, as every out-of-band post goes.
func (h *Handler) postFileLink(token, name, uploadPath, caption string) ([]string, error) {
	text := "[" + linkText(name) + "](" + uploadPath + ")"
	if c := strings.TrimSpace(caption); c != "" {
		text = c + "\n\n" + text
	}
	return h.PostToIDs(token, text)
}

// linkText makes a file name safe inside markdown link text.
func linkText(name string) string {
	return strings.NewReplacer("[", "(", "]", ")", "\n", " ").Replace(name)
}

// AttachmentsIn satisfies command.DownloadLinker: the `/user_uploads/…`
// links humans posted in the conversation's recent messages, newest
// first. The id is
// the upload path without its prefix.
func (h *Handler) AttachmentsIn(token string) ([]command.InboundAttachment, error) {
	key, err := journal.ParseToken(token)
	if err != nil {
		return nil, err
	}
	key = h.cfg.Journal.Current(key)
	narrow := zulipproto.TopicNarrow(key.StreamID, key.Topic)
	if key.IsDM() {
		narrow = zulipproto.DMNarrow(key.UserIDs)
	}
	ctx, cancel := context.WithTimeout(context.Background(), h.cfg.ZulipCallTimeout)
	defer cancel()
	msgs, err := h.cfg.Client.Messages(ctx, narrow, attachmentScan, 0)
	if err != nil {
		return nil, fmt.Errorf("read the conversation: %w", err)
	}
	hosts := h.siteHosts()
	seen := map[string]bool{}
	var out []command.InboundAttachment
	for i := len(msgs) - 1; i >= 0 && len(out) < maxListed; i-- {
		// Only what a human attached. The agent can post any upload
		// path through `post`; listing it would let it read any file in
		// the realm with the bot's access.
		if _, bot := h.cfg.BotSenderIDs[msgs[i].SenderID]; bot || msgs[i].SenderID == h.cfg.BotUserID {
			continue
		}
		for _, ref := range uploadRefs(msgs[i].Content, hosts) {
			id := strings.TrimPrefix(ref.Path, zulipproto.UploadPrefix)
			if seen[id] || len(out) >= maxListed {
				continue
			}
			seen[id] = true
			out = append(out, command.InboundAttachment{ID: id, Name: ref.Name, MimeType: typeByName(ref.Name), Size: -1})
		}
	}
	return out, nil
}

// typeByName guesses a media type from a file name.
func typeByName(name string) string {
	if t := mime.TypeByExtension(path.Ext(name)); t != "" {
		return t
	}
	return zulipproto.DefaultContentType
}

// DownloadLinkFor satisfies command.DownloadLinker. The id must be one
// AttachmentsIn lists for THIS conversation, so a tool call cannot read
// an upload from elsewhere in the realm. The link is a fresh temporary
// signed URL, minted now because it expires in about a minute.
func (h *Handler) DownloadLinkFor(token, id string) (mcphost.DownloadTarget, error) {
	as, err := h.AttachmentsIn(token)
	if err != nil {
		return mcphost.DownloadTarget{}, err
	}
	var found *command.InboundAttachment
	for i := range as {
		if as[i].ID == id {
			found = &as[i]
			break
		}
	}
	if found == nil {
		return mcphost.DownloadTarget{}, fmt.Errorf("no attachment %q in this conversation's recent messages", id)
	}
	ctx, cancel := context.WithTimeout(context.Background(), h.cfg.ZulipCallTimeout)
	defer cancel()
	now := time.Now()
	u, size, ct, err := h.cfg.Client.TemporaryUpload(ctx, zulipproto.UploadPrefix+id)
	if err != nil {
		return mcphost.DownloadTarget{}, fmt.Errorf("link %s: %w", found.Name, err)
	}
	if !strings.HasPrefix(u, "https://") {
		return mcphost.DownloadTarget{}, errors.New("the Zulip server is not on https, so it cannot hand out download links")
	}
	if mt, _, perr := mime.ParseMediaType(ct); perr == nil {
		found.MimeType = mt
	}
	return mcphost.DownloadTarget{URL: u, Name: found.Name, MimeType: found.MimeType, Size: size, ExpiresAt: now.Add(downloadLinkTTL)}, nil
}

// Uploads is the command.UploadTargeter for large `send_file` uploads,
// over the one-shot endpoint in internal/fileshare.
type Uploads struct {
	h  func() *Handler
	fs *fileshare.Server
}

// NewUploads returns an UploadTargeter. h is called at use time, so it
// may be wired before the Handler exists.
func NewUploads(h func() *Handler, fs *fileshare.Server) *Uploads {
	return &Uploads{h: h, fs: fs}
}

// UploadTargetFor mints a single-use PUT target for one file.
func (u *Uploads) UploadTargetFor(token string, spec mcphost.UploadSpec) (mcphost.UploadTarget, error) {
	if _, err := journal.ParseToken(token); err != nil {
		return mcphost.UploadTarget{}, err
	}
	url, ref, exp, err := u.fs.Mint(token, fileshare.Spec{
		Name:     command.SafeFileName(spec.Name),
		MimeType: spec.MimeType,
		Size:     spec.Size,
	})
	if err != nil {
		return mcphost.UploadTarget{}, err
	}
	return mcphost.UploadTarget{URL: url, Method: "PUT", ExpiresAt: exp, Ref: ref}, nil
}

// FinishUploadFor collects what the endpoint stored for ref.
func (u *Uploads) FinishUploadFor(token string, spec mcphost.UploadSpec, ref string) (mcphost.UploadedFile, error) {
	p, size, err := u.fs.Finish(token, ref)
	if err != nil {
		return mcphost.UploadedFile{}, err
	}
	return mcphost.UploadedFile{Ref: p, Name: spec.Name, MimeType: spec.MimeType, Size: size}, nil
}

// PostUploadedTo posts the stored file's link into the conversation.
func (u *Uploads) PostUploadedTo(token string, f mcphost.UploadedFile, caption string) ([]string, error) {
	return u.h().postFileLink(token, f.Name, f.Ref, caption)
}

// UploadTargetAPI is the zlite upload-target extension of the Zulip
// client (zulipproto/uploadtargets.go).
type UploadTargetAPI interface {
	CreateUploadTarget(ctx context.Context, name string, size int64, mimeType string, ttl time.Duration) (zulipproto.UploadTarget, error)
	GetUploadTarget(ctx context.Context, token string) (zulipproto.UploadTargetStatus, error)
}

// zliteTargetTTL is how long a zlite target stays usable.
const zliteTargetTTL = 10 * time.Minute

// ZliteUploads is the command.UploadTargeter over zlite's own upload
// targets: the server mints the credential-free PUT URL, so the relay
// needs no endpoint of its own. It is preferred over Uploads.
type ZliteUploads struct {
	h       func() *Handler
	api     UploadTargetAPI
	timeout time.Duration
}

// NewZliteUploads returns an UploadTargeter. h is called at use time.
func NewZliteUploads(h func() *Handler, api UploadTargetAPI, timeout time.Duration) *ZliteUploads {
	return &ZliteUploads{h: h, api: api, timeout: timeout}
}

// UploadTargetFor asks the server for a single-use PUT URL.
func (z *ZliteUploads) UploadTargetFor(token string, spec mcphost.UploadSpec) (mcphost.UploadTarget, error) {
	if _, err := journal.ParseToken(token); err != nil {
		return mcphost.UploadTarget{}, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), z.timeout)
	defer cancel()
	t, err := z.api.CreateUploadTarget(ctx, command.SafeFileName(spec.Name), spec.Size, spec.MimeType, zliteTargetTTL)
	if err != nil {
		return mcphost.UploadTarget{}, fmt.Errorf("upload target: %w", err)
	}
	return mcphost.UploadTarget{URL: t.URL, Method: "PUT", ExpiresAt: t.ExpiresAt, Ref: t.Token}, nil
}

// FinishUploadFor asks the server what it stored for the target. It
// fails unless the server holds a valid upload path.
func (z *ZliteUploads) FinishUploadFor(_ string, spec mcphost.UploadSpec, ref string) (mcphost.UploadedFile, error) {
	ctx, cancel := context.WithTimeout(context.Background(), z.timeout)
	defer cancel()
	st, err := z.api.GetUploadTarget(ctx, ref)
	if err != nil {
		return mcphost.UploadedFile{}, fmt.Errorf("upload status: %w", err)
	}
	if _, ok := zulipproto.UploadRest(st.Path); !ok || !strings.HasPrefix(st.Path, zulipproto.UploadPrefix) {
		return mcphost.UploadedFile{}, fmt.Errorf("the file was not uploaded (status %q)", st.Status)
	}
	return mcphost.UploadedFile{Ref: st.Path, Name: spec.Name, MimeType: spec.MimeType, Size: st.Size}, nil
}

// PostUploadedTo posts the stored file's link into the conversation.
func (z *ZliteUploads) PostUploadedTo(token string, f mcphost.UploadedFile, caption string) ([]string, error) {
	return z.h().postFileLink(token, f.Name, f.Ref, caption)
}
