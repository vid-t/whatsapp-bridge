package oauth

import (
	"context"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/vid-t/whatsapp-bridge/internal/auth"
)

// ---- discovery ------------------------------------------------------------------------------

func TestMetadataDocuments(t *testing.T) {
	h := newHarness(t, harnessOpts{code: testCode})
	for _, path := range []string{MetadataPath, MetadataPath + "/mcp"} {
		resp, body := h.get(path)
		if resp.StatusCode != http.StatusOK || resp.Header.Get("Access-Control-Allow-Origin") != "*" {
			t.Fatalf("%s: %d %v", path, resp.StatusCode, resp.Header)
		}
		m := jsonMap(t, body)
		want := map[string]any{
			"issuer":                                         h.url,
			"authorization_endpoint":                         h.url + AuthorizePath,
			"token_endpoint":                                 h.url + TokenPath,
			"registration_endpoint":                          h.url + RegisterPath,
			"revocation_endpoint":                            h.url + RevokePath,
			"client_id_metadata_document_supported":          true,
			"authorization_response_iss_parameter_supported": true,
		}
		for k, v := range want {
			if m[k] != v {
				t.Errorf("%s: %s = %v, want %v", path, k, m[k], v)
			}
		}
		for k, v := range map[string]string{
			"response_types_supported":              "code",
			"code_challenge_methods_supported":      "S256",
			"token_endpoint_auth_methods_supported": "none",
			"scopes_supported":                      "mcp",
		} {
			list, _ := m[k].([]any)
			if len(list) != 1 || list[0] != v {
				t.Errorf("%s: %s = %v, want [%s]", path, k, m[k], v)
			}
		}
		gt, _ := m["grant_types_supported"].([]any)
		if fmt.Sprint(gt) != "[authorization_code refresh_token]" {
			t.Errorf("grant_types_supported = %v", gt)
		}
	}

	// The protected resource names this issuer; the 401 points at it.
	_, body := h.get("/.well-known/oauth-protected-resource/mcp")
	prm := jsonMap(t, body)
	as, _ := prm["authorization_servers"].([]any)
	if prm["resource"] != h.url+"/mcp" || len(as) != 1 || as[0] != h.url {
		t.Fatalf("protected-resource metadata: %v", prm)
	}
	resp := h.ping("")
	challenge := resp.Header.Get("WWW-Authenticate")
	if resp.StatusCode != http.StatusUnauthorized ||
		!strings.Contains(challenge, `resource_metadata="`+h.url+`/.well-known/oauth-protected-resource/mcp"`) ||
		!strings.Contains(challenge, `scope="mcp"`) {
		t.Fatalf("401 challenge: %d %q", resp.StatusCode, challenge)
	}

	// CORS preflight on the token endpoint for browser-based clients.
	resp, _ = h.do(http.MethodOptions, TokenPath, nil, "", func(r *http.Request) {
		r.Header.Set("Origin", "https://inspector.example")
		r.Header.Set("Access-Control-Request-Method", "POST")
	})
	if resp.StatusCode != http.StatusNoContent || resp.Header.Get("Access-Control-Allow-Origin") != "*" {
		t.Fatalf("preflight: %d %v", resp.StatusCode, resp.Header)
	}
}

func TestPublicURLValidation(t *testing.T) {
	for raw, ok := range map[string]bool{
		"": true, "https://bridge.example.org": true, "https://bridge.example.org/": true,
		"http://127.0.0.1:8095": true, "http://localhost:8095": true,
		"http://bridge.example.org": false, "https://bridge.example.org/base": false, "ftp://x": false,
	} {
		if _, err := normalizePublicURL(raw); (err == nil) != ok {
			t.Errorf("normalizePublicURL(%q) err=%v, want ok=%v", raw, err, ok)
		}
	}
}

// ---- dynamic client registration ---------------------------------------------------------------

func TestRegisterHappyPath(t *testing.T) {
	h := newHarness(t, harnessOpts{code: testCode})
	resp, body := h.postJSON(RegisterPath, `{"client_name":"Claude","redirect_uris":["`+claudeCB+`","http://127.0.0.1:5555/cb"],
		"grant_types":["authorization_code","refresh_token"],"response_types":["code"],"token_endpoint_auth_method":"client_secret_basic","logo_uri":"https://x/y.png"}`)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("register: %d %s", resp.StatusCode, body)
	}
	m := jsonMap(t, body)
	id, _ := m["client_id"].(string)
	if !strings.HasPrefix(id, "wbc_") || m["client_id_issued_at"] == nil || m["client_name"] != "Claude" {
		t.Fatalf("response: %v", m)
	}
	if m["token_endpoint_auth_method"] != "none" || m["client_secret"] != nil || m["scope"] != "mcp" {
		t.Fatalf("public client expected: %v", m)
	}
	if uris, _ := m["redirect_uris"].([]any); len(uris) != 2 {
		t.Fatalf("redirect_uris: %v", m["redirect_uris"])
	}
	if !h.hasAudit("oauth.register client=" + id) {
		t.Fatalf("audit: %v", h.auditLog())
	}
}

