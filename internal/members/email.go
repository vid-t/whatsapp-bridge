package members

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/vid-t/whatsapp-bridge/internal/auth"
	"github.com/vid-t/whatsapp-bridge/internal/mail"
	"github.com/vid-t/whatsapp-bridge/internal/oauth"
)

// ---- POST /auth/email/start ------------------------------------------------------

// start accepts an address and answers the same way whether or not it is a
// member. The admission itself (lookups, writes, mails) runs in the
// background, so neither the answer nor its timing depends on the address.
func (s *Server) start(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, maxBody)
	mt, _, _ := strings.Cut(r.Header.Get("Content-Type"), ";")
	form := strings.EqualFold(strings.TrimSpace(mt), "application/x-www-form-urlencoded")
	ip := auth.ClientIP(r)

	var body struct {
		Email string `json:"email"`
		Next  string `json:"next"`
	}
	switch {
	case form:
		if err := r.ParseForm(); err != nil {
			s.startError(w, r, true, http.StatusBadRequest, "The form could not be read.", "/")
			return
		}
		body.Email, body.Next = r.PostForm.Get("email"), r.PostForm.Get("next")
	case strings.EqualFold(strings.TrimSpace(mt), "application/json"):
		dec := json.NewDecoder(r.Body)
		if err := dec.Decode(&body); err != nil || dec.Decode(&struct{}{}) != io.EOF {
			writeError(w, http.StatusBadRequest, "invalid JSON body")
			return
		}
	default:
		writeError(w, http.StatusUnsupportedMediaType, "content type must be application/json or a form")
		return
	}
	next := auth.SafeNext(body.Next)

	if ok, wait := s.limiter.Allow("email-ip:"+ip, 20, time.Hour); !ok {
		retryAfter(w, wait)
		s.startError(w, r, form, http.StatusTooManyRequests, "Too many sign-in requests from this network. Try again later.", next)
		return
	}
	email, ok := auth.NormalizeEmail(body.Email)
	if !ok {
		s.startError(w, r, form, http.StatusBadRequest, "That does not look like an email address.", next)
		return
	}
	if ok, wait := s.limiter.Allow("email-addr:"+email, 5, 15*time.Minute); !ok {
		retryAfter(w, wait)
		s.startError(w, r, form, http.StatusTooManyRequests, "Too many sign-in requests for this address. Wait a few minutes, then try again.", next)
		return
	}
	base := s.base(r)
	s.enqueue("admit", func(ctx context.Context) { s.admit(ctx, email, next, ip, base) })
	if form {
		http.Redirect(w, r, "/signin?sent=1", http.StatusSeeOther)
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]bool{"ok": true})
}

// startError answers a refused start: JSON for the console, a page with a
// way back for the no-JavaScript form.
func (s *Server) startError(w http.ResponseWriter, r *http.Request, form bool, status int, msg, next string) {
	if !form {
		switch status {
		case http.StatusBadRequest:
			writeError(w, status, "email must be a valid address")
		default:
			writeError(w, status, "too many sign-in requests; try again later")
		}
		return
	}
	back := next
	if back == "/" {
		back = "/signin"
	}
	s.notice(w, status, oauth.Notice{Eyebrow: "Sign in", Heading: "No link sent", Message: msg, Link: back, LinkLabel: "Back"})
}

// admit applies the admission rule to a start request. Errors are logged;
// the requester has already been told to check their email.
func (s *Server) admit(ctx context.Context, email, next, ip, base string) {
	m, err := s.auth.MemberByEmail(ctx, email)
	_, self := s.admittedBy(email)
	switch {
	case err == nil && m.Approved(), self && (err == nil || errors.Is(err, auth.ErrNoMember)):
		s.sendSignIn(ctx, email, next, ip, base)
	case err == nil: // pending
		s.notifyRequest(ctx, m, ip, base)
	case errors.Is(err, auth.ErrNoMember):
		if n, err := s.auth.CountPending(ctx); err != nil || n >= maxPending {
			s.log.Warn("access request not recorded: too many pending requests", "pending", n, "err", err)
			return
		}
		m, _, err := s.auth.RequestMember(ctx, email)
		if err != nil {
			s.log.Error("record access request", "err", err)
			return
		}
		if m.Approved() { // approved concurrently
			s.sendSignIn(ctx, email, next, ip, base)
			return
		}
		s.notifyRequest(ctx, m, ip, base)
	default:
		s.log.Error("look up member", "err", err)
	}
}

