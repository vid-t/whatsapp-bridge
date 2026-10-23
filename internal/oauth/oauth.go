// Package oauth is the bridge's OAuth 2.1 authorization server for MCP
// clients (claude.ai, Claude Code, Codex, ChatGPT). It lets an assistant
// connect with a click instead of a pasted key:
//
//  1. The client calls /mcp without a token and gets a 401 whose
//     WWW-Authenticate names the protected-resource metadata (internal/mcp).
//     That document lists this bridge's origin as the authorization server.
//  2. The client reads /.well-known/oauth-authorization-server (RFC 8414),
//     then either registers itself (RFC 7591, /oauth/register) or uses an
//     https URL as its client_id (Client ID Metadata Document, CIMD).
//  3. The user's browser opens /oauth/authorize, signs in (access code,
//     email link, Google or Microsoft) if it has no bridge session, and
//     approves read-only access on a consent page.
//  4. The client exchanges the single-use code (PKCE S256 only) at
//     /oauth/token for an opaque access token (wb_at_, 1 hour) and a rotating
//     refresh token (wb_rt_, 30 days), and calls /mcp with the access token.
//
// Only public clients exist: no client secrets are issued or checked. Tokens
// and codes are 32 random bytes and stored only as SHA-256 hashes. One
// approval is one grant; its tokens form a family that is revoked as a whole
// on refresh-token reuse, code reuse, /oauth/revoke or the grants API.
package oauth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/vid-t/whatsapp-bridge/internal/auth"
)

// Routes served by this package. Paths returns the patterns main mounts.
const (
	MetadataPath  = "/.well-known/oauth-authorization-server"
	AuthorizePath = "/oauth/authorize"
	TokenPath     = "/oauth/token"
	RegisterPath  = "/oauth/register"
	RevokePath    = "/oauth/revoke"
	StaticPath    = "/oauth/static/"
	// GrantsPath is mounted on the web layer's mux through RegisterAPI.
	GrantsPath = "/api/oauth/grants"

	// Scope is the only scope: read-only use of the MCP tools.
	Scope = "mcp"
	// ResourcePath is the protected resource, relative to the issuer.
	ResourcePath = "/mcp"

	AccessTokenPrefix  = "wb_at_"
	RefreshTokenPrefix = "wb_rt_"
)

const (
	codeTTL      = 10 * time.Minute
	accessTTL    = time.Hour
	refreshTTL   = 30 * 24 * time.Hour
	clientTTL    = 24 * time.Hour // registered clients that never obtained a grant
	deadGrantTTL = 30 * 24 * time.Hour
	cimdTTL      = time.Hour
	cimdFailTTL  = time.Minute
	cimdTimeout  = 5 * time.Second
	cimdMaxBytes = 64 << 10
	maxBody      = 64 << 10
	pruneEvery   = 10 * time.Minute
	touchEvery   = time.Minute
	tokenBytes   = 32
	maxParamLen  = 2048

	jsonCSP = "default-src 'none'; frame-ancestors 'none'; base-uri 'none'"
)

var schema = `
CREATE TABLE IF NOT EXISTS bridge_oauth_clients (
  client_id     TEXT PRIMARY KEY,
  client_name   TEXT NOT NULL DEFAULT '',
  redirect_uris TEXT NOT NULL,
  created_at    INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS bridge_oauth_codes (
  hash           BLOB PRIMARY KEY,
  client_id      TEXT NOT NULL,
  client_name    TEXT NOT NULL DEFAULT '',
  redirect_uri   TEXT NOT NULL,
  code_challenge TEXT NOT NULL,
  scope          TEXT NOT NULL,
  resource       TEXT NOT NULL DEFAULT '',
  session_ref    TEXT NOT NULL DEFAULT '',
  created_at     INTEGER NOT NULL,
  expires_at     INTEGER NOT NULL,
  used_at        INTEGER,
  grant_id       TEXT
);
CREATE TABLE IF NOT EXISTS bridge_oauth_grants (
  id           TEXT PRIMARY KEY,
  client_id    TEXT NOT NULL,
  client_name  TEXT NOT NULL DEFAULT '',
  scope        TEXT NOT NULL,
  resource     TEXT NOT NULL DEFAULT '',
  session_ref  TEXT NOT NULL DEFAULT '',
  created_at   INTEGER NOT NULL,
  expires_at   INTEGER NOT NULL,
  last_used_at INTEGER,
  revoked_at   INTEGER
);
CREATE INDEX IF NOT EXISTS bridge_oauth_grants_client ON bridge_oauth_grants(client_id);
CREATE TABLE IF NOT EXISTS bridge_oauth_tokens (
  hash         BLOB PRIMARY KEY,
  kind         TEXT NOT NULL,
  grant_id     TEXT NOT NULL,
  client_id    TEXT NOT NULL,
  scope        TEXT NOT NULL,
  resource     TEXT NOT NULL DEFAULT '',
  created_at   INTEGER NOT NULL,
  expires_at   INTEGER NOT NULL,
  last_used_at INTEGER,
  used_at      INTEGER,
  revoked_at   INTEGER
);
CREATE INDEX IF NOT EXISTS bridge_oauth_tokens_grant ON bridge_oauth_tokens(grant_id);
`

