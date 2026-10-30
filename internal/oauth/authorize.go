package oauth

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/vid-t/whatsapp-bridge/internal/auth"
)

// authzRequest is an authorization request. On the consent and sign-in
// pages it travels as hidden form fields and is validated again on POST.
type authzRequest struct {
	ResponseType        string
	ClientID            string
	RedirectURI         string
	State               string
	CodeChallenge       string
	CodeChallengeMethod string
	Scope               string
	Resource            string
}

var (
	challengeRe = regexp.MustCompile(`^[A-Za-z0-9._~-]{43,128}$`)
	verifierRe  = challengeRe // RFC 7636: 43-128 unreserved characters
)

// parseAuthz reads the request parameters and reports the first one that
// appears more than once (RFC 6749 section 3.1 forbids repeats).
func parseAuthz(v url.Values) (authzRequest, string) {
	dup := ""
	get := func(k string) string {
		if len(v[k]) > 1 && dup == "" {
			dup = k
		}
		return v.Get(k)
	}
	req := authzRequest{
		ResponseType:        get("response_type"),
		ClientID:            get("client_id"),
		RedirectURI:         get("redirect_uri"),
		State:               get("state"),
		CodeChallenge:       get("code_challenge"),
		CodeChallengeMethod: get("code_challenge_method"),
		Scope:               get("scope"),
	}
	// RFC 8707 allows several resource values; this server has one resource,
	// so repeats are fine only when they are all the same.
	for i, r := range v["resource"] {
		if i == 0 {
			req.Resource = r
		} else if r != req.Resource && dup == "" {
			dup = "resource"
		}
	}
	return req, dup
}

func (s *Server) authorize(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet, http.MethodHead, http.MethodPost:
	default:
		w.Header().Set("Allow", "GET, POST")
		s.renderError(w, r, http.StatusMethodNotAllowed, "Method not allowed", "Open this page from the app you are connecting.")
		return
	}
	if ok, wait := s.limited(r, "authorize", 30, time.Minute); !ok {
		setRetryAfter(w, wait)
		s.renderError(w, r, http.StatusTooManyRequests, "Too many attempts", "Wait a minute, then start connecting again from the app.")
		return
	}
	vals := r.URL.Query()
	if r.Method == http.MethodPost {
		// The consent and sign-in forms change state: same-origin only.
		if err := s.cop.Check(r); err != nil {
			s.renderError(w, r, http.StatusForbidden, "Request blocked", "This form was submitted from another site. Start connecting again from the app.")
			return
		}
		if err := r.ParseForm(); err != nil {
			s.renderError(w, r, http.StatusBadRequest, "This sign-in link cannot be used", "The form could not be read. Start connecting again from the app.")
			return
		}
		vals = r.PostForm
	}
	req, dup := parseAuthz(vals)

	// Until client_id and redirect_uri are trusted, never redirect.
	cl, ok := s.trust(w, r, req, dup)
	if !ok {
		return
	}
	iss := s.issuer(r)
	if code, desc := s.checkAuthz(req, dup, iss); code != "" {
		s.redirectError(w, r, req, iss, code, desc)
		return
	}

	signedIn, ref, actor := s.session(r)
	if r.Method != http.MethodPost {
		if !signedIn {
			s.renderSignIn(w, r, http.StatusOK, req, cl, "")
			return
		}
		s.renderConsent(w, r, req, cl)
		return
	}
	switch vals.Get("action") {
	case "login":
		s.signIn(w, r, req, cl)
	case "allow", "deny":
		if !signedIn {
			s.renderSignIn(w, r, http.StatusUnauthorized, req, cl, "Your sign-in has expired. Enter the access code again.")
			return
		}
		// Sending is approved only when the page shown said so, and the
		// bridge still has sending on.
		canSend := s.send && vals.Get("send") == "1"
		s.decide(w, r, req, cl, iss, ref, actor, vals.Get("action") == "allow", canSend)
	default:
		s.renderError(w, r, http.StatusBadRequest, "This sign-in link cannot be used", "The form was incomplete. Start connecting again from the app.")
	}
}

