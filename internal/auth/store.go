package auth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"
)

const (
	keyPrefix     = "wb_live_"
	sessionIdle   = 12 * time.Hour
	sessionMax    = 30 * 24 * time.Hour
	sessionCookie = "__Host-bridge_session"
)

// CookieName is the UI session cookie.
const CookieName = sessionCookie

var (
	ErrBadCode   = errors.New("auth: wrong access code")
	ErrNoSession = errors.New("auth: no such session")
	ErrBadKind   = errors.New("auth: unknown key kind")
	ErrNoKey     = errors.New("auth: no such key")
	ErrOpenMode  = errors.New("auth: server runs in open mode; no access code configured")
	schema       = `
CREATE TABLE IF NOT EXISTS bridge_api_keys (
  id           TEXT PRIMARY KEY,
  label        TEXT NOT NULL DEFAULT '',
  kind         TEXT NOT NULL,
  hash         BLOB NOT NULL,
  created_at   TEXT NOT NULL,
  last_used_at TEXT,
  revoked_at   TEXT
);
CREATE TABLE IF NOT EXISTS bridge_ui_sessions (
  hash         BLOB PRIMARY KEY,
  created_at   TEXT NOT NULL,
  last_seen_at TEXT NOT NULL,
  ua           TEXT NOT NULL DEFAULT '',
  ip           TEXT NOT NULL DEFAULT ''
);
CREATE TABLE IF NOT EXISTS bridge_members (
  id            TEXT PRIMARY KEY,
  email         TEXT NOT NULL UNIQUE,
  name          TEXT NOT NULL DEFAULT '',
  provider      TEXT NOT NULL DEFAULT '',
  subject       TEXT NOT NULL DEFAULT '',
  role          TEXT NOT NULL DEFAULT 'member',
  created_at    TEXT NOT NULL,
  last_login_at TEXT
);
`
)

// Options configures the Store.
type Options struct {
	// AccessCode gates the UI. Empty means open mode (local development only).
	AccessCode string
	// Name is the bridge's display name (BRIDGE_NAME).
	Name string
}

// Store implements access-code sessions and bearer keys on the bridge database.
type Store struct {
	db   *sql.DB
	opts Options
	now  func() time.Time
}

// New prepares the auth tables.
func New(ctx context.Context, db *sql.DB, opts Options) (*Store, error) {
	if _, err := db.ExecContext(ctx, schema); err != nil {
		return nil, fmt.Errorf("auth: schema: %w", err)
	}
	for _, c := range []struct{ table, column, decl string }{
		{"bridge_ui_sessions", "member_id", `TEXT NOT NULL DEFAULT ''`},
		// The sign-in method of a member session ("email", "google", ...).
		{"bridge_ui_sessions", "provider", `TEXT NOT NULL DEFAULT ''`},
		// Rows from before approvals existed are approved members.
		{"bridge_members", "status", `TEXT NOT NULL DEFAULT 'approved'`},
		{"bridge_members", "requested_at", `TEXT`},
		{"bridge_members", "approved_at", `TEXT`},
		{"bridge_members", "approved_by", `TEXT NOT NULL DEFAULT ''`},
		{"bridge_members", "notified_at", `TEXT`},
	} {
		if err := addColumn(ctx, db, c.table, c.column, c.decl); err != nil {
			return nil, fmt.Errorf("auth: migrate %s: %w", c.table, err)
		}
	}
	return &Store{db: db, opts: opts, now: func() time.Time { return time.Now().UTC() }}, nil
}

// OpenMode reports whether no access code is configured.
func (s *Store) OpenMode() bool { return s.opts.AccessCode == "" }

// Name returns the bridge's display name.
func (s *Store) Name() string { return s.opts.Name }

// ---- UI sessions -------------------------------------------------------------

// Login checks the access code and creates a session, returning the cookie value.
func (s *Store) Login(ctx context.Context, code, ua, ip string) (string, error) {
	if s.OpenMode() {
		return "", ErrOpenMode
	}
	if subtle.ConstantTimeCompare([]byte(strings.TrimSpace(code)), []byte(s.opts.AccessCode)) != 1 {
		return "", ErrBadCode
	}
	return s.newSession(ctx, "", "", ua, ip)
}