func TestRegisterRejections(t *testing.T) {
	h := newHarness(t, harnessOpts{code: testCode})
	for name, tc := range map[string]struct{ body, err string }{
		"http non-loopback":   {`{"redirect_uris":["http://example.com/cb"]}`, "invalid_redirect_uri"},
		"custom scheme":       {`{"redirect_uris":["myapp://callback"]}`, "invalid_redirect_uri"},
		"fragment":            {`{"redirect_uris":["https://example.com/cb#x"]}`, "invalid_redirect_uri"},
		"relative":            {`{"redirect_uris":["/cb"]}`, "invalid_redirect_uri"},
		"credentials":         {`{"redirect_uris":["https://u:p@example.com/cb"]}`, "invalid_redirect_uri"},
		"loopback lookalike":  {`{"redirect_uris":["http://127.0.0.2/cb"]}`, "invalid_redirect_uri"},
		"none":                {`{"client_name":"x"}`, "invalid_redirect_uri"},
		"client credentials":  {`{"redirect_uris":["https://example.com/cb"],"grant_types":["client_credentials"]}`, "invalid_client_metadata"},
		"implicit":            {`{"redirect_uris":["https://example.com/cb"],"response_types":["token"]}`, "invalid_client_metadata"},
		"wrong type":          {`{"redirect_uris":"https://example.com/cb"}`, "invalid_client_metadata"},
		"not json":            {`redirect_uris=x`, "invalid_client_metadata"},
		"other scope":         {`{"redirect_uris":["https://example.com/cb"],"scope":"admin"}`, "invalid_client_metadata"},
		"control in the name": {`{"redirect_uris":["https://example.com/cb"],"client_name":"a\u0007b"}`, "invalid_client_metadata"},
	} {
		resp, body := h.postJSON(RegisterPath, tc.body)
		if resp.StatusCode != http.StatusBadRequest || jsonMap(t, body)["error"] != tc.err {
			t.Errorf("%s: %d %s, want 400 %s", name, resp.StatusCode, body, tc.err)
		}
	}
	resp, _ := h.do(http.MethodPost, RegisterPath, strings.NewReader(`{"redirect_uris":["https://example.com/cb"]}`), "text/plain")
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("wrong content type: %d", resp.StatusCode)
	}
	if resp, _ := h.get(RegisterPath); resp.StatusCode != http.StatusMethodNotAllowed {
		t.Errorf("GET register: %d", resp.StatusCode)
	}
}

func TestRegisterRateLimit(t *testing.T) {
	h := newHarness(t, harnessOpts{code: testCode})
	for i := 0; i < 20; i++ {
		h.register(claudeCB)
	}
	resp, _ := h.postJSON(RegisterPath, `{"redirect_uris":["`+claudeCB+`"]}`)
	if resp.StatusCode != http.StatusTooManyRequests || resp.Header.Get("Retry-After") == "" {
		t.Fatalf("21st registration in an hour: %d", resp.StatusCode)
	}
}

func TestPruneClientsWithoutGrant(t *testing.T) {
	h := newHarness(t, harnessOpts{code: testCode})
	idle := h.register(claudeCB)
	f := h.fullFlow() // this client has a grant
	h.advance(25 * time.Hour)
	h.register(claudeCB) // triggers pruning
	p := newPKCE()
	resp, page := h.get(AuthorizePath + "?" + authzQuery(idle, claudeCB, p, "s").Encode())
	if resp.StatusCode != http.StatusBadRequest || !strings.Contains(page, "not registered") {
		t.Fatalf("idle client should be pruned: %d", resp.StatusCode)
	}
	resp, page = h.get(AuthorizePath+"?"+authzQuery(f.clientID, claudeCB, p, "s").Encode(), withCookie(f.cookie))
	if resp.StatusCode != http.StatusOK || !strings.Contains(page, `value="allow"`) {
		t.Fatalf("client with a grant must survive: %d", resp.StatusCode)
	}
}

// ---- authorize: sign-in and consent -------------------------------------------------------------

