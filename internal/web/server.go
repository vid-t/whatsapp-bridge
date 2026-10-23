// Package web serves the bridge's HTTP UI and JSON API: connecting WhatsApp
// accounts (QR or phone pairing code) and managing the resulting sessions.
//
// Everything the browser needs is embedded in the binary. Every request
// passes through server.wrap, which sets the security headers and is the one
// place where authentication will be added later.
package web

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"go.mau.fi/whatsmeow/types"

	"github.com/vid-t/whatsapp-bridge/internal/auth"
	"github.com/vid-t/whatsapp-bridge/internal/wa"
)

const (
	maxBodyBytes = 1 << 20
	heartbeat    = 20 * time.Second
	writeTimeout = 10 * time.Second // per SSE write
	maxStreams   = 32               // concurrent SSE clients
	maxSteps     = 256              // cached pairing steps

	csp = "default-src 'self'; img-src 'self' data:; style-src 'self'; script-src 'self'; connect-src 'self'; " +
		"base-uri 'none'; form-action 'self'; frame-ancestors 'none'"
)

// manager is the part of *wa.Manager the web layer uses; tests substitute a fake.
type manager interface {
	Subscribe() (<-chan wa.Event, func())
	Sessions() []wa.SessionInfo
	Session(id string) (wa.SessionInfo, error)
	Chats(ctx context.Context, sessionID string) ([]wa.Chat, error)
	RecentMessages(ctx context.Context, sessionID, chatJID string, limit int) ([]wa.Message, error)
	RecentLogs(ctx context.Context, sessionID string, limit int) ([]wa.LogEntry, error)
	Reconnect(id string) error
	StartPairing(label string) (string, <-chan wa.PairEvent, error)
	PairWithCode(ctx context.Context, phone, label string) (string, string, error)
	SubmitPasskeyResponse(ctx context.Context, id string, resp *types.WebAuthnResponse) error
	ConfirmPasskey(ctx context.Context, id string) error
	CancelPairing(id string) error
	Logout(ctx context.Context, id string) error
	Remove(ctx context.Context, id string) error
	SetChatTicked(ctx context.Context, sessionID, chatJID string, ticked bool) (wa.Chat, error)
	SearchMessages(ctx context.Context, sessionID, chatJID, query string, limit int) ([]wa.SearchHit, error)
	MessageContext(ctx context.Context, sessionID, chatJID, messageID string, before, after int) ([]wa.Message, error)
}

var _ manager = (*wa.Manager)(nil)

// Options configures the web layer.
type Options struct {
	// AllowedHosts are Host header values accepted in addition to localhost,
	// 127.0.0.1 and [::1]. An entry without a port matches any port; an entry
	// with a port ("bridge.example.com:8443") must match exactly.
	AllowedHosts []string
	// Auth gates the UI and API. nil behaves like open mode (local use only).
	Auth *auth.Store
	// Version is reported by /api/me.
	Version string
	// Audit receives auth and admin events (type, detail). Never message text.
	Audit func(typ, detail string)
	// ExtraRoutes, when set, registers more handlers on the inner mux, so they
	// run behind the same middleware as the built-in routes (Host allowlist,
	// headers, cross-origin protection, principal resolution, access policy).
	// The OAuth grants API uses it.
	ExtraRoutes func(mux *http.ServeMux)
	// SignInProviders are the sign-in methods enabled besides the access
	// code, in display order ("email", "google", "microsoft"); /api/me lists
	// them after "code".
	SignInProviders []string
}

type server struct {
	mgr       manager
	log       *slog.Logger
	mux       *http.ServeMux
	hosts     hostSet
	assets    map[string]asset
	qr        qrCache
	auth      *auth.Store
	limiter   *auth.Limiter
	version   string
	auditFn   func(typ, detail string)
	providers []string

	stepMu sync.Mutex
	steps  map[string]cachedStep // latest screen step per pairing session

	streams      atomic.Int32
	shutdownOnce sync.Once
	done         chan struct{} // closed when the http.Server shuts down; ends SSE streams
}

