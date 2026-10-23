package members

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"html"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/vid-t/whatsapp-bridge/internal/auth"
	"github.com/vid-t/whatsapp-bridge/internal/mail"
)

func TestTokenLifecycle(t *testing.T) {
	h := newHarness(t, hopts{})
	ctx := context.Background()
	tok, err := h.srv.issue(ctx, link{Purpose: purposeSignIn, Email: "anna@example.org", Next: "/members"}, "192.0.2.1", signinTTL)
	if err != nil || len(tok) != 43 {
		t.Fatalf("issue: %q %v", tok, err)
	}
	// Only the hash is stored, with purpose, email, next, expiry and IP.
	var stored []byte
	var purpose, email, next, ip string
	var created, expires int64
	if err := h.db.QueryRow(`SELECT hash, purpose, email, next, ip, created_at, expires_at FROM bridge_email_tokens`).
		Scan(&stored, &purpose, &email, &next, &ip, &created, &expires); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256([]byte(tok))
	if string(stored) != string(sum[:]) || purpose != "signin" || email != "anna@example.org" || next != "/members" ||
		ip != "192.0.2.1" || time.Duration(expires-created)*time.Millisecond != 15*time.Minute {
		t.Fatalf("stored row: %x %s %s %s %s %d", stored, purpose, email, next, ip, expires-created)
	}
	var raw int
	_ = h.db.QueryRow(`SELECT COUNT(*) FROM bridge_email_tokens WHERE CAST(hash AS TEXT) = ? OR email = ?`, tok, tok).Scan(&raw)
	if raw != 0 {
		t.Fatal("the token itself is stored")
	}

	if l, err := h.srv.peek(ctx, tok, purposeSignIn); err != nil || l.Email != "anna@example.org" {
		t.Fatalf("peek: %+v %v", l, err)
	}
	if _, err := h.srv.peek(ctx, tok, purposeApprove); !errors.Is(err, errLinkInvalid) {
		t.Fatalf("wrong purpose: %v", err)
	}
	for _, bad := range []string{"", "x", strings.Repeat("A", 43), tok + "A", tok[:42]} {
		if _, err := h.srv.consume(ctx, bad, purposeSignIn); !errors.Is(err, errLinkInvalid) {
			t.Fatalf("consume(%q): %v", bad, err)
		}
	}
	if _, err := h.srv.consume(ctx, tok, purposeSignIn); err != nil {
		t.Fatalf("consume: %v", err)
	}
	if _, err := h.srv.consume(ctx, tok, purposeSignIn); !errors.Is(err, errLinkUsed) {
		t.Fatalf("reuse: %v", err)
	}
	if _, err := h.srv.peek(ctx, tok, purposeSignIn); !errors.Is(err, errLinkUsed) {
		t.Fatalf("peek after use: %v", err)
	}

	old, _ := h.srv.issue(ctx, link{Purpose: purposeSignIn, Email: "anna@example.org"}, "", signinTTL)
	h.advance(signinTTL)
	if _, err := h.srv.consume(ctx, old, purposeSignIn); !errors.Is(err, errLinkExpired) {
		t.Fatalf("expired: %v", err)
	}
}

