package members

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"errors"
	"net/http"
	"time"

	"github.com/vid-t/whatsapp-bridge/internal/auth"
)

// Link purposes.
const (
	purposeSignIn  = "signin"
	purposeApprove = "approve"
)

var schema = `
CREATE TABLE IF NOT EXISTS bridge_email_tokens (
  hash       BLOB PRIMARY KEY,
  purpose    TEXT NOT NULL,
  email      TEXT NOT NULL,
  approver   TEXT NOT NULL DEFAULT '',
  next       TEXT NOT NULL DEFAULT '/',
  ip         TEXT NOT NULL DEFAULT '',
  created_at INTEGER NOT NULL,
  expires_at INTEGER NOT NULL,
  used_at    INTEGER
);
CREATE INDEX IF NOT EXISTS bridge_email_tokens_email ON bridge_email_tokens(email);
`

var (
	errLinkInvalid = errors.New("link_invalid")
	errLinkUsed    = errors.New("link_used")
	errLinkExpired = errors.New("link_expired")
)

// link is a stored token, without the token itself.
type link struct {
	Purpose  string
	Email    string // sign-in: who signs in; approve: who asked for access
	Approver string // approve: the approver the link was sent to
	Next     string
}

// issue stores a new single-use token and returns it; only its hash is kept.
func (s *Server) issue(ctx context.Context, l link, ip string, ttl time.Duration) (string, error) {
	s.maybePrune(ctx)
	raw := make([]byte, 32)
	_, _ = rand.Read(raw) // never fails (Go 1.24+)
	tok := base64.RawURLEncoding.EncodeToString(raw)
	now := s.now()
	if l.Next == "" {
		l.Next = "/"
	}
	_, err := s.db.ExecContext(ctx, `INSERT INTO bridge_email_tokens (hash, purpose, email, approver, next, ip, created_at, expires_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)`, hashOf(tok), l.Purpose, l.Email, l.Approver, l.Next, trunc(ip, 64),
		now.UnixMilli(), now.Add(ttl).UnixMilli())
	if err != nil {
		return "", err
	}
	return tok, nil
}

// peek checks a token without using it: unknown or another purpose is
// errLinkInvalid, then errLinkUsed, then errLinkExpired.
func (s *Server) peek(ctx context.Context, tok, purpose string) (link, error) {
	if len(tok) != 43 { // 32 bytes, base64url without padding
		return link{}, errLinkInvalid
	}
	var l link
	var expires int64
	var used sql.NullInt64
	err := s.db.QueryRowContext(ctx, `SELECT purpose, email, approver, next, expires_at, used_at FROM bridge_email_tokens WHERE hash = ?`,
		hashOf(tok)).Scan(&l.Purpose, &l.Email, &l.Approver, &l.Next, &expires, &used)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return link{}, errLinkInvalid
	case err != nil:
		return link{}, err
	case l.Purpose != purpose:
		return link{}, errLinkInvalid
	case used.Valid:
		return l, errLinkUsed
	case s.now().UnixMilli() >= expires:
		return l, errLinkExpired
	}
	return l, nil
}

// consume uses a token exactly once; a concurrent second use gets errLinkUsed.
func (s *Server) consume(ctx context.Context, tok, purpose string) (link, error) {
	l, err := s.peek(ctx, tok, purpose)
	if err != nil {
		return l, err
	}
	now := s.now().UnixMilli()
	res, err := s.db.ExecContext(ctx, `UPDATE bridge_email_tokens SET used_at = ? WHERE hash = ? AND purpose = ? AND used_at IS NULL AND expires_at > ?`,
		now, hashOf(tok), purpose, now)
	if err != nil {
		return link{}, err
	}
	if n, _ := res.RowsAffected(); n != 1 {
		if _, err := s.peek(ctx, tok, purpose); err != nil {
			return l, err
		}
		return l, errLinkUsed
	}
	return l, nil
}

// forget deletes every outstanding link for an address (the member was
// removed or the request denied).
func (s *Server) forget(ctx context.Context, email string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM bridge_email_tokens WHERE email = ?`, email)
	return err
}

func (s *Server) maybePrune(ctx context.Context) {
	now := s.now()
	last := s.lastPrune.Load()
	if last != 0 && now.Sub(time.UnixMilli(last)) < pruneEvery {
		return
	}
	if !s.lastPrune.CompareAndSwap(last, now.UnixMilli()) {
		return
	}
	// Used and expired links are kept a day so a late click can still say
	// "used" or "expired" instead of "invalid".
	if _, err := s.db.ExecContext(ctx, `DELETE FROM bridge_email_tokens WHERE expires_at < ?`, now.Add(-24*time.Hour).UnixMilli()); err != nil {
		s.log.Warn("prune links", "err", err)
	}
}

// ---- the link cookie -------------------------------------------------------------

// The token moves from the emailed URL into this cookie and the browser is
// redirected to the same path without it, so the token leaves the address
// bar and history, and the page never contains it.
func linkCookieName(purpose string, secure bool) string {
	name := "bridge_" + map[string]string{purposeSignIn: "link", purposeApprove: "approve"}[purpose]
	if secure {
		return "__Host-" + name
	}
	return name
}

func setLinkCookie(w http.ResponseWriter, r *http.Request, purpose, tok string, maxAge int) {
	secure := auth.SecureRequest(r)
	http.SetCookie(w, &http.Cookie{Name: linkCookieName(purpose, secure), Value: tok, Path: "/", MaxAge: maxAge,
		HttpOnly: true, Secure: secure, SameSite: http.SameSiteLaxMode})
}

func clearLinkCookie(w http.ResponseWriter, r *http.Request, purpose string) {
	setLinkCookie(w, r, purpose, "", -1)
}

func linkCookie(r *http.Request, purpose string) string {
	for _, secure := range []bool{true, false} {
		if c, err := r.Cookie(linkCookieName(purpose, secure)); err == nil {
			return c.Value
		}
	}
	return ""
}

func hashOf(secret string) []byte {
	h := sha256.Sum256([]byte(secret))
	return h[:]
}

func trunc(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}