// trust validates client_id and redirect_uri. On failure it renders the
// error page itself and returns false: an unverified redirect_uri is never
// followed (RFC 6749 section 4.1.2.1).
func (s *Server) trust(w http.ResponseWriter, r *http.Request, req authzRequest, dup string) (client, bool) {
	bad := func(msg string) (client, bool) {
		s.renderError(w, r, http.StatusBadRequest, "This sign-in link cannot be used", msg)
		return client{}, false
	}
	if dup == "client_id" || dup == "redirect_uri" {
		return bad("The request repeats the " + dup + " parameter.")
	}
	if req.ClientID == "" {
		return bad("The request does not say which app is asking (client_id is missing).")
	}
	cl, err := s.lookupClient(r.Context(), req.ClientID)
	if err != nil {
		var ue userError
		if errors.As(err, &ue) {
			return bad(ue.msg)
		}
		s.log.Error("look up client", "err", err)
		s.renderError(w, r, http.StatusInternalServerError, "Something went wrong", "The bridge could not look up this app. Try again in a moment.")
		return client{}, false
	}
	if req.RedirectURI == "" {
		return bad("The request does not say where to return (redirect_uri is missing).")
	}
	if err := validRedirectURI(req.RedirectURI); err != nil {
		return bad("The return address is not acceptable: " + err.Error() + ".")
	}
	if !cl.allowsRedirect(req.RedirectURI) {
		return bad("The return address " + req.RedirectURI + " is not registered for this app.")
	}
	return cl, true
}

// checkAuthz validates the rest of the request once the redirect target is
// trusted; errors are returned to the client by redirect.
func (s *Server) checkAuthz(req authzRequest, dup, iss string) (string, string) {
	switch {
	case dup != "":
		return "invalid_request", "the " + dup + " parameter is repeated"
	case req.ResponseType != "code":
		return "unsupported_response_type", `response_type must be "code"`
	case req.CodeChallenge == "":
		return "invalid_request", "code_challenge is required (PKCE with S256)"
	case req.CodeChallengeMethod != "S256":
		return "invalid_request", "code_challenge_method must be S256"
	case !challengeRe.MatchString(req.CodeChallenge):
		return "invalid_request", "code_challenge is malformed"
	case !validScope(req.Scope):
		return "invalid_scope", `the only scope is "mcp"`
	case req.Resource != "" && !sameResource(req.Resource, iss+ResourcePath):
		return "invalid_target", "resource must be " + iss + ResourcePath
	case len(req.State) > maxParamLen:
		return "invalid_request", "state is too long"
	}
	return "", ""
}

// session reports whether the browser may approve, a reference to the
// approving session or member for the code and grant (never the session
// token), and the actor for audit lines.
func (s *Server) session(r *http.Request) (bool, string, string) {
	p := s.auth.ResolveSession(r)
	switch {
	case p.Kind == auth.PrincipalOpen:
		return true, "open", "open"
	case p.User != nil:
		return true, "m:" + p.User.ID, p.User.Email
	case p.Kind == auth.PrincipalSession:
		return true, sessionRef(auth.CookieValue(r)), "session"
	}
	return false, "", ""
}

func sessionRef(token string) string {
	h := sha256.Sum256([]byte(token))
	return "s:" + hex.EncodeToString(h[:8])
}

// signIn handles the access-code form shown inline on the authorize page.
func (s *Server) signIn(w http.ResponseWriter, r *http.Request, req authzRequest, cl client) {
	if s.auth.OpenMode() {
		s.renderConsent(w, r, req, cl)
		return
	}
	ip := clientIP(r)
	if ok, wait := s.limited(r, "login", 5, time.Minute); !ok {
		setRetryAfter(w, wait)
		s.renderSignIn(w, r, http.StatusTooManyRequests, req, cl, "Too many attempts. Wait a minute and try again.")
		return
	}
	token, err := s.auth.Login(r.Context(), r.PostForm.Get("access_code"), r.UserAgent(), ip)
	if err != nil {
		if errors.Is(err, auth.ErrBadCode) {
			s.audit("auth.login_failed", "ip="+ip+" via=oauth")
			s.renderSignIn(w, r, http.StatusUnauthorized, req, cl, "That access code is not right.")
			return
		}
		s.log.Error("login", "err", err)
		s.renderError(w, r, http.StatusInternalServerError, "Something went wrong", "The bridge could not sign you in. Try again in a moment.")
		return
	}
	http.SetCookie(w, auth.SessionCookie(token, secureRequest(r)))
	s.audit("auth.login", "ip="+ip+" via=oauth")
	s.renderConsent(w, r, req, cl)
}