func TestSignInByEmail(t *testing.T) {
	h := newHarness(t, hopts{})
	if _, err := h.srv.Invite(context.Background(), "Anna@Example.org", "Anna", "session", ""); err != nil {
		t.Fatal(err)
	}
	if resp, body := h.start("anna@example.org", "/members?tab=pending"); resp.StatusCode != http.StatusAccepted || strings.TrimSpace(body) != `{"ok":true}` {
		t.Fatalf("start: %d %s", resp.StatusCode, body)
	}
	msgs := h.mail.take()
	if len(msgs) != 1 {
		t.Fatalf("mails: %+v", msgs)
	}
	m := h.mailTo(msgs, mail.KindSignIn, "anna@example.org")
	if m.Subject != "Sign in to "+bridgeName+"" || !strings.Contains(m.Body, "15 minutes") {
		t.Fatalf("sign-in mail: %+v", m)
	}
	link := h.linkIn(m)
	if !strings.HasPrefix(link, h.url+"/auth/email/callback?token=") {
		t.Fatalf("link: %s", link)
	}

	// Opening the link moves the token into a cookie and drops it from the
	// URL; the page asks for a click and does not contain the token.
	j := jar{}
	resp, _ := h.do("GET", link, "", "", j)
	if resp.StatusCode != http.StatusSeeOther || resp.Header.Get("Location") != "/auth/email/callback" || j["bridge_link"] == nil ||
		!j["bridge_link"].HttpOnly || j["bridge_link"].MaxAge != 600 || j["bridge_session"] != nil {
		t.Fatalf("open link: %d %q %v", resp.StatusCode, resp.Header.Get("Location"), j)
	}
	resp, page := h.do("GET", "/auth/email/callback", "", "", j)
	if resp.StatusCode != http.StatusOK || strings.Contains(page, tokenOf(link)) || !strings.Contains(page, "Continue as anna@example.org") ||
		strings.Contains(page, "style=") || strings.Contains(page, "<script") || j["bridge_session"] != nil {
		t.Fatalf("continue page: %d\n%s", resp.StatusCode, page)
	}
	if csp := resp.Header.Get("Content-Security-Policy"); !strings.Contains(csp, "default-src 'self'") || strings.Contains(csp, "unsafe") {
		t.Fatalf("CSP: %q", csp)
	}
	// A cross-site POST is refused (web layer).
	if resp, _ := h.do("POST", "/auth/email/callback", "application/x-www-form-urlencoded", "", j,
		func(r *http.Request) { r.Header.Set("Sec-Fetch-Site", "cross-site") }); resp.StatusCode != http.StatusForbidden {
		t.Fatalf("cross-site POST: %d", resp.StatusCode)
	}
	resp, _ = h.press(page, j)
	if resp.StatusCode != http.StatusSeeOther || resp.Header.Get("Location") != "/members?tab=pending" || j["bridge_session"] == nil || j["bridge_link"] != nil {
		t.Fatalf("sign in: %d %q %v", resp.StatusCode, resp.Header.Get("Location"), j)
	}
	me := h.me(j)
	user, _ := me["user"].(map[string]any)
	if me["authenticated"] != true || user["email"] != "anna@example.org" || user["provider"] != "email" || user["name"] != "Anna" {
		t.Fatalf("/api/me: %v", me)
	}
	if p, _ := me["providers"].([]any); len(p) != 2 || p[0] != "code" || p[1] != "email" {
		t.Fatalf("providers: %v", me["providers"])
	}
	if got := h.member("anna@example.org"); got.Provider != "email" || got.LastLoginAt == nil {
		t.Fatalf("member after sign-in: %+v", got)
	}
	if !strings.Contains(h.auditLog(), "signin.email email=anna@example.org") ||
		!strings.Contains(h.auditLog(), "mail.sent kind=signin to=anna@example.org") {
		t.Fatalf("audit:\n%s", h.auditLog())
	}

	// The same link again: used. Unknown and tampered: invalid. Expired: expired.
	if resp, _ := h.open(link, jar{}); resp.Header.Get("Location") != "/signin?error=link_used" {
		t.Fatalf("reuse: %d %q", resp.StatusCode, resp.Header.Get("Location"))
	}
	if resp, _ := h.open(h.url+"/auth/email/callback?token="+strings.Repeat("x", 43), jar{}); resp.Header.Get("Location") != "/signin?error=link_invalid" {
		t.Fatalf("unknown: %q", resp.Header.Get("Location"))
	}
	if resp, _ := h.do("POST", "/auth/email/callback", "application/x-www-form-urlencoded", "", jar{}); resp.Header.Get("Location") != "/signin?error=link_invalid" {
		t.Fatalf("POST without cookie: %q", resp.Header.Get("Location"))
	}
	h.start("anna@example.org", "")
	late := h.linkIn(h.mail.take()[0])
	h.advance(16 * time.Minute)
	if resp, _ := h.open(late, jar{}); resp.Header.Get("Location") != "/signin?error=link_expired" {
		t.Fatalf("expired: %q", resp.Header.Get("Location"))
	}
	// Expiry is checked again when the button is pressed.
	h.start("anna@example.org", "")
	slow := h.linkIn(h.mail.take()[0])
	sj := jar{}
	_, page = h.open(slow, sj)
	h.advance(16 * time.Minute)
	if resp, _ := h.press(page, sj); resp.Header.Get("Location") != "/signin?error=link_expired" || sj["bridge_session"] != nil {
		t.Fatalf("expired between open and press: %q", resp.Header.Get("Location"))
	}
}