// Register mounts the UI and API on mux with the default (loopback) hosts.
func Register(mux *http.ServeMux, mgr *wa.Manager, log *slog.Logger) {
	RegisterWithOptions(mux, mgr, log, Options{})
}

// RegisterWithOptions mounts the UI and API on mux. All routes live on an
// inner mux that is wrapped once, so middleware (auth later) applies to all.
func RegisterWithOptions(mux *http.ServeMux, mgr *wa.Manager, log *slog.Logger, opts Options) {
	mux.Handle("/", newServer(mgr, log, opts).handler())
}

func newServer(mgr manager, log *slog.Logger, opts Options) *server {
	s := &server{
		mgr:       mgr,
		log:       log.With("module", "web"),
		mux:       http.NewServeMux(),
		hosts:     newHostSet(opts.AllowedHosts),
		assets:    loadAssets(),
		qr:        qrCache{m: map[string]string{}},
		steps:     map[string]cachedStep{},
		done:      make(chan struct{}),
		auth:      opts.Auth,
		limiter:   auth.NewLimiter(),
		version:   opts.Version,
		auditFn:   opts.Audit,
		providers: opts.SignInProviders,
	}
	s.routes()
	if opts.ExtraRoutes != nil {
		opts.ExtraRoutes(s.mux)
	}
	events, _ := mgr.Subscribe() // never unsubscribed: lives as long as the process
	go s.trackPairing(events)
	return s
}

func (s *server) routes() {
	m := s.mux
	// Pages: one HTML shell for every app path (spaFallback); the script
	// picks the view from the path and renders its own not-found view.
	m.HandleFunc(spaPattern, s.spaFallback)
	m.HandleFunc("GET /sessions/{id}", s.sessionPage)
	m.HandleFunc("GET /static/{file}", s.static)

	// API.
	m.HandleFunc("GET /api/sessions", s.listSessions)
	m.HandleFunc("GET /api/sessions/{id}", s.getSession)
	m.HandleFunc("DELETE /api/sessions/{id}", s.removeSession)
	m.HandleFunc("POST /api/sessions/pair", s.limited("pair", 10, time.Minute, s.startPairQR))
	m.HandleFunc("POST /api/sessions/pair-code", s.limited("pair", 10, time.Minute, s.startPairCode))
	m.HandleFunc("GET /api/sessions/{id}/pairing", s.pairingState)
	m.HandleFunc("POST /api/sessions/{id}/cancel", s.cancelPairing)
	m.HandleFunc("POST /api/sessions/{id}/reconnect", s.reconnect)
	m.HandleFunc("POST /api/sessions/{id}/logout", s.logout)
	m.HandleFunc("POST /api/sessions/{id}/passkey/confirm", s.passkeyConfirm)
	m.HandleFunc("POST /api/sessions/{id}/passkey/response", s.passkeyResponse)
	m.HandleFunc("GET /api/sessions/{id}/chats", s.listChats)
	m.HandleFunc("GET /api/sessions/{id}/chats/{jid}/messages", s.listMessages)
	m.HandleFunc("GET /api/sessions/{id}/logs", s.sessionLogs)
	m.HandleFunc("GET /api/logs", s.allLogs)
	m.HandleFunc("GET /api/events", s.events)

	// Auth, keys, chat selection, context and search .
	m.HandleFunc("POST /auth/login", s.login)
	m.HandleFunc("POST /auth/logout", s.logoutUI)
	m.HandleFunc("GET /api/me", s.me)
	m.HandleFunc("GET /api/keys", s.listKeys)
	m.HandleFunc("POST /api/keys", s.createKey)
	m.HandleFunc("DELETE /api/keys/{id}", s.revokeKey)
	m.HandleFunc("PATCH /api/sessions/{id}/chats/{jid}", s.setChatTicked)
	m.HandleFunc("GET /api/sessions/{id}/chats/{jid}/messages/{mid}/context", s.messageContext)
	m.HandleFunc("GET /api/search", s.search)
}