// decide turns Allow into a single-use code and Deny into access_denied.
// canSend is the consent to send, carried by the code to the grant.
func (s *Server) decide(w http.ResponseWriter, r *http.Request, req authzRequest, cl client, iss, ref, actor string, allow, canSend bool) {
	detail := "client=" + cl.ID + " name=" + quoteName(cl.Name) + " redirect_host=" + hostOf(req.RedirectURI) + " by=" + actor
	if !allow {
		s.audit("oauth.consent_denied", detail)
		s.redirectError(w, r, req, iss, "access_denied", "the user denied access")
		return
	}
	code := randomToken()
	now := s.now()
	resource := ""
	if req.Resource != "" {
		resource = iss + ResourcePath
	}
	_, err := s.db.ExecContext(r.Context(), `INSERT INTO bridge_oauth_codes
		(hash, client_id, client_name, redirect_uri, code_challenge, scope, resource, session_ref, created_at, expires_at, can_send)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		hashOf(code), cl.ID, cl.Name, req.RedirectURI, req.CodeChallenge, Scope, resource, ref, ms(now), ms(now.Add(codeTTL)), canSend)
	if err != nil {
		s.log.Error("store code", "err", err)
		s.redirectError(w, r, req, iss, "server_error", "the bridge could not issue a code")
		return
	}
	if canSend {
		detail += " send=on"
	} else {
		detail += " send=off"
	}
	s.audit("oauth.consent_granted", detail)
	q := url.Values{"code": {code}}
	s.finishRedirect(w, r, req, iss, q)
}

func (s *Server) redirectError(w http.ResponseWriter, r *http.Request, req authzRequest, iss, code, desc string) {
	s.finishRedirect(w, r, req, iss, url.Values{"error": {code}, "error_description": {desc}})
}

// finishRedirect sends the browser back to the (trusted) redirect_uri with
// the result, the unchanged state and the issuer (RFC 9207).
func (s *Server) finishRedirect(w http.ResponseWriter, r *http.Request, req authzRequest, iss string, params url.Values) {
	u, err := url.Parse(req.RedirectURI)
	if err != nil {
		s.renderError(w, r, http.StatusBadRequest, "This sign-in link cannot be used", "The return address is not valid.")
		return
	}
	q := u.Query()
	for k, v := range params {
		q[k] = v
	}
	if req.State != "" {
		q.Set("state", req.State)
	}
	if iss != "" {
		q.Set("iss", iss)
	}
	u.RawQuery = q.Encode()
	status := http.StatusFound
	if r.Method == http.MethodPost {
		status = http.StatusSeeOther
	}
	w.Header().Set("Location", u.String())
	w.WriteHeader(status)
}

// sameResource compares resource indicators: scheme and host
// case-insensitively, default ports dropped, one trailing slash ignored.
func sameResource(a, b string) bool {
	na, ok1 := normResource(a)
	nb, ok2 := normResource(b)
	return ok1 && ok2 && na == nb
}

func normResource(raw string) (string, bool) {
	u, err := url.Parse(raw)
	if err != nil || !u.IsAbs() || u.Host == "" || u.Fragment != "" || u.RawQuery != "" || u.User != nil {
		return "", false
	}
	host := strings.ToLower(u.Host)
	scheme := strings.ToLower(u.Scheme)
	host = strings.TrimSuffix(host, map[string]string{"https": ":443", "http": ":80"}[scheme])
	return scheme + "://" + host + strings.TrimSuffix(u.EscapedPath(), "/"), true
}

// query re-encodes the request, for links that must come back to it.
func (req authzRequest) query() url.Values {
	q := url.Values{}
	for k, v := range map[string]string{
		"response_type": req.ResponseType, "client_id": req.ClientID, "redirect_uri": req.RedirectURI,
		"state": req.State, "code_challenge": req.CodeChallenge, "code_challenge_method": req.CodeChallengeMethod,
		"scope": req.Scope, "resource": req.Resource,
	} {
		if v != "" {
			q.Set(k, v)
		}
	}
	return q
}

func hostOf(raw string) string {
	if u, err := url.Parse(raw); err == nil {
		return u.Host
	}
	return ""
}
