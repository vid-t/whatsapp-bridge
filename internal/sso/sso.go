// Package sso signs people in to the bridge with Google or Microsoft (OpenID
// Connect, authorization code with PKCE). It complements the access code,
// which stays the bootstrap and emergency path.
//
//	GET /auth/{provider}/start?next=/path  -> state, nonce and PKCE in a signed cookie, redirect to the provider
//	GET /auth/{provider}/callback          -> verify, admit, create the session, redirect to next (same origin only)
//
// Admission: the verified email must belong to an approved member, or its
// domain must be in BRIDGE_ALLOWED_DOMAINS. The first provider sign-in
// creates or updates the member and binds it to that provider account
// (provider + subject); later sign-ins for the same email from another
// account are refused. Members, email sign-in and access requests live in
// internal/members, which is mounted whether or not a provider is.
//
// Routes are mounted on the web layer's mux (web.Options.ExtraRoutes), so the
// Host allowlist, security headers and principal resolution apply.
package sso

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"

	"github.com/vid-t/whatsapp-bridge/internal/auth"
)

const (
	microsoftBase = "https://login.microsoftonline.com"
	googleIssuer  = "https://accounts.google.com"
	// consumersTenant is the tenant id of personal Microsoft accounts.
	consumersTenant = "9188040d-6c67-4c5b-b112-36a304b66dad"
	httpTimeout     = 10 * time.Second
)

var guidRe = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)

// Config is the provider configuration, normally from the environment
// (ConfigFromEnv). A provider is enabled only when both its client id and
// secret are set.
type Config struct {
	GoogleClientID        string
	GoogleClientSecret    string
	MicrosoftClientID     string
	MicrosoftClientSecret string
	// MicrosoftTenant is "common" (default: any work, school or personal
	// account), "organizations", "consumers", or a tenant id (GUID).
	MicrosoftTenant string
	// AllowedDomains admit any verified email of these domains.
	AllowedDomains []string
	// PublicURL is the bridge origin; redirect URIs are
	// <PublicURL>/auth/{google,microsoft}/callback. Empty: from the request.
	PublicURL string

	// Tests: replace the provider hosts.
	googleIssuer  string
	microsoftBase string
	httpClient    *http.Client
	now           func() time.Time
}

// ConfigFromEnv reads BRIDGE_GOOGLE_CLIENT_ID, BRIDGE_GOOGLE_CLIENT_SECRET,
// BRIDGE_MICROSOFT_CLIENT_ID, BRIDGE_MICROSOFT_CLIENT_SECRET,
// BRIDGE_MICROSOFT_TENANT and BRIDGE_ALLOWED_DOMAINS.
func ConfigFromEnv(getenv func(string) string, publicURL string) Config {
	var domains []string
	for _, d := range strings.Split(getenv("BRIDGE_ALLOWED_DOMAINS"), ",") {
		if d = strings.ToLower(strings.TrimPrefix(strings.TrimSpace(d), "@")); d != "" {
			domains = append(domains, d)
		}
	}
	return Config{
		GoogleClientID:        strings.TrimSpace(getenv("BRIDGE_GOOGLE_CLIENT_ID")),
		GoogleClientSecret:    strings.TrimSpace(getenv("BRIDGE_GOOGLE_CLIENT_SECRET")),
		MicrosoftClientID:     strings.TrimSpace(getenv("BRIDGE_MICROSOFT_CLIENT_ID")),
		MicrosoftClientSecret: strings.TrimSpace(getenv("BRIDGE_MICROSOFT_CLIENT_SECRET")),
		MicrosoftTenant:       strings.TrimSpace(getenv("BRIDGE_MICROSOFT_TENANT")),
		AllowedDomains:        domains,
		PublicURL:             publicURL,
	}
}

// Deps are the bridge services sign-in uses.
type Deps struct {
	Auth  *auth.Store
	Log   *slog.Logger
	Audit func(typ, detail string)
}

// Server handles provider sign-in.
type Server struct {
	auth      *auth.Store
	log       *slog.Logger
	auditFn   func(typ, detail string)
	publicURL string
	domains   map[string]bool
	providers []*provider // enabled, in display order
	byName    map[string]*provider
	client    *http.Client
	now       func() time.Time
	cookieKey []byte
	limiter   *auth.Limiter
}