func TestNextIsSameOrigin(t *testing.T) {
	h := newHarness(t, hopts{})
	i := 0
	for next, want := range map[string]string{
		"//evil.example/x": "/", "https://evil.example/": "/", "/\\evil.example": "/", "javascript:alert(1)": "/",
		"/auth/email/start": "/", "": "/", "/oauth/authorize?client_id=a": "/oauth/authorize?client_id=a",
	} {
		i++ // one address per case: at most 5 starts per address per 15 minutes
		email := "u" + strconv.Itoa(i) + "@example.org"
		_, _ = h.srv.Invite(context.Background(), email, "", "session", "")
		h.start(email, next)
		msgs := h.mail.take()
		if len(msgs) != 1 {
			t.Fatalf("%q: %d mails", next, len(msgs))
		}
		resp, _ := h.signIn(h.linkIn(msgs[0]), jar{})
		if resp.Header.Get("Location") != want {
			t.Errorf("next %q -> %q, want %q", next, resp.Header.Get("Location"), want)
		}
	}
}

// The start answer is the same for members, pending requests, strangers and
// allowed domains; only the mails differ.
func TestStartNeverRevealsMembership(t *testing.T) {
	h := newHarness(t, hopts{domains: []string{"partner.example"}})
	ctx := context.Background()
	_, _ = h.srv.Invite(ctx, "anna@example.org", "", "session", "")
	_, _, _ = h.auth.RequestMember(ctx, "waiting@else.example")
	type answer struct {
		status int
		body   string
		hdr    string
	}
	var first *answer
	for _, email := range []string{"anna@example.org", "waiting@else.example", "stranger@else.example", "new@partner.example"} {
		resp, body := h.start(email, "/")
		a := answer{resp.StatusCode, body, resp.Header.Get("Content-Type") + resp.Header.Get("Cache-Control")}
		if first == nil {
			first = &a
		} else if a != *first {
			t.Fatalf("%s: %+v differs from %+v", email, a, *first)
		}
	}
	if first.status != http.StatusAccepted {
		t.Fatalf("status %d", first.status)
	}
	// The no-JavaScript form gets the same redirect for everyone.
	for _, email := range []string{"anna@example.org", "nobody@else.example"} {
		resp, _ := h.do("POST", "/auth/email/start", "application/x-www-form-urlencoded", url.Values{"email": {email}, "next": {"/"}}.Encode(), nil)
		if resp.StatusCode != http.StatusSeeOther || resp.Header.Get("Location") != "/signin?sent=1" {
			t.Fatalf("form %s: %d %q", email, resp.StatusCode, resp.Header.Get("Location"))
		}
	}
	for _, bad := range []string{`{"email":"nope"}`, `{"email":"Anna <a@b.ch>"}`, `{"email":""}`, `{}`, `{"email":"a@b.ch"} {}`} {
		if resp, _ := h.json("POST", "/auth/email/start", bad, nil); resp.StatusCode != http.StatusBadRequest {
			t.Fatalf("bad %s: %d", bad, resp.StatusCode)
		}
	}
	if resp, _ := h.do("POST", "/auth/email/start", "text/plain", "a@b.ch", nil); resp.StatusCode != http.StatusUnsupportedMediaType {
		t.Fatalf("text/plain: %d", resp.StatusCode)
	}
	resp, page := h.do("POST", "/auth/email/start", "application/x-www-form-urlencoded", "email=nope", nil)
	if resp.StatusCode != http.StatusBadRequest || !strings.Contains(page, "does not look like an email address") {
		t.Fatalf("bad form: %d\n%s", resp.StatusCode, page)
	}
}

