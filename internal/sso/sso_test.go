package sso

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"html"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/vid-t/whatsapp-bridge/internal/auth"
	"github.com/vid-t/whatsapp-bridge/internal/members"
	"github.com/vid-t/whatsapp-bridge/internal/oauth"
	"github.com/vid-t/whatsapp-bridge/internal/store"
	"github.com/vid-t/whatsapp-bridge/internal/wa"
	"github.com/vid-t/whatsapp-bridge/internal/web"
)

const (
	accessCode  = "bootstrap-code-1"
	bridgeName  = "Acme Team"
	tenantA     = "11111111-2222-3333-4444-555555555555"
	tenantOther = "66666666-7777-8888-9999-000000000000"
)

// harness wires web + sso (+ oauth for the MCP sign-in step) on one
// database, like cmd/whatsapp-bridge, against the fake provider.
type harness struct {
	t      *testing.T
	url    string
	idp    *fakeIdP
	auth   *auth.Store
	sso    *Server
	client *http.Client

	mu     sync.Mutex
	audits []string
	offset atomic.Int64 // added to the bridge's clock
}

type hopts struct {
	google, microsoft bool
	tenant            string
	domains           []string
}

func newHarness(t *testing.T, o hopts) *harness {
	t.Helper()
	h := &harness{t: t, idp: newFakeIdP(t)}
	db, err := store.Open(filepath.Join(t.TempDir(), "bridge.db"), "")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	log := slog.New(slog.DiscardHandler)
	ctx := context.Background()
	h.auth, err = auth.New(ctx, db, auth.Options{AccessCode: accessCode, Name: bridgeName})
	if err != nil {
		t.Fatal(err)
	}
	mgr, err := wa.NewManager(ctx, db, log, wa.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(mgr.Close)
	audit := func(typ, detail string) {
		h.mu.Lock()
		defer h.mu.Unlock()
		h.audits = append(h.audits, typ+" "+detail)
	}
	cfg := Config{AllowedDomains: o.domains, MicrosoftTenant: o.tenant, googleIssuer: h.idp.ts.URL, microsoftBase: h.idp.ts.URL,
		now: func() time.Time { return time.Now().Add(time.Duration(h.offset.Load())) }}
	if o.google {
		cfg.GoogleClientID, cfg.GoogleClientSecret = "google-client", "google-secret"
	}
	if o.microsoft {
		cfg.MicrosoftClientID, cfg.MicrosoftClientSecret = "ms-client", "ms-secret"
	}
	h.sso, err = New(Deps{Auth: h.auth, Log: log, Audit: audit}, cfg)
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	oauthMux := http.NewServeMux()
	osrv, err := oauth.Register(oauthMux, oauth.Deps{DB: db, Auth: h.auth, Log: log, Audit: audit}, oauth.Options{SignInProviders: h.sso.Providers()})
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range oauth.Paths() {
		mux.Handle(p, oauthMux)
	}
	// Members (and their API) are mounted beside sso, as cmd/whatsapp-bridge does.
	msrv, err := members.New(members.Deps{Auth: h.auth, DB: db, Log: log, Audit: audit}, members.Config{AllowedDomains: o.domains})
	if err != nil {
		t.Fatal(err)
	}
	web.RegisterWithOptions(mux, mgr, log, web.Options{Auth: h.auth, Audit: audit, SignInProviders: h.sso.Providers(),
		ExtraRoutes: func(m *http.ServeMux) { h.sso.RegisterRoutes(m); msrv.RegisterRoutes(m); osrv.RegisterAPI(m) }})
	ts := httptest.NewServer(mux)
	t.Cleanup(ts.Close)
	h.url = ts.URL
	h.client = &http.Client{Timeout: 10 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	return h
}

func (h *harness) auditLog() string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return strings.Join(h.audits, "\n")
}

type jar map[string]*http.Cookie

func (j jar) take(resp *http.Response) {
	for _, c := range resp.Cookies() {
		if c.MaxAge < 0 {
			delete(j, c.Name)
		} else {
			j[c.Name] = c
		}
	}
}

func (h *harness) do(method, target, body string, j jar, mods ...func(*http.Request)) (*http.Response, string) {
	h.t.Helper()
	if strings.HasPrefix(target, "/") {
		target = h.url + target
	}
	var rd io.Reader
	if body != "" {
		rd = strings.NewReader(body)
	}
	req, err := http.NewRequest(method, target, rd)
	if err != nil {
		h.t.Fatal(err)
	}
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	for _, c := range j {
		req.AddCookie(c)
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
	if j != nil {
		j.take(resp)
	}
	return resp, string(b)
}

// codeSession signs in with the access code (the bootstrap path).
func (h *harness) codeSession() jar {
	h.t.Helper()
	j := jar{}
	resp, body := h.do("POST", "/auth/login", `{"code":"`+accessCode+`"}`, j)
	if resp.StatusCode != http.StatusNoContent {
		h.t.Fatalf("access-code login: %d %s", resp.StatusCode, body)
	}
	return j
}

func (h *harness) addMember(email string) {
	h.t.Helper()
	resp, body := h.do("POST", "/api/members", `{"email":"`+email+`"}`, h.codeSession())
	if resp.StatusCode != http.StatusCreated {
		h.t.Fatalf("add member: %d %s", resp.StatusCode, body)
	}
}

// login runs start -> provider -> callback and returns the final response.
func (h *harness) login(provider, next string, claims map[string]any, j jar) (*http.Response, string) {
	h.t.Helper()
	path := "/auth/" + provider + "/start"
	if next != "" {
		path += "?next=" + url.QueryEscape(next)
	}
	resp, body := h.do("GET", path, "", j)
	if resp.StatusCode != http.StatusFound {
		h.t.Fatalf("start: %d %s", resp.StatusCode, body)
	}
	code, q := h.idp.authorize(resp.Header.Get("Location"), claims)
	cb := q.Get("redirect_uri") + "?" + url.Values{"code": {code}, "state": {q.Get("state")}}.Encode()
	return h.do("GET", cb, "", j)
}

func googleClaims(email string, verified bool, sub string) map[string]any {
	return map[string]any{"sub": sub, "email": email, "email_verified": verified, "name": "Anna Example"}
}

func (h *harness) me(j jar) map[string]any {
	h.t.Helper()
	_, body := h.do("GET", "/api/me", "", j)
	var m map[string]any
	if err := json.Unmarshal([]byte(body), &m); err != nil {
		h.t.Fatal(err)
	}
	return m
}

// ---- tests ----------------------------------------------------------------------------------

func TestStartRedirectCarriesStateNonceAndPKCE(t *testing.T) {
	h := newHarness(t, hopts{google: true})
	j := jar{}
	resp, _ := h.do("GET", "/auth/google/start?next=/sessions/abc", "", j)
	if resp.StatusCode != http.StatusFound {
		t.Fatalf("start: %d", resp.StatusCode)
	}
	u, _ := url.Parse(resp.Header.Get("Location"))
	q := u.Query()
	if !strings.HasPrefix(u.String(), h.idp.ts.URL+"/authorize?") || q.Get("response_type") != "code" ||
		q.Get("client_id") != "google-client" || q.Get("redirect_uri") != h.url+"/auth/google/callback" ||
		q.Get("scope") != "openid email profile" || len(q.Get("state")) < 20 || len(q.Get("nonce")) < 20 ||
		q.Get("code_challenge_method") != "S256" || len(q.Get("code_challenge")) != 43 {
		t.Fatalf("authorization request: %s", u)
	}
	c := j["bridge_oidc"]
	if c == nil || !c.HttpOnly || c.MaxAge != 600 || c.SameSite != http.SameSiteLaxMode || strings.Contains(c.Value, q.Get("code_challenge")) {
		t.Fatalf("pending cookie: %+v", c)
	}
	if resp, _ := h.do("GET", "/auth/microsoft/start", "", nil); resp.StatusCode != http.StatusNotFound {
		t.Fatalf("disabled provider: %d", resp.StatusCode)
	}
}

func TestCallbackCreatesSessionAndMember(t *testing.T) {
	h := newHarness(t, hopts{google: true})
	h.addMember("Anna@Example.org")
	j := jar{}
	resp, _ := h.login("google", "/sessions/abc?tab=chats", googleClaims("anna@example.org", true, "g-sub-1"), j)
	if resp.StatusCode != http.StatusFound || resp.Header.Get("Location") != "/sessions/abc?tab=chats" {
		t.Fatalf("callback: %d %q", resp.StatusCode, resp.Header.Get("Location"))
	}
	if j["bridge_session"] == nil || j["bridge_oidc"] != nil {
		t.Fatalf("cookies after callback: %v", j)
	}
	me := h.me(j)
	user, _ := me["user"].(map[string]any)
	if me["authenticated"] != true || me["principal"] != "session" || user["email"] != "anna@example.org" ||
		user["name"] != "Anna Example" || user["provider"] != "google" {
		t.Fatalf("/api/me: %v", me)
	}
	h.idp.mu.Lock()
	hits := h.idp.hits
	h.idp.mu.Unlock()
	if hits != 1 {
		t.Fatalf("the code must be exchanged at the provider exactly once, got %d", hits)
	}
	ms, _ := h.auth.ListMembers(context.Background())
	if len(ms) != 1 || ms[0].Provider != "google" || ms[0].Subject != "g-sub-1" || ms[0].LastLoginAt == nil || ms[0].Name != "Anna Example" {
		t.Fatalf("member row: %+v", ms)
	}
	// Audit names the person.
	if resp, _ := h.do("POST", "/api/keys", `{"label":"Claude","kind":"mcp"}`, j); resp.StatusCode != http.StatusCreated {
		t.Fatalf("mint key as member: %d", resp.StatusCode)
	}
	log := h.auditLog()
	if !strings.Contains(log, "auth.login ip=127.0.0.1 via=google email=anna@example.org") ||
		!strings.Contains(log, "key.created") || !strings.Contains(log, "by=anna@example.org") {
		t.Fatalf("audit:\n%s", log)
	}
	// A next pointing elsewhere is not followed.
	resp, _ = h.login("google", "//evil.example/x", googleClaims("anna@example.org", true, "g-sub-1"), jar{})
	if resp.Header.Get("Location") != "/" {
		t.Fatalf("unsafe next: %q", resp.Header.Get("Location"))
	}
}

func TestCallbackRefusesNonMember(t *testing.T) {
	h := newHarness(t, hopts{google: true})
	j := jar{}
	resp, body := h.login("google", "", googleClaims("stranger@gmail.com", true, "g-x"), j)
	if resp.StatusCode != http.StatusForbidden || !strings.Contains(body, "stranger@gmail.com is not a member of "+bridgeName) || j["bridge_session"] != nil {
		t.Fatalf("non-member: %d %v\n%s", resp.StatusCode, j, body)
	}
	if ms, _ := h.auth.ListMembers(context.Background()); len(ms) != 0 {
		t.Fatalf("no member row may be created: %+v", ms)
	}
	if !strings.Contains(h.auditLog(), "auth.login_refused") {
		t.Fatal("refusal not audited")
	}
}

func TestCallbackRefusesTamperedState(t *testing.T) {
	h := newHarness(t, hopts{google: true, microsoft: true})
	h.addMember("anna@example.org")
	start := func() (jar, string, url.Values) {
		j := jar{}
		resp, _ := h.do("GET", "/auth/google/start", "", j)
		code, q := h.idp.authorize(resp.Header.Get("Location"), googleClaims("anna@example.org", true, "s"))
		return j, code, q
	}
	refused := func(name string, resp *http.Response, body string, j jar) {
		t.Helper()
		if resp.StatusCode != http.StatusBadRequest || !strings.Contains(body, "cannot be used") || j["bridge_session"] != nil {
			t.Errorf("%s: %d %v", name, resp.StatusCode, j)
		}
	}
	j, code, q := start()
	resp, body := h.do("GET", "/auth/google/callback?"+url.Values{"code": {code}, "state": {"forged"}}.Encode(), "", j)
	refused("wrong state", resp, body, j)

	j, code, q = start()
	resp, body = h.do("GET", "/auth/google/callback?"+url.Values{"code": {code}, "state": {q.Get("state")}}.Encode(), "", jar{})
	refused("no cookie", resp, body, jar{})

	j, code, q = start()
	c := j["bridge_oidc"]
	body64, sig, _ := strings.Cut(c.Value, ".")
	c.Value = body64 + "x." + sig
	resp, body = h.do("GET", "/auth/google/callback?"+url.Values{"code": {code}, "state": {q.Get("state")}}.Encode(), "", j)
	refused("forged cookie", resp, body, j)

	j, code, q = start()
	resp, body = h.do("GET", "/auth/microsoft/callback?"+url.Values{"code": {code}, "state": {q.Get("state")}}.Encode(), "", j)
	refused("other provider", resp, body, j)

	// The pending sign-in expires after ten minutes.
	j, code, q = start()
	h.offset.Store(int64(11 * time.Minute))
	resp, body = h.do("GET", "/auth/google/callback?"+url.Values{"code": {code}, "state": {q.Get("state")}}.Encode(), "", j)
	refused("expired", resp, body, j)
}

func TestCallbackRefusesBadTokens(t *testing.T) {
	h := newHarness(t, hopts{google: true})
	h.addMember("anna@example.org")
	for name, claims := range map[string]map[string]any{
		"unverified email": googleClaims("anna@example.org", false, "s"),
		"no email":         {"sub": "s", "email_verified": true},
		"wrong nonce":      {"sub": "s", "email": "anna@example.org", "email_verified": true, "nonce": "other"},
		"wrong audience":   {"sub": "s", "email": "anna@example.org", "email_verified": true, "aud": "someone-else"},
		"expired":          {"sub": "s", "email": "anna@example.org", "email_verified": true, "exp": time.Now().Add(-time.Hour).Unix()},
		"wrong issuer":     {"sub": "s", "email": "anna@example.org", "email_verified": true, "iss": "https://evil.example"},
	} {
		j := jar{}
		resp, body := h.login("google", "", claims, j)
		if resp.StatusCode < 400 || j["bridge_session"] != nil {
			t.Errorf("%s: %d %v\n%s", name, resp.StatusCode, j, body)
		}
		if name == "unverified email" && !strings.Contains(body, "not verified") {
			t.Errorf("unverified email should say so: %s", body)
		}
	}
}

func TestDomainAllowlist(t *testing.T) {
	h := newHarness(t, hopts{google: true, domains: []string{"Example.org"}})
	j := jar{}
	resp, _ := h.login("google", "", googleClaims("new.hire@example.org", true, "g-new"), j)
	if resp.StatusCode != http.StatusFound || j["bridge_session"] == nil {
		t.Fatalf("allowed domain: %d", resp.StatusCode)
	}
	m, err := h.auth.MemberByEmail(context.Background(), "new.hire@example.org")
	if err != nil || m.Provider != "google" || m.Subject != "g-new" {
		t.Fatalf("member created on first sign-in: %+v %v", m, err)
	}
	resp, _ = h.login("google", "", googleClaims("someone@example.org.evil.com", true, "g-evil"), jar{})
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("lookalike domain: %d", resp.StatusCode)
	}
}

func TestAccountBinding(t *testing.T) {
	h := newHarness(t, hopts{google: true, microsoft: true})
	h.addMember("anna@example.org")
	if resp, _ := h.login("google", "", googleClaims("anna@example.org", true, "g-1"), jar{}); resp.StatusCode != http.StatusFound {
		t.Fatalf("first sign-in: %d", resp.StatusCode)
	}
	resp, body := h.login("google", "", googleClaims("anna@example.org", true, "g-2"), jar{})
	if resp.StatusCode != http.StatusForbidden || !strings.Contains(body, "linked to a different Google account") {
		t.Fatalf("other Google account for the same email: %d %s", resp.StatusCode, body)
	}
	resp, _ = h.login("microsoft", "", map[string]any{"sub": "m-1", "tid": tenantA, "iss": h.idp.ts.URL + "/" + tenantA + "/v2.0",
		"email": "anna@example.org"}, jar{})
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("other provider for a bound member: %d", resp.StatusCode)
	}
	if resp, _ := h.login("google", "", googleClaims("anna@example.org", true, "g-1"), jar{}); resp.StatusCode != http.StatusFound {
		t.Fatalf("bound account again: %d", resp.StatusCode)
	}
}

