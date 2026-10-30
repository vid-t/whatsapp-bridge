package auth

import (
	"context"
	"errors"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/vid-t/whatsapp-bridge/internal/store"
)

func newStore(t *testing.T, code string) *Store {
	t.Helper()
	db, err := store.Open(filepath.Join(t.TempDir(), "a.db"), "")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	s, err := New(context.Background(), db, Options{AccessCode: code, Name: "Test Team"})
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestSessions(t *testing.T) {
	s := newStore(t, "secret-code")
	ctx := context.Background()
	if _, err := s.Login(ctx, "wrong", "ua", "ip"); !errors.Is(err, ErrBadCode) {
		t.Fatalf("expected ErrBadCode, got %v", err)
	}
	tok, err := s.Login(ctx, " secret-code ", "ua", "ip")
	if err != nil || tok == "" {
		t.Fatal(err)
	}
	if !s.VerifySession(ctx, tok) {
		t.Fatal("fresh session should verify")
	}
	// Idle expiry.
	s.now = func() time.Time { return time.Now().UTC().Add(13 * time.Hour) }
	if s.VerifySession(ctx, tok) {
		t.Fatal("idle session should expire")
	}
	s.now = func() time.Time { return time.Now().UTC() }
	tok2, _ := s.Login(ctx, "secret-code", "ua", "ip")
	s.Logout(ctx, tok2)
	if s.VerifySession(ctx, tok2) {
		t.Fatal("logged-out session should not verify")
	}
}

func TestKeys(t *testing.T) {
	s := newStore(t, "")
	ctx := context.Background()
	info, secret, err := s.CreateKey(ctx, "  Claude laptop  ", KindMCP)
	if err != nil || info.Label != "Claude laptop" || len(secret) < 60 {
		t.Fatalf("create: %+v %q %v", info, secret, err)
	}
	if _, _, err := s.CreateKey(ctx, "x", KeyKind("bogus")); !errors.Is(err, ErrBadKind) {
		t.Fatal("expected ErrBadKind")
	}
	got, err := s.VerifyKey(ctx, "Bearer "+secret)
	if err != nil || got.ID != info.ID || got.Kind != KindMCP {
		t.Fatalf("verify: %+v %v", got, err)
	}
	tampered := secret[:len(secret)-1] + map[bool]string{true: "1", false: "0"}[secret[len(secret)-1] == '0']
	for _, bad := range []string{"", "wb_live_x", tampered, "Bearer nope"} {
		if _, err := s.VerifyKey(ctx, bad); !errors.Is(err, ErrInvalidKey) {
			t.Fatalf("expected ErrInvalidKey for %q, got %v", bad, err)
		}
	}
	list, _ := s.ListKeys(ctx)
	if len(list) != 1 || list[0].LastUsedAt.IsZero() {
		t.Fatalf("list: %+v", list)
	}
	if err := s.RevokeKey(ctx, info.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.VerifyKey(ctx, secret); !errors.Is(err, ErrInvalidKey) {
		t.Fatal("revoked key should fail")
	}
	if err := s.RevokeKey(ctx, info.ID); !errors.Is(err, ErrNoKey) {
		t.Fatal("double revoke should report ErrNoKey")
	}
}

func TestKeyConsentToSend(t *testing.T) {
	s := newStore(t, "")
	ctx := context.Background()
	ro, roSecret, err := s.CreateKey(ctx, "reader", KindMCP)
	if err != nil || ro.CanSend {
		t.Fatalf("a plain key must be read-only: %+v %v", ro, err)
	}
	sk, sendSecret, err := s.CreateKeyWith(ctx, "sender", KindMCP, KeyOptions{CanSend: true})
	if err != nil || !sk.CanSend {
		t.Fatalf("a key created with sending: %+v %v", sk, err)
	}
	if k, err := s.VerifyKey(ctx, roSecret); err != nil || k.CanSend || k.OAuth {
		t.Fatalf("verify read-only key: %+v %v", k, err)
	}
	if k, err := s.VerifyKey(ctx, sendSecret); err != nil || !k.CanSend {
		t.Fatalf("verify send key: %+v %v", k, err)
	}
	list, _ := s.ListKeys(ctx)
	if len(list) != 2 || list[0].CanSend || !list[1].CanSend {
		t.Fatalf("list: %+v", list)
	}
}

func TestOldKeysStayReadOnly(t *testing.T) {
	db, err := store.Open(filepath.Join(t.TempDir(), "old.db"), "")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	ctx := context.Background()
	// A key table from before the consent to send existed.
	if _, err := db.ExecContext(ctx, `CREATE TABLE bridge_api_keys (id TEXT PRIMARY KEY, label TEXT NOT NULL DEFAULT '', kind TEXT NOT NULL,
		hash BLOB NOT NULL, created_at TEXT NOT NULL, last_used_at TEXT, revoked_at TEXT)`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO bridge_api_keys (id, label, kind, hash, created_at) VALUES ('0ld0ld00', 'old', 'mcp', x'00', ?)`,
		time.Now().UTC().Format(time.RFC3339Nano)); err != nil {
		t.Fatal(err)
	}
	s, err := New(ctx, db, Options{})
	if err != nil {
		t.Fatal(err)
	}
	list, err := s.ListKeys(ctx)
	if err != nil || len(list) != 1 || list[0].CanSend {
		t.Fatalf("an old key must not gain sending: %+v %v", list, err)
	}
	if _, err := New(ctx, db, Options{}); err != nil {
		t.Fatalf("migrating twice: %v", err)
	}
}

func TestResolve(t *testing.T) {
	open := newStore(t, "")
	r := httptest.NewRequest("GET", "/api/sessions", nil)
	if p := open.Resolve(r); p.Kind != PrincipalOpen || !p.Authenticated() {
		t.Fatalf("open mode: %+v", p)
	}
	gated := newStore(t, "code")
	if p := gated.Resolve(r); p.Kind != PrincipalAnonymous || p.Authenticated() {
		t.Fatalf("anonymous: %+v", p)
	}
	tok, _ := gated.Login(context.Background(), "code", "", "")
	r.AddCookie(SessionCookie(tok, true))
	if p := gated.Resolve(r); p.Kind != PrincipalSession {
		t.Fatalf("session: %+v", p)
	}
	_, secret, _ := gated.CreateKey(context.Background(), "script", KindMCP)
	r2 := httptest.NewRequest("GET", "/api/search", nil)
	r2.Header.Set("Authorization", "Bearer "+secret)
	if p := gated.Resolve(r2); p.Kind != PrincipalKey || p.Key.Kind != KindMCP {
		t.Fatalf("key: %+v", p)
	}
	r2.Header.Set("Authorization", "Bearer wb_live_bad")
	if p := gated.Resolve(r2); p.Kind != PrincipalAnonymous {
		t.Fatalf("bad key should be anonymous: %+v", p)
	}
}

func TestLimiter(t *testing.T) {
	l := NewLimiter()
	for i := 0; i < 5; i++ {
		if ok, _ := l.Allow("ip", 5, time.Minute); !ok {
			t.Fatalf("attempt %d should pass", i)
		}
	}
	if ok, wait := l.Allow("ip", 5, time.Minute); ok || wait <= 0 {
		t.Fatal("sixth attempt should be denied with a wait")
	}
	if ok, _ := l.Allow("other", 5, time.Minute); !ok {
		t.Fatal("other key should pass")
	}
}

func TestMigratesOldSessionsTable(t *testing.T) {
	db, err := store.Open(filepath.Join(t.TempDir(), "old.db"), "")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	ctx := context.Background()
	// The schema before member sessions existed.
	if _, err := db.ExecContext(ctx, `CREATE TABLE bridge_ui_sessions (hash BLOB PRIMARY KEY, created_at TEXT NOT NULL,
		last_seen_at TEXT NOT NULL, ua TEXT NOT NULL DEFAULT '', ip TEXT NOT NULL DEFAULT '')`); err != nil {
		t.Fatal(err)
	}
	s1, err := New(ctx, db, Options{AccessCode: "code"})
	if err != nil {
		t.Fatal(err)
	}
	tok, err := s1.Login(ctx, "code", "", "")
	if err != nil {
		t.Fatal(err)
	}
	// Opening again is a no-op migration; the old session still works.
	s2, err := New(ctx, db, Options{AccessCode: "code"})
	if err != nil {
		t.Fatal(err)
	}
	if !s2.VerifySession(ctx, tok) {
		t.Fatal("session lost across the migration")
	}
}

func TestMembers(t *testing.T) {
	s := newStore(t, "code")
	ctx := context.Background()
	for in, ok := range map[string]bool{"a@b.ch": true, " A@B.CH ": true, "a@b": false, "Anna <a@b.ch>": false, "": false, "a@@b.ch": false} {
		if _, got := NormalizeEmail(in); got != ok {
			t.Errorf("NormalizeEmail(%q) = %v", in, got)
		}
	}
	m, err := s.InviteMember(ctx, "Anna@B.ch", "", "session")
	if err != nil || m.Email != "anna@b.ch" || m.Role != "member" || m.Status != StatusApproved || m.ApprovedBy != "session" || m.ApprovedAt == nil {
		t.Fatalf("add: %+v %v", m, err)
	}
	if _, err := s.InviteMember(ctx, "anna@b.ch", "", "session"); !errors.Is(err, ErrMemberExists) {
		t.Fatalf("duplicate: %v", err)
	}
	got, err := s.RecordLogin(ctx, "anna@b.ch", "Anna", "google", "sub-1")
	if err != nil || got.ID != m.ID || got.Provider != "google" || got.Subject != "sub-1" || got.Name != "Anna" || got.LastLoginAt == nil {
		t.Fatalf("record login: %+v %v", got, err)
	}
	// The binding is not overwritten by a later login.
	got, _ = s.RecordLogin(ctx, "anna@b.ch", "", "microsoft", "sub-2")
	if got.Provider != "google" || got.Subject != "sub-1" || got.Name != "Anna" {
		t.Fatalf("binding overwritten: %+v", got)
	}
	tok, err := s.LoginMember(ctx, m.ID, "", "")
	if err != nil {
		t.Fatal(err)
	}
	r := httptest.NewRequest("GET", "/api/me", nil)
	r.AddCookie(SessionCookie(tok, true))
	if p := s.Resolve(r); p.User == nil || p.User.Email != "anna@b.ch" || p.Actor() != "anna@b.ch" {
		t.Fatalf("member principal: %+v", p)
	}
	if err := s.RemoveMember(ctx, m.ID); err != nil {
		t.Fatal(err)
	}
	if p := s.Resolve(r); p.Kind != PrincipalAnonymous {
		t.Fatalf("removed member still signed in: %+v", p)
	}
	if err := s.RemoveMember(ctx, m.ID); !errors.Is(err, ErrNoMember) {
		t.Fatalf("second remove: %v", err)
	}
}

func TestRequestsAndApproval(t *testing.T) {
	s := newStore(t, "code")
	ctx := context.Background()
	clock := time.Date(2026, 10, 18, 9, 0, 0, 0, time.UTC)
	s.now = func() time.Time { return clock }

	m, created, err := s.RequestMember(ctx, " Max@example.net ")
	if err != nil || !created || m.Status != StatusPending || m.RequestedAt == nil || m.ApprovedAt != nil || m.Approved() {
		t.Fatalf("request: %+v %v %v", m, created, err)
	}
	again, created, err := s.RequestMember(ctx, "max@example.net")
	if err != nil || created || again.ID != m.ID {
		t.Fatalf("second request must keep the row: %+v %v %v", again, created, err)
	}
	if n, _ := s.CountPending(ctx); n != 1 {
		t.Fatalf("pending count: %d", n)
	}

	// Re-notification: once now, not again within 24 hours, then again.
	if ok, err := s.ClaimNotify(ctx, m.ID, 24*time.Hour); !ok || err != nil {
		t.Fatalf("first notify: %v %v", ok, err)
	}
	if ok, _ := s.ClaimNotify(ctx, m.ID, 24*time.Hour); ok {
		t.Fatal("second notify within the interval")
	}
	clock = clock.Add(25 * time.Hour)
	if ok, _ := s.ClaimNotify(ctx, m.ID, 24*time.Hour); !ok {
		t.Fatal("notify after the interval")
	}

	// A pending member's session is not a signed-in principal.
	tok, _ := s.LoginMemberVia(ctx, m.ID, "email", "", "")
	r := httptest.NewRequest("GET", "/api/me", nil)
	r.AddCookie(SessionCookie(tok, true))
	if p := s.Resolve(r); p.Kind != PrincipalAnonymous {
		t.Fatalf("pending member must not be signed in: %+v", p)
	}

	got, changed, err := s.ApproveMember(ctx, m.ID, "boss@example.net")
	if err != nil || !changed || !got.Approved() || got.ApprovedBy != "boss@example.net" || got.ApprovedAt == nil {
		t.Fatalf("approve: %+v %v %v", got, changed, err)
	}
	if _, changed, _ := s.ApproveMember(ctx, m.ID, "other@example.net"); changed {
		t.Fatal("approving twice must report no change")
	}
	if ok, _ := s.ClaimNotify(ctx, m.ID, time.Nanosecond); ok {
		t.Fatal("an approved member is never notified about")
	}
	tok, _ = s.LoginMemberVia(ctx, m.ID, "email", "", "")
	r = httptest.NewRequest("GET", "/api/me", nil)
	r.AddCookie(SessionCookie(tok, true))
	if p := s.Resolve(r); p.User == nil || p.User.Provider != "email" {
		t.Fatalf("email session principal: %+v", p)
	}

	// Inviting a pending address approves it.
	p2, _, _ := s.RequestMember(ctx, "lea@example.net")
	inv, err := s.InviteMember(ctx, "lea@example.net", "Lea", "anna@b.ch")
	if err != nil || inv.ID != p2.ID || !inv.Approved() || inv.Name != "Lea" || inv.ApprovedBy != "anna@b.ch" {
		t.Fatalf("invite pending: %+v %v", inv, err)
	}

	// EnsureApproved creates, approves or keeps.
	for _, e := range []string{"new@corp.example", "max@example.net"} {
		if m, err := s.EnsureApproved(ctx, e, "domain"); err != nil || !m.Approved() {
			t.Fatalf("ensure %s: %+v %v", e, m, err)
		}
	}
	p3, _, _ := s.RequestMember(ctx, "late@corp.example")
	if m, err := s.EnsureApproved(ctx, "late@corp.example", "domain"); err != nil || m.ID != p3.ID || m.ApprovedBy != "domain" {
		t.Fatalf("ensure pending: %+v %v", m, err)
	}
}

func TestEmailSignInNeverBinds(t *testing.T) {
	s := newStore(t, "code")
	ctx := context.Background()
	if _, err := s.InviteMember(ctx, "anna@b.ch", "", "session"); err != nil {
		t.Fatal(err)
	}
	m, _ := s.RecordLogin(ctx, "anna@b.ch", "", "email", "")
	if m.Provider != "email" || m.Subject != "" {
		t.Fatalf("first email sign-in: %+v", m)
	}
	// A later Google sign-in binds the account; email sign-ins leave it.
	m, _ = s.RecordLogin(ctx, "anna@b.ch", "Anna", "google", "g-1")
	if m.Provider != "google" || m.Subject != "g-1" {
		t.Fatalf("google after email: %+v", m)
	}
	m, _ = s.RecordLogin(ctx, "anna@b.ch", "", "email", "")
	if m.Provider != "google" || m.Subject != "g-1" || m.Name != "Anna" {
		t.Fatalf("email after google changed the binding: %+v", m)
	}
}
