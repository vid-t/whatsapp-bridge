package oauth

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net/http"
	"net/url"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

// client is a registered (RFC 7591) or metadata-document (CIMD) client.
// Both kinds are public clients.
type client struct {
	ID           string
	Name         string
	RedirectURIs []string
	// Document is true for CIMD clients; Host is then the host of the
	// client_id URL, which is what the consent page names.
	Document bool
	Host     string
}

// userError carries a message that is safe to show on the error page.
type userError struct{ msg string }

func (e userError) Error() string { return e.msg }

var errUnknownClient = userError{"This app is not registered with the bridge, or its registration expired. Remove the connector in the app and add it again."}

// lookupClient resolves client_id: an https URL is a CIMD client, anything
// else must be a registered client.
func (s *Server) lookupClient(ctx context.Context, id string) (client, error) {
	switch {
	case strings.HasPrefix(id, "https://"):
		return s.cimd.get(ctx, id)
	case strings.Contains(id, "://"):
		return client{}, userError{"This app identifies itself with a non-https address. Only https client identity documents are accepted."}
	case len(id) > 64:
		return client{}, errUnknownClient
	}
	var c client
	var uris string
	err := s.db.QueryRowContext(ctx, `SELECT client_id, client_name, redirect_uris FROM bridge_oauth_clients WHERE client_id = ?`, id).
		Scan(&c.ID, &c.Name, &uris)
	if errors.Is(err, sql.ErrNoRows) {
		return client{}, errUnknownClient
	}
	if err != nil {
		return client{}, err
	}
	if err := json.Unmarshal([]byte(uris), &c.RedirectURIs); err != nil {
		return client{}, err
	}
	return c, nil
}

// allowsRedirect matches a requested redirect_uri against the client's list.
func (c client) allowsRedirect(requested string) bool {
	for _, reg := range c.RedirectURIs {
		if redirectMatches(reg, requested) {
			return true
		}
	}
	return false
}

// validRedirectURI enforces the registration policy: absolute https URLs, or
// http on a loopback address (127.0.0.1, [::1], localhost) with any port, for
// command-line clients. No fragments, no credentials.
func validRedirectURI(raw string) error {
	if raw == "" || len(raw) > maxParamLen {
		return errors.New("redirect URI is empty or too long")
	}
	if strings.Contains(raw, "#") {
		return errors.New("redirect URI must not contain a fragment")
	}
	if strings.IndexFunc(raw, func(r rune) bool { return r <= ' ' || r == 0x7f }) >= 0 {
		return errors.New("redirect URI must not contain spaces or control characters")
	}
	u, err := url.Parse(raw)
	if err != nil || !u.IsAbs() || u.Host == "" || u.Opaque != "" {
		return errors.New("redirect URI must be an absolute URL")
	}
	if u.User != nil {
		return errors.New("redirect URI must not contain credentials")
	}
	switch u.Scheme {
	case "https":
		return nil
	case "http":
		if isLoopbackHost(u.Hostname()) {
			return nil
		}
		return errors.New("plain http redirect URIs are allowed only for 127.0.0.1, [::1] and localhost")
	}
	return errors.New("redirect URI must use https (or http on a loopback address)")
}

// redirectMatches compares exactly, except that for http loopback URIs the
// port is ignored: native clients such as Claude Code bind an ephemeral port
// at the time of the request (RFC 8252 section 7.3, OAuth 2.1 section 8.4.2;
// Claude Code's metadata document lists http://localhost/callback).
func redirectMatches(registered, requested string) bool {
	if registered == requested {
		return true
	}
	a, err1 := url.Parse(registered)
	b, err2 := url.Parse(requested)
	if err1 != nil || err2 != nil || a.Scheme != "http" || b.Scheme != "http" {
		return false
	}
	if !isLoopbackHost(a.Hostname()) || !strings.EqualFold(a.Hostname(), b.Hostname()) {
		return false
	}
	return a.EscapedPath() == b.EscapedPath() && a.RawQuery == b.RawQuery &&
		a.User == nil && b.User == nil && a.Fragment == "" && b.Fragment == ""
}

// ---- dynamic client registration (RFC 7591) -------------------------------------

type registration struct {
	RedirectURIs            []string `json:"redirect_uris"`
	ClientName              string   `json:"client_name"`
	GrantTypes              []string `json:"grant_types"`
	ResponseTypes           []string `json:"response_types"`
	TokenEndpointAuthMethod string   `json:"token_endpoint_auth_method"`
	Scope                   string   `json:"scope"`
}

type registered struct {
	ClientID                string   `json:"client_id"`
	ClientIDIssuedAt        int64    `json:"client_id_issued_at"`
	ClientName              string   `json:"client_name"`
	RedirectURIs            []string `json:"redirect_uris"`
	GrantTypes              []string `json:"grant_types"`
	ResponseTypes           []string `json:"response_types"`
	TokenEndpointAuthMethod string   `json:"token_endpoint_auth_method"`
	Scope                   string   `json:"scope"`
}

