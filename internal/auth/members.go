package auth

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"net/mail"
	"strings"
	"time"
)

// Members are the people who may sign in by email link or with an identity
// provider (Google, Microsoft). The access code stays the bootstrap and
// emergency path; any session can manage members. A member is "approved"
// (may sign in) or "pending" (asked for access by email and waits for an
// approver). See internal/members for email sign-in and the members API, and
// internal/sso for the provider admission rule.

var (
	ErrNoMember     = errors.New("auth: no such member")
	ErrMemberExists = errors.New("auth: member already exists")
	ErrBadEmail     = errors.New("auth: not a valid email address")
)

// Member statuses.
const (
	StatusApproved = "approved"
	StatusPending  = "pending"
)

// Member is one row of bridge_members. Provider is the sign-in method of the
// first sign-in ("email", "google", "microsoft"); Subject is empty until the
// member signs in with Google or Microsoft, after which provider and subject
// bind the member to that account.
type Member struct {
	ID          string     `json:"id"`
	Email       string     `json:"email"`
	Name        string     `json:"name"`
	Provider    string     `json:"provider"`
	Subject     string     `json:"-"`
	Role        string     `json:"role"`
	Status      string     `json:"status"`
	CreatedAt   time.Time  `json:"created_at"`
	LastLoginAt *time.Time `json:"last_login_at"`
	RequestedAt *time.Time `json:"requested_at"`
	ApprovedAt  *time.Time `json:"approved_at"`
	// ApprovedBy names who let the member in: an approver's email, the
	// inviting member, "session" (access code), "admin" (the admin command)
	// or "domain" (BRIDGE_ALLOWED_DOMAINS). Empty for members created before
	// approvals existed.
	ApprovedBy string `json:"approved_by"`
	// NotifiedAt is when approvers were last told about a pending request.
	NotifiedAt *time.Time `json:"-"`
}

// Approved reports whether the member may sign in.
func (m Member) Approved() bool { return m.Status == StatusApproved }

// NormalizeEmail lowercases and validates a bare address (no display name).
func NormalizeEmail(raw string) (string, bool) {
	e := strings.ToLower(strings.TrimSpace(raw))
	if e == "" || len(e) > 254 {
		return "", false
	}
	a, err := mail.ParseAddress(e)
	if err != nil || a.Address != e || a.Name != "" {
		return "", false
	}
	at := strings.LastIndexByte(e, '@')
	if at < 1 || at == len(e)-1 || !strings.Contains(e[at+1:], ".") {
		return "", false
	}
	return e, true
}

// EmailDomain returns the part after the last "@".
func EmailDomain(email string) string {
	if i := strings.LastIndexByte(email, '@'); i >= 0 {
		return email[i+1:]
	}
	return ""
}

const memberCols = `id, email, name, provider, subject, role, status, created_at, last_login_at, requested_at, approved_at, approved_by, notified_at`

func scanMember(sc interface{ Scan(...any) error }) (Member, error) {
	var m Member
	var created string
	var last, requested, approved, notified sql.NullString
	if err := sc.Scan(&m.ID, &m.Email, &m.Name, &m.Provider, &m.Subject, &m.Role, &m.Status, &created, &last,
		&requested, &approved, &m.ApprovedBy, &notified); err != nil {
		return Member{}, err
	}
	m.CreatedAt, _ = time.Parse(time.RFC3339Nano, created)
	m.LastLoginAt = parseOptTime(last)
	m.RequestedAt = parseOptTime(requested)
	m.ApprovedAt = parseOptTime(approved)
	m.NotifiedAt = parseOptTime(notified)
	return m, nil
}

func parseOptTime(v sql.NullString) *time.Time {
	if !v.Valid || v.String == "" {
		return nil
	}
	t, err := time.Parse(time.RFC3339Nano, v.String)
	if err != nil {
		return nil
	}
	return &t
}