func TestMicrosoftMultiTenant(t *testing.T) {
	h := newHarness(t, hopts{microsoft: true})
	h.addMember("max@example.net")
	iss := func(tid string) string { return h.idp.ts.URL + "/" + tid + "/v2.0" }
	ok := map[string]any{"sub": "m-1", "tid": tenantA, "iss": iss(tenantA), "email": "max@example.net", "name": "Max Example"}
	j := jar{}
	if resp, body := h.login("microsoft", "", ok, j); resp.StatusCode != http.StatusFound || j["bridge_session"] == nil {
		t.Fatalf("valid multi-tenant token: %d %s", resp.StatusCode, body)
	}
	if u, _ := h.me(j)["user"].(map[string]any); u["provider"] != "microsoft" {
		t.Fatalf("me: %v", u)
	}
	for name, c := range map[string]map[string]any{
		"issuer of another tenant": {"sub": "m-1", "tid": tenantA, "iss": iss(tenantOther), "email": "max@example.net"},
		"no tid":                   {"sub": "m-1", "tid": nil, "iss": iss(tenantA), "email": "max@example.net"},
		"template issuer":          {"sub": "m-1", "tid": tenantA, "iss": h.idp.ts.URL + "/{tenantid}/v2.0", "email": "max@example.net"},
		"foreign issuer":           {"sub": "m-1", "tid": tenantA, "iss": "https://evil.example/" + tenantA + "/v2.0", "email": "max@example.net"},
	} {
		if resp, body := h.login("microsoft", "", c, jar{}); resp.StatusCode != http.StatusForbidden {
			t.Errorf("%s: %d %s", name, resp.StatusCode, body)
		}
	}
}