// limited wraps a handler with a per-IP rate limit.
func (s *server) limited(bucket string, limit int, window time.Duration, next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if ok, wait := s.limiter.Allow(bucket+":"+clientIP(r), limit, window); !ok {
			retryAfter(w, wait)
			writeError(w, http.StatusTooManyRequests, "too many requests; try again shortly")
			return
		}
		next(w, r)
	}
}

// authorized applies the access policy: the shell and static files are
// public, /api/* needs a principal, and bearer keys may only read (GET, HEAD)
// and never manage keys.
func (s *server) authorized(r *http.Request, p auth.Principal) bool {
	path := r.URL.Path
	switch {
	case path == "/auth/login", path == "/auth/logout", path == "/api/me":
		return true
	case strings.HasPrefix(path, "/static/"), strings.HasPrefix(path, "/assets/"):
		return true
	case !strings.HasPrefix(path, "/api/"):
		return true // pages: the shell itself is public; data is not
	}
	if !p.Authenticated() {
		return false
	}
	if p.Kind == auth.PrincipalKey {
		if strings.HasPrefix(path, "/api/keys") {
			return false
		}
		return r.Method == http.MethodGet || r.Method == http.MethodHead
	}
	return true
}

// handler applies headers, limits and request checks to every UI and API request.
func (s *server) handler() http.Handler {
	cop := http.NewCrossOriginProtection()
	cop.SetDenyHandler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeError(w, http.StatusForbidden, "cross-origin request rejected")
	}))
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hdr := w.Header()
		hdr.Set("Content-Security-Policy", csp)
		hdr.Set("X-Content-Type-Options", "nosniff")
		hdr.Set("Referrer-Policy", "no-referrer")
		hdr.Set("X-Frame-Options", "DENY")
		isAPI := strings.HasPrefix(r.URL.Path, "/api/")
		if isAPI {
			hdr.Set("Cache-Control", "no-store")
		}
		// Rejecting unknown Host headers blocks DNS rebinding, on any interface
		// and behind a reverse proxy that forwards the original Host.
		if !s.hosts.allows(r.Host) {
			writeError(w, http.StatusForbidden, "unexpected Host header")
			return
		}
		r.Body = http.MaxBytesReader(w, r.Body, maxBodyBytes)

		// Authentication: resolve the principal once; handlers read it from
		// the context. Pages are always served; the SPA asks /api/me.
		p := auth.Principal{Kind: auth.PrincipalOpen}
		if s.auth != nil {
			p = s.auth.Resolve(r)
		}
		r = r.WithContext(auth.WithPrincipal(r.Context(), p))
		if !s.authorized(r, p) {
			if p.Kind == auth.PrincipalAnonymous && r.Header.Get("Authorization") != "" {
				hdr.Set("WWW-Authenticate", `Bearer realm="whatsapp-bridge"`)
			}
			status := http.StatusUnauthorized
			if p.Authenticated() {
				status = http.StatusForbidden
			}
			writeError(w, status, http.StatusText(status))
			return
		}

		if isAPI {
			if _, pattern := s.mux.Handler(r); pattern == "" || pattern == spaPattern {
				s.notRouted(w, r)
				return
			}
		}
		s.mux.ServeHTTP(w, r)
	})
	return cop.Handler(h)
}

// notRouted answers unknown API paths and methods with JSON instead of the
// mux's plain-text defaults.
func (s *server) notRouted(w http.ResponseWriter, r *http.Request) {
	var allow []string
	for _, m := range []string{http.MethodGet, http.MethodPost, http.MethodDelete, http.MethodPatch} {
		probe := r.WithContext(r.Context())
		probe.Method = m
		if _, p := s.mux.Handler(probe); p != "" && p != spaPattern {
			allow = append(allow, m)
		}
	}
	if len(allow) == 0 {
		writeError(w, http.StatusNotFound, "not found")
		return
	}
	w.Header().Set("Allow", strings.Join(allow, ", "))
	writeError(w, http.StatusMethodNotAllowed, "method not allowed")
}