// ListMembers returns all members, oldest first.
func (s *Store) ListMembers(ctx context.Context) ([]Member, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+memberCols+` FROM bridge_members ORDER BY created_at, email`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Member{}
	for rows.Next() {
		m, err := scanMember(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// CountPending returns the number of pending requests.
func (s *Store) CountPending(ctx context.Context) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM bridge_members WHERE status = ?`, StatusPending).Scan(&n)
	return n, err
}

// Member returns one member by id.
func (s *Store) Member(ctx context.Context, id string) (Member, error) {
	m, err := scanMember(s.db.QueryRowContext(ctx, `SELECT `+memberCols+` FROM bridge_members WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return Member{}, ErrNoMember
	}
	return m, err
}

// MemberByEmail returns the member with this (normalized) email.
func (s *Store) MemberByEmail(ctx context.Context, email string) (Member, error) {
	e, ok := NormalizeEmail(email)
	if !ok {
		return Member{}, ErrBadEmail
	}
	m, err := scanMember(s.db.QueryRowContext(ctx, `SELECT `+memberCols+` FROM bridge_members WHERE email = ?`, e))
	if errors.Is(err, sql.ErrNoRows) {
		return Member{}, ErrNoMember
	}
	return m, err
}

// InviteMember admits an email address as an approved member; by names who
// invited it. A pending request for the address is approved instead. The
// name is optional; a provider sign-in fills it in later.
func (s *Store) InviteMember(ctx context.Context, email, name, by string) (Member, error) {
	e, ok := NormalizeEmail(email)
	if !ok {
		return Member{}, ErrBadEmail
	}
	m, err := s.MemberByEmail(ctx, e)
	switch {
	case err == nil && m.Approved():
		return Member{}, ErrMemberExists
	case err == nil:
		if n := trunc(strings.TrimSpace(name), 100); n != "" {
			if _, err := s.db.ExecContext(ctx, `UPDATE bridge_members SET name = ? WHERE id = ?`, n, m.ID); err != nil {
				return Member{}, err
			}
		}
		m, _, err = s.ApproveMember(ctx, m.ID, by)
		return m, err
	case !errors.Is(err, ErrNoMember):
		return Member{}, err
	}
	return s.insertApproved(ctx, e, trunc(strings.TrimSpace(name), 100), by)
}

func (s *Store) insertApproved(ctx context.Context, email, name, by string) (Member, error) {
	now := s.now()
	m := Member{ID: "m_" + randomID(), Email: email, Name: name, Role: "member", Status: StatusApproved, CreatedAt: now,
		ApprovedAt: &now, ApprovedBy: trunc(by, 254)}
	if _, err := s.db.ExecContext(ctx, `INSERT INTO bridge_members (id, email, name, role, status, created_at, approved_at, approved_by)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		m.ID, m.Email, m.Name, m.Role, m.Status, now.Format(time.RFC3339Nano), now.Format(time.RFC3339Nano), m.ApprovedBy); err != nil {
		if isUnique(err) {
			return Member{}, ErrMemberExists
		}
		return Member{}, err
	}
	return m, nil
}

// EnsureApproved makes the address an approved member whatever its state:
// it creates the member, approves a pending request, or returns the approved
// member. Callers have decided the address is admitted (an allowed domain, a
// configured approver); by is recorded as approved_by when it changes.
func (s *Store) EnsureApproved(ctx context.Context, email, by string) (Member, error) {
	e, ok := NormalizeEmail(email)
	if !ok {
		return Member{}, ErrBadEmail
	}
	for range 2 { // a concurrent insert of the same address is retried once
		m, err := s.MemberByEmail(ctx, e)
		switch {
		case err == nil && m.Approved():
			return m, nil
		case err == nil:
			m, _, err = s.ApproveMember(ctx, m.ID, by)
			return m, err
		case !errors.Is(err, ErrNoMember):
			return Member{}, err
		}
		m, err = s.insertApproved(ctx, e, "", by)
		if !errors.Is(err, ErrMemberExists) {
			return m, err
		}
	}
	return s.MemberByEmail(ctx, e)
}

// RequestMember records an access request: a pending member, created unless
// the address already has a row. created reports whether the row is new.
func (s *Store) RequestMember(ctx context.Context, email string) (m Member, created bool, err error) {
	e, ok := NormalizeEmail(email)
	if !ok {
		return Member{}, false, ErrBadEmail
	}
	now := s.now()
	res, err := s.db.ExecContext(ctx, `INSERT INTO bridge_members (id, email, role, status, created_at, requested_at)
		VALUES (?, ?, 'member', ?, ?, ?) ON CONFLICT(email) DO NOTHING`,
		"m_"+randomID(), e, StatusPending, now.Format(time.RFC3339Nano), now.Format(time.RFC3339Nano))
	if err != nil {
		return Member{}, false, err
	}
	n, _ := res.RowsAffected()
	m, err = s.MemberByEmail(ctx, e)
	return m, n == 1, err
}

// ApproveMember turns a pending member into an approved one; by names the
// approver. changed is false when the member was already approved.
func (s *Store) ApproveMember(ctx context.Context, id, by string) (m Member, changed bool, err error) {
	now := s.now().Format(time.RFC3339Nano)
	res, err := s.db.ExecContext(ctx, `UPDATE bridge_members SET status = ?, approved_at = ?, approved_by = ? WHERE id = ? AND status = ?`,
		StatusApproved, now, trunc(by, 254), id, StatusPending)
	if err != nil {
		return Member{}, false, err
	}
	n, _ := res.RowsAffected()
	m, err = s.Member(ctx, id)
	return m, n == 1, err
}

// ClaimNotify reports whether approvers should be told (again) about a
// pending request, and if so records that they are being told now. It
// returns true at most once per interval per member, also across processes.
func (s *Store) ClaimNotify(ctx context.Context, id string, interval time.Duration) (bool, error) {
	var prev sql.NullString
	if err := s.db.QueryRowContext(ctx, `SELECT notified_at FROM bridge_members WHERE id = ? AND status = ?`, id, StatusPending).Scan(&prev); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return false, nil
		}
		return false, err
	}
	now := s.now()
	if t := parseOptTime(prev); t != nil && now.Sub(*t) < interval {
		return false, nil
	}
	// Compare-and-set on the value read, so two starts racing notify once.
	res, err := s.db.ExecContext(ctx, `UPDATE bridge_members SET notified_at = ? WHERE id = ? AND notified_at IS ?`,
		now.Format(time.RFC3339Nano), id, prev)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n == 1, nil
}

// RemoveMember deletes a member (or denies a pending request) and signs out
// all their sessions.
func (s *Store) RemoveMember(ctx context.Context, id string) error {
	res, err := s.db.ExecContext(ctx, `DELETE FROM bridge_members WHERE id = ?`, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNoMember
	}
	_, err = s.db.ExecContext(ctx, `DELETE FROM bridge_ui_sessions WHERE member_id = ?`, id)
	return err
}

// RecordLogin updates a member after a successful sign-in (name, last login,
// sign-in method), creating it when absent; callers have applied the
// admission rule. A Google or Microsoft sign-in (non-empty subject) binds an
// unbound member to that account; an email sign-in records "email" as the
// provider of a member that has none and never changes a binding.
func (s *Store) RecordLogin(ctx context.Context, email, name, provider, subject string) (Member, error) {
	e, ok := NormalizeEmail(email)
	if !ok {
		return Member{}, ErrBadEmail
	}
	now := s.now().Format(time.RFC3339Nano)
	name = trunc(strings.TrimSpace(name), 100)
	_, err := s.db.ExecContext(ctx, `INSERT INTO bridge_members (id, email, name, provider, subject, role, created_at, last_login_at)
		VALUES (?, ?, ?, ?, ?, 'member', ?, ?)
		ON CONFLICT(email) DO UPDATE SET
		  name = CASE WHEN excluded.name <> '' THEN excluded.name ELSE bridge_members.name END,
		  provider = CASE
		    WHEN bridge_members.subject <> '' THEN bridge_members.provider
		    WHEN excluded.subject <> '' OR bridge_members.provider = '' THEN excluded.provider
		    ELSE bridge_members.provider END,
		  subject = CASE WHEN bridge_members.subject = '' THEN excluded.subject ELSE bridge_members.subject END,
		  last_login_at = excluded.last_login_at`,
		"m_"+randomID(), e, name, provider, subject, now, now)
	if err != nil {
		return Member{}, err
	}
	return s.MemberByEmail(ctx, e)
}

func isUnique(err error) bool {
	return err != nil && strings.Contains(strings.ToLower(err.Error()), "unique")
}

func randomID() string {
	b := make([]byte, 6)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}