func TestAdmission(t *testing.T) {
	h := newHarness(t, hopts{domains: []string{"partner.example"}})
	ctx := context.Background()

	// A stranger becomes a pending request; requester and both approvers hear of it.
	h.start("Max@example.net", "/")
	msgs := h.mail.take()
	if len(msgs) != 3 {
		t.Fatalf("mails for a new request: %+v", msgs)
	}
	req := h.mailTo(msgs, mail.KindRequest, "max@example.net")
	if req.Subject != "Your request for access to "+bridgeName || linkRe.MatchString(req.Body) || !strings.Contains(req.Body, "sent to the people who approve access") {
		t.Fatalf("request mail: %+v", req)
	}
	for _, a := range []string{approverA, approverB} {
		m := h.mailTo(msgs, mail.KindApprove, a)
		if m.Subject != "max@example.net asks for access to "+bridgeName || !strings.Contains(m.Body, "7 days") ||
			!strings.HasPrefix(h.linkIn(m), h.url+"/auth/email/approve?token=") {
			t.Fatalf("approve mail to %s: %+v", a, m)
		}
	}
	m := h.member("max@example.net")
	if m.Status != auth.StatusPending || m.RequestedAt == nil || m.ApprovedAt != nil {
		t.Fatalf("pending member: %+v", m)
	}
	if !strings.Contains(h.auditLog(), "member.requested id="+m.ID+" email=max@example.net ip=127.0.0.1") {
		t.Fatalf("audit:\n%s", h.auditLog())
	}

	// Asking again within 24 hours notifies nobody; after 24 hours it does.
	h.start("max@example.net", "/")
	if msgs := h.mail.take(); len(msgs) != 0 {
		t.Fatalf("re-notified within 24h: %+v", msgs)
	}
	if _, err := h.db.Exec(`UPDATE bridge_members SET notified_at = ? WHERE id = ?`,
		time.Now().UTC().Add(-25*time.Hour).Format(time.RFC3339Nano), m.ID); err != nil {
		t.Fatal(err)
	}
	h.start("max@example.net", "/")
	if msgs := h.mail.take(); len(msgs) != 3 {
		t.Fatalf("re-notify after 24h: %d mails", len(msgs))
	}

	// A pending request never gets a sign-in link, and cannot sign in with Google either (see internal/sso).
	tok, _ := h.srv.issue(ctx, link{Purpose: purposeSignIn, Email: "max@example.net"}, "", signinTTL)
	j := jar{}
	if resp, _ := h.signIn(h.url+"/auth/email/callback?token="+tok, j); resp.Header.Get("Location") != "/signin?status=pending" || j["bridge_session"] != nil {
		t.Fatalf("pending sign-in: %q %v", resp.Header.Get("Location"), j)
	}

	// An allowed domain is admitted on first sign-in, without approval.
	h.start("new.hire@partner.example", "/")
	msgs = h.mail.take()
	if len(msgs) != 1 || msgs[0].Kind != mail.KindSignIn {
		t.Fatalf("allowed domain mails: %+v", msgs)
	}
	if _, err := h.auth.MemberByEmail(ctx, "new.hire@partner.example"); !errors.Is(err, auth.ErrNoMember) {
		t.Fatalf("no row before the sign-in: %v", err)
	}
	dj := jar{}
	if resp, _ := h.signIn(h.linkIn(msgs[0]), dj); resp.StatusCode != http.StatusSeeOther || dj["bridge_session"] == nil {
		t.Fatalf("allowed domain sign-in: %d", resp.StatusCode)
	}
	if got := h.member("new.hire@partner.example"); !got.Approved() || got.ApprovedBy != "domain" || got.Provider != "email" {
		t.Fatalf("domain member: %+v", got)
	}
	// Lookalike domains are strangers.
	h.start("x@partner.example.evil.com", "/")
	if msgs := h.mail.take(); len(msgs) != 3 {
		t.Fatalf("lookalike domain: %+v", msgs)
	}

	// A configured approver signs in without approving themselves.
	h.start(approverA, "/")
	msgs = h.mail.take()
	if len(msgs) != 1 || msgs[0].Kind != mail.KindSignIn || msgs[0].To != approverA {
		t.Fatalf("approver mails: %+v", msgs)
	}
	if resp, _ := h.signIn(h.linkIn(msgs[0]), jar{}); resp.StatusCode != http.StatusSeeOther || resp.Header.Get("Location") != "/" {
		t.Fatalf("approver sign-in: %d %q", resp.StatusCode, resp.Header.Get("Location"))
	}
	if got := h.member(approverA); got.ApprovedBy != "approvers" {
		t.Fatalf("approver member: %+v", got)
	}
}