func (s *Server) sendSignIn(ctx context.Context, email, next, ip, base string) {
	if s.mail == nil {
		return
	}
	tok, err := s.issue(ctx, link{Purpose: purposeSignIn, Email: email, Next: next}, ip, signinTTL)
	if err != nil {
		s.log.Error("issue sign-in link", "err", err)
		return
	}
	s.send(mail.SignIn(email, s.name(), base+"/auth/email/callback?token="+tok, signinTTL))
}

// notifyRequest tells the requester and every approver, at most once per
// renotifyEvery per address.
func (s *Server) notifyRequest(ctx context.Context, m auth.Member, ip, base string) {
	ok, err := s.auth.ClaimNotify(ctx, m.ID, renotifyEvery)
	if err != nil {
		s.log.Error("claim notification", "err", err)
		return
	}
	if !ok {
		return
	}
	s.audit("member.requested", "id="+m.ID+" email="+m.Email+" ip="+ip)
	if s.mail == nil {
		return
	}
	s.send(mail.Request(m.Email, s.name()))
	for _, a := range s.approvers {
		if ok, _ := s.limiter.Allow("approver-mails", approverMailRate, time.Hour); !ok {
			s.log.Warn("approve mail not sent: hourly limit reached", "request", m.ID)
			return
		}
		tok, err := s.issue(ctx, link{Purpose: purposeApprove, Email: m.Email, Approver: a}, ip, approveTTL)
		if err != nil {
			s.log.Error("issue approve link", "err", err)
			return
		}
		s.send(mail.Approve(a, s.name(), m.Email, base+"/auth/email/approve?token="+tok, approveTTL))
	}
}

// sendAccess emails a fresh sign-in link after an approval or invitation.
func (s *Server) sendAccess(ctx context.Context, email, base string) {
	if s.mail == nil || base == "" {
		return
	}
	tok, err := s.issue(ctx, link{Purpose: purposeSignIn, Email: email, Next: "/"}, "", accessTTL)
	if err != nil {
		s.log.Error("issue sign-in link", "err", err)
		return
	}
	s.send(mail.Approved(email, s.name(), base+"/auth/email/callback?token="+tok, accessTTL))
}

// ---- /auth/email/callback ----------------------------------------------------------

// callbackPage moves a token from the URL into the link cookie (step 1), or
// shows the "Continue" button for the token in the cookie (step 2). Neither
// step uses the token.
func (s *Server) callbackPage(w http.ResponseWriter, r *http.Request) {
	if s.linkLimited(w, r) {
		return
	}
	if tok := r.URL.Query().Get("token"); tok != "" {
		if _, err := s.peek(r.Context(), tok, purposeSignIn); err != nil {
			s.signinError(w, r, err)
			return
		}
		setLinkCookie(w, r, purposeSignIn, tok, int(linkCookieTTL.Seconds()))
		http.Redirect(w, r, "/auth/email/callback", http.StatusSeeOther)
		return
	}
	l, err := s.peek(r.Context(), linkCookie(r, purposeSignIn), purposeSignIn)
	if err != nil {
		s.signinError(w, r, err)
		return
	}
	s.notice(w, http.StatusOK, oauth.Notice{Eyebrow: "Sign in", Heading: "Sign in to " + s.name(),
		Message: "Continue as " + l.Email + ".", Detail: "The link works once.",
		Action: "/auth/email/callback", ActionLabel: "Continue"})
}