func TestMicrosoftEmailRules(t *testing.T) {
	for name, tc := range map[string]struct {
		c    claims
		want string
	}{
		"email":                        {claims{Email: "A@x.example"}, "a@x.example"},
		"preferred_username fallback":  {claims{PreferredUsername: "upn@x.example"}, "upn@x.example"},
		"unverified domain owner":      {claims{Email: "spoof@victim.example", EmailDomainOwner: false, PreferredUsername: "real@attacker.example"}, "real@attacker.example"},
		"verified domain owner":        {claims{Email: "ok@x.example", EmailDomainOwner: true}, "ok@x.example"},
		"preferred_username not email": {claims{PreferredUsername: "+41790000000"}, ""},
	} {
		got, err := microsoftEmail(tc.c)
		if got != tc.want || (tc.want == "") != (err != nil) {
			t.Errorf("%s: %q %v, want %q", name, got, err, tc.want)
		}
	}
	h := newHarness(t, hopts{microsoft: true, tenant: "organizations"})
	h.addMember("me@outlook.example")
	resp, body := h.login("microsoft", "", map[string]any{"sub": "p", "tid": consumersTenant, "iss": h.idp.ts.URL + "/" + consumersTenant + "/v2.0",
		"email": "me@outlook.example"}, jar{})
	if resp.StatusCode != http.StatusForbidden || !strings.Contains(body, "work or school") {
		t.Fatalf("organizations must refuse personal accounts: %d %s", resp.StatusCode, body)
	}
}

