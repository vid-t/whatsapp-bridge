package oauth

import (
	"context"
	"crypto/sha256"
	"crypto/x509"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"html"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/vid-t/whatsapp-bridge/internal/auth"
	bmcp "github.com/vid-t/whatsapp-bridge/internal/mcp"
	"github.com/vid-t/whatsapp-bridge/internal/store"
	"github.com/vid-t/whatsapp-bridge/internal/wa"
	"github.com/vid-t/whatsapp-bridge/internal/web"
)

const (
	testCode     = "open-sesame-123"
	testName     = "Acme Team"
	loopbackCB   = "http://localhost/callback"
	loopbackUsed = "http://localhost:43123/callback"
	claudeCB     = "https://claude.ai/api/mcp/auth_callback"
)

// harness runs the bridge the way cmd/whatsapp-bridge wires it: the oauth mux, the
// MCP endpoint with the composite verifier, and the web layer with the
// grants API hook, on one real database.
type harness struct {
	t      *testing.T
	url    string
	srv    *Server
	auth   *auth.Store
	db     *sql.DB
	raw    *httptest.Server
	client *http.Client

	mu     sync.Mutex
	clock  time.Time
	audits []string
	tools  []bmcp.AuditEvent
}

type harnessOpts struct {
	code      string // access code; "" = open mode
	derive    bool   // leave PublicURL empty (issuer from Host)
	cimdRoots *x509.CertPool
	cimdAllow func(netip.Addr) bool
	send      bool // the consent page offers sending (the bridge had sending on at approval)
	mcpSend   bool // the MCP endpoint offers send_message (the bridge has sending on now)
}

func newHarness(t *testing.T, ho harnessOpts) *harness {
	t.Helper()
	h := &harness{t: t, clock: time.Now().UTC()}
	var handler http.Handler
	h.raw = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { handler.ServeHTTP(w, r) }))
	t.Cleanup(h.raw.Close)
	h.url = h.raw.URL

	db, err := store.Open(filepath.Join(t.TempDir(), "bridge.db"), "")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	h.db = db
	log := slog.New(slog.DiscardHandler)
	ctx := context.Background()
	h.auth, err = auth.New(ctx, db, auth.Options{AccessCode: ho.code, Name: testName})
	if err != nil {
		t.Fatal(err)
	}
	mgr, err := wa.NewManager(ctx, db, log, wa.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(mgr.Close)

	public := h.url
	if ho.derive {
		public = ""
	}
	audit := func(typ, detail string) {
		h.mu.Lock()
		defer h.mu.Unlock()
		h.audits = append(h.audits, typ+" "+detail)
	}
	oauthMux := http.NewServeMux()
	h.srv, err = Register(oauthMux, Deps{DB: db, Auth: h.auth, Log: log, Audit: audit}, Options{
		PublicURL: public, now: h.now, cimdRoots: ho.cimdRoots, cimdAllow: ho.cimdAllow, SendEnabled: ho.send,
	})
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	for _, p := range Paths() {
		mux.Handle(p, oauthMux)
	}
	mcpMux := http.NewServeMux()
	mcpOpts := bmcp.Options{
		Name: testName, PublicURL: public, OAuth: true,
		Audit: func(e bmcp.AuditEvent) {
			h.mu.Lock()
			defer h.mu.Unlock()
			h.tools = append(h.tools, e)
		},
	}
	if ho.mcpSend {
		mcpOpts.Sender = mgr
	}
	bmcp.Register(mcpMux, mgr, h.srv.Verifier(h.auth), log, mcpOpts)
	for _, p := range []string{"/mcp", "/.well-known/oauth-protected-resource", "/.well-known/oauth-protected-resource/"} {
		mux.Handle(p, mcpMux)
	}
	web.RegisterWithOptions(mux, mgr, log, web.Options{Auth: h.auth, Audit: audit, ExtraRoutes: h.srv.RegisterAPI})
	handler = mux

	h.client = &http.Client{
		Timeout:       10 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	return h
}

func (h *harness) now() time.Time {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.clock
}

func (h *harness) advance(d time.Duration) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.clock = h.clock.Add(d)
}

func (h *harness) auditLog() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]string(nil), h.audits...)
}

func (h *harness) hasAudit(prefix string) bool {
	for _, a := range h.auditLog() {
		if strings.HasPrefix(a, prefix) {
			return true
		}
	}
	return false
}

func (h *harness) toolEvents() []bmcp.AuditEvent {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]bmcp.AuditEvent(nil), h.tools...)
}