// callback uses the token in the link cookie and signs the person in.
func (s *Server) callback(w http.ResponseWriter, r *http.Request) {
	if s.linkLimited(w, r) {
		return
	}
	tok := linkCookie(r, purposeSignIn)
	clearLinkCookie(w, r, purposeSignIn)
	l, err := s.consume(r.Context(), tok, purposeSignIn)
	if err != nil {
		s.signinError(w, r, err)
		return
	}
	ip := auth.ClientIP(r)
	m, err := s.auth.MemberByEmail(r.Context(), l.Email)
	if err != nil && !errors.Is(err, auth.ErrNoMember) {
		s.serverError(w, "look up member", err)
		return
	}
	if err != nil || !m.Approved() {
		by, ok := s.admittedBy(l.Email)
		if !ok {
			s.audit("auth.login_refused", "ip="+ip+" via=email email="+l.Email+" reason=pending")
			http.Redirect(w, r, "/signin?status=pending", http.StatusSeeOther)
			return
		}
		if _, err := s.auth.EnsureApproved(r.Context(), l.Email, by); err != nil {
			s.serverError(w, "admit member", err)
			return
		}
	}
	m, err = s.auth.RecordLogin(r.Context(), l.Email, "", "email", "")
	if err != nil {
		s.serverError(w, "record sign-in", err)
		return
	}
	session, err := s.auth.LoginMemberVia(r.Context(), m.ID, "email", r.UserAgent(), ip)
	if err != nil {
		s.serverError(w, "create session", err)
		return
	}
	http.SetCookie(w, auth.SessionCookie(session, auth.SecureRequest(r)))
	s.audit("signin.email", "email="+m.Email+" ip="+ip)
	http.Redirect(w, r, auth.SafeNext(l.Next), http.StatusSeeOther)
}

func (s *Server) signinError(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, errLinkUsed), errors.Is(err, errLinkExpired), errors.Is(err, errLinkInvalid):
		clearLinkCookie(w, r, purposeSignIn)
		http.Redirect(w, r, "/signin?error="+err.Error(), http.StatusSeeOther)
	default:
		s.serverError(w, "check sign-in link", err)
	}
}

// ---- /auth/email/approve --------------------------------------------------------------

// approvePage is callbackPage for an approver's link: it shows who asked and
// an Approve button; it approves nothing by itself.
func (s *Server) approvePage(w http.ResponseWriter, r *http.Request) {
	if s.linkLimited(w, r) {
		return
	}
	if tok := r.URL.Query().Get("token"); tok != "" {
		if _, err := s.peek(r.Context(), tok, purposeApprove); err != nil {
			s.approveError(w, r, err)
			return
		}
		setLinkCookie(w, r, purposeApprove, tok, int(linkCookieTTL.Seconds()))
		http.Redirect(w, r, "/auth/email/approve", http.StatusSeeOther)
		return
	}
	l, err := s.peek(r.Context(), linkCookie(r, purposeApprove), purposeApprove)
	if err != nil {
		s.approveError(w, r, err)
		return
	}
	m, err := s.auth.MemberByEmail(r.Context(), l.Email)
	switch {
	case errors.Is(err, auth.ErrNoMember):
		s.requestGone(w, r, l.Email)
		return
	case err != nil:
		s.serverError(w, "look up member", err)
		return
	case m.Approved():
		clearLinkCookie(w, r, purposeApprove)
		s.notice(w, http.StatusOK, oauth.Notice{Eyebrow: "Access request", Heading: "Already approved",
			Message: m.Email + " can already sign in" + approvedByText(m) + ".", Link: "/", LinkLabel: "Open " + s.name()})
		return
	}
	detail := ""
	if m.RequestedAt != nil {
		detail = "Asked on " + m.RequestedAt.UTC().Format("2 January 2006 at 15:04") + " UTC."
	}
	s.notice(w, http.StatusOK, oauth.Notice{Eyebrow: "Access request", Heading: "Approve " + m.Email + "?",
		Message: m.Email + " asks for access to " + s.name() + ". Approving lets this person sign in to the bridge and read the chats shared there.",
		Detail:  detail, Action: "/auth/email/approve", ActionLabel: "Approve"})
}