func TestTenantConfig(t *testing.T) {
	a, _ := auth.New(context.Background(), mustDB(t), auth.Options{AccessCode: "x"})
	for tenant, ok := range map[string]bool{"": true, "common": true, "organizations": true, "consumers": true, tenantA: true, "contoso.onmicrosoft.com": false} {
		_, err := New(Deps{Auth: a}, Config{MicrosoftClientID: "id", MicrosoftClientSecret: "s", MicrosoftTenant: tenant})
		if (err == nil) != ok {
			t.Errorf("tenant %q: err=%v", tenant, err)
		}
	}
	env := map[string]string{
		"BRIDGE_GOOGLE_CLIENT_ID": "g", "BRIDGE_GOOGLE_CLIENT_SECRET": "gs",
		"BRIDGE_MICROSOFT_CLIENT_ID": "m", // no secret: Microsoft stays off
		"BRIDGE_ALLOWED_DOMAINS":     " @Acme.ch, example.com ,",
	}
	cfg := ConfigFromEnv(func(k string) string { return env[k] }, "https://bridge.example.org")
	s, err := New(Deps{Auth: a}, cfg)
	if err != nil || strings.Join(s.Providers(), ",") != "google" || !s.domains["acme.ch"] || !s.domains["example.com"] || len(s.domains) != 2 {
		t.Fatalf("from env: %v %v %v", s.Providers(), s.domains, err)
	}
}

func mustDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := store.Open(filepath.Join(t.TempDir(), "x.db"), "")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

func TestMembersAPI(t *testing.T) {
	h := newHarness(t, hopts{google: true})
	j := h.codeSession()
	resp, body := h.do("POST", "/api/members", `{"email":" Max@example.net ","name":"Max"}`, j)
	var m map[string]any
	_ = json.Unmarshal([]byte(body), &m)
	if resp.StatusCode != http.StatusCreated || m["email"] != "max@example.net" || m["role"] != "member" || m["provider"] != "" ||
		m["last_login_at"] != nil || m["subject"] != nil {
		t.Fatalf("add: %d %s", resp.StatusCode, body)
	}
	id := m["id"].(string)
	if resp, _ := h.do("POST", "/api/members", `{"email":"max@example.net"}`, j); resp.StatusCode != http.StatusConflict {
		t.Fatalf("duplicate: %d", resp.StatusCode)
	}
	for _, bad := range []string{`{"email":"nope"}`, `{"email":"Max <max@example.net>"}`, `{"email":""}`, `{}`} {
		if resp, _ := h.do("POST", "/api/members", bad, j); resp.StatusCode != http.StatusBadRequest {
			t.Fatalf("bad %s: %d", bad, resp.StatusCode)
		}
	}
	resp, body = h.do("GET", "/api/members", "", j)
	if resp.StatusCode != http.StatusOK || !strings.Contains(body, `"email":"max@example.net"`) {
		t.Fatalf("list: %d %s", resp.StatusCode, body)
	}

	// Policy: anonymous 401, bearer keys 403 (even GET), cross-site writes 403.
	if resp, _ := h.do("GET", "/api/members", "", nil); resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("anonymous: %d", resp.StatusCode)
	}
	_, key, _ := h.auth.CreateKey(context.Background(), "k", auth.KindMCP)
	bearer := func(r *http.Request) { r.Header.Set("Authorization", "Bearer "+key) }
	if resp, _ := h.do("GET", "/api/members", "", nil, bearer); resp.StatusCode != http.StatusForbidden {
		t.Fatalf("key GET: %d", resp.StatusCode)
	}
	if resp, _ := h.do("POST", "/api/members", `{"email":"x@y.example"}`, nil, bearer); resp.StatusCode != http.StatusForbidden {
		t.Fatalf("key POST: %d", resp.StatusCode)
	}
	if resp, _ := h.do("DELETE", "/api/members/"+id, "", j, func(r *http.Request) { r.Header.Set("Sec-Fetch-Site", "cross-site") }); resp.StatusCode != http.StatusForbidden {
		t.Fatalf("cross-site delete: %d", resp.StatusCode)
	}

	// The member signs in, then is removed: the session dies with the row.
	mj := jar{}
	if resp, _ := h.login("google", "", googleClaims("max@example.net", true, "g-max"), mj); resp.StatusCode != http.StatusFound {
		t.Fatalf("member sign-in: %d", resp.StatusCode)
	}
	if resp, _ := h.do("DELETE", "/api/members/"+id, "", j); resp.StatusCode != http.StatusNoContent {
		t.Fatalf("delete: %d", resp.StatusCode)
	}
	if resp, _ := h.do("GET", "/api/sessions", "", mj); resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("removed member's session: %d", resp.StatusCode)
	}
	if resp, _ := h.do("DELETE", "/api/members/"+id, "", j); resp.StatusCode != http.StatusNotFound {
		t.Fatalf("second delete: %d", resp.StatusCode)
	}
	log := h.auditLog()
	if !strings.Contains(log, "member.invited id="+id+" email=max@example.net by=session") || !strings.Contains(log, "member.removed id="+id) {
		t.Fatalf("audit:\n%s", log)
	}
}