func TestApproveFlow(t *testing.T) {
	h := newHarness(t, hopts{})
	h.start("max@example.net", "/")
	msgs := h.mail.take()
	linkA := h.linkIn(h.mailTo(msgs, mail.KindApprove, approverA))
	linkB := h.linkIn(h.mailTo(msgs, mail.KindApprove, approverB))

	// Opening the link (a mail scanner does this) approves nothing.
	j := jar{}
	resp, page := h.open(linkA, j)
	if resp.StatusCode != http.StatusOK || !strings.Contains(page, "Approve max@example.net?") || strings.Contains(page, tokenOf(linkA)) ||
		!strings.Contains(page, `action="/auth/email/approve"`) || strings.Contains(page, "style=") {
		t.Fatalf("approve page: %d\n%s", resp.StatusCode, page)
	}
	if h.member("max@example.net").Approved() {
		t.Fatal("GET approved the request")
	}
	if resp, _ := h.do("GET", linkA, "", "", jar{}); resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("a second open is still allowed: %d", resp.StatusCode)
	}

	resp, page = h.press(page, j)
	if resp.StatusCode != http.StatusOK || !strings.Contains(page, "Approved") || !strings.Contains(page, "max@example.net can now sign in.") {
		t.Fatalf("approve: %d\n%s", resp.StatusCode, page)
	}
	m := h.member("max@example.net")
	if !m.Approved() || m.ApprovedBy != approverA || m.ApprovedAt == nil {
		t.Fatalf("approved member: %+v", m)
	}
	if !strings.Contains(h.auditLog(), "member.approved id="+m.ID+" email=max@example.net by="+approverA) {
		t.Fatalf("audit:\n%s", h.auditLog())
	}
	h.settle()
	msgs = h.mail.take()
	if len(msgs) != 1 {
		t.Fatalf("after approval: %+v", msgs)
	}
	got := h.mailTo(msgs, mail.KindApproved, "max@example.net")
	if got.Subject != "You now have access to "+bridgeName || !strings.Contains(got.Body, "1 hour") {
		t.Fatalf("approved mail: %+v", got)
	}
	signin := h.linkIn(got)

	// The used link says so; the other approver's link says it is done already.
	if resp, page := h.open(linkA, jar{}); resp.StatusCode != http.StatusGone || !strings.Contains(page, "already used") || !strings.Contains(page, h.url) {
		t.Fatalf("reused approve link: %d\n%s", resp.StatusCode, page)
	}
	bj := jar{}
	resp, page = h.open(linkB, bj)
	if resp.StatusCode != http.StatusOK || !strings.Contains(page, "Already approved") || !strings.Contains(page, "approved by "+approverA) {
		t.Fatalf("second approver: %d\n%s", resp.StatusCode, page)
	}

	// The requester signs in with the link from the approval, valid one hour.
	h.advance(59 * time.Minute)
	sj := jar{}
	if resp, _ := h.signIn(signin, sj); resp.StatusCode != http.StatusSeeOther || resp.Header.Get("Location") != "/" || sj["bridge_session"] == nil {
		t.Fatalf("sign in after approval: %d %q", resp.StatusCode, resp.Header.Get("Location"))
	}
	if u, _ := h.me(sj)["user"].(map[string]any); u["email"] != "max@example.net" {
		t.Fatalf("me: %v", u)
	}

	// Approve links expire after 7 days.
	h.start("late@example.net", "/")
	expired := h.linkIn(h.mailTo(h.mail.take(), mail.KindApprove, approverA))
	h.advance(7*24*time.Hour + time.Second)
	if resp, page := h.open(expired, jar{}); resp.StatusCode != http.StatusGone || !strings.Contains(page, "expired") {
		t.Fatalf("expired approve link: %d\n%s", resp.StatusCode, page)
	}
	// A sign-in token is not an approve token, and the reverse.
	if resp, _ := h.open(strings.Replace(signin, "/callback?", "/approve?", 1), jar{}); resp.StatusCode != http.StatusNotFound {
		t.Fatalf("sign-in token on the approve route: %d", resp.StatusCode)
	}
	if resp, _ := h.open(strings.Replace(linkB, "/approve?", "/callback?", 1), jar{}); resp.Header.Get("Location") != "/signin?error=link_invalid" {
		t.Fatalf("approve token on the callback route: %q", resp.Header.Get("Location"))
	}
}