// Deps are the bridge services the authorization server uses.
type Deps struct {
	// DB is the bridge database; the oauth tables are created on Register.
	DB *sql.DB
	// Auth provides the access-code sign-in and UI sessions. In open mode
	// the sign-in step is skipped but consent is still asked.
	Auth *auth.Store
	Log  *slog.Logger
	// Audit receives events (type, detail). Details never contain tokens,
	// codes or access codes.
	Audit func(typ, detail string)
}

// Options configures the authorization server.
type Options struct {
	// PublicURL is the issuer, e.g. "https://bridge.example.org". It must be
	// https, or http on a loopback address. Empty means the issuer is derived
	// from each request's Host (local development only).
	PublicURL string
	// SignInProviders are the sign-in methods enabled for the bridge besides
	// the access code, in display order: "email" (internal/members) adds an
	// "Email me a sign-in link" form, "google" and "microsoft"
	// (internal/sso) a "Continue with ..." link. Each returns to the pending
	// authorization request after sign-in.
	SignInProviders []string

	// Tests.
	now       func() time.Time
	cimdRoots *x509.CertPool        // trust these roots for CIMD fetches
	cimdAllow func(netip.Addr) bool // replaces the public-address check
}

// Server is the authorization server. Create it with Register.
type Server struct {
	db        *sql.DB
	auth      *auth.Store
	log       *slog.Logger
	auditFn   func(typ, detail string)
	publicURL string
	now       func() time.Time
	limiter   *auth.Limiter
	cop       *http.CrossOriginProtection
	cimd      *cimdFetcher
	pages     *template.Template
	css       asset
	providers []string
	lastPrune atomic.Int64
}

// Paths are the patterns main must route to the mux passed to Register.
func Paths() []string {
	return []string{"/oauth/", MetadataPath, MetadataPath + "/"}
}

// Register creates the tables and mounts the OAuth endpoints on mux. The
// returned Server supplies the token verifier for /mcp and the grants API.
func Register(mux *http.ServeMux, deps Deps, opts Options) (*Server, error) {
	s, err := newServer(deps, opts)
	if err != nil {
		return nil, err
	}
	mux.Handle(MetadataPath, s.wrap(s.metadata))
	mux.Handle(MetadataPath+ResourcePath, s.wrap(s.metadata))
	mux.Handle(RegisterPath, s.wrap(s.register))
	mux.Handle(AuthorizePath, s.wrap(s.authorize))
	mux.Handle(TokenPath, s.wrap(s.token))
	mux.Handle(RevokePath, s.wrap(s.revoke))
	mux.Handle("GET "+StaticPath+"{file}", s.wrap(s.static))
	return s, nil
}

func newServer(deps Deps, opts Options) (*Server, error) {
	if deps.DB == nil || deps.Auth == nil {
		return nil, errors.New("oauth: DB and Auth are required")
	}
	issuer, err := normalizePublicURL(opts.PublicURL)
	if err != nil {
		return nil, err
	}
	if _, err := deps.DB.ExecContext(context.Background(), schema); err != nil {
		return nil, fmt.Errorf("oauth: schema: %w", err)
	}
	log := deps.Log
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	now := opts.now
	if now == nil {
		now = func() time.Time { return time.Now().UTC() }
	}
	pages, css, err := loadPages()
	if err != nil {
		return nil, err
	}
	s := &Server{
		db:        deps.DB,
		auth:      deps.Auth,
		log:       log.With("module", "oauth"),
		auditFn:   deps.Audit,
		publicURL: issuer,
		now:       now,
		limiter:   auth.NewLimiter(),
		cop:       http.NewCrossOriginProtection(),
		pages:     pages,
		css:       css,
		providers: opts.SignInProviders,
	}
	allow := opts.cimdAllow
	if allow == nil {
		allow = publicAddr
	}
	s.cimd = &cimdFetcher{client: newCIMDClient(allow, opts.cimdRoots), allow: allow, now: now, cache: map[string]cimdEntry{}}
	return s, nil
}