// hostSet is the Host header allowlist.
type hostSet struct {
	anyPort map[string]bool // bare host names, any or no port
	exact   map[string]bool // host:port entries
}

func newHostSet(extra []string) hostSet {
	hs := hostSet{anyPort: map[string]bool{"localhost": true, "127.0.0.1": true, "::1": true}, exact: map[string]bool{}}
	for _, h := range extra {
		h = strings.ToLower(strings.TrimSpace(h))
		if h == "" {
			continue
		}
		if _, _, err := net.SplitHostPort(h); err == nil {
			hs.exact[h] = true
		} else {
			hs.anyPort[strings.Trim(h, "[]")] = true
		}
	}
	return hs
}

func (hs hostSet) allows(host string) bool {
	host = strings.ToLower(host)
	if hs.exact[host] {
		return true
	}
	name := host
	if h, _, err := net.SplitHostPort(host); err == nil {
		name = h
	}
	return hs.anyPort[strings.Trim(name, "[]")]
}

// ---- JSON helpers ------------------------------------------------------------

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}

// decodeJSON reads exactly one JSON value into dst. An absent body is fine
// unless required; a present body must be declared as application/json.
func decodeJSON(w http.ResponseWriter, r *http.Request, dst any, required bool) bool {
	if r.ContentLength == 0 {
		if required {
			writeError(w, http.StatusBadRequest, "request body required")
			return false
		}
		return true
	}
	ct := r.Header.Get("Content-Type")
	if mt, _, _ := strings.Cut(ct, ";"); !strings.EqualFold(strings.TrimSpace(mt), "application/json") {
		writeError(w, http.StatusUnsupportedMediaType, "content type must be application/json")
		return false
	}
	dec := json.NewDecoder(r.Body)
	err := dec.Decode(dst)
	if err == nil {
		if extra := dec.Decode(&struct{}{}); extra != io.EOF {
			err = errors.Join(errors.New("unexpected data after JSON body"), extra)
		}
	}
	if err != nil {
		var tooBig *http.MaxBytesError
		if errors.As(err, &tooBig) {
			writeError(w, http.StatusRequestEntityTooLarge, "request body too large")
			return false
		}
		writeError(w, http.StatusBadRequest, "invalid JSON body")
		return false
	}
	return true
}

// fail maps manager errors to HTTP statuses. Unknown errors are logged with
// the route pattern (never the path, which can carry a phone number) and
// answered generically.
func (s *server) fail(w http.ResponseWriter, r *http.Request, err error, fallback int) {
	switch {
	case errors.Is(err, wa.ErrNoSession):
		writeError(w, http.StatusNotFound, "session not found")
	case errors.Is(err, wa.ErrNotPaired):
		writeError(w, http.StatusConflict, "session is not paired")
	case errors.Is(err, wa.ErrNotPairing):
		writeError(w, http.StatusConflict, "session is not pairing")
	case errors.Is(err, wa.ErrStillLinked):
		writeError(w, http.StatusConflict, "session is still linked; use Log out")
	case errors.Is(err, wa.ErrBadPhone):
		writeError(w, http.StatusBadRequest, "phone number must include the country code and have at least 7 digits")
	case errors.Is(err, wa.ErrTooManyPairings):
		w.Header().Set("Retry-After", "60")
		writeError(w, http.StatusTooManyRequests, "too many pairings in progress, try again in a minute")
	case errors.Is(err, context.Canceled) && r.Context().Err() != nil:
		// Client went away; nothing useful to send.
	case fallback == http.StatusBadGateway:
		s.log.Warn("whatsapp request failed", "route", r.Pattern, "err", err)
		writeError(w, fallback, "WhatsApp did not respond; try again in a moment")
	default:
		s.log.Error("request failed", "route", r.Pattern, "err", err)
		writeError(w, fallback, http.StatusText(fallback))
	}
}