func TestMembersAPI(t *testing.T) {
	h := newHarness(t, hopts{})
	j := h.codeSession()
	resp, body := h.json("POST", "/api/members", `{"email":" Lea@example.net ","name":"Lea"}`, j)
	var m map[string]any
	_ = json.Unmarshal([]byte(body), &m)
	if resp.StatusCode != http.StatusCreated || m["email"] != "lea@example.net" || m["status"] != "approved" || m["approved_by"] != "session" ||
		m["approved_at"] == nil || m["requested_at"] != nil || m["last_login_at"] != nil || m["provider"] != "" || m["role"] != "member" || m["subject"] != nil {
		t.Fatalf("invite: %d %s", resp.StatusCode, body)
	}
	h.settle()
	inv := h.mailTo(h.mail.take(), mail.KindApproved, "lea@example.net")
	if !strings.HasPrefix(h.linkIn(inv), h.url+"/auth/email/callback?token=") {
		t.Fatalf("invitation: %+v", inv)
	}
	if !strings.Contains(h.auditLog(), "member.invited id="+m["id"].(string)+" email=lea@example.net by=session") {
		t.Fatalf("audit:\n%s", h.auditLog())
	}
	if resp, _ := h.json("POST", "/api/members", `{"email":"lea@example.net"}`, j); resp.StatusCode != http.StatusConflict {
		t.Fatalf("duplicate: %d", resp.StatusCode)
	}
	for _, bad := range []string{`{"email":"nope"}`, `{"email":""}`, `{}`, `[`} {
		if resp, _ := h.json("POST", "/api/members", bad, j); resp.StatusCode != http.StatusBadRequest {
			t.Fatalf("bad %s: %d", bad, resp.StatusCode)
		}
	}

	// A pending request, approved from the console.
	h.start("max@example.net", "/")
	approveLink := h.linkIn(h.mailTo(h.mail.take(), mail.KindApprove, approverA))
	resp, body = h.json("GET", "/api/members", "", j)
	var list []map[string]any
	_ = json.Unmarshal([]byte(body), &list)
	if resp.StatusCode != http.StatusOK || len(list) != 2 || list[1]["status"] != "pending" || list[1]["requested_at"] == nil || list[1]["approved_by"] != "" {
		t.Fatalf("list: %d %s", resp.StatusCode, body)
	}
	for _, k := range []string{"id", "email", "name", "provider", "role", "status", "created_at", "last_login_at", "requested_at", "approved_at", "approved_by"} {
		if _, ok := list[1][k]; !ok {
			t.Fatalf("list item lacks %q: %v", k, list[1])
		}
	}
	id := list[1]["id"].(string)
	resp, body = h.json("POST", "/api/members/"+id+"/approve", "", j)
	if resp.StatusCode != http.StatusOK || !strings.Contains(body, `"status":"approved"`) || !strings.Contains(body, `"approved_by":"session"`) {
		t.Fatalf("approve: %d %s", resp.StatusCode, body)
	}
	h.settle()
	if msgs := h.mail.take(); len(msgs) != 1 || msgs[0].Kind != mail.KindApproved || msgs[0].To != "max@example.net" {
		t.Fatalf("approval mail: %+v", msgs)
	}
	if resp, _ := h.json("POST", "/api/members/"+id+"/approve", "", j); resp.StatusCode != http.StatusOK {
		t.Fatalf("approve again: %d", resp.StatusCode)
	}
	h.settle()
	if msgs := h.mail.take(); len(msgs) != 0 {
		t.Fatalf("approving again must not mail: %+v", msgs)
	}
	if resp, _ := h.json("POST", "/api/members/m_nope/approve", "", j); resp.StatusCode != http.StatusNotFound {
		t.Fatalf("approve unknown: %d", resp.StatusCode)
	}
	// The approver's link now finds the request handled.
	if resp, page := h.open(approveLink, jar{}); resp.StatusCode != http.StatusOK || !strings.Contains(page, "Already approved") {
		t.Fatalf("approver after console approval: %d\n%s", resp.StatusCode, page)
	}

	// Denying a request: its links stop working.
	h.start("deny@example.net", "/")
	denyLink := h.linkIn(h.mailTo(h.mail.take(), mail.KindApprove, approverA))
	deny := h.member("deny@example.net")
	if resp, _ := h.json("DELETE", "/api/members/"+deny.ID, "", j); resp.StatusCode != http.StatusNoContent {
		t.Fatalf("deny: %d", resp.StatusCode)
	}
	if resp, page := h.open(denyLink, jar{}); resp.StatusCode != http.StatusNotFound || !strings.Contains(page, "not valid") {
		t.Fatalf("link of a denied request: %d\n%s", resp.StatusCode, page)
	}
	if !strings.Contains(h.auditLog(), "member.removed id="+deny.ID+" email=deny@example.net status=pending by=session") {
		t.Fatalf("audit:\n%s", h.auditLog())
	}

	// A member signs in, then is removed: the session ends with the row.
	h.start("max@example.net", "/")
	mj := jar{}
	if resp, _ := h.signIn(h.linkIn(h.mail.take()[0]), mj); resp.StatusCode != http.StatusSeeOther || mj["bridge_session"] == nil {
		t.Fatalf("member sign-in: %d", resp.StatusCode)
	}
	if resp, _ := h.json("DELETE", "/api/members/"+id, "", j); resp.StatusCode != http.StatusNoContent {
		t.Fatalf("remove: %d", resp.StatusCode)
	}
	if resp, _ := h.json("GET", "/api/sessions", "", mj); resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("removed member's session: %d", resp.StatusCode)
	}
	if resp, _ := h.json("DELETE", "/api/members/"+id, "", j); resp.StatusCode != http.StatusNotFound {
		t.Fatalf("second delete: %d", resp.StatusCode)
	}

	// Policy: anonymous 401, bearer keys 403 (even GET), cross-site writes 403.
	if resp, _ := h.json("GET", "/api/members", "", nil); resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("anonymous: %d", resp.StatusCode)
	}
	_, key, _ := h.auth.CreateKey(context.Background(), "k", auth.KindMCP)
	bearer := func(r *http.Request) { r.Header.Set("Authorization", "Bearer "+key) }
	for _, c := range [][2]string{{"GET", "/api/members"}, {"POST", "/api/members"}, {"POST", "/api/members/" + id + "/approve"}, {"DELETE", "/api/members/" + id}} {
		if resp, _ := h.json(c[0], c[1], "", nil, bearer); resp.StatusCode != http.StatusForbidden {
			t.Fatalf("key %s %s: %d", c[0], c[1], resp.StatusCode)
		}
	}
	if resp, _ := h.json("POST", "/api/members", `{"email":"x@y.example"}`, j, func(r *http.Request) { r.Header.Set("Sec-Fetch-Site", "cross-site") }); resp.StatusCode != http.StatusForbidden {
		t.Fatalf("cross-site invite: %d", resp.StatusCode)
	}
}

