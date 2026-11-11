// Package fileshare is the relay-hosted, one-shot upload endpoint behind
// out-of-band `send_file` uploads.
//
// The MCP files extension lets the agent's client upload a large file
// straight to an https target, with no credentials. Zulip's upload API
// needs the bot's credentials, so it cannot be that target. This package
// mints a random, single-use, short-lived URL per file instead. The
// client PUTs the bytes there; the relay forwards them to Zulip with its
// own credentials (Config.Store) and keeps the returned path under the
// token until the tool call collects it with Finish.
//
// It has NO Zulip imports: the platform hop is Config.Store. See
// acp-kit docs/files-oob.md.
package fileshare

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

// DefaultTTL is how long a minted target accepts its upload.
const DefaultTTL = 10 * time.Minute

// pathPrefix is the URL path every upload target lives under.
const pathPrefix = "/up/"

// StoredTTL is how long a received file waits for Finish. It matches
// mcphost.UploadStateTTL, the life of the client's retry.
const StoredTTL = 30 * time.Minute

const (
	// maxPending bounds the tokens held at one time, so a looping agent
	// cannot grow the table without limit.
	maxPending = 256
	// maxPerOwner bounds one conversation's share of maxPending.
	maxPerOwner = 16
	// maxReceiving bounds the bodies held in memory at one time.
	maxReceiving = 2
)

// Store forwards one received file to the platform and returns the
// platform's handle for it (a Zulip `/user_uploads/…` path).
type Store func(ctx context.Context, name, mimeType string, data []byte) (string, error)

// Config configures a Server.
type Config struct {
	// PublicURL is the https origin a reverse proxy serves Handler at.
	PublicURL string
	// TTL is how long a target stays usable. 0 is DefaultTTL.
	TTL time.Duration
	// StoreTimeout bounds one Store call. 0 is one minute.
	StoreTimeout time.Duration
	// Store forwards the bytes to the platform. Required.
	Store Store
	// Now is the clock. nil is time.Now.
	Now func() time.Time
	// Logf logs. nil discards.
	Logf func(string, ...any)
}

// Spec is one file the client announced.
type Spec struct {
	Name     string
	MimeType string
	Size     int64
}

// state is the life of one token.
type state int

const (
	pending   state = iota // minted, nothing received
	receiving              // a PUT is in progress
	stored                 // Store succeeded
)

type entry struct {
	owner string
	spec  Spec
	exp   time.Time
	state state
	ref   string
}

// Server holds the minted tokens and serves the upload endpoint.
type Server struct {
	cfg     Config
	mu      sync.Mutex
	entries map[string]*entry
	busy    chan struct{} // one slot per body being received
}