func (s *Server) register(w http.ResponseWriter, r *http.Request) {
	if corsPreflight(w, r, "POST, OPTIONS") {
		return
	}
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST, OPTIONS")
		oauthError(w, http.StatusMethodNotAllowed, "invalid_request", "method not allowed")
		return
	}
	if ok, wait := s.limited(r, "register", 20, time.Hour); !ok {
		setRetryAfter(w, wait)
		oauthError(w, http.StatusTooManyRequests, "invalid_request", "too many registrations from this address; try again later")
		return
	}
	if mt, _, _ := mime.ParseMediaType(r.Header.Get("Content-Type")); mt != "application/json" {
		oauthError(w, http.StatusBadRequest, "invalid_client_metadata", "content type must be application/json")
		return
	}
	var req registration
	dec := json.NewDecoder(r.Body)
	if err := dec.Decode(&req); err != nil {
		var tooBig *http.MaxBytesError
		if errors.As(err, &tooBig) {
			oauthError(w, http.StatusRequestEntityTooLarge, "invalid_client_metadata", "registration request too large")
			return
		}
		oauthError(w, http.StatusBadRequest, "invalid_client_metadata", "body must be a JSON object with valid client metadata")
		return
	}
	if dec.Decode(&struct{}{}) != io.EOF {
		oauthError(w, http.StatusBadRequest, "invalid_client_metadata", "unexpected data after the JSON object")
		return
	}
	reg, code, msg := validateRegistration(req)
	if code != "" {
		oauthError(w, http.StatusBadRequest, code, msg)
		return
	}
	s.maybePrune(r.Context())
	now := s.now()
	reg.ClientID = "wbc_" + randomToken()[:22]
	reg.ClientIDIssuedAt = now.Unix()
	uris, _ := json.Marshal(reg.RedirectURIs)
	if _, err := s.db.ExecContext(r.Context(), `INSERT INTO bridge_oauth_clients (client_id, client_name, redirect_uris, created_at) VALUES (?, ?, ?, ?)`,
		reg.ClientID, reg.ClientName, string(uris), ms(now)); err != nil {
		s.log.Error("register client", "err", err)
		oauthError(w, http.StatusInternalServerError, "server_error", "registration failed; try again")
		return
	}
	s.audit("oauth.register", "client="+reg.ClientID+" name="+quoteName(reg.ClientName)+" redirect_hosts="+redirectHosts(reg.RedirectURIs)+" ip="+clientIP(r))
	writeJSON(w, http.StatusCreated, reg)
}

// validateRegistration applies the public-client policy. Requested auth
// methods other than "none" are replaced by "none" (RFC 7591 section 3.2.1
// lets the server substitute values); the response tells the client.
func validateRegistration(req registration) (registered, string, string) {
	if len(req.RedirectURIs) == 0 {
		return registered{}, "invalid_redirect_uri", "redirect_uris is required"
	}
	if len(req.RedirectURIs) > 10 {
		return registered{}, "invalid_redirect_uri", "at most 10 redirect_uris"
	}
	for _, u := range req.RedirectURIs {
		if err := validRedirectURI(u); err != nil {
			return registered{}, "invalid_redirect_uri", err.Error()
		}
	}
	for _, g := range req.GrantTypes {
		if g != "authorization_code" && g != "refresh_token" {
			return registered{}, "invalid_client_metadata", "grant_types may contain only authorization_code and refresh_token"
		}
	}
	for _, rt := range req.ResponseTypes {
		if rt != "code" {
			return registered{}, "invalid_client_metadata", `response_types may contain only "code"`
		}
	}
	if !validScope(req.Scope) {
		return registered{}, "invalid_client_metadata", `the only scope is "mcp"`
	}
	name, ok := cleanName(req.ClientName)
	if !ok {
		return registered{}, "invalid_client_metadata", "client_name is too long or contains control characters"
	}
	if name == "" {
		name = firstHost(req.RedirectURIs)
	}
	return registered{
		ClientName:              name,
		RedirectURIs:            req.RedirectURIs,
		GrantTypes:              []string{"authorization_code", "refresh_token"},
		ResponseTypes:           []string{"code"},
		TokenEndpointAuthMethod: "none",
		Scope:                   Scope,
	}, "", ""
}

// cleanName trims a client name and rejects control characters.
func cleanName(name string) (string, bool) {
	name = strings.TrimSpace(name)
	if utf8.RuneCountInString(name) > 100 || strings.IndexFunc(name, unicode.IsControl) >= 0 {
		return "", false
	}
	return name, true
}

func firstHost(uris []string) string {
	for _, raw := range uris {
		if u, err := url.Parse(raw); err == nil && u.Hostname() != "" {
			return u.Hostname()
		}
	}
	return "Unnamed app"
}

func redirectHosts(uris []string) string {
	seen := map[string]bool{}
	var out []string
	for _, raw := range uris {
		if u, err := url.Parse(raw); err == nil && !seen[u.Hostname()] {
			seen[u.Hostname()] = true
			out = append(out, u.Hostname())
		}
	}
	return strings.Join(out, ",")
}

func validScope(scope string) bool {
	for _, f := range strings.Split(scope, " ") {
		if f != "" && f != Scope {
			return false
		}
	}
	return true
}