// New validates the configuration. It does no network I/O: provider
// discovery happens on first use and is retried until it succeeds.
func New(deps Deps, cfg Config) (*Server, error) {
	if deps.Auth == nil {
		return nil, errors.New("sso: Auth is required")
	}
	log := deps.Log
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	s := &Server{
		auth:      deps.Auth,
		log:       log.With("module", "sso"),
		auditFn:   deps.Audit,
		publicURL: strings.TrimRight(strings.TrimSpace(cfg.PublicURL), "/"),
		domains:   map[string]bool{},
		byName:    map[string]*provider{},
		client:    cfg.httpClient,
		now:       cfg.now,
		cookieKey: make([]byte, 32),
		limiter:   auth.NewLimiter(),
	}
	_, _ = rand.Read(s.cookieKey) // pending sign-ins do not survive a restart
	if s.client == nil {
		s.client = &http.Client{Timeout: httpTimeout}
	}
	if s.now == nil {
		s.now = time.Now
	}
	for _, d := range cfg.AllowedDomains {
		if d = strings.ToLower(strings.TrimPrefix(strings.TrimSpace(d), "@")); d != "" {
			s.domains[d] = true
		}
	}
	if (cfg.GoogleClientID == "") != (cfg.GoogleClientSecret == "") {
		s.log.Warn("Google sign-in disabled: set both BRIDGE_GOOGLE_CLIENT_ID and BRIDGE_GOOGLE_CLIENT_SECRET")
	}
	if cfg.GoogleClientID != "" && cfg.GoogleClientSecret != "" {
		iss := cfg.googleIssuer
		if iss == "" {
			iss = googleIssuer
		}
		s.add(&provider{name: "google", label: "Google", clientID: cfg.GoogleClientID, secret: cfg.GoogleClientSecret,
			discovery: iss, email: googleEmail})
	}
	if (cfg.MicrosoftClientID == "") != (cfg.MicrosoftClientSecret == "") {
		s.log.Warn("Microsoft sign-in disabled: set both BRIDGE_MICROSOFT_CLIENT_ID and BRIDGE_MICROSOFT_CLIENT_SECRET")
	}
	if cfg.MicrosoftClientID != "" && cfg.MicrosoftClientSecret != "" {
		p, err := microsoftProvider(cfg)
		if err != nil {
			return nil, err
		}
		s.add(p)
	}
	return s, nil
}

func (s *Server) add(p *provider) {
	s.providers = append(s.providers, p)
	s.byName[p.name] = p
}

// Providers lists the enabled providers in display order ("google", "microsoft").
func (s *Server) Providers() []string {
	out := make([]string, 0, len(s.providers))
	for _, p := range s.providers {
		out = append(out, p.name)
	}
	return out
}

// RegisterRoutes mounts the sign-in routes on the web layer's inner mux.
func (s *Server) RegisterRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /auth/{provider}/start", s.start)
	mux.HandleFunc("GET /auth/{provider}/callback", s.callback)
}

// microsoftProvider configures Entra ID. For the multi-tenant endpoints
// (common, organizations, consumers) the ID token's issuer names the user's
// own tenant, so the library's issuer check is replaced by the documented
// multi-tenant validation: iss must equal
// https://login.microsoftonline.com/{tid}/v2.0 for the token's own tid claim.
func microsoftProvider(cfg Config) (*provider, error) {
	base := cfg.microsoftBase
	if base == "" {
		base = microsoftBase
	}
	tenant := strings.ToLower(cfg.MicrosoftTenant)
	if tenant == "" {
		tenant = "common"
	}
	p := &provider{name: "microsoft", label: "Microsoft", clientID: cfg.MicrosoftClientID, secret: cfg.MicrosoftClientSecret,
		discovery: base + "/" + tenant + "/v2.0", email: microsoftEmail}
	switch {
	case tenant == "common" || tenant == "organizations" || tenant == "consumers":
		p.issuerTemplate = base + "/{tenantid}/v2.0"
		p.checkTenant = func(tid string) error {
			switch {
			case tenant == "organizations" && tid == consumersTenant:
				return errors.New("personal Microsoft accounts are not accepted; use a work or school account")
			case tenant == "consumers" && tid != consumersTenant:
				return errors.New("only personal Microsoft accounts are accepted")
			}
			return nil
		}
	case guidRe.MatchString(tenant):
		p.checkTenant = func(tid string) error {
			if !strings.EqualFold(tid, tenant) {
				return errors.New("this account belongs to another Microsoft tenant")
			}
			return nil
		}
	default:
		return nil, fmt.Errorf("sso: BRIDGE_MICROSOFT_TENANT must be common, organizations, consumers or a tenant id (GUID), got %q", cfg.MicrosoftTenant)
	}
	return p, nil
}