// normalizePublicURL validates the configured issuer. It is trimmed the same
// way internal/mcp trims PublicURL, so both name the same origin.
func normalizePublicURL(raw string) (string, error) {
	raw = strings.TrimRight(strings.TrimSpace(raw), "/")
	if raw == "" {
		return "", nil
	}
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" || u.User != nil || u.Path != "" || u.RawQuery != "" || u.Fragment != "" {
		return "", fmt.Errorf("oauth: public URL must be an origin such as https://bridge.example.ch, got %q", raw)
	}
	switch u.Scheme {
	case "https":
	case "http":
		if !isLoopbackHost(u.Hostname()) {
			return "", fmt.Errorf("oauth: the issuer must use https; plain http is allowed only on a loopback address (got %q)", raw)
		}
	default:
		return "", fmt.Errorf("oauth: public URL must use https, got %q", raw)
	}
	return raw, nil
}

// issuer is the public origin for this request, or "" when the Host header is
// unusable. It mirrors internal/mcp's baseURL so the protected-resource
// metadata and this server's metadata name the same issuer.
func (s *Server) issuer(r *http.Request) string {
	if s.publicURL != "" {
		return s.publicURL
	}
	host := r.Host
	if host == "" || strings.ContainsAny(host, "\"\\ /\t\r\n") {
		return ""
	}
	scheme := "http"
	if r.TLS != nil || strings.EqualFold(strings.TrimSpace(strings.Split(r.Header.Get("X-Forwarded-Proto"), ",")[0]), "https") {
		scheme = "https"
	}
	return scheme + "://" + host
}

// ---- metadata (RFC 8414) --------------------------------------------------------

type serverMetadata struct {
	Issuer                                 string   `json:"issuer"`
	AuthorizationEndpoint                  string   `json:"authorization_endpoint"`
	TokenEndpoint                          string   `json:"token_endpoint"`
	RegistrationEndpoint                   string   `json:"registration_endpoint"`
	RevocationEndpoint                     string   `json:"revocation_endpoint"`
	ScopesSupported                        []string `json:"scopes_supported"`
	ResponseTypesSupported                 []string `json:"response_types_supported"`
	ResponseModesSupported                 []string `json:"response_modes_supported"`
	GrantTypesSupported                    []string `json:"grant_types_supported"`
	TokenEndpointAuthMethodsSupported      []string `json:"token_endpoint_auth_methods_supported"`
	RevocationEndpointAuthMethodsSupported []string `json:"revocation_endpoint_auth_methods_supported"`
	CodeChallengeMethodsSupported          []string `json:"code_challenge_methods_supported"`
	ClientIDMetadataDocumentSupported      bool     `json:"client_id_metadata_document_supported"`
	AuthorizationResponseIssParameter      bool     `json:"authorization_response_iss_parameter_supported"`
}

func (s *Server) metadata(w http.ResponseWriter, r *http.Request) {
	if corsPreflight(w, r, "GET, HEAD, OPTIONS") {
		return
	}
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD, OPTIONS")
		oauthError(w, http.StatusMethodNotAllowed, "invalid_request", "method not allowed")
		return
	}
	iss := s.issuer(r)
	if iss == "" {
		oauthError(w, http.StatusBadRequest, "invalid_request", "unexpected Host header")
		return
	}
	w.Header().Set("Cache-Control", "public, max-age=300")
	writeJSON(w, http.StatusOK, serverMetadata{
		Issuer:                                 iss,
		AuthorizationEndpoint:                  iss + AuthorizePath,
		TokenEndpoint:                          iss + TokenPath,
		RegistrationEndpoint:                   iss + RegisterPath,
		RevocationEndpoint:                     iss + RevokePath,
		ScopesSupported:                        []string{Scope},
		ResponseTypesSupported:                 []string{"code"},
		ResponseModesSupported:                 []string{"query"},
		GrantTypesSupported:                    []string{"authorization_code", "refresh_token"},
		TokenEndpointAuthMethodsSupported:      []string{"none"},
		RevocationEndpointAuthMethodsSupported: []string{"none"},
		CodeChallengeMethodsSupported:          []string{"S256"},
		ClientIDMetadataDocumentSupported:      true,
		AuthorizationResponseIssParameter:      true,
	})
}

// ---- HTTP helpers ----------------------------------------------------------------

// wrap sets the headers every OAuth response carries and caps the body. Pages
// replace the CSP with their own (see pageCSP).
func (s *Server) wrap(next http.HandlerFunc) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Referrer-Policy", "no-referrer")
		h.Set("X-Frame-Options", "DENY")
		h.Set("Content-Security-Policy", jsonCSP)
		h.Set("Cache-Control", "no-store")
		r.Body = http.MaxBytesReader(w, r.Body, maxBody)
		next(w, r)
	})
}

