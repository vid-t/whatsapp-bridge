// Package mcp serves the bridge's Model Context Protocol endpoint at /mcp
// (streamable HTTP). Assistants such as Claude and Codex connect with a
// bearer key of kind "mcp", or an OAuth access token when the verifier accepts
// them (internal/oauth), and get six read tools over the ticked WhatsApp
// chats. When the operator turns sending on (Options.Sender), a seventh tool,
// send_message, sends text to ticked chats. Nothing here can change or delete
// anything.
//
// Layers, outermost first:
//
//	bearer key check (401 + WWW-Authenticate)  -> HTTP request limit (429 + Retry-After)
//	-> SDK streamable handler (stateless, JSON responses)
//	-> tools/call middleware: tool-call limit (MCP tool error) and audit
//	-> tool handler (store queries, plain-text results)
package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"net/http"
	"strconv"
	"strings"
	"time"

	sdkauth "github.com/modelcontextprotocol/go-sdk/auth"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/vid-t/whatsapp-bridge/internal/auth"
	"github.com/vid-t/whatsapp-bridge/internal/wa"
)

// Path is where the MCP endpoint is mounted.
const Path = "/mcp"

const (
	metadataPath = "/.well-known/oauth-protected-resource"
	oauthScope   = "mcp" // internal/oauth.Scope
	window       = time.Minute
	// httpFactor scales the tool-call limits into the limits on raw HTTP
	// requests (initialize, tools/list, pings, refused GETs, tool calls).
	// Normal sessions stay far below it; floods get 429 with Retry-After.
	httpFactor = 5
)

// Store is the read side of the WhatsApp store the tools need. *wa.Manager
// satisfies it; tests use a fake.
type Store interface {
	Sessions() []wa.SessionInfo
	Session(id string) (wa.SessionInfo, error)
	Chats(ctx context.Context, sessionID string) ([]wa.Chat, error)
	RecentMessages(ctx context.Context, sessionID, chatJID string, limit int) ([]wa.Message, error)
	RecentLogs(ctx context.Context, sessionID string, limit int) ([]wa.LogEntry, error)
	SearchMessages(ctx context.Context, sessionID, chatJID, query string, limit int) ([]wa.SearchHit, error)
	MessageContext(ctx context.Context, sessionID, chatJID, messageID string, before, after int) ([]wa.Message, error)
	FindChat(ctx context.Context, sessionID, title string) (wa.Chat, error)
}

var _ Store = (*wa.Manager)(nil)

// visible is the single place that decides whether tools may see a chat.
func visible(c wa.Chat) bool { return c.Ticked }

// Limits are per-minute caps on tool calls. Zero values take the defaults.
type Limits struct {
	PerKey  int // tool calls per key per minute (default 60)
	Overall int // tool calls across all keys per minute (default 600)
	// Sends are capped further: WhatsApp restricts accounts that send in
	// bulk.
	SendPerKey  int // messages sent per key per minute (default 10)
	SendOverall int // messages sent across all keys per minute (default 20)
}

// Options configures the MCP endpoint.
type Options struct {
	Name    string // the bridge's display name, shown in get_status and the server title
	Version string // server version (git sha); "dev" when empty
	// PublicURL is the externally visible origin, e.g. "https://bridge.example.org".
	// It is used in the protected-resource metadata and WWW-Authenticate. When
	// empty it is derived from each request's Host (and TLS or X-Forwarded-Proto).
	PublicURL string
	// OAuth names this origin as the OAuth authorization server (served by
	// internal/oauth) in the protected-resource metadata, advertises the
	// "mcp" scope there and in the 401 challenge, so clients can sign in
	// with a click. The verifier must then also accept OAuth access tokens.
	OAuth bool
	// Audit, when set, is called once per tool call (including rejected and
	// failed calls). Events never contain message text or raw arguments.
	Audit  func(AuditEvent)
	Limits Limits
	// Media, when set, supplies file contents for get_document. This build has
	// no media store; leave nil.
	Media MediaSource
	// Sender, when set, adds the send_message tool. Leave it nil for a
	// read-only bridge.
	Sender Sender

	now func() time.Time // tests
}

// AuditEvent records one tool call without its content.
type AuditEvent struct {
	Time       time.Time     `json:"time"`
	KeyID      string        `json:"key_id"`
	KeyLabel   string        `json:"key_label,omitempty"`
	Tool       string        `json:"tool"`
	ArgsSHA256 string        `json:"args_sha256"` // hex SHA-256 of the canonical JSON arguments
	ChatJIDs   []string      `json:"chat_jids,omitempty"`
	Rows       int           `json:"rows"`
	Duration   time.Duration `json:"duration_ns"`
	Error      string        `json:"error,omitempty"` // an error code such as "unknown_chat", never content
}

