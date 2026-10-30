package oauth

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"database/sql"
	"encoding/base64"
	"errors"
	"mime"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/vid-t/whatsapp-bridge/internal/auth"
)

type tokenResponse struct {
	AccessToken  string `json:"access_token"`
	TokenType    string `json:"token_type"`
	ExpiresIn    int    `json:"expires_in"`
	Scope        string `json:"scope"`
	RefreshToken string `json:"refresh_token"`
}

// tokenError is an RFC 6749 section 5.2 error from the grant handlers.
type tokenError struct{ code, desc string }

func (e *tokenError) Error() string { return e.code + ": " + e.desc }

func invalidGrant(desc string) *tokenError { return &tokenError{"invalid_grant", desc} }

// readForm parses an application/x-www-form-urlencoded POST and rejects
// repeated parameters (RFC 6749 section 3.2).
func readForm(r *http.Request) (url.Values, *tokenError) {
	if mt, _, _ := mime.ParseMediaType(r.Header.Get("Content-Type")); mt != "application/x-www-form-urlencoded" {
		return nil, &tokenError{"invalid_request", "content type must be application/x-www-form-urlencoded"}
	}
	if err := r.ParseForm(); err != nil {
		return nil, &tokenError{"invalid_request", "the form could not be parsed"}
	}
	for k, v := range r.PostForm {
		if len(v) > 1 && k != "resource" {
			return nil, &tokenError{"invalid_request", "the " + k + " parameter is repeated"}
		}
	}
	return r.PostForm, nil
}

// formClientID returns client_id from the form, or from HTTP Basic for
// clients that send it there; a public client has no secret to check.
func formClientID(r *http.Request, form url.Values) string {
	if id := form.Get("client_id"); id != "" {
		return id
	}
	if u, _, ok := r.BasicAuth(); ok {
		if dec, err := url.QueryUnescape(u); err == nil {
			return dec
		}
		return u
	}
	return ""
}

func (s *Server) token(w http.ResponseWriter, r *http.Request) {
	if corsPreflight(w, r, "POST, OPTIONS") {
		return
	}
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST, OPTIONS")
		oauthError(w, http.StatusMethodNotAllowed, "invalid_request", "method not allowed")
		return
	}
	if ok, wait := s.limited(r, "token", 60, time.Minute); !ok {
		setRetryAfter(w, wait)
		oauthError(w, http.StatusTooManyRequests, "invalid_request", "too many token requests from this address; retry later")
		return
	}
	form, terr := readForm(r)
	if terr != nil {
		oauthError(w, http.StatusBadRequest, terr.code, terr.desc)
		return
	}
	s.maybePrune(r.Context())
	clientID := formClientID(r, form)
	if clientID == "" {
		oauthError(w, http.StatusUnauthorized, "invalid_client", "client_id is required")
		return
	}
	iss := s.issuer(r)
	var resp tokenResponse
	var err error
	switch form.Get("grant_type") {
	case "authorization_code":
		resp, err = s.exchangeCode(r.Context(), form, clientID, iss)
	case "refresh_token":
		resp, err = s.refresh(r.Context(), form, clientID, iss)
	case "":
		err = &tokenError{"invalid_request", "grant_type is required"}
	default:
		err = &tokenError{"unsupported_grant_type", "grant_type must be authorization_code or refresh_token"}
	}
	if err != nil {
		var te *tokenError
		if errors.As(err, &te) {
			oauthError(w, http.StatusBadRequest, te.code, te.desc)
			return
		}
		s.log.Error("token", "grant_type", form.Get("grant_type"), "err", err)
		oauthError(w, http.StatusInternalServerError, "server_error", "the bridge could not issue tokens; try again")
		return
	}
	w.Header().Set("Pragma", "no-cache")
	writeJSON(w, http.StatusOK, resp)
}

