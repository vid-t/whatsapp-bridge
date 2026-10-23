package sso

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"
	"golang.org/x/oauth2"

	"github.com/vid-t/whatsapp-bridge/internal/auth"
	"github.com/vid-t/whatsapp-bridge/internal/oauth"
)

const (
	pendingTTL    = 10 * time.Minute
	pendingCookie = "__Host-bridge_oidc"
	plainCookie   = "bridge_oidc" // plain-http loopback development
)

// pending is the sign-in in progress, kept in a signed cookie between start
// and callback.
type pending struct {
	Provider string `json:"p"`
	State    string `json:"s"`
	Nonce    string `json:"n"`
	Verifier string `json:"v"` // PKCE code_verifier
	Next     string `json:"x"`
	Expires  int64  `json:"e"`
}

func (s *Server) sign(b []byte) []byte {
	m := hmac.New(sha256.New, s.cookieKey)
	m.Write(b)
	return m.Sum(nil)
}

func (s *Server) setPending(w http.ResponseWriter, r *http.Request, p pending) {
	body, _ := json.Marshal(p)
	val := base64.RawURLEncoding.EncodeToString(body) + "." + base64.RawURLEncoding.EncodeToString(s.sign(body))
	http.SetCookie(w, s.pendingCookieFor(r, val, int(pendingTTL.Seconds())))
}

func (s *Server) clearPending(w http.ResponseWriter, r *http.Request) {
	http.SetCookie(w, s.pendingCookieFor(r, "", -1))
}

func (s *Server) pendingCookieFor(r *http.Request, val string, maxAge int) *http.Cookie {
	secure := secureRequest(r)
	name := pendingCookie
	if !secure {
		name = plainCookie
	}
	return &http.Cookie{Name: name, Value: val, Path: "/", MaxAge: maxAge, HttpOnly: true, Secure: secure, SameSite: http.SameSiteLaxMode}
}

// readPending returns the signed, unexpired pending sign-in, if any.
func (s *Server) readPending(r *http.Request) (pending, bool) {
	var raw string
	for _, n := range []string{pendingCookie, plainCookie} {
		if c, err := r.Cookie(n); err == nil {
			raw = c.Value
			break
		}
	}
	b64, sig64, ok := strings.Cut(raw, ".")
	if !ok {
		return pending{}, false
	}
	body, err1 := base64.RawURLEncoding.DecodeString(b64)
	sig, err2 := base64.RawURLEncoding.DecodeString(sig64)
	if err1 != nil || err2 != nil || !hmac.Equal(sig, s.sign(body)) {
		return pending{}, false
	}
	var p pending
	if json.Unmarshal(body, &p) != nil || s.now().Unix() > p.Expires {
		return pending{}, false
	}
	return p, true
}

// safePath and safeNext keep return addresses on this origin (auth.SafePath,
// auth.SafeNext).
func safePath(raw string) string { return auth.SafePath(raw) }

func safeNext(raw string) string { return auth.SafeNext(raw) }

// base is the public origin for redirect URIs.
func (s *Server) base(r *http.Request) string {
	if s.publicURL != "" {
		return s.publicURL
	}
	scheme := "http"
	if secureRequest(r) {
		scheme = "https"
	}
	return scheme + "://" + r.Host
}

func secureRequest(r *http.Request) bool { return auth.SecureRequest(r) }

func (s *Server) oauthConfig(r *http.Request, p *provider, op *oidc.Provider) *oauth2.Config {
	return &oauth2.Config{
		ClientID:     p.clientID,
		ClientSecret: p.secret,
		Endpoint:     op.Endpoint(),
		RedirectURL:  s.base(r) + "/auth/" + p.name + "/callback",
		Scopes:       []string{oidc.ScopeOpenID, "email", "profile"},
	}
}

func (s *Server) limited(w http.ResponseWriter, r *http.Request) bool {
	if ok, wait := s.limiter.Allow("sso:"+clientIP(r), 30, time.Minute); !ok {
		w.Header().Set("Retry-After", itoa(int(wait.Seconds())+1))
		s.page(w, http.StatusTooManyRequests, "Too many attempts", "Wait a minute, then try signing in again.", "/")
		return true
	}
	return false
}