func TestMeShape(t *testing.T) {
	plain := newHarness(t, hopts{})
	me := plain.me(nil)
	if p, _ := me["providers"].([]any); len(p) != 1 || p[0] != "code" {
		t.Fatalf("without providers: %v", me["providers"])
	}
	if _, has := me["user"]; has {
		t.Fatalf("anonymous me must not have user: %v", me)
	}
	if _, has := plain.me(plain.codeSession())["user"]; has {
		t.Fatal("access-code session has no user")
	}
	both := newHarness(t, hopts{google: true, microsoft: true})
	if p, _ := both.me(nil)["providers"].([]any); len(p) != 3 || p[0] != "code" || p[1] != "google" || p[2] != "microsoft" {
		t.Fatalf("with providers: %v", p)
	}
}

func TestSafeNext(t *testing.T) {
	for in, want := range map[string]string{
		"": "/", "/": "/", "/sessions/x?y=1": "/sessions/x?y=1", "/oauth/authorize?client_id=a": "/oauth/authorize?client_id=a",
		"//evil.example": "/", "https://evil.example/": "/", "/\\evil.example": "/", "javascript:alert(1)": "/",
		"/auth/google/start": "/", "evil": "/", "/a\nb": "/",
	} {
		if got := safeNext(in); got != want {
			t.Errorf("safeNext(%q) = %q, want %q", in, got, want)
		}
	}
}