func TestRateLimits(t *testing.T) {
	h := newHarness(t, hopts{})
	var last *http.Response
	for range 6 {
		last, _ = h.start("same@else.example", "/")
	}
	if last.StatusCode != http.StatusTooManyRequests || last.Header.Get("Retry-After") == "" {
		t.Fatalf("sixth start for one address: %d", last.StatusCode)
	}
	if secs, _ := strconv.Atoi(last.Header.Get("Retry-After")); secs < 1 || secs > 15*60+1 {
		t.Fatalf("Retry-After %q", last.Header.Get("Retry-After"))
	}
	// 20 per IP per hour, across addresses (6 used above).
	for i := range 14 {
		if resp, _ := h.start("p"+strconv.Itoa(i)+"@else.example", "/"); resp.StatusCode != http.StatusAccepted {
			t.Fatalf("start %d: %d", i, resp.StatusCode)
		}
	}
	if resp, _ := h.start("other@else.example", "/"); resp.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("21st start from one IP: %d", resp.StatusCode)
	}
	resp, page := h.do("POST", "/auth/email/start", "application/x-www-form-urlencoded", "email=a%40b.ch", nil)
	if resp.StatusCode != http.StatusTooManyRequests || resp.Header.Get("Retry-After") == "" || !strings.Contains(page, "Too many") {
		t.Fatalf("limited form: %d", resp.StatusCode)
	}
}

func TestWithoutMail(t *testing.T) {
	h := newHarness(t, hopts{noMail: true})
	if h.srv.EmailEnabled() {
		t.Fatal("email sign-in offered without mail")
	}
	if p, _ := h.me(nil)["providers"].([]any); len(p) != 1 || p[0] != "code" {
		t.Fatalf("providers without mail: %v", p)
	}
	// Requests still wait for the Members page; nothing is mailed or issued.
	if resp, _ := h.start("max@example.net", "/"); resp.StatusCode != http.StatusAccepted {
		t.Fatalf("start: %d", resp.StatusCode)
	}
	if m := h.member("max@example.net"); m.Status != auth.StatusPending {
		t.Fatalf("request without mail: %+v", m)
	}
	var n int
	_ = h.db.QueryRow(`SELECT COUNT(*) FROM bridge_email_tokens`).Scan(&n)
	if n != 0 {
		t.Fatalf("%d links issued without mail", n)
	}
	j := h.codeSession()
	if resp, _ := h.json("POST", "/api/members", `{"email":"lea@example.net"}`, j); resp.StatusCode != http.StatusCreated {
		t.Fatalf("invite without mail: %d", resp.StatusCode)
	}
}

var hiddenRe = regexp.MustCompile(`<input type="hidden" name="([a-z_]+)" value="([^"]*)">`)