// resourceParam checks an optional resource at the token endpoint against
// what the grant is bound to, and returns the binding to keep.
func resourceParam(form url.Values, bound, iss string) (string, *tokenError) {
	vals := form["resource"]
	if len(vals) == 0 {
		return bound, nil
	}
	want := bound
	if want == "" {
		want = iss + ResourcePath
	}
	for _, v := range vals {
		if !sameResource(v, want) {
			return "", &tokenError{"invalid_target", "resource must be " + want}
		}
	}
	return want, nil
}

func pkceOK(verifier, challenge string) bool {
	if !verifierRe.MatchString(verifier) {
		return false
	}
	sum := sha256.Sum256([]byte(verifier))
	got := base64.RawURLEncoding.EncodeToString(sum[:])
	return subtle.ConstantTimeCompare([]byte(got), []byte(challenge)) == 1
}

// exchangeCode redeems an authorization code. The code is marked used in the
// same transaction that creates the grant; a second redemption fails and
// revokes everything issued from the first (RFC 6749 section 4.1.2).
func (s *Server) exchangeCode(ctx context.Context, form url.Values, clientID, iss string) (tokenResponse, error) {
	code := form.Get("code")
	verifier := form.Get("code_verifier")
	redirect := form.Get("redirect_uri")
	if code == "" || verifier == "" {
		return tokenResponse{}, &tokenError{"invalid_request", "code and code_verifier are required"}
	}
	h := hashOf(code)
	now := s.now()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return tokenResponse{}, err
	}
	defer tx.Rollback()
	// Write first, so the transaction holds the write lock from the start.
	res, err := tx.ExecContext(ctx, `UPDATE bridge_oauth_codes SET used_at = ? WHERE hash = ? AND used_at IS NULL AND expires_at > ?`, ms(now), h, ms(now))
	if err != nil {
		return tokenResponse{}, err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		_ = tx.Rollback()
		return tokenResponse{}, s.deadCode(ctx, h)
	}
	var c struct {
		clientID, clientName, redirect, challenge, scope, resource, ref string
		canSend                                                         bool
	}
	if err := tx.QueryRowContext(ctx, `SELECT client_id, client_name, redirect_uri, code_challenge, scope, resource, session_ref, can_send FROM bridge_oauth_codes WHERE hash = ?`, h).
		Scan(&c.clientID, &c.clientName, &c.redirect, &c.challenge, &c.scope, &c.resource, &c.ref, &c.canSend); err != nil {
		return tokenResponse{}, err
	}
	// Failures below roll back, so the code stays usable by its real owner.
	switch {
	case c.clientID != clientID:
		return tokenResponse{}, invalidGrant("the code was issued to another client")
	case redirect != c.redirect:
		return tokenResponse{}, invalidGrant("redirect_uri does not match the authorization request")
	case !pkceOK(verifier, c.challenge):
		return tokenResponse{}, invalidGrant("code_verifier does not match the code_challenge")
	}
	resource, terr := resourceParam(form, c.resource, iss)
	if terr != nil {
		return tokenResponse{}, terr
	}
	grantID := "oa_" + randomHex(8)
	if _, err := tx.ExecContext(ctx, `INSERT INTO bridge_oauth_grants (id, client_id, client_name, scope, resource, session_ref, created_at, expires_at, can_send)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`, grantID, c.clientID, c.clientName, c.scope, resource, c.ref, ms(now), ms(now.Add(refreshTTL)), c.canSend); err != nil {
		return tokenResponse{}, err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE bridge_oauth_codes SET grant_id = ? WHERE hash = ?`, grantID, h); err != nil {
		return tokenResponse{}, err
	}
	resp, err := s.issueTokens(ctx, tx, grantID, c.clientID, c.scope, resource, now, false)
	if err != nil {
		return tokenResponse{}, err
	}
	if err := tx.Commit(); err != nil {
		return tokenResponse{}, err
	}
	s.audit("oauth.token_issued", "grant="+grantID+" client="+c.clientID+" name="+quoteName(c.clientName))
	return resp, nil
}

// deadCode explains why a code could not be redeemed, revoking the grant it
// produced when it is being replayed.
func (s *Server) deadCode(ctx context.Context, h []byte) error {
	var used sql.NullInt64
	var grantID sql.NullString
	err := s.db.QueryRowContext(ctx, `SELECT used_at, grant_id FROM bridge_oauth_codes WHERE hash = ?`, h).Scan(&used, &grantID)
	if errors.Is(err, sql.ErrNoRows) {
		return invalidGrant("unknown authorization code")
	}
	if err != nil {
		return err
	}
	if used.Valid {
		if grantID.Valid {
			if _, err := s.revokeGrant(ctx, grantID.String); err != nil {
				return err
			}
			s.audit("oauth.code_reuse", "grant="+grantID.String+" revoked=family")
		}
		return invalidGrant("the authorization code was already used")
	}
	return invalidGrant("the authorization code has expired")
}

// refresh rotates a refresh token. Presenting a refresh token that was
// already rotated revokes the whole family (OAuth 2.1 section 4.3.1).
func (s *Server) refresh(ctx context.Context, form url.Values, clientID, iss string) (tokenResponse, error) {
	rt := form.Get("refresh_token")
	if rt == "" {
		return tokenResponse{}, &tokenError{"invalid_request", "refresh_token is required"}
	}
	if !validScope(form.Get("scope")) {
		return tokenResponse{}, &tokenError{"invalid_scope", `the only scope is "mcp"`}
	}
	if !strings.HasPrefix(rt, RefreshTokenPrefix) {
		return tokenResponse{}, invalidGrant("unknown refresh token")
	}
	h := hashOf(rt)
	now := s.now()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return tokenResponse{}, err
	}
	defer tx.Rollback()
	res, err := tx.ExecContext(ctx, `UPDATE bridge_oauth_tokens SET used_at = ?, last_used_at = ?
		WHERE hash = ? AND kind = 'refresh' AND used_at IS NULL AND revoked_at IS NULL AND expires_at > ?`, ms(now), ms(now), h, ms(now))
	if err != nil {
		return tokenResponse{}, err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		_ = tx.Rollback()
		return tokenResponse{}, s.deadRefresh(ctx, h)
	}
	var t struct {
		grantID, clientID, scope, resource, name string
		revoked                                  sql.NullInt64
	}
	if err := tx.QueryRowContext(ctx, `SELECT t.grant_id, t.client_id, t.scope, t.resource, g.client_name, g.revoked_at
		FROM bridge_oauth_tokens t JOIN bridge_oauth_grants g ON g.id = t.grant_id WHERE t.hash = ?`, h).
		Scan(&t.grantID, &t.clientID, &t.scope, &t.resource, &t.name, &t.revoked); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return tokenResponse{}, invalidGrant("unknown refresh token")
		}
		return tokenResponse{}, err
	}
	switch {
	case t.revoked.Valid:
		return tokenResponse{}, invalidGrant("access was revoked")
	case t.clientID != clientID:
		return tokenResponse{}, invalidGrant("the refresh token was issued to another client")
	}
	resource, terr := resourceParam(form, t.resource, iss)
	if terr != nil {
		return tokenResponse{}, terr
	}
	resp, err := s.issueTokens(ctx, tx, t.grantID, t.clientID, t.scope, resource, now, true)
	if err != nil {
		return tokenResponse{}, err
	}
	if err := tx.Commit(); err != nil {
		return tokenResponse{}, err
	}
	s.audit("oauth.refresh", "grant="+t.grantID+" client="+t.clientID)
	return resp, nil
}

func (s *Server) deadRefresh(ctx context.Context, h []byte) error {
	var grantID string
	var used, revoked sql.NullInt64
	err := s.db.QueryRowContext(ctx, `SELECT grant_id, used_at, revoked_at FROM bridge_oauth_tokens WHERE hash = ? AND kind = 'refresh'`, h).
		Scan(&grantID, &used, &revoked)
	if errors.Is(err, sql.ErrNoRows) {
		return invalidGrant("unknown refresh token")
	}
	if err != nil {
		return err
	}
	if used.Valid && !revoked.Valid {
		if _, err := s.revokeGrant(ctx, grantID); err != nil {
			return err
		}
		s.audit("oauth.refresh_reuse", "grant="+grantID+" revoked=family")
		return invalidGrant("the refresh token was already used; access has been revoked, sign in again")
	}
	if revoked.Valid {
		return invalidGrant("access was revoked")
	}
	return invalidGrant("the refresh token has expired")
}

// issueTokens mints an access and a refresh token in grant's family and
// extends the grant. A refresh counts as use; the first issue does not.
func (s *Server) issueTokens(ctx context.Context, tx *sql.Tx, grantID, clientID, scope, resource string, now time.Time, used bool) (tokenResponse, error) {
	at := AccessTokenPrefix + randomToken()
	rt := RefreshTokenPrefix + randomToken()
	const q = `INSERT INTO bridge_oauth_tokens (hash, kind, grant_id, client_id, scope, resource, created_at, expires_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?)`
	if _, err := tx.ExecContext(ctx, q, hashOf(at), "access", grantID, clientID, scope, resource, ms(now), ms(now.Add(accessTTL))); err != nil {
		return tokenResponse{}, err
	}
	if _, err := tx.ExecContext(ctx, q, hashOf(rt), "refresh", grantID, clientID, scope, resource, ms(now), ms(now.Add(refreshTTL))); err != nil {
		return tokenResponse{}, err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE bridge_oauth_grants SET expires_at = ?, last_used_at = CASE WHEN ? THEN ? ELSE last_used_at END WHERE id = ?`,
		ms(now.Add(refreshTTL)), used, ms(now), grantID); err != nil {
		return tokenResponse{}, err
	}
	return tokenResponse{AccessToken: at, TokenType: "Bearer", ExpiresIn: int(accessTTL.Seconds()), Scope: scope, RefreshToken: rt}, nil
}