func TestAuthorizeSignInThenConsent(t *testing.T) {
	h := newHarness(t, harnessOpts{code: testCode})
	id := h.register(claudeCB)
	p := newPKCE()
	q := authzQuery(id, claudeCB, p, "opaque/state+value=1")

	resp, page := h.get(AuthorizePath + "?" + q.Encode())
	if resp.StatusCode != http.StatusOK || !strings.Contains(page, `name="access_code"`) || !strings.Contains(page, "Test Client") ||
		strings.Contains(page, `value="allow"`) {
		t.Fatalf("without a session the sign-in form is expected: %d\n%s", resp.StatusCode, page)
	}
	csp := resp.Header.Get("Content-Security-Policy")
	if !strings.Contains(csp, "default-src 'none'") || strings.Contains(page, "<script") || strings.Contains(page, "style=") {
		t.Fatalf("page CSP: %q", csp)
	}

	form := hiddenFields(page)
	if form.Get("state") != "opaque/state+value=1" || form.Get("code_challenge") != p.challenge {
		t.Fatalf("hidden fields must round-trip: %v", form)
	}
	form.Set("access_code", "wrong")
	resp, page = h.postForm(AuthorizePath, form)
	if resp.StatusCode != http.StatusUnauthorized || !strings.Contains(page, "not right") || sessionCookie(resp) != nil {
		t.Fatalf("wrong code: %d", resp.StatusCode)
	}
	if !h.hasAudit("auth.login_failed") {
		t.Fatal("failed login not audited")
	}

	form.Set("access_code", testCode)
	resp, page = h.postForm(AuthorizePath, form)
	cookie := sessionCookie(resp)
	if resp.StatusCode != http.StatusOK || cookie == nil || !cookie.HttpOnly || !strings.Contains(page, `value="allow"`) ||
		!strings.Contains(page, testName) || !strings.Contains(page, "claude.ai") ||
		!strings.Contains(strings.ToLower(page), "read-only access to the chats this bridge stores; it can never send") {
		t.Fatalf("right code should show consent: %d %v\n%s", resp.StatusCode, cookie, page)
	}
	if csp := resp.Header.Get("Content-Security-Policy"); !strings.Contains(csp, "form-action 'self' https://claude.ai;") {
		t.Fatalf("consent CSP must allow the redirect target: %q", csp)
	}

	// With the session a fresh authorize goes straight to consent.
	resp, page = h.get(AuthorizePath+"?"+q.Encode(), withCookie(cookie))
	if resp.StatusCode != http.StatusOK || !strings.Contains(page, `value="allow"`) || strings.Contains(page, `name="access_code"`) {
		t.Fatalf("signed in: %d", resp.StatusCode)
	}

	// Deny.
	u := h.decide(page, cookie, "deny")
	if !strings.HasPrefix(u.String(), claudeCB+"?") || u.Query().Get("error") != "access_denied" ||
		u.Query().Get("state") != "opaque/state+value=1" || u.Query().Get("iss") != h.url || u.Query().Get("code") != "" {
		t.Fatalf("deny redirect: %s", u)
	}
	// Allow.
	u = h.decide(page, cookie, "allow")
	if u.Query().Get("code") == "" || u.Query().Get("state") != "opaque/state+value=1" || u.Query().Get("iss") != h.url {
		t.Fatalf("allow redirect: %s", u)
	}
	for _, typ := range []string{"auth.login ", "oauth.consent_denied client=" + id, "oauth.consent_granted client=" + id} {
		if !h.hasAudit(typ) {
			t.Fatalf("audit lacks %q: %v", typ, h.auditLog())
		}
	}
}

func TestConsentRejectsCrossOriginPost(t *testing.T) {
	h := newHarness(t, harnessOpts{code: testCode})
	id := h.register(claudeCB)
	cookie, consent := h.signIn(authzQuery(id, claudeCB, newPKCE(), "s"))
	form := hiddenFields(consent)
	form.Set("action", "allow")
	for name, m := range map[string]mod{
		"sec-fetch-site": func(r *http.Request) { r.Header.Set("Sec-Fetch-Site", "cross-site") },
		"origin":         func(r *http.Request) { r.Header.Set("Origin", "https://evil.example") },
	} {
		resp, _ := h.postForm(AuthorizePath, form, withCookie(cookie), m)
		if resp.StatusCode != http.StatusForbidden || resp.Header.Get("Location") != "" {
			t.Errorf("%s: cross-origin consent must be refused: %d %q", name, resp.StatusCode, resp.Header.Get("Location"))
		}
	}
	// Without a session the POST shows the sign-in form again, never a code.
	resp, page := h.postForm(AuthorizePath, form)
	if resp.StatusCode != http.StatusUnauthorized || resp.Header.Get("Location") != "" || !strings.Contains(page, `name="access_code"`) {
		t.Fatalf("consent without session: %d", resp.StatusCode)
	}
}

func TestOpenModeSkipsSignInButAsksConsent(t *testing.T) {
	h := newHarness(t, harnessOpts{code: "", derive: true})
	id := h.register(loopbackCB)
	p := newPKCE()
	q := authzQuery(id, loopbackUsed, p, "s1")
	resp, page := h.get(AuthorizePath + "?" + q.Encode())
	if resp.StatusCode != http.StatusOK || strings.Contains(page, `name="access_code"`) || !strings.Contains(page, `value="allow"`) {
		t.Fatalf("open mode should show consent directly: %d\n%s", resp.StatusCode, page)
	}
	if !strings.Contains(page, "runs on a computer") {
		t.Fatal("loopback redirect should carry the local-program warning")
	}
	if csp := resp.Header.Get("Content-Security-Policy"); !strings.Contains(csp, "form-action 'self' http://localhost:43123;") {
		t.Fatalf("CSP: %q", csp)
	}
	u := h.decide(page, nil, "allow")
	if !strings.HasPrefix(u.String(), loopbackUsed+"?") || u.Query().Get("iss") != h.url {
		t.Fatalf("redirect: %s", u)
	}
	status, tok := h.exchange(id, u.Query().Get("code"), loopbackUsed, p.verifier, nil)
	if status != http.StatusOK {
		t.Fatalf("exchange: %d %v", status, tok)
	}
	if resp := h.ping(tok["access_token"].(string)); resp.StatusCode != http.StatusOK {
		t.Fatalf("mcp with token (derived issuer): %d", resp.StatusCode)
	}
}

func TestLoopbackRedirectIgnoresPortOnly(t *testing.T) {
	cl := client{RedirectURIs: []string{"http://localhost/callback", "http://127.0.0.1/callback", claudeCB}}
	for uri, ok := range map[string]bool{
		"http://localhost:43123/callback":             true,
		"http://127.0.0.1:1/callback":                 true,
		"http://localhost/callback":                   true,
		claudeCB:                                      true,
		"http://localhost:43123/callback/":            false,
		"http://localhost:43123/other":                false,
		"http://127.0.0.1:43123/callback?x=1":         false,
		"https://localhost:43123/callback":            false,
		"http://[::1]:43123/callback":                 false,
		"https://claude.ai/api/mcp/auth_callback/":    false,
		"https://claude.ai:444/api/mcp/auth_callback": false,
	} {
		if cl.allowsRedirect(uri) != ok {
			t.Errorf("allowsRedirect(%q) = %v, want %v", uri, !ok, ok)
		}
	}
}