// Detail renders the event as one compact line for an event log.
func (e AuditEvent) Detail() string {
	var b strings.Builder
	fmt.Fprintf(&b, "key=%s tool=%s args=%.16s rows=%d dur=%dms", e.KeyID, e.Tool, e.ArgsSHA256, e.Rows, e.Duration.Milliseconds())
	if len(e.ChatJIDs) > 0 {
		fmt.Fprintf(&b, " chats=%s", strings.Join(e.ChatJIDs, ","))
	}
	if e.Error != "" {
		fmt.Fprintf(&b, " err=%s", e.Error)
	}
	return b.String()
}

// server holds everything shared by the HTTP layer and the tools.
type server struct {
	store    Store
	sender   Sender // nil: read-only
	verifier auth.Verifier
	log      *slog.Logger
	opts     Options
	now      func() time.Time
	tools    *limiter     // tool calls
	requests *limiter     // raw HTTP requests
	sends    *limiter     // messages sent
	recent   *recentSends // repeated sends
	handler  http.Handler
}

// Register mounts the MCP endpoint at /mcp and the protected-resource
// metadata at /.well-known/oauth-protected-resource (root and /mcp suffix).
func Register(mux *http.ServeMux, store Store, verifier auth.Verifier, log *slog.Logger, opts Options) {
	s := newServer(store, verifier, log, opts)
	mux.Handle(Path, s)
	mux.HandleFunc(metadataPath, s.metadata)
	mux.HandleFunc(metadataPath+Path, s.metadata)
}

func newServer(store Store, verifier auth.Verifier, log *slog.Logger, opts Options) *server {
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	if verifier == nil {
		verifier = denyAll{}
	}
	if opts.Limits.PerKey <= 0 {
		opts.Limits.PerKey = 60
	}
	if opts.Limits.Overall <= 0 {
		opts.Limits.Overall = 600
	}
	if opts.Limits.SendPerKey <= 0 {
		opts.Limits.SendPerKey = 10
	}
	if opts.Limits.SendOverall <= 0 {
		opts.Limits.SendOverall = 20
	}
	if opts.Version == "" {
		opts.Version = "dev"
	}
	opts.PublicURL = strings.TrimRight(strings.TrimSpace(opts.PublicURL), "/")
	now := opts.now
	if now == nil {
		now = time.Now
	}
	s := &server{
		store:    store,
		sender:   opts.Sender,
		verifier: verifier,
		log:      log.With("module", "mcp"),
		opts:     opts,
		now:      now,
		tools:    newLimiter(opts.Limits.PerKey, opts.Limits.Overall, window, now),
		requests: newLimiter(opts.Limits.PerKey*httpFactor, opts.Limits.Overall*httpFactor, window, now),
		sends:    newLimiter(opts.Limits.SendPerKey, opts.Limits.SendOverall, window, now),
		recent:   newRecentSends(repeatWindow, now),
	}
	srv := s.mcpServer()
	s.handler = mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return srv }, &mcp.StreamableHTTPOptions{
		// Stateless: every POST stands alone, so there is no session table to
		// grow or hijack, and restarts are invisible to clients.
		Stateless:    true,
		JSONResponse: true,
		Logger:       s.log,
		// The SDK's DNS-rebinding guard rejects a non-loopback Host arriving on
		// a loopback socket, which is exactly what a TLS proxy (Caddy,
		// tailscale serve) on the same machine produces. Every request here
		// must carry a bearer key, which a rebinding page cannot supply.
		DisableLocalhostProtection: true,
		MaxRequestBodyBytes:        1 << 20,
	})
	return s
}

// nameSuffix is " (<name>)" when the bridge has a display name of its own.
func (s *server) nameSuffix() string {
	if s.opts.Name == "" || s.opts.Name == "whatsapp-bridge" {
		return ""
	}
	return " (" + s.opts.Name + ")"
}

func (s *server) mcpServer() *mcp.Server {
	title := "whatsapp-bridge" + s.nameSuffix()
	ins := instructions
	if s.sender != nil {
		ins = instructionsSend
	}
	srv := mcp.NewServer(&mcp.Implementation{Name: "whatsapp-bridge", Title: title, Version: s.opts.Version}, &mcp.ServerOptions{
		Instructions: ins,
		Logger:       s.log,
		Capabilities: &mcp.ServerCapabilities{Tools: &mcp.ToolCapabilities{}},
	})
	srv.AddReceivingMiddleware(s.toolCallMiddleware)
	s.addTools(srv)
	return srv
}

// ---- HTTP layer -------------------------------------------------------------