// revokeGrant revokes a grant and every token of its family.
func (s *Server) revokeGrant(ctx context.Context, grantID string) (bool, error) {
	now := ms(s.now())
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	res, err := tx.ExecContext(ctx, `UPDATE bridge_oauth_grants SET revoked_at = ? WHERE id = ? AND revoked_at IS NULL`, now, grantID)
	if err != nil {
		return false, err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE bridge_oauth_tokens SET revoked_at = ? WHERE grant_id = ? AND revoked_at IS NULL`, now, grantID); err != nil {
		return false, err
	}
	if err := tx.Commit(); err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

// ---- revocation (RFC 7009) -----------------------------------------------------------

func (s *Server) revoke(w http.ResponseWriter, r *http.Request) {
	if corsPreflight(w, r, "POST, OPTIONS") {
		return
	}
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST, OPTIONS")
		oauthError(w, http.StatusMethodNotAllowed, "invalid_request", "method not allowed")
		return
	}
	if ok, wait := s.limited(r, "revoke", 60, time.Minute); !ok {
		setRetryAfter(w, wait)
		oauthError(w, http.StatusTooManyRequests, "invalid_request", "too many requests; retry later")
		return
	}
	form, terr := readForm(r)
	if terr != nil {
		oauthError(w, http.StatusBadRequest, terr.code, terr.desc)
		return
	}
	tok := form.Get("token")
	if tok == "" {
		oauthError(w, http.StatusBadRequest, "invalid_request", "token is required")
		return
	}
	var grantID, owner string
	err := s.db.QueryRowContext(r.Context(), `SELECT grant_id, client_id FROM bridge_oauth_tokens WHERE hash = ?`, hashOf(tok)).Scan(&grantID, &owner)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		// RFC 7009 section 2.2: unknown tokens are not an error.
		w.WriteHeader(http.StatusOK)
		return
	case err != nil:
		s.log.Error("revoke lookup", "err", err)
		oauthError(w, http.StatusInternalServerError, "server_error", "revocation failed; try again")
		return
	}
	if id := formClientID(r, form); id != "" && id != owner {
		oauthError(w, http.StatusUnauthorized, "invalid_client", "the token was issued to another client")
		return
	}
	revoked, err := s.revokeGrant(r.Context(), grantID)
	if err != nil {
		s.log.Error("revoke", "err", err)
		oauthError(w, http.StatusInternalServerError, "server_error", "revocation failed; try again")
		return
	}
	if revoked {
		s.audit("oauth.revoke", "grant="+grantID+" client="+owner+" via=endpoint")
	}
	w.WriteHeader(http.StatusOK)
}

// ---- token verification for /mcp -------------------------------------------------------

// Verifier returns an auth.Verifier for the MCP endpoint: OAuth access
// tokens (wb_at_...) are checked here, everything else by next (bearer keys).
// An access token resolves to KeyInfo{ID: grant id, Label: client name,
// Kind: mcp, CanSend: the grant's consent, OAuth: true}, so rate limits and
// the audit log are per grant.
func (s *Server) Verifier(next auth.Verifier) auth.Verifier {
	return verifier{s: s, next: next}
}

type verifier struct {
	s    *Server
	next auth.Verifier
}

func (v verifier) VerifyKey(ctx context.Context, bearer string) (auth.KeyInfo, error) {
	tok := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(bearer), "Bearer "))
	if strings.HasPrefix(tok, AccessTokenPrefix) {
		return v.s.VerifyAccessToken(ctx, tok)
	}
	if v.next == nil {
		return auth.KeyInfo{}, auth.ErrInvalidKey
	}
	return v.next.VerifyKey(ctx, bearer)
}

// VerifyAccessToken checks an access token. Unknown, expired and revoked
// tokens, and tokens bound to another resource, give auth.ErrInvalidKey.
func (s *Server) VerifyAccessToken(ctx context.Context, token string) (auth.KeyInfo, error) {
	if !strings.HasPrefix(token, AccessTokenPrefix) || len(token) != len(AccessTokenPrefix)+43 {
		return auth.KeyInfo{}, auth.ErrInvalidKey
	}
	var t struct {
		grantID, resource, name string
		expires, created        int64
		revoked, gRevoked, used sql.NullInt64
		canSend                 bool
	}
	err := s.db.QueryRowContext(ctx, `SELECT t.grant_id, t.resource, t.expires_at, t.revoked_at, t.last_used_at, g.client_name, g.created_at, g.revoked_at, g.can_send
		FROM bridge_oauth_tokens t JOIN bridge_oauth_grants g ON g.id = t.grant_id WHERE t.hash = ? AND t.kind = 'access'`, hashOf(token)).
		Scan(&t.grantID, &t.resource, &t.expires, &t.revoked, &t.used, &t.name, &t.created, &t.gRevoked, &t.canSend)
	if errors.Is(err, sql.ErrNoRows) {
		return auth.KeyInfo{}, auth.ErrInvalidKey
	}
	if err != nil {
		return auth.KeyInfo{}, err
	}
	now := s.now()
	if t.revoked.Valid || t.gRevoked.Valid || ms(now) >= t.expires || !s.resourceBound(t.resource) {
		return auth.KeyInfo{}, auth.ErrInvalidKey
	}
	if !t.used.Valid || now.Sub(fromMS(t.used.Int64)) >= touchEvery {
		_, _ = s.db.ExecContext(ctx, `UPDATE bridge_oauth_tokens SET last_used_at = ? WHERE hash = ?`, ms(now), hashOf(token))
		_, _ = s.db.ExecContext(ctx, `UPDATE bridge_oauth_grants SET last_used_at = ? WHERE id = ?`, ms(now), t.grantID)
	}
	return auth.KeyInfo{ID: t.grantID, Label: t.name, Kind: auth.KindMCP, CreatedAt: fromMS(t.created), LastUsedAt: now, CanSend: t.canSend, OAuth: true}, nil
}

// resourceBound checks the audience: a token bound to a resource is valid
// only for this server's /mcp. Without a configured issuer (local
// development) the origin is not known here, so only the path is checked.
func (s *Server) resourceBound(resource string) bool {
	if resource == "" {
		return true
	}
	if s.publicURL != "" {
		return sameResource(resource, s.publicURL+ResourcePath)
	}
	u, err := url.Parse(resource)
	return err == nil && strings.TrimSuffix(u.Path, "/") == ResourcePath
}