// corsPreflight opens the JSON endpoints to browser-based clients. None of
// them use cookies, so any origin may call them, never with credentials.
func corsPreflight(w http.ResponseWriter, r *http.Request, methods string) bool {
	h := w.Header()
	h.Set("Access-Control-Allow-Origin", "*")
	if r.Method != http.MethodOptions {
		return false
	}
	h.Set("Access-Control-Allow-Methods", methods)
	h.Set("Access-Control-Allow-Headers", "Authorization, Content-Type, MCP-Protocol-Version")
	h.Set("Access-Control-Max-Age", "600")
	w.WriteHeader(http.StatusNoContent)
	return true
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// oauthError writes an RFC 6749 section 5.2 error body.
func oauthError(w http.ResponseWriter, status int, code, desc string) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Pragma", "no-cache")
	writeJSON(w, status, map[string]string{"error": code, "error_description": desc})
}

// apiError writes the bridge API's error shape ({"error": "..."}).
func apiError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}

// limited applies a per-IP limit and reports the wait when denied.
func (s *Server) limited(r *http.Request, bucket string, limit int, window time.Duration) (bool, time.Duration) {
	return s.limiter.Allow(bucket+":"+clientIP(r), limit, window)
}

func setRetryAfter(w http.ResponseWriter, wait time.Duration) {
	secs := int(wait.Seconds()) + 1
	w.Header().Set("Retry-After", strconv.Itoa(secs))
}

// clientIP returns the peer address, honouring the first X-Forwarded-For
// entry only when the request arrived on loopback (from the reverse proxy).
// Same rule as internal/web.
func clientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	if ip := net.ParseIP(host); ip != nil && ip.IsLoopback() {
		if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
			first, _, _ := strings.Cut(xff, ",")
			if f := strings.TrimSpace(first); f != "" {
				return f
			}
		}
	}
	return host
}

func secureRequest(r *http.Request) bool {
	return r.TLS != nil || strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https")
}

func (s *Server) audit(typ, detail string) {
	if s.auditFn != nil {
		s.auditFn(typ, detail)
	}
}

// quoteName renders a client-supplied name safely for an audit line.
func quoteName(name string) string {
	if len(name) > 60 {
		name = name[:60]
	}
	return strconv.Quote(name)
}

// ---- small utilities ---------------------------------------------------------------

func randomToken() string {
	b := make([]byte, tokenBytes)
	_, _ = rand.Read(b) // never fails (Go 1.24+)
	return base64.RawURLEncoding.EncodeToString(b)
}

func randomHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

func hashOf(secret string) []byte {
	h := sha256.Sum256([]byte(secret))
	return h[:]
}

func ms(t time.Time) int64 { return t.UnixMilli() }

func fromMS(v int64) time.Time { return time.UnixMilli(v).UTC() }

func isLoopbackHost(h string) bool {
	switch strings.ToLower(strings.Trim(h, "[]")) {
	case "localhost", "127.0.0.1", "::1":
		return true
	}
	return false
}

// maybePrune deletes stale rows at most once per pruneEvery.
func (s *Server) maybePrune(ctx context.Context) {
	now := s.now()
	last := s.lastPrune.Load()
	if last != 0 && now.Sub(time.UnixMilli(last)) < pruneEvery {
		return
	}
	if !s.lastPrune.CompareAndSwap(last, ms(now)) {
		return
	}
	if err := s.prune(ctx, now); err != nil {
		s.log.Warn("prune", "err", err)
	}
}

// prune removes expired codes and tokens, grants dead for 30 days, and
// registered clients that never obtained a grant within 24 hours.
func (s *Server) prune(ctx context.Context, now time.Time) error {
	n := ms(now)
	stmts := []struct {
		q    string
		args []any
	}{
		{`DELETE FROM bridge_oauth_codes WHERE expires_at < ?`, []any{n - int64(time.Hour/time.Millisecond)}},
		{`DELETE FROM bridge_oauth_tokens WHERE expires_at < ?`, []any{n - int64(24*time.Hour/time.Millisecond)}},
		{`DELETE FROM bridge_oauth_tokens WHERE grant_id IN (SELECT id FROM bridge_oauth_grants WHERE revoked_at < ?)`, []any{n - deadGrantTTL.Milliseconds()}},
		{`DELETE FROM bridge_oauth_grants WHERE revoked_at < ? OR expires_at < ?`, []any{n - deadGrantTTL.Milliseconds(), n - deadGrantTTL.Milliseconds()}},
		{`DELETE FROM bridge_oauth_clients WHERE created_at < ? AND NOT EXISTS (SELECT 1 FROM bridge_oauth_grants g WHERE g.client_id = bridge_oauth_clients.client_id)`, []any{n - clientTTL.Milliseconds()}},
	}
	for _, st := range stmts {
		if _, err := s.db.ExecContext(ctx, st.q, st.args...); err != nil {
			return err
		}
	}
	return nil
}