// ---- HTTP helpers ----------------------------------------------------------------------

type mod func(*http.Request)

func withCookie(c *http.Cookie) mod {
	return func(r *http.Request) {
		if c != nil {
			r.AddCookie(c)
		}
	}
}

func withBearer(tok string) mod {
	return func(r *http.Request) { r.Header.Set("Authorization", "Bearer "+tok) }
}

func (h *harness) do(method, path string, body io.Reader, ctype string, mods ...mod) (*http.Response, string) {
	h.t.Helper()
	req, err := http.NewRequest(method, h.url+path, body)
	if err != nil {
		h.t.Fatal(err)
	}
	if ctype != "" {
		req.Header.Set("Content-Type", ctype)
	}
	for _, m := range mods {
		m(req)
	}
	resp, err := h.client.Do(req)
	if err != nil {
		h.t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp, string(b)
}

func (h *harness) get(path string, mods ...mod) (*http.Response, string) {
	h.t.Helper()
	return h.do(http.MethodGet, path, nil, "", mods...)
}

func (h *harness) postForm(path string, form url.Values, mods ...mod) (*http.Response, string) {
	h.t.Helper()
	return h.do(http.MethodPost, path, strings.NewReader(form.Encode()), "application/x-www-form-urlencoded", mods...)
}

func (h *harness) postJSON(path, body string, mods ...mod) (*http.Response, string) {
	h.t.Helper()
	return h.do(http.MethodPost, path, strings.NewReader(body), "application/json", mods...)
}

func jsonMap(t *testing.T, body string) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal([]byte(body), &m); err != nil {
		t.Fatalf("not JSON: %v: %s", err, body)
	}
	return m
}

func sessionCookie(resp *http.Response) *http.Cookie {
	for _, c := range resp.Cookies() {
		if strings.Contains(c.Name, "bridge_session") {
			return c
		}
	}
	return nil
}

var hiddenRe = regexp.MustCompile(`<input type="hidden" name="([a-z_]+)" value="([^"]*)">`)

// hiddenFields reads the authorization request the page carries.
func hiddenFields(page string) url.Values {
	v := url.Values{}
	for _, m := range hiddenRe.FindAllStringSubmatch(page, -1) {
		v.Add(m[1], html.UnescapeString(m[2]))
	}
	return v
}

// ---- OAuth flow helpers ------------------------------------------------------------------

type pkcePair struct{ verifier, challenge string }

func newPKCE() pkcePair {
	v := randomToken() + randomToken()[:10] // 53 unreserved characters
	sum := sha256.Sum256([]byte(v))
	return pkcePair{verifier: v, challenge: base64.RawURLEncoding.EncodeToString(sum[:])}
}

func (h *harness) register(redirects ...string) string {
	h.t.Helper()
	body, _ := json.Marshal(map[string]any{"client_name": "Test Client", "redirect_uris": redirects})
	resp, out := h.postJSON(RegisterPath, string(body))
	if resp.StatusCode != http.StatusCreated {
		h.t.Fatalf("register: %d %s", resp.StatusCode, out)
	}
	return jsonMap(h.t, out)["client_id"].(string)
}

func authzQuery(clientID, redirect string, p pkcePair, state string) url.Values {
	q := url.Values{
		"response_type":         {"code"},
		"client_id":             {clientID},
		"redirect_uri":          {redirect},
		"code_challenge":        {p.challenge},
		"code_challenge_method": {"S256"},
		"scope":                 {"mcp"},
	}
	if state != "" {
		q.Set("state", state)
	}
	return q
}

// signIn opens the authorize page, submits the access code and returns the
// session cookie and the consent page.
func (h *harness) signIn(q url.Values) (*http.Cookie, string) {
	h.t.Helper()
	resp, page := h.get(AuthorizePath + "?" + q.Encode())
	if resp.StatusCode != http.StatusOK || !strings.Contains(page, `name="access_code"`) {
		h.t.Fatalf("expected the sign-in form: %d\n%s", resp.StatusCode, page)
	}
	form := hiddenFields(page)
	form.Set("access_code", testCode)
	resp, page = h.postForm(AuthorizePath, form)
	c := sessionCookie(resp)
	if resp.StatusCode != http.StatusOK || c == nil || !strings.Contains(page, `value="allow"`) {
		h.t.Fatalf("sign-in should show consent and set a cookie: %d %v\n%s", resp.StatusCode, c, page)
	}
	return c, page
}