// start sends the browser to the provider with state, nonce and PKCE.
func (s *Server) start(w http.ResponseWriter, r *http.Request) {
	p := s.byName[r.PathValue("provider")]
	if p == nil {
		s.page(w, http.StatusNotFound, "Sign-in method not available", "This bridge does not offer that sign-in method.", "/")
		return
	}
	if s.limited(w, r) {
		return
	}
	next := safeNext(r.URL.Query().Get("next"))
	ctx, cancel := context.WithTimeout(r.Context(), httpTimeout)
	defer cancel()
	op, _, err := p.load(ctx, s.client, s.now)
	if err != nil {
		s.log.Warn("provider discovery", "provider", p.name, "err", err)
		s.page(w, http.StatusBadGateway, p.label+" sign-in is unavailable", p.label+" did not answer. Try again in a moment, or use the access code.", next)
		return
	}
	pend := pending{Provider: p.name, State: randomString(24), Nonce: randomString(24), Verifier: oauth2.GenerateVerifier(),
		Next: next, Expires: s.now().Add(pendingTTL).Unix()}
	s.setPending(w, r, pend)
	target := s.oauthConfig(r, p, op).AuthCodeURL(pend.State, oidc.Nonce(pend.Nonce),
		oauth2.S256ChallengeOption(pend.Verifier), oauth2.SetAuthURLParam("prompt", "select_account"))
	http.Redirect(w, r, target, http.StatusFound)
}

// callback verifies the provider's answer, admits the person and signs in.
func (s *Server) callback(w http.ResponseWriter, r *http.Request) {
	p := s.byName[r.PathValue("provider")]
	if p == nil {
		s.page(w, http.StatusNotFound, "Sign-in method not available", "This bridge does not offer that sign-in method.", "/")
		return
	}
	if s.limited(w, r) {
		return
	}
	pend, ok := s.readPending(r)
	s.clearPending(w, r)
	q := r.URL.Query()
	if !ok || pend.Provider != p.name || subtle.ConstantTimeCompare([]byte(pend.State), []byte(q.Get("state"))) != 1 || pend.State == "" {
		s.page(w, http.StatusBadRequest, "This sign-in link cannot be used",
			"The sign-in expired, was started in another browser, or was altered. Start again.", "/")
		return
	}
	retry := "/auth/" + p.name + "/start?" + url.Values{"next": {pend.Next}}.Encode()
	if e := q.Get("error"); e != "" {
		msg := p.label + " did not complete the sign-in."
		if e == "access_denied" {
			msg = "The sign-in was cancelled at " + p.label + "."
		}
		s.page(w, http.StatusUnauthorized, "Not signed in", msg, pend.Next)
		return
	}
	email, name, subject, err := s.verify(r, p, pend, q.Get("code"))
	if err != nil {
		var ue userError
		if errors.As(err, &ue) {
			s.audit("auth.login_refused", "ip="+clientIP(r)+" via="+p.name+" reason="+ue.code)
			s.page(w, http.StatusForbidden, "Sign-in refused", ue.msg, retry)
			return
		}
		s.log.Warn("provider sign-in", "provider", p.name, "err", err)
		s.page(w, http.StatusBadGateway, "Sign-in failed", p.label+" could not confirm the sign-in. Try again.", retry)
		return
	}
	m, err := s.admit(r.Context(), p, email, name, subject)
	if err != nil {
		var ue userError
		if errors.As(err, &ue) {
			s.audit("auth.login_refused", "ip="+clientIP(r)+" via="+p.name+" email="+email+" reason="+ue.code)
			s.page(w, http.StatusForbidden, "Sign-in refused", ue.msg, "/")
			return
		}
		s.log.Error("admit member", "err", err)
		s.page(w, http.StatusInternalServerError, "Something went wrong", "The bridge could not sign you in. Try again in a moment.", "/")
		return
	}
	token, err := s.auth.LoginMember(r.Context(), m.ID, r.UserAgent(), clientIP(r))
	if err != nil {
		s.log.Error("create session", "err", err)
		s.page(w, http.StatusInternalServerError, "Something went wrong", "The bridge could not sign you in. Try again in a moment.", "/")
		return
	}
	http.SetCookie(w, auth.SessionCookie(token, secureRequest(r)))
	s.audit("auth.login", "ip="+clientIP(r)+" via="+p.name+" email="+m.Email)
	http.Redirect(w, r, pend.Next, http.StatusFound)
}