// ServeHTTP authenticates, applies the request limit, then hands over to the SDK.
func (s *server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")

	token, ok := bearerToken(r)
	if !ok {
		s.unauthorized(w, r, "", "missing bearer key: send Authorization: Bearer wb_live_...")
		return
	}
	key, err := s.verifier.VerifyKey(r.Context(), token)
	switch {
	case errors.Is(err, auth.ErrInvalidKey):
		s.unauthorized(w, r, "invalid_token", "invalid, revoked or unknown key")
		return
	case err != nil:
		s.log.Error("verify key", "err", err)
		writeJSONError(w, http.StatusInternalServerError, "key verification failed; try again")
		return
	case key.Kind != auth.KindMCP:
		s.unauthorized(w, r, "invalid_token", fmt.Sprintf("this is a %q key; assistants need a key of kind \"mcp\"", key.Kind))
		return
	}

	if ok, wait := s.requests.allow(key.ID); !ok {
		w.Header().Set("Retry-After", retryAfter(wait))
		writeJSONError(w, http.StatusTooManyRequests, "too many requests for this key; retry after "+retryAfter(wait)+" seconds")
		return
	}

	// Hand the verified key to the SDK, which copies it into every request's
	// Extra.TokenInfo. UserID binds any session to this key.
	info := &sdkauth.TokenInfo{UserID: key.ID, Extra: map[string]any{"label": key.Label}}
	pass := sdkauth.RequireBearerToken(func(context.Context, string, *http.Request) (*sdkauth.TokenInfo, error) {
		return info, nil
	}, &sdkauth.RequireBearerTokenOptions{AllowMissingExpiration: true})
	pass(s.handler).ServeHTTP(w, r)
}

// denyAll stands in for a missing verifier: every key is rejected.
type denyAll struct{}

func (denyAll) VerifyKey(context.Context, string) (auth.KeyInfo, error) {
	return auth.KeyInfo{}, auth.ErrInvalidKey
}

func bearerToken(r *http.Request) (string, bool) {
	scheme, token, ok := strings.Cut(strings.TrimSpace(r.Header.Get("Authorization")), " ")
	if !ok || !strings.EqualFold(scheme, "Bearer") {
		return "", false
	}
	token = strings.TrimSpace(token)
	return token, token != ""
}

// unauthorized writes a 401 with an RFC 6750 / RFC 9728 challenge.
func (s *server) unauthorized(w http.ResponseWriter, r *http.Request, code, msg string) {
	params := []string{}
	if code != "" {
		params = append(params, `error="`+code+`"`, `error_description="`+headerSafe(msg)+`"`)
	}
	if base := s.baseURL(r); base != "" {
		params = append(params, `resource_metadata="`+base+metadataPath+Path+`"`)
	}
	if s.opts.OAuth {
		// MCP authorization: the scope the client should request.
		params = append(params, `scope="`+oauthScope+`"`)
	}
	challenge := "Bearer"
	if len(params) > 0 {
		challenge += " " + strings.Join(params, ", ")
	}
	w.Header().Set("WWW-Authenticate", challenge)
	writeJSONError(w, http.StatusUnauthorized, msg)
}

// metadata serves RFC 9728 protected-resource metadata. With Options.OAuth
// the bridge's own origin is the authorization server (internal/oauth) and
// clients discover it here; without it the list is empty (bearer keys only).
func (s *server) metadata(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.Header().Set("Access-Control-Allow-Methods", "GET, OPTIONS")
	w.Header().Set("Access-Control-Allow-Headers", "Content-Type, MCP-Protocol-Version")
	switch r.Method {
	case http.MethodOptions:
		w.WriteHeader(http.StatusNoContent)
		return
	case http.MethodGet, http.MethodHead:
	default:
		w.Header().Set("Allow", "GET, HEAD, OPTIONS")
		writeJSONError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	base := s.baseURL(r)
	if base == "" {
		writeJSONError(w, http.StatusBadRequest, "unexpected Host header")
		return
	}
	// RFC 9728 section 3.3: the resource must equal the identifier the
	// metadata URL was built from, so the root document describes the origin
	// and the path-suffixed one describes /mcp.
	resource := base
	if strings.HasSuffix(r.URL.Path, Path) {
		resource = base + Path
	}
	name := "whatsapp-bridge" + s.nameSuffix()
	doc := map[string]any{
		"resource":                 resource,
		"authorization_servers":    []string{},
		"bearer_methods_supported": []string{"header"},
		"resource_name":            name,
	}
	if s.opts.OAuth {
		doc["authorization_servers"] = []string{base}
		doc["scopes_supported"] = []string{oauthScope}
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "public, max-age=300")
	_ = json.NewEncoder(w).Encode(doc)
}

// baseURL returns the public origin, or "" when the request's Host is unusable.
func (s *server) baseURL(r *http.Request) string {
	if s.opts.PublicURL != "" {
		return s.opts.PublicURL
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

func headerSafe(s string) string {
	return strings.Map(func(r rune) rune {
		if r == '"' || r == '\\' || r < 0x20 || r == 0x7f {
			return '\''
		}
		return r
	}, s)
}

func retryAfter(d time.Duration) string {
	sec := int(math.Ceil(d.Seconds()))
	if sec < 1 {
		sec = 1
	}
	return strconv.Itoa(sec)
}

func writeJSONError(w http.ResponseWriter, status int, msg string) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": msg})
}