// LoginMember creates a session for a member who signed in through an
// identity provider (internal/sso), returning the cookie value. The session
// carries the member, so the principal and the audit log name the person.
func (s *Store) LoginMember(ctx context.Context, memberID, ua, ip string) (string, error) {
	return s.LoginMemberVia(ctx, memberID, "", ua, ip)
}

// LoginMemberVia is LoginMember that records the sign-in method on the
// session ("email" for an email link), which /api/me reports as the user's
// provider. Empty means the member's own provider.
func (s *Store) LoginMemberVia(ctx context.Context, memberID, provider, ua, ip string) (string, error) {
	if memberID == "" {
		return "", ErrNoMember
	}
	return s.newSession(ctx, memberID, provider, ua, ip)
}

func (s *Store) newSession(ctx context.Context, memberID, provider, ua, ip string) (string, error) {
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	token := base64.RawURLEncoding.EncodeToString(raw)
	h := sha256.Sum256([]byte(token))
	now := s.now()
	if _, err := s.db.ExecContext(ctx, `INSERT INTO bridge_ui_sessions (hash, created_at, last_seen_at, ua, ip, member_id, provider) VALUES (?, ?, ?, ?, ?, ?, ?)`,
		h[:], now.Format(time.RFC3339Nano), now.Format(time.RFC3339Nano), trunc(ua, 200), trunc(ip, 64), memberID, trunc(provider, 32)); err != nil {
		return "", err
	}
	return token, nil
}

// VerifySession checks a cookie value and slides its idle expiry.
func (s *Store) VerifySession(ctx context.Context, token string) bool {
	_, _, ok := s.verifySession(ctx, token)
	return ok
}

// verifySession is VerifySession that also returns the session's member id
// ("" for an access-code session) and its sign-in method.
func (s *Store) verifySession(ctx context.Context, token string) (member, provider string, ok bool) {
	if token == "" {
		return "", "", false
	}
	h := sha256.Sum256([]byte(token))
	var created, seen string
	err := s.db.QueryRowContext(ctx, `SELECT created_at, last_seen_at, member_id, provider FROM bridge_ui_sessions WHERE hash = ?`, h[:]).Scan(&created, &seen, &member, &provider)
	if err != nil {
		return "", "", false
	}
	now := s.now()
	c, _ := time.Parse(time.RFC3339Nano, created)
	l, _ := time.Parse(time.RFC3339Nano, seen)
	if now.Sub(c) > sessionMax || now.Sub(l) > sessionIdle {
		_, _ = s.db.ExecContext(ctx, `DELETE FROM bridge_ui_sessions WHERE hash = ?`, h[:])
		return "", "", false
	}
	if now.Sub(l) > time.Minute {
		_, _ = s.db.ExecContext(ctx, `UPDATE bridge_ui_sessions SET last_seen_at = ? WHERE hash = ?`, now.Format(time.RFC3339Nano), h[:])
	}
	return member, provider, true
}

// addColumn adds a column unless it exists (SQLite has no ADD COLUMN IF NOT EXISTS).
func addColumn(ctx context.Context, db *sql.DB, table, column, decl string) error {
	rows, err := db.QueryContext(ctx, `SELECT name FROM pragma_table_info(?)`, table)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return err
		}
		if name == column {
			return nil
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}
	_, err = db.ExecContext(ctx, `ALTER TABLE `+table+` ADD COLUMN `+column+` `+decl)
	if err != nil && strings.Contains(strings.ToLower(err.Error()), "duplicate column") {
		return nil // another process (the admin command, the service) migrated first
	}
	return err
}

// Logout deletes a session.
func (s *Store) Logout(ctx context.Context, token string) {
	if token == "" {
		return
	}
	h := sha256.Sum256([]byte(token))
	_, _ = s.db.ExecContext(ctx, `DELETE FROM bridge_ui_sessions WHERE hash = ?`, h[:])
}

// ---- bearer keys -------------------------------------------------------------