func TestUntrustedRedirectNeverRedirects(t *testing.T) {
	h := newHarness(t, harnessOpts{code: testCode})
	id := h.register(claudeCB)
	p := newPKCE()
	cases := map[string]url.Values{
		"unknown client":      authzQuery("wbc_doesnotexist", claudeCB, p, "s"),
		"missing client":      authzQuery("", claudeCB, p, "s"),
		"unregistered target": authzQuery(id, "https://evil.example/cb", p, "s"),
		"missing redirect":    authzQuery(id, "", p, "s"),
		"http client_id":      authzQuery("http://example.com/client.json", claudeCB, p, "s"),
		"bad response_type too": func() url.Values {
			q := authzQuery(id, "https://evil.example/cb", p, "s")
			q.Set("response_type", "token")
			return q
		}(),
		"repeated redirect": func() url.Values {
			q := authzQuery(id, claudeCB, p, "s")
			q.Add("redirect_uri", "https://evil.example/cb")
			return q
		}(),
	}
	for name, q := range cases {
		resp, page := h.get(AuthorizePath + "?" + q.Encode())
		if resp.StatusCode != http.StatusBadRequest || resp.Header.Get("Location") != "" || !strings.Contains(page, "cannot be used") {
			t.Errorf("%s: want an error page and no redirect, got %d %q", name, resp.StatusCode, resp.Header.Get("Location"))
		}
	}
	// A consent form tampered to point elsewhere is refused the same way.
	cookie, consent := h.signIn(authzQuery(id, claudeCB, p, "s"))
	form := hiddenFields(consent)
	form.Set("redirect_uri", "https://evil.example/cb")
	form.Set("action", "allow")
	resp, _ := h.postForm(AuthorizePath, form, withCookie(cookie))
	if resp.StatusCode != http.StatusBadRequest || resp.Header.Get("Location") != "" {
		t.Fatalf("tampered consent: %d %q", resp.StatusCode, resp.Header.Get("Location"))
	}
}

func TestAuthorizeErrorsRedirectOnceTrusted(t *testing.T) {
	h := newHarness(t, harnessOpts{code: testCode})
	id := h.register(claudeCB)
	p := newPKCE()
	for name, tc := range map[string]struct {
		edit func(url.Values)
		err  string
	}{
		"no challenge":    {func(q url.Values) { q.Del("code_challenge") }, "invalid_request"},
		"plain":           {func(q url.Values) { q.Set("code_challenge_method", "plain") }, "invalid_request"},
		"no method":       {func(q url.Values) { q.Del("code_challenge_method") }, "invalid_request"},
		"short challenge": {func(q url.Values) { q.Set("code_challenge", "abc") }, "invalid_request"},
		"token flow":      {func(q url.Values) { q.Set("response_type", "token") }, "unsupported_response_type"},
		"other scope":     {func(q url.Values) { q.Set("scope", "mcp admin") }, "invalid_scope"},
		"wrong resource":  {func(q url.Values) { q.Set("resource", "https://evil.example/mcp") }, "invalid_target"},
		"root resource":   {func(q url.Values) { q.Set("resource", h.url) }, "invalid_target"},
	} {
		q := authzQuery(id, claudeCB, p, "keep-me")
		tc.edit(q)
		resp, _ := h.get(AuthorizePath + "?" + q.Encode())
		loc, _ := url.Parse(resp.Header.Get("Location"))
		if resp.StatusCode != http.StatusFound || loc == nil || loc.Query().Get("error") != tc.err ||
			loc.Query().Get("state") != "keep-me" || loc.Query().Get("iss") != h.url || !strings.HasPrefix(loc.String(), claudeCB) {
			t.Errorf("%s: %d %s, want redirect with %s", name, resp.StatusCode, resp.Header.Get("Location"), tc.err)
		}
	}
	// No state is fine (PKCE is mandatory) and none is invented.
	q := authzQuery(id, claudeCB, p, "")
	q.Set("response_type", "token")
	resp, _ := h.get(AuthorizePath + "?" + q.Encode())
	if loc, _ := url.Parse(resp.Header.Get("Location")); loc == nil || loc.Query().Has("state") {
		t.Fatalf("stateless error redirect: %q", resp.Header.Get("Location"))
	}
}

// ---- token endpoint ---------------------------------------------------------------------------