var startLinkRe = regexp.MustCompile(`href="(/auth/google/start\?[^"]+)"`)

// The MCP sign-in step offers the providers and returns to the pending
// authorization request afterwards.
func TestMCPSignInOffersProviders(t *testing.T) {
	h := newHarness(t, hopts{google: true, microsoft: true})
	h.addMember("anna@example.org")
	resp, body := h.do("POST", "/oauth/register", `{"client_name":"Claude","redirect_uris":["https://claude.ai/api/mcp/auth_callback"]}`, nil)
	var reg map[string]any
	_ = json.Unmarshal([]byte(body), &reg)
	verifier := strings.Repeat("v", 50)
	sum := sha256.Sum256([]byte(verifier))
	q := url.Values{"response_type": {"code"}, "client_id": {reg["client_id"].(string)}, "redirect_uri": {"https://claude.ai/api/mcp/auth_callback"},
		"state": {"st"}, "code_challenge": {b64(sum[:])}, "code_challenge_method": {"S256"}, "resource": {h.url + "/mcp"}}
	authorize := "/oauth/authorize?" + q.Encode()
	j := jar{}
	resp, page := h.do("GET", authorize, "", j)
	if resp.StatusCode != http.StatusOK || !strings.Contains(page, "Continue with Google") || !strings.Contains(page, "Continue with Microsoft") ||
		!strings.Contains(page, `name="access_code"`) {
		t.Fatalf("sign-in step: %d\n%s", resp.StatusCode, page)
	}
	m := startLinkRe.FindStringSubmatch(page)
	if m == nil {
		t.Fatalf("no Google link:\n%s", page)
	}
	link := html.UnescapeString(m[1])
	lu, _ := url.Parse(link)
	if next := lu.Query().Get("next"); !strings.HasPrefix(next, "/oauth/authorize?") {
		t.Fatalf("link must return to the authorize request: %q", next)
	}
	resp, _ = h.do("GET", link, "", j)
	code, iq := h.idp.authorize(resp.Header.Get("Location"), googleClaims("anna@example.org", true, "g-anna"))
	resp, _ = h.do("GET", iq.Get("redirect_uri")+"?"+url.Values{"code": {code}, "state": {iq.Get("state")}}.Encode(), "", j)
	back := resp.Header.Get("Location")
	if resp.StatusCode != http.StatusFound || !strings.HasPrefix(back, "/oauth/authorize?") {
		t.Fatalf("callback should return to authorize: %d %q", resp.StatusCode, back)
	}
	bu, _ := url.Parse(back)
	for k := range q {
		if bu.Query().Get(k) != q.Get(k) {
			t.Fatalf("authorize parameter %s lost: %q vs %q", k, bu.Query().Get(k), q.Get(k))
		}
	}
	resp, page = h.do("GET", back, "", j)
	if resp.StatusCode != http.StatusOK || !strings.Contains(page, `value="allow"`) {
		t.Fatalf("consent after provider sign-in: %d\n%s", resp.StatusCode, page)
	}
	form := url.Values{"action": {"allow"}}
	for _, hm := range regexp.MustCompile(`<input type="hidden" name="([a-z_]+)" value="([^"]*)">`).FindAllStringSubmatch(page, -1) {
		form.Set(hm[1], html.UnescapeString(hm[2]))
	}
	resp, _ = h.do("POST", "/oauth/authorize", "", j, func(r *http.Request) {
		r.Body = io.NopCloser(strings.NewReader(form.Encode()))
		r.ContentLength = int64(len(form.Encode()))
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	})
	if resp.StatusCode != http.StatusSeeOther || !strings.Contains(resp.Header.Get("Location"), "code=") {
		t.Fatalf("allow: %d %q", resp.StatusCode, resp.Header.Get("Location"))
	}
	if !strings.Contains(h.auditLog(), "oauth.consent_granted client="+reg["client_id"].(string)) ||
		!strings.Contains(h.auditLog(), "by=anna@example.org") {
		t.Fatalf("consent audit should name the member:\n%s", h.auditLog())
	}
}