// decide submits the consent form and returns the redirect target.
func (h *harness) decide(consent string, cookie *http.Cookie, action string) *url.URL {
	h.t.Helper()
	form := hiddenFields(consent)
	form.Set("action", action)
	resp, body := h.postForm(AuthorizePath, form, withCookie(cookie))
	if resp.StatusCode != http.StatusSeeOther {
		h.t.Fatalf("%s: expected 303, got %d\n%s", action, resp.StatusCode, body)
	}
	u, err := url.Parse(resp.Header.Get("Location"))
	if err != nil {
		h.t.Fatal(err)
	}
	return u
}

func (h *harness) exchange(clientID, code, redirect, verifier string, extra url.Values) (int, map[string]any) {
	h.t.Helper()
	form := url.Values{
		"grant_type":    {"authorization_code"},
		"code":          {code},
		"code_verifier": {verifier},
		"client_id":     {clientID},
		"redirect_uri":  {redirect},
	}
	for k, v := range extra {
		form[k] = v
	}
	resp, body := h.postForm(TokenPath, form)
	return resp.StatusCode, jsonMap(h.t, body)
}

func (h *harness) refresh(clientID, rt string) (int, map[string]any) {
	h.t.Helper()
	resp, body := h.postForm(TokenPath, url.Values{"grant_type": {"refresh_token"}, "refresh_token": {rt}, "client_id": {clientID}})
	return resp.StatusCode, jsonMap(h.t, body)
}

type flow struct {
	clientID, redirect string
	pkce               pkcePair
	cookie             *http.Cookie
	code               string
	access, refresh    string
}

// fullFlow registers a client, signs in, approves and exchanges the code.
func (h *harness) fullFlow() flow {
	h.t.Helper()
	f := flow{redirect: claudeCB, pkce: newPKCE()}
	f.clientID = h.register(claudeCB)
	q := authzQuery(f.clientID, f.redirect, f.pkce, "st-1")
	var consent string
	f.cookie, consent = h.signIn(q)
	u := h.decide(consent, f.cookie, "allow")
	f.code = u.Query().Get("code")
	if f.code == "" || u.Query().Get("state") != "st-1" {
		h.t.Fatalf("redirect: %s", u)
	}
	status, tok := h.exchange(f.clientID, f.code, f.redirect, f.pkce.verifier, nil)
	if status != http.StatusOK {
		h.t.Fatalf("exchange: %d %v", status, tok)
	}
	f.access, f.refresh = tok["access_token"].(string), tok["refresh_token"].(string)
	return f
}

// ping calls /mcp with a bearer token and returns the response.
func (h *harness) ping(token string) *http.Response {
	h.t.Helper()
	resp, _ := h.do(http.MethodPost, "/mcp", strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"ping"}`), "application/json",
		withBearer(token), func(r *http.Request) { r.Header.Set("Accept", "application/json, text/event-stream") })
	return resp
}

type bearerRT struct{ token string }

func (b bearerRT) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	r.Header.Set("Authorization", "Bearer "+b.token)
	return http.DefaultTransport.RoundTrip(r)
}

// callTool connects an MCP client with the token and runs one tool.
func (h *harness) callTool(token, name string) string {
	h.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	c := sdk.NewClient(&sdk.Implementation{Name: "oauth-test", Version: "1"}, nil)
	cs, err := c.Connect(ctx, &sdk.StreamableClientTransport{
		Endpoint:             h.url + "/mcp",
		HTTPClient:           &http.Client{Transport: bearerRT{token}, Timeout: 10 * time.Second},
		DisableStandaloneSSE: true,
		MaxRetries:           -1,
	}, nil)
	if err != nil {
		h.t.Fatalf("connect: %v", err)
	}
	defer cs.Close()
	res, err := cs.CallTool(ctx, &sdk.CallToolParams{Name: name})
	if err != nil {
		h.t.Fatalf("%s: %v", name, err)
	}
	var b strings.Builder
	for _, c := range res.Content {
		if tc, ok := c.(*sdk.TextContent); ok {
			b.WriteString(tc.Text)
		}
	}
	if res.IsError {
		h.t.Fatalf("%s: tool error %s", name, b.String())
	}
	return b.String()
}

func (h *harness) grants(cookie *http.Cookie) []Grant {
	h.t.Helper()
	resp, body := h.get(GrantsPath, withCookie(cookie))
	if resp.StatusCode != http.StatusOK {
		h.t.Fatalf("grants: %d %s", resp.StatusCode, body)
	}
	var out []Grant
	if err := json.Unmarshal([]byte(body), &out); err != nil {
		h.t.Fatal(err)
	}
	return out
}