func TestCodeExchangePKCEAndReuse(t *testing.T) {
	h := newHarness(t, harnessOpts{code: testCode})
	id := h.register(claudeCB)
	p := newPKCE()
	cookie, consent := h.signIn(authzQuery(id, claudeCB, p, "s"))
	code := h.decide(consent, cookie, "allow").Query().Get("code")

	if status, m := h.exchange(id, code, claudeCB, newPKCE().verifier, nil); status != http.StatusBadRequest || m["error"] != "invalid_grant" {
		t.Fatalf("wrong verifier: %d %v", status, m)
	}
	if status, m := h.exchange("wbc_other", code, claudeCB, p.verifier, nil); status != http.StatusBadRequest || m["error"] != "invalid_grant" {
		t.Fatalf("other client: %d %v", status, m)
	}
	if status, m := h.exchange(id, code, claudeCB+"x", p.verifier, nil); status != http.StatusBadRequest || m["error"] != "invalid_grant" {
		t.Fatalf("other redirect_uri: %d %v", status, m)
	}
	if status, m := h.exchange(id, code, claudeCB, p.verifier, url.Values{"resource": {"https://evil.example/mcp"}}); status != http.StatusBadRequest || m["error"] != "invalid_target" {
		t.Fatalf("other resource: %d %v", status, m)
	}
	// Failed attempts do not burn the code for its real owner.
	status, tok := h.exchange(id, code, claudeCB, p.verifier, nil)
	if status != http.StatusOK {
		t.Fatalf("exchange: %d %v", status, tok)
	}
	at, _ := tok["access_token"].(string)
	rt, _ := tok["refresh_token"].(string)
	if !strings.HasPrefix(at, AccessTokenPrefix) || len(at) != len(AccessTokenPrefix)+43 || !strings.HasPrefix(rt, RefreshTokenPrefix) ||
		tok["token_type"] != "Bearer" || tok["expires_in"] != float64(3600) || tok["scope"] != "mcp" {
		t.Fatalf("token response: %v", tok)
	}
	if resp := h.ping(at); resp.StatusCode != http.StatusOK {
		t.Fatalf("access token on /mcp: %d", resp.StatusCode)
	}
	// Reuse: refused, and everything issued from the code is revoked.
	if status, m := h.exchange(id, code, claudeCB, p.verifier, nil); status != http.StatusBadRequest || m["error"] != "invalid_grant" {
		t.Fatalf("code reuse: %d %v", status, m)
	}
	if resp := h.ping(at); resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("token from a replayed code must die: %d", resp.StatusCode)
	}
	if !h.hasAudit("oauth.code_reuse") || !h.hasAudit("oauth.token_issued grant=oa_") {
		t.Fatalf("audit: %v", h.auditLog())
	}
}

func TestCodeExpires(t *testing.T) {
	h := newHarness(t, harnessOpts{code: testCode})
	id := h.register(claudeCB)
	p := newPKCE()
	cookie, consent := h.signIn(authzQuery(id, claudeCB, p, "s"))
	code := h.decide(consent, cookie, "allow").Query().Get("code")
	h.advance(11 * time.Minute)
	if status, m := h.exchange(id, code, claudeCB, p.verifier, nil); status != http.StatusBadRequest || m["error"] != "invalid_grant" {
		t.Fatalf("expired code: %d %v", status, m)
	}
}

func TestTokenEndpointRequests(t *testing.T) {
	h := newHarness(t, harnessOpts{code: testCode})
	resp, body := h.postJSON(TokenPath, `{"grant_type":"authorization_code"}`)
	if resp.StatusCode != http.StatusBadRequest || jsonMap(t, body)["error"] != "invalid_request" {
		t.Fatalf("JSON body: %d %s", resp.StatusCode, body)
	}
	resp, body = h.postForm(TokenPath, url.Values{"grant_type": {"client_credentials"}, "client_id": {"x"}})
	if resp.StatusCode != http.StatusBadRequest || jsonMap(t, body)["error"] != "unsupported_grant_type" {
		t.Fatalf("client_credentials: %d %s", resp.StatusCode, body)
	}
	resp, body = h.postForm(TokenPath, url.Values{"grant_type": {"authorization_code"}, "code": {"x"}, "code_verifier": {"y"}})
	if resp.StatusCode != http.StatusUnauthorized || jsonMap(t, body)["error"] != "invalid_client" {
		t.Fatalf("no client_id: %d %s", resp.StatusCode, body)
	}
	if resp.Header.Get("Cache-Control") != "no-store" {
		t.Fatalf("token responses must not be cached: %v", resp.Header)
	}
}

func TestAccessTokenOnMCPAndAudit(t *testing.T) {
	h := newHarness(t, harnessOpts{code: testCode})
	f := h.fullFlow()
	text := h.callTool(f.access, "get_status")
	if !strings.Contains(text, testName) && !strings.Contains(text, "as_of") {
		t.Fatalf("get_status: %s", text)
	}
	grants := h.grants(f.cookie)
	if len(grants) != 1 || !strings.HasPrefix(grants[0].ID, "oa_") {
		t.Fatalf("grants: %+v", grants)
	}
	var seen bool
	for _, e := range h.toolEvents() {
		if e.Tool == "get_status" {
			seen = true
			if e.KeyID != grants[0].ID || e.KeyLabel != "Test Client" {
				t.Fatalf("tool audit should carry the grant: %+v", e)
			}
			if !strings.Contains(e.Detail(), "key="+grants[0].ID) {
				t.Fatalf("detail: %s", e.Detail())
			}
		}
	}
	if !seen {
		t.Fatal("no audit event for the tool call")
	}
	// The verifier itself reports the principal shape.
	k, err := h.srv.Verifier(h.auth).VerifyKey(context.Background(), "Bearer "+f.access)
	if err != nil || k.ID != grants[0].ID || k.Label != "Test Client" || k.Kind != auth.KindMCP {
		t.Fatalf("verify: %+v %v", k, err)
	}
	// Access tokens are for /mcp only: the web API does not take them.
	if resp, _ := h.get("/api/sessions", withBearer(f.access)); resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("access token on /api: %d", resp.StatusCode)
	}
	// Bearer keys keep working through the composite verifier.
	_, key, _ := h.auth.CreateKey(context.Background(), "Codex", auth.KindMCP)
	if resp := h.ping(key); resp.StatusCode != http.StatusOK {
		t.Fatalf("bearer key: %d", resp.StatusCode)
	}
}