// New checks cfg and returns a Server.
func New(cfg Config) (*Server, error) {
	if !strings.HasPrefix(cfg.PublicURL, "https://") {
		return nil, fmt.Errorf("fileshare: public URL %q must be https", cfg.PublicURL)
	}
	if cfg.Store == nil {
		return nil, errors.New("fileshare: Store is required")
	}
	if cfg.TTL <= 0 {
		cfg.TTL = DefaultTTL
	}
	if cfg.StoreTimeout <= 0 {
		cfg.StoreTimeout = time.Minute
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.Logf == nil {
		cfg.Logf = func(string, ...any) {}
	}
	cfg.PublicURL = strings.TrimRight(cfg.PublicURL, "/")
	return &Server{cfg: cfg, entries: map[string]*entry{}, busy: make(chan struct{}, maxReceiving)}, nil
}

// Mint makes a single-use target for one file of owner (the
// conversation). It returns the URL, the token (the upload's ref) and
// the expiry.
func (s *Server) Mint(owner string, spec Spec) (url, ref string, exp time.Time, err error) {
	if spec.Size <= 0 {
		return "", "", time.Time{}, errors.New("fileshare: the size must be positive")
	}
	tok := newToken()
	now := s.cfg.Now()
	exp = now.Add(s.cfg.TTL)
	s.mu.Lock()
	defer s.mu.Unlock()
	s.purge(now)
	n := 0
	for _, e := range s.entries {
		if e.owner == owner {
			n++
		}
	}
	if len(s.entries) >= maxPending || n >= maxPerOwner {
		return "", "", time.Time{}, errors.New("fileshare: too many uploads pending; try again later")
	}
	s.entries[tok] = &entry{owner: owner, spec: spec, exp: exp}
	return s.cfg.PublicURL + pathPrefix + tok, tok, exp, nil
}

// purge drops expired tokens. Caller holds mu.
func (s *Server) purge(now time.Time) {
	for k, e := range s.entries {
		if now.After(e.exp) && e.state != receiving {
			delete(s.entries, k)
		}
	}
}

// Finish collects the stored file behind ref and forgets the token. It
// fails when ref belongs to another owner or when nothing was stored.
func (s *Server) Finish(owner, ref string) (string, int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	e, ok := s.entries[ref]
	if !ok || e.owner != owner {
		return "", 0, errors.New("fileshare: no such upload (expired, or the relay restarted); upload again")
	}
	if e.state != stored {
		return "", 0, errors.New("fileshare: the file was not uploaded")
	}
	delete(s.entries, ref)
	return e.ref, e.spec.Size, nil
}

// Handler serves `PUT /up/<token>`.
func (s *Server) Handler() http.Handler { return http.HandlerFunc(s.serve) }

func (s *Server) serve(w http.ResponseWriter, r *http.Request) {
	tok, ok := strings.CutPrefix(r.URL.Path, pathPrefix)
	if !ok || tok == "" || strings.Contains(tok, "/") {
		http.NotFound(w, r)
		return
	}
	if r.Method != http.MethodPut {
		w.Header().Set("Allow", http.MethodPut)
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	select {
	case s.busy <- struct{}{}:
		defer func() { <-s.busy }()
	default:
		w.Header().Set("Retry-After", "5")
		http.Error(w, "too many uploads in progress", http.StatusServiceUnavailable)
		return
	}
	e, code, msg := s.claim(tok)
	if e == nil {
		http.Error(w, msg, code)
		return
	}
	ref, code, msg := s.receive(r, e)
	s.mu.Lock()
	if ref == "" {
		// A failed upload burns the token: single use.
		delete(s.entries, tok)
	} else {
		e.state, e.ref, e.exp = stored, ref, s.cfg.Now().Add(StoredTTL)
	}
	s.mu.Unlock()
	if ref == "" {
		s.cfg.Logf("fileshare: upload %s… refused: %s", tok[:8], msg)
		http.Error(w, msg, code)
		return
	}
	w.WriteHeader(http.StatusCreated)
}

// claim moves a pending, unexpired token to receiving.
func (s *Server) claim(tok string) (*entry, int, string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	e, ok := s.entries[tok]
	if !ok || s.cfg.Now().After(e.exp) {
		return nil, http.StatusNotFound, "unknown or expired upload target"
	}
	if e.state != pending {
		return nil, http.StatusConflict, "this upload target was already used"
	}
	e.state = receiving
	return e, 0, ""
}

// receive reads exactly the announced size and forwards it. ref is ""
// on failure.
func (s *Server) receive(r *http.Request, e *entry) (string, int, string) {
	if cl := r.Header.Get("Content-Length"); cl != "" {
		if n, err := strconv.ParseInt(cl, 10, 64); err != nil || n != e.spec.Size {
			return "", http.StatusRequestEntityTooLarge, "body length differs from the announced size"
		}
	}
	data, err := io.ReadAll(io.LimitReader(r.Body, e.spec.Size+1))
	if err != nil {
		return "", http.StatusBadRequest, "read body: " + err.Error()
	}
	if int64(len(data)) != e.spec.Size {
		return "", http.StatusRequestEntityTooLarge, "body length differs from the announced size"
	}
	ctx, cancel := context.WithTimeout(r.Context(), s.cfg.StoreTimeout)
	defer cancel()
	ref, err := s.cfg.Store(ctx, e.spec.Name, e.spec.MimeType, data)
	if err != nil {
		return "", http.StatusBadGateway, "store: " + err.Error()
	}
	return ref, 0, ""
}

// newToken is 128 random bits, hex encoded.
func newToken() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b) // crypto/rand.Read never fails (Go ≥ 1.24)
	return hex.EncodeToString(b)
}