// userError is a refusal whose message is safe to show.
type userError struct{ code, msg string }

func (e userError) Error() string { return e.code }

// verify exchanges the code (with the PKCE verifier) and validates the ID
// token: signature, audience, expiry, issuer (multi-tenant aware), nonce and
// a verified email.
func (s *Server) verify(r *http.Request, p *provider, pend pending, code string) (email, name, subject string, err error) {
	if code == "" {
		return "", "", "", userError{"no_code", p.label + " returned no authorization code."}
	}
	ctx, cancel := context.WithTimeout(r.Context(), httpTimeout)
	defer cancel()
	op, verifier, err := p.load(ctx, s.client, s.now)
	if err != nil {
		return "", "", "", err
	}
	ctx = oidc.ClientContext(ctx, s.client)
	tok, err := s.oauthConfig(r, p, op).Exchange(ctx, code, oauth2.VerifierOption(pend.Verifier))
	if err != nil {
		return "", "", "", err
	}
	raw, _ := tok.Extra("id_token").(string)
	if raw == "" {
		return "", "", "", errors.New("no id_token in the token response")
	}
	idt, err := verifier.Verify(ctx, raw)
	if err != nil {
		return "", "", "", err
	}
	if subtle.ConstantTimeCompare([]byte(idt.Nonce), []byte(pend.Nonce)) != 1 {
		return "", "", "", errors.New("nonce mismatch")
	}
	var c claims
	if err := idt.Claims(&c); err != nil {
		return "", "", "", err
	}
	if err := p.checkIssuer(c); err != nil {
		return "", "", "", userError{"tenant", err.Error() + "."}
	}
	email, err = p.email(c)
	if err != nil {
		return "", "", "", userError{"email", err.Error() + "."}
	}
	if c.Subject == "" {
		return "", "", "", errors.New("no subject")
	}
	return email, c.Name, c.Subject, nil
}

// admit applies the admission rule and the account binding, then records
// the sign-in on the member row. A pending request (internal/members) is
// not a member yet, unless its domain is allowed.
func (s *Server) admit(ctx context.Context, p *provider, email, name, subject string) (auth.Member, error) {
	m, err := s.auth.MemberByEmail(ctx, email)
	switch {
	case errors.Is(err, auth.ErrNoMember), err == nil && !m.Approved():
		if !s.domains[auth.EmailDomain(email)] {
			if err == nil {
				return auth.Member{}, userError{"pending", email + " is waiting for approval at " + s.name() +
					". You will get an email when someone there approves it."}
			}
			return auth.Member{}, userError{"not_member", email + " is not a member of " + s.name() +
				". Ask your administrator to add you, then sign in again."}
		}
		if _, err := s.auth.EnsureApproved(ctx, email, "domain"); err != nil {
			return auth.Member{}, err
		}
	case err != nil:
		return auth.Member{}, err
	case m.Subject != "" && (m.Provider != p.name || m.Subject != subject):
		// Bound to a Google or Microsoft account. Email sign-ins never bind.
		other := strings.ToUpper(m.Provider[:1]) + m.Provider[1:]
		return auth.Member{}, userError{"other_account", email + " is linked to a different " + other +
			" account. Sign in with that account, or ask your administrator to remove and re-add you."}
	}
	return s.auth.RecordLogin(ctx, email, name, p.name, subject)
}

func (s *Server) name() string {
	if f := s.auth.Name(); f != "" {
		return f
	}
	return "this bridge"
}

// ---- helpers -----------------------------------------------------------------------------------

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}

// clientIP mirrors internal/web (auth.ClientIP).
func clientIP(r *http.Request) string { return auth.ClientIP(r) }

func itoa(n int) string { return strconv.Itoa(n) }

// page answers a refused or failed sign-in in the bridge's page shell
// (internal/oauth), with one way back.
func (s *Server) page(w http.ResponseWriter, status int, heading, msg, back string) {
	back = safePath(back)
	label := "Try again"
	if back == "/" {
		label = "Back to the sign-in page"
	}
	oauth.RenderNotice(w, status, oauth.Notice{Name: s.auth.Name(), Eyebrow: "Sign in", Heading: heading, Message: msg, Link: back, LinkLabel: label})
}