// CreateKey mints a key. The returned secret is the only copy.
func (s *Store) CreateKey(ctx context.Context, label string, kind KeyKind) (KeyInfo, string, error) {
	if kind != KindMCP {
		return KeyInfo{}, "", ErrBadKind
	}
	idb := make([]byte, 4)
	sec := make([]byte, 24)
	if _, err := rand.Read(idb); err != nil {
		return KeyInfo{}, "", err
	}
	if _, err := rand.Read(sec); err != nil {
		return KeyInfo{}, "", err
	}
	id := hex.EncodeToString(idb)
	secret := hex.EncodeToString(sec)
	h := sha256.Sum256([]byte(secret))
	now := s.now()
	if _, err := s.db.ExecContext(ctx, `INSERT INTO bridge_api_keys (id, label, kind, hash, created_at) VALUES (?, ?, ?, ?, ?)`,
		id, trunc(strings.TrimSpace(label), 80), string(kind), h[:], now.Format(time.RFC3339Nano)); err != nil {
		return KeyInfo{}, "", err
	}
	return KeyInfo{ID: id, Label: trunc(strings.TrimSpace(label), 80), Kind: kind, CreatedAt: now}, keyPrefix + id + "_" + secret, nil
}

// ListKeys returns active keys without secrets, oldest first.
func (s *Store) ListKeys(ctx context.Context) ([]KeyInfo, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id, label, kind, created_at, last_used_at FROM bridge_api_keys WHERE revoked_at IS NULL ORDER BY created_at`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []KeyInfo
	for rows.Next() {
		var k KeyInfo
		var kind, created string
		var used sql.NullString
		if err := rows.Scan(&k.ID, &k.Label, &kind, &created, &used); err != nil {
			return nil, err
		}
		k.Kind = KeyKind(kind)
		k.CreatedAt, _ = time.Parse(time.RFC3339Nano, created)
		if used.Valid {
			k.LastUsedAt, _ = time.Parse(time.RFC3339Nano, used.String)
		}
		out = append(out, k)
	}
	return out, rows.Err()
}

// RevokeKey marks a key revoked.
func (s *Store) RevokeKey(ctx context.Context, id string) error {
	res, err := s.db.ExecContext(ctx, `UPDATE bridge_api_keys SET revoked_at = ? WHERE id = ? AND revoked_at IS NULL`, s.now().Format(time.RFC3339Nano), id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNoKey
	}
	return nil
}

// VerifyKey implements Verifier. bearer is the full "wb_live_<id>_<secret>"
// value, with or without the "Bearer " prefix.
func (s *Store) VerifyKey(ctx context.Context, bearer string) (KeyInfo, error) {
	bearer = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(bearer), "Bearer "))
	if !strings.HasPrefix(bearer, keyPrefix) {
		return KeyInfo{}, ErrInvalidKey
	}
	rest := strings.TrimPrefix(bearer, keyPrefix)
	id, secret, ok := strings.Cut(rest, "_")
	if !ok || len(id) != 8 || len(secret) != 48 {
		return KeyInfo{}, ErrInvalidKey
	}
	var k KeyInfo
	var kind, created string
	var hash []byte
	var revoked sql.NullString
	err := s.db.QueryRowContext(ctx, `SELECT id, label, kind, hash, created_at, revoked_at FROM bridge_api_keys WHERE id = ?`, id).
		Scan(&k.ID, &k.Label, &kind, &hash, &created, &revoked)
	if err != nil || revoked.Valid {
		return KeyInfo{}, ErrInvalidKey
	}
	h := sha256.Sum256([]byte(secret))
	if subtle.ConstantTimeCompare(h[:], hash) != 1 {
		return KeyInfo{}, ErrInvalidKey
	}
	k.Kind = KeyKind(kind)
	k.CreatedAt, _ = time.Parse(time.RFC3339Nano, created)
	k.LastUsedAt = s.now()
	_, _ = s.db.ExecContext(ctx, `UPDATE bridge_api_keys SET last_used_at = ? WHERE id = ?`, k.LastUsedAt.Format(time.RFC3339Nano), id)
	return k, nil
}

func trunc(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}