func TestExpiredAndRebound(t *testing.T) {
	h := newHarness(t, harnessOpts{code: testCode})
	f := h.fullFlow()
	h.advance(61 * time.Minute)
	resp := h.ping(f.access)
	if resp.StatusCode != http.StatusUnauthorized || !strings.Contains(resp.Header.Get("WWW-Authenticate"), `error="invalid_token"`) {
		t.Fatalf("expired token: %d %q", resp.StatusCode, resp.Header.Get("WWW-Authenticate"))
	}
	// The refresh token still works after the access token expired.
	status, tok := h.refresh(f.clientID, f.refresh)
	if status != http.StatusOK {
		t.Fatalf("refresh: %d %v", status, tok)
	}
	at := tok["access_token"].(string)
	if resp := h.ping(at); resp.StatusCode != http.StatusOK {
		t.Fatalf("refreshed token: %d", resp.StatusCode)
	}
	// A token bound to another resource is refused (audience check).
	if _, err := h.db.Exec(`UPDATE bridge_oauth_tokens SET resource = 'https://other.example/mcp'`); err != nil {
		t.Fatal(err)
	}
	if resp := h.ping(at); resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("token for another resource: %d", resp.StatusCode)
	}
}

func TestRefreshRotationAndReuse(t *testing.T) {
	h := newHarness(t, harnessOpts{code: testCode})
	f := h.fullFlow()

	if status, m := h.refresh("wbc_someone_else", f.refresh); status != http.StatusBadRequest || m["error"] != "invalid_grant" {
		t.Fatalf("other client: %d %v", status, m)
	}
	status, t2 := h.refresh(f.clientID, f.refresh)
	if status != http.StatusOK {
		t.Fatalf("refresh: %d %v", status, t2)
	}
	at2, rt2 := t2["access_token"].(string), t2["refresh_token"].(string)
	if at2 == f.access || rt2 == f.refresh || !strings.HasPrefix(rt2, RefreshTokenPrefix) {
		t.Fatalf("rotation must issue new tokens: %v", t2)
	}
	if resp := h.ping(at2); resp.StatusCode != http.StatusOK {
		t.Fatalf("new access token: %d", resp.StatusCode)
	}
	// Second rotation works; the chain continues.
	status, t3 := h.refresh(f.clientID, rt2)
	if status != http.StatusOK {
		t.Fatalf("second refresh: %d %v", status, t3)
	}
	at3, rt3 := t3["access_token"].(string), t3["refresh_token"].(string)

	// Replaying an old refresh token revokes the whole family.
	if status, m := h.refresh(f.clientID, f.refresh); status != http.StatusBadRequest || m["error"] != "invalid_grant" {
		t.Fatalf("reuse: %d %v", status, m)
	}
	for _, at := range []string{f.access, at2, at3} {
		if resp := h.ping(at); resp.StatusCode != http.StatusUnauthorized {
			t.Fatalf("family member still valid after reuse: %d", resp.StatusCode)
		}
	}
	if status, m := h.refresh(f.clientID, rt3); status != http.StatusBadRequest || m["error"] != "invalid_grant" {
		t.Fatalf("newest refresh token after reuse: %d %v", status, m)
	}
	if !h.hasAudit("oauth.refresh grant=") || !h.hasAudit("oauth.refresh_reuse grant=") {
		t.Fatalf("audit: %v", h.auditLog())
	}
	if len(h.grants(f.cookie)) != 0 {
		t.Fatal("a revoked grant must not be listed")
	}
}

func TestRevokeEndpoint(t *testing.T) {
	h := newHarness(t, harnessOpts{code: testCode})
	f := h.fullFlow()
	if resp, _ := h.postForm(RevokePath, url.Values{"token": {"wb_rt_unknown"}}); resp.StatusCode != http.StatusOK {
		t.Fatalf("unknown token: %d", resp.StatusCode)
	}
	if resp, _ := h.postForm(RevokePath, url.Values{"token": {f.refresh}, "client_id": {"wbc_other"}}); resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("other client: %d", resp.StatusCode)
	}
	if resp := h.ping(f.access); resp.StatusCode != http.StatusOK {
		t.Fatal("refused revocation must not revoke")
	}
	if resp, body := h.postForm(RevokePath, url.Values{"token": {f.refresh}, "token_type_hint": {"refresh_token"}, "client_id": {f.clientID}}); resp.StatusCode != http.StatusOK {
		t.Fatalf("revoke: %d %s", resp.StatusCode, body)
	}
	if resp := h.ping(f.access); resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("access token after revoking the refresh token: %d", resp.StatusCode)
	}
	if status, _ := h.refresh(f.clientID, f.refresh); status != http.StatusBadRequest {
		t.Fatalf("revoked refresh token: %d", status)
	}
	// Revoking by access token works the same way.
	g := h.fullFlow()
	if resp, _ := h.postForm(RevokePath, url.Values{"token": {g.access}}); resp.StatusCode != http.StatusOK {
		t.Fatal("revoke by access token")
	}
	if status, _ := h.refresh(g.clientID, g.refresh); status != http.StatusBadRequest {
		t.Fatalf("family of a revoked access token: %d", status)
	}
	if !h.hasAudit("oauth.revoke grant=") {
		t.Fatalf("audit: %v", h.auditLog())
	}
}