// A pending access request (email sign-in) is not a member: Google refuses
// it unless its domain is allowed, which approves it.
func TestPendingRequestIsNotAMember(t *testing.T) {
	h := newHarness(t, hopts{google: true, domains: []string{"partner.example"}})
	ctx := context.Background()
	if _, _, err := h.auth.RequestMember(ctx, "max@example.net"); err != nil {
		t.Fatal(err)
	}
	j := jar{}
	resp, body := h.login("google", "", googleClaims("max@example.net", true, "g-max"), j)
	if resp.StatusCode != http.StatusForbidden || !strings.Contains(body, "waiting for approval") || j["bridge_session"] != nil {
		t.Fatalf("pending member via Google: %d %s", resp.StatusCode, body)
	}
	if m, _ := h.auth.MemberByEmail(ctx, "max@example.net"); m.Approved() || m.Subject != "" {
		t.Fatalf("refusal changed the row: %+v", m)
	}
	if _, _, err := h.auth.RequestMember(ctx, "lea@partner.example"); err != nil {
		t.Fatal(err)
	}
	if resp, _ := h.login("google", "", googleClaims("lea@partner.example", true, "g-lea"), jar{}); resp.StatusCode != http.StatusFound {
		t.Fatalf("pending member of an allowed domain: %d", resp.StatusCode)
	}
	if m, _ := h.auth.MemberByEmail(ctx, "lea@partner.example"); !m.Approved() || m.ApprovedBy != "domain" || m.Provider != "google" {
		t.Fatalf("domain admission: %+v", m)
	}
}

// A member who signed in by email first can still bind Google.
func TestEmailMemberBindsGoogle(t *testing.T) {
	h := newHarness(t, hopts{google: true})
	h.addMember("anna@example.org")
	if _, err := h.auth.RecordLogin(context.Background(), "anna@example.org", "", "email", ""); err != nil {
		t.Fatal(err)
	}
	if resp, body := h.login("google", "", googleClaims("anna@example.org", true, "g-1"), jar{}); resp.StatusCode != http.StatusFound {
		t.Fatalf("google after email: %d %s", resp.StatusCode, body)
	}
	if m, _ := h.auth.MemberByEmail(context.Background(), "anna@example.org"); m.Provider != "google" || m.Subject != "g-1" {
		t.Fatalf("binding: %+v", m)
	}
}