// The MCP sign-in step offers an email link that returns to the pending
// authorization request.
func TestOAuthEmailSignIn(t *testing.T) {
	h := newHarness(t, hopts{})
	_, _ = h.srv.Invite(context.Background(), "anna@example.org", "", "session", "")
	_, body := h.json("POST", "/oauth/register", `{"client_name":"Claude","redirect_uris":["https://claude.ai/api/mcp/auth_callback"]}`, nil)
	var reg map[string]any
	_ = json.Unmarshal([]byte(body), &reg)
	sum := sha256.Sum256([]byte(strings.Repeat("v", 50)))
	q := url.Values{"response_type": {"code"}, "client_id": {reg["client_id"].(string)}, "redirect_uri": {"https://claude.ai/api/mcp/auth_callback"},
		"state": {"st"}, "code_challenge": {base64.RawURLEncoding.EncodeToString(sum[:])}, "code_challenge_method": {"S256"}}
	resp, page := h.do("GET", "/oauth/authorize?"+q.Encode(), "", "", nil)
	if resp.StatusCode != http.StatusOK || !strings.Contains(page, `action="/auth/email/start"`) || !strings.Contains(page, "Email me a sign-in link") ||
		!strings.Contains(page, `name="access_code"`) || strings.Contains(page, "style=") || strings.Contains(page, "<script") {
		t.Fatalf("sign-in step: %d\n%s", resp.StatusCode, page)
	}
	var next string
	for _, m := range hiddenRe.FindAllStringSubmatch(page, -1) {
		if m[1] == "next" {
			next = html.UnescapeString(m[2])
		}
	}
	if !strings.HasPrefix(next, "/oauth/authorize?") {
		t.Fatalf("next: %q", next)
	}
	resp, _ = h.do("POST", "/auth/email/start", "application/x-www-form-urlencoded",
		url.Values{"email": {"anna@example.org"}, "next": {next}}.Encode(), nil, func(r *http.Request) { r.Header.Set("Origin", h.url) })
	if resp.StatusCode != http.StatusSeeOther || resp.Header.Get("Location") != "/signin?sent=1" {
		t.Fatalf("form start: %d %q", resp.StatusCode, resp.Header.Get("Location"))
	}
	h.settle()
	j := jar{}
	resp, _ = h.signIn(h.linkIn(h.mail.take()[0]), j)
	back := resp.Header.Get("Location")
	if resp.StatusCode != http.StatusSeeOther || back != next {
		t.Fatalf("callback should return to authorize: %d %q", resp.StatusCode, back)
	}
	resp, page = h.do("GET", back, "", "", j)
	if resp.StatusCode != http.StatusOK || !strings.Contains(page, `value="allow"`) {
		t.Fatalf("consent after email sign-in: %d\n%s", resp.StatusCode, page)
	}
}

// No token or link ever reaches an audit line or a log line, at any level.
func TestNoTokensInAuditOrLogs(t *testing.T) {
	h := newHarness(t, hopts{})
	_, _ = h.srv.Invite(context.Background(), "anna@example.org", "", "session", "")
	h.start("anna@example.org", "/")
	h.start("max@example.net", "/")
	var tokens []string
	for _, m := range h.mail.take() {
		if m.Kind != mail.KindRequest {
			tokens = append(tokens, tokenOf(h.linkIn(m)))
		}
	}
	if len(tokens) != 3 {
		t.Fatalf("tokens: %d", len(tokens))
	}
	_, _ = h.signIn(h.url+"/auth/email/callback?token="+tokens[0], jar{})
	_, page := h.open(h.url+"/auth/email/approve?token="+tokens[len(tokens)-1], jar{})
	_, _ = h.press(page, jar{})
	logs, audits := h.logText(), h.auditLog()
	for _, tok := range tokens {
		if tok == "" || strings.Contains(logs, tok) || strings.Contains(audits, tok) {
			t.Fatalf("token %q leaked:\nlogs:\n%s\naudit:\n%s", tok, logs, audits)
		}
	}
	if strings.Contains(logs+audits, "token=") {
		t.Fatalf("a link leaked:\n%s\n%s", logs, audits)
	}
}

func TestConfig(t *testing.T) {
	env := map[string]string{"BRIDGE_ALLOWED_DOMAINS": " @Partner.Example, ,x.ch", "BRIDGE_APPROVERS": " Boss@Acme.ch ,not-an-address,boss@acme.ch,ops@acme.ch"}
	cfg := ConfigFromEnv(func(k string) string { return env[k] }, "https://bridge.example.org/")
	h := newHarness(t, hopts{})
	s, err := New(Deps{Auth: h.auth, DB: h.db}, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(s.Approvers(), ","); got != "boss@acme.ch,ops@acme.ch" {
		t.Fatalf("approvers: %q", got)
	}
	if !s.domains["partner.example"] || !s.domains["x.ch"] || len(s.domains) != 2 || s.publicURL != "https://bridge.example.org" {
		t.Fatalf("config: %v %q", s.domains, s.publicURL)
	}
	if by, ok := s.admittedBy("anna@partner.example"); !ok || by != "domain" {
		t.Fatalf("domain: %q %v", by, ok)
	}
	if by, ok := s.admittedBy("ops@acme.ch"); !ok || by != "approvers" {
		t.Fatalf("approver: %q %v", by, ok)
	}
	if _, ok := s.admittedBy("anna@acme.ch"); ok {
		t.Fatal("stranger admitted")
	}
}