// ---- grants API ---------------------------------------------------------------------------

func TestGrantsAPI(t *testing.T) {
	h := newHarness(t, harnessOpts{code: testCode})
	f := h.fullFlow()
	_ = h.callTool(f.access, "get_status")
	grants := h.grants(f.cookie)
	if len(grants) != 1 || grants[0].ClientName != "Test Client" || grants[0].ClientID != f.clientID ||
		grants[0].CreatedAt.IsZero() || grants[0].LastUsedAt == nil {
		t.Fatalf("grants: %+v", grants)
	}
	_, raw := h.get(GrantsPath, withCookie(f.cookie))
	var shape []map[string]any
	_ = json.Unmarshal([]byte(raw), &shape)
	for _, k := range []string{"id", "client_name", "client_id", "created_at", "last_used_at"} {
		if _, ok := shape[0][k]; !ok {
			t.Fatalf("grant JSON lacks %s: %s", k, raw)
		}
	}

	if resp, _ := h.get(GrantsPath); resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("anonymous: %d", resp.StatusCode)
	}
	_, key, _ := h.auth.CreateKey(context.Background(), "script", auth.KindMCP)
	if resp, _ := h.get(GrantsPath, withBearer(key)); resp.StatusCode != http.StatusForbidden {
		t.Fatalf("bearer key listing grants: %d", resp.StatusCode)
	}
	if resp, _ := h.get(GrantsPath, withBearer(f.access)); resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("access token listing grants: %d", resp.StatusCode)
	}
	if resp, _ := h.do(http.MethodDelete, GrantsPath+"/"+grants[0].ID, nil, "", withCookie(f.cookie),
		func(r *http.Request) { r.Header.Set("Sec-Fetch-Site", "cross-site") }); resp.StatusCode != http.StatusForbidden {
		t.Fatalf("cross-site delete: %d", resp.StatusCode)
	}
	if resp, _ := h.do(http.MethodDelete, GrantsPath+"/"+grants[0].ID, nil, "", withCookie(f.cookie)); resp.StatusCode != http.StatusNoContent {
		t.Fatalf("delete: %d", resp.StatusCode)
	}
	if resp := h.ping(f.access); resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("token of a deleted grant: %d", resp.StatusCode)
	}
	if status, _ := h.refresh(f.clientID, f.refresh); status != http.StatusBadRequest {
		t.Fatalf("refresh of a deleted grant: %d", status)
	}
	if len(h.grants(f.cookie)) != 0 {
		t.Fatal("deleted grant still listed")
	}
	if resp, _ := h.do(http.MethodDelete, GrantsPath+"/"+grants[0].ID, nil, "", withCookie(f.cookie)); resp.StatusCode != http.StatusNotFound {
		t.Fatalf("second delete: %d", resp.StatusCode)
	}
	if !h.hasAudit("oauth.revoke grant=" + grants[0].ID + " via=ui") {
		t.Fatalf("audit: %v", h.auditLog())
	}
}

// ---- audit hygiene ----------------------------------------------------------------------------

func TestAuditNeverCarriesSecrets(t *testing.T) {
	h := newHarness(t, harnessOpts{code: testCode})
	f := h.fullFlow()
	_, t2 := h.refresh(f.clientID, f.refresh)
	h.refresh(f.clientID, f.refresh) // reuse
	h.postForm(RevokePath, url.Values{"token": {t2["refresh_token"].(string)}})
	secrets := []string{f.access, f.refresh, f.code, f.pkce.verifier, f.pkce.challenge, testCode, t2["access_token"].(string), t2["refresh_token"].(string), f.cookie.Value}
	for _, line := range h.auditLog() {
		for _, s := range secrets {
			if strings.Contains(line, s) {
				t.Fatalf("audit line leaks a secret: %q", line)
			}
		}
	}
	if len(h.auditLog()) < 6 {
		t.Fatalf("expected a full audit trail: %v", h.auditLog())
	}
}

// ---- Client ID Metadata Documents --------------------------------------------------------------

type docServer struct {
	ts    *httptest.Server
	hits  atomic.Int32
	roots *x509.CertPool
}