// provider is one OpenID Connect identity provider.
type provider struct {
	name, label      string
	clientID, secret string
	discovery        string
	// issuerTemplate, when set, marks a multi-tenant issuer ("{tenantid}").
	issuerTemplate string
	checkTenant    func(tid string) error
	email          func(claims) (string, error)

	mu       sync.Mutex
	op       *oidc.Provider
	verifier *oidc.IDTokenVerifier
}

// load runs discovery once; failures are retried on the next sign-in.
func (p *provider) load(ctx context.Context, client *http.Client, now func() time.Time) (*oidc.Provider, *oidc.IDTokenVerifier, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.op != nil {
		return p.op, p.verifier, nil
	}
	ctx = oidc.ClientContext(ctx, client)
	if p.issuerTemplate != "" {
		ctx = oidc.InsecureIssuerURLContext(ctx, p.issuerTemplate)
	}
	op, err := oidc.NewProvider(ctx, p.discovery)
	if err != nil {
		return nil, nil, err
	}
	p.op = op
	p.verifier = op.Verifier(&oidc.Config{ClientID: p.clientID, SkipIssuerCheck: p.issuerTemplate != "", Now: now})
	return p.op, p.verifier, nil
}

// claims are the ID token claims sign-in reads.
type claims struct {
	Subject           string `json:"sub"`
	Issuer            string `json:"iss"`
	Email             string `json:"email"`
	EmailVerified     any    `json:"email_verified"` // bool, or "true" from some providers
	Name              string `json:"name"`
	TenantID          string `json:"tid"`
	PreferredUsername string `json:"preferred_username"`
	EmailDomainOwner  any    `json:"xms_edov"` // Entra optional claim: email domain owner verified
}

func truthy(v any) (bool, bool) {
	switch t := v.(type) {
	case bool:
		return t, true
	case string:
		return strings.EqualFold(t, "true"), true
	}
	return false, false
}

// googleEmail requires email_verified.
func googleEmail(c claims) (string, error) {
	e, ok := auth.NormalizeEmail(c.Email)
	if !ok {
		return "", errors.New("your Google account did not share an email address")
	}
	if v, _ := truthy(c.EmailVerified); !v {
		return "", errors.New("your Google account's email address is not verified")
	}
	return e, nil
}

// microsoftEmail uses the email claim, which Entra emits only for verified
// domain owners by default (and never when the optional xms_edov claim says
// false), else preferred_username when it is an address (the sign-in name,
// whose domain the tenant has verified).
func microsoftEmail(c claims) (string, error) {
	if e, ok := auth.NormalizeEmail(c.Email); ok {
		if v, present := truthy(c.EmailDomainOwner); !present || v {
			return e, nil
		}
	}
	if e, ok := auth.NormalizeEmail(c.PreferredUsername); ok {
		return e, nil
	}
	return "", errors.New("your Microsoft account did not share a usable email address")
}

// checkIssuer completes validation for multi-tenant Microsoft tokens.
func (p *provider) checkIssuer(c claims) error {
	if p.issuerTemplate == "" {
		if p.checkTenant != nil {
			return p.checkTenant(c.TenantID)
		}
		return nil
	}
	if !guidRe.MatchString(c.TenantID) {
		return errors.New("the token has no tenant id")
	}
	if c.Issuer != strings.Replace(p.issuerTemplate, "{tenantid}", c.TenantID, 1) {
		return errors.New("the token's issuer does not match its tenant")
	}
	return p.checkTenant(c.TenantID)
}

func (s *Server) audit(typ, detail string) {
	if s.auditFn != nil {
		s.auditFn(typ, detail)
	}
}

func randomString(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return base64.RawURLEncoding.EncodeToString(b)
}