// approve uses the approver's token from the link cookie.
func (s *Server) approve(w http.ResponseWriter, r *http.Request) {
	if s.linkLimited(w, r) {
		return
	}
	tok := linkCookie(r, purposeApprove)
	clearLinkCookie(w, r, purposeApprove)
	l, err := s.consume(r.Context(), tok, purposeApprove)
	if err != nil {
		s.approveError(w, r, err)
		return
	}
	m, err := s.auth.MemberByEmail(r.Context(), l.Email)
	switch {
	case errors.Is(err, auth.ErrNoMember):
		s.requestGone(w, r, l.Email)
		return
	case err != nil:
		s.serverError(w, "look up member", err)
		return
	}
	m, changed, err := s.auth.ApproveMember(r.Context(), m.ID, l.Approver)
	if err != nil {
		s.serverError(w, "approve member", err)
		return
	}
	if changed {
		s.audit("member.approved", "id="+m.ID+" email="+m.Email+" by="+l.Approver)
		s.sendAccess(r.Context(), m.Email, s.base(r))
	}
	msg := m.Email + " can now sign in."
	if s.mail != nil && changed {
		msg += " They will get an email with a sign-in link."
	}
	s.notice(w, http.StatusOK, oauth.Notice{Eyebrow: "Access request", Heading: "Approved", Message: msg,
		Link: "/", LinkLabel: "Open " + s.name()})
}

func (s *Server) approveError(w http.ResponseWriter, r *http.Request, err error) {
	clearLinkCookie(w, r, purposeApprove)
	n := oauth.Notice{Eyebrow: "Access request", Link: "/", LinkLabel: "Open " + s.name(),
		Detail: "Pending requests can also be approved on the Members page at " + s.base(r) + "."}
	switch {
	case errors.Is(err, errLinkUsed):
		n.Heading, n.Message = "This link was already used", "The request was already handled with this link."
		s.notice(w, http.StatusGone, n)
	case errors.Is(err, errLinkExpired):
		n.Heading, n.Message = "This link has expired", "Approval links work for 7 days."
		s.notice(w, http.StatusGone, n)
	case errors.Is(err, errLinkInvalid):
		n.Heading, n.Message = "This link is not valid", "It may be incomplete, or the request was removed."
		s.notice(w, http.StatusNotFound, n)
	default:
		s.serverError(w, "check approve link", err)
	}
}

func (s *Server) requestGone(w http.ResponseWriter, r *http.Request, email string) {
	clearLinkCookie(w, r, purposeApprove)
	msg := "Someone at " + s.name() + " removed the request from " + email + ". Nothing was approved."
	s.notice(w, http.StatusGone, oauth.Notice{Eyebrow: "Access request", Heading: "This request was removed", Message: msg,
		Link: "/", LinkLabel: "Open " + s.name()})
}

func approvedByText(m auth.Member) string {
	switch m.ApprovedBy {
	case "", "domain", "approvers", "session", "open", "admin":
		return ""
	}
	return " (approved by " + m.ApprovedBy + ")"
}

// linkLimited caps link opens per IP; tokens cannot be guessed, this only
// keeps the pages cheap.
func (s *Server) linkLimited(w http.ResponseWriter, r *http.Request) bool {
	if ok, wait := s.limiter.Allow("email-link:"+auth.ClientIP(r), 30, time.Minute); !ok {
		retryAfter(w, wait)
		s.notice(w, http.StatusTooManyRequests, oauth.Notice{Eyebrow: "Sign in", Heading: "Too many attempts",
			Message: "Wait a minute, then open the link again."})
		return true
	}
	return false
}

func (s *Server) serverError(w http.ResponseWriter, what string, err error) {
	s.log.Error(what, "err", err)
	s.notice(w, http.StatusInternalServerError, oauth.Notice{Eyebrow: "Sign in", Heading: "Something went wrong",
		Message: "The bridge could not finish this step. Try again in a moment.", Link: "/", LinkLabel: "Back"})
}

func (s *Server) notice(w http.ResponseWriter, status int, n oauth.Notice) {
	n.Name = s.name()
	oauth.RenderNotice(w, status, n)
}

func retryAfter(w http.ResponseWriter, wait time.Duration) {
	secs := int(wait.Seconds()) + 1
	w.Header().Set("Retry-After", strconv.Itoa(secs))
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}