func newDocServer(t *testing.T) *docServer {
	d := &docServer{}
	mux := http.NewServeMux()
	doc := func(clientID string, extra map[string]any) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			d.hits.Add(1)
			m := map[string]any{
				"client_id": clientID, "client_name": "Claude Code", "client_uri": "https://claude.ai",
				"redirect_uris": []string{"http://localhost/callback", "http://127.0.0.1/callback"},
				"grant_types":   []string{"authorization_code", "refresh_token"}, "response_types": []string{"code"},
				"token_endpoint_auth_method": "none",
			}
			for k, v := range extra {
				m[k] = v
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(m)
		}
	}
	d.ts = httptest.NewTLSServer(mux)
	t.Cleanup(d.ts.Close)
	mux.HandleFunc("/good", doc(d.ts.URL+"/good", nil))
	mux.HandleFunc("/mismatch", doc("https://claude.ai/oauth/claude-code-client-metadata", nil))
	mux.HandleFunc("/secret", doc(d.ts.URL+"/secret", map[string]any{"client_secret": "s3cret"}))
	mux.HandleFunc("/noredirects", doc(d.ts.URL+"/noredirects", map[string]any{"redirect_uris": []string{"myapp://cb", "http://example.com/cb"}}))
	mux.HandleFunc("/moved", func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, "/good", http.StatusFound) })
	mux.HandleFunc("/huge", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"client_id":"` + strings.Repeat("x", 70<<10) + `"}`))
	})
	d.roots = x509.NewCertPool()
	d.roots.AddCert(d.ts.Certificate())
	return d
}

func TestCIMD(t *testing.T) {
	d := newDocServer(t)
	allowAll := func(netip.Addr) bool { return true }
	h := newHarness(t, harnessOpts{code: testCode, cimdRoots: d.roots, cimdAllow: allowAll})
	good := d.ts.URL + "/good"
	p := newPKCE()

	q := authzQuery(good, loopbackUsed, p, "s")
	resp, page := h.get(AuthorizePath + "?" + q.Encode())
	if resp.StatusCode != http.StatusOK || !strings.Contains(page, "Claude Code") || !strings.Contains(page, "Identity published by 127.0.0.1") {
		t.Fatalf("valid document: %d\n%s", resp.StatusCode, page)
	}
	// Cached: a second request does not fetch again.
	cookie, consent := h.signIn(q)
	if d.hits.Load() != 1 {
		t.Fatalf("document fetched %d times, want 1 (cache)", d.hits.Load())
	}
	code := h.decide(consent, cookie, "allow").Query().Get("code")
	status, tok := h.exchange(good, code, loopbackUsed, p.verifier, nil)
	if status != http.StatusOK {
		t.Fatalf("CIMD client exchange (public client): %d %v", status, tok)
	}
	if g := h.grants(cookie); len(g) != 1 || g[0].ClientID != good || g[0].ClientName != "Claude Code" || g[0].LastUsedAt != nil {
		t.Fatalf("grant: %+v", g)
	}
	// The document's own redirect list is enforced.
	resp, _ = h.get(AuthorizePath + "?" + authzQuery(good, claudeCB, p, "s").Encode())
	if resp.StatusCode != http.StatusBadRequest || resp.Header.Get("Location") != "" {
		t.Fatalf("redirect outside the document: %d", resp.StatusCode)
	}

	for name, tc := range map[string]struct{ id, want string }{
		"mismatched client_id": {d.ts.URL + "/mismatch", "does not match"},
		"client secret":        {d.ts.URL + "/secret", "client_secret"},
		"no usable redirects":  {d.ts.URL + "/noredirects", "no usable redirect_uris"},
		"redirect":             {d.ts.URL + "/moved", "could not be fetched"},
		"too large":            {d.ts.URL + "/huge", "64 KB"},
		"missing":              {d.ts.URL + "/nope", "HTTP 404"},
		"non-https":            {strings.Replace(good, "https://", "http://", 1), "non-https"},
		"no path":              {d.ts.URL + "/", "must have a path"},
		"dot segment":          {d.ts.URL + "/a/../good", ". or .."},
	} {
		resp, page := h.get(AuthorizePath + "?" + authzQuery(tc.id, loopbackUsed, p, "s").Encode())
		if resp.StatusCode != http.StatusBadRequest || resp.Header.Get("Location") != "" || !strings.Contains(page, tc.want) {
			t.Errorf("%s: %d %q, want error page containing %q\n%s", name, resp.StatusCode, resp.Header.Get("Location"), tc.want, page)
		}
	}
}

func TestCIMDRefusesPrivateAddresses(t *testing.T) {
	d := newDocServer(t)
	h := newHarness(t, harnessOpts{code: testCode, cimdRoots: d.roots}) // default: public addresses only
	p := newPKCE()
	for _, id := range []string{d.ts.URL + "/good", "https://localhost/client.json", "https://10.1.2.3/client.json", "https://[::1]/c.json", "https://printer.local/c.json"} {
		resp, page := h.get(AuthorizePath + "?" + authzQuery(id, loopbackUsed, p, "s").Encode())
		if resp.StatusCode != http.StatusBadRequest || !strings.Contains(page, "private address") {
			t.Errorf("%s: %d, want refusal\n%s", id, resp.StatusCode, page)
		}
	}
	if d.hits.Load() != 0 {
		t.Fatalf("a private document must not be fetched: %d", d.hits.Load())
	}
}

func TestDialGuard(t *testing.T) {
	for addr, ok := range map[string]bool{
		"8.8.8.8:443": true, "[2606:4700:4700::1111]:443": true, "160.79.104.10:443": true,
		"127.0.0.1:443": false, "10.0.0.1:443": false, "172.16.5.4:443": false, "192.168.1.1:443": false,
		"100.100.1.1:443": false, "169.254.169.254:80": false, "0.0.0.0:443": false, "[::1]:443": false,
		"[fe80::1]:443": false, "[fd00::1]:443": false, "[::ffff:127.0.0.1]:443": false, "[64:ff9b::a00:1]:443": false,
		"[64:ff9b::808:808]:443": true, "224.0.0.1:443": false, "localhost:443": false,
	} {
		if err := checkDialAddr(addr, publicAddr); (err == nil) != ok {
			t.Errorf("checkDialAddr(%s) = %v, want allowed=%v", addr, err, ok)
		}
	}
}
