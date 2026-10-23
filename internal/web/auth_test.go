package web

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/vid-t/whatsapp-bridge/internal/auth"
	"github.com/vid-t/whatsapp-bridge/internal/store"
)

func newAuthStore(t *testing.T, code string) *auth.Store {
	t.Helper()
	db, err := store.Open(filepath.Join(t.TempDir(), "auth.db"), "")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	st, err := auth.New(context.Background(), db, auth.Options{AccessCode: code, Name: "Test Team"})
	if err != nil {
		t.Fatal(err)
	}
	return st
}

func gatedHandler(t *testing.T, opts Options) (http.Handler, *fakeManager) {
	t.Helper()
	f := newFakeManager()
	s := newServer(f, slog.New(slog.NewTextHandler(io.Discard, nil)), opts)
	return s.handler(), f
}

func call(h http.Handler, method, path, body string, mod func(*http.Request)) *httptest.ResponseRecorder {
	var rd io.Reader
	if body != "" {
		rd = strings.NewReader(body)
	}
	r := httptest.NewRequest(method, path, rd)
	r.Host = "127.0.0.1:8080"
	if body != "" {
		r.Header.Set("Content-Type", "application/json")
	}
	if mod != nil {
		mod(r)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

func TestGatedModeFlow(t *testing.T) {
	st := newAuthStore(t, "open-sesame")
	var audited []string
	h, _ := gatedHandler(t, Options{Auth: st, Version: "test", Audit: func(typ, _ string) { audited = append(audited, typ) }})

	// Anonymous: API denied, pages served, /api/me open.
	if w := call(h, "GET", "/api/sessions", "", nil); w.Code != http.StatusUnauthorized {
		t.Fatalf("anonymous /api/sessions: %d %s", w.Code, w.Body)
	}
	if w := call(h, "GET", "/", "", nil); w.Code != http.StatusOK {
		t.Fatalf("page should be served: %d", w.Code)
	}
	w := call(h, "GET", "/api/me", "", nil)
	var me map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &me)
	if me["auth_required"] != true || me["authenticated"] != false || me["version"] != "test" {
		t.Fatalf("me anonymous: %v", me)
	}

	// Wrong then right code.
	if w := call(h, "POST", "/auth/login", `{"code":"nope"}`, nil); w.Code != http.StatusUnauthorized {
		t.Fatalf("wrong code: %d", w.Code)
	}
	w = call(h, "POST", "/auth/login", `{"code":"open-sesame"}`, nil)
	if w.Code != http.StatusNoContent {
		t.Fatalf("login: %d %s", w.Code, w.Body)
	}
	var cookie *http.Cookie
	for _, c := range w.Result().Cookies() {
		if strings.Contains(c.Name, "bridge_session") {
			cookie = c
		}
	}
	if cookie == nil || !cookie.HttpOnly {
		t.Fatalf("expected HttpOnly session cookie, got %+v", w.Result().Cookies())
	}
	withCookie := func(r *http.Request) { r.AddCookie(cookie) }

	if w := call(h, "GET", "/api/sessions", "", withCookie); w.Code != http.StatusOK {
		t.Fatalf("session GET: %d", w.Code)
	}
	w = call(h, "GET", "/api/me", "", withCookie)
	_ = json.Unmarshal(w.Body.Bytes(), &me)
	if me["authenticated"] != true || me["principal"] != "session" {
		t.Fatalf("me session: %v", me)
	}

	// Keys: create with session, use as bearer, scope rules, revoke.
	w = call(h, "POST", "/api/keys", `{"label":"Claude","kind":"mcp"}`, withCookie)
	if w.Code != http.StatusCreated {
		t.Fatalf("create key: %d %s", w.Code, w.Body)
	}
	var created struct{ ID, Key string }
	_ = json.Unmarshal(w.Body.Bytes(), &created)
	if !strings.HasPrefix(created.Key, "wb_live_") {
		t.Fatalf("key format: %q", created.Key)
	}
	bearer := func(r *http.Request) { r.Header.Set("Authorization", "Bearer "+created.Key) }
	if w := call(h, "GET", "/api/sessions", "", bearer); w.Code != http.StatusOK {
		t.Fatalf("bearer GET: %d", w.Code)
	}
	if w := call(h, "POST", "/api/sessions/pair", "", bearer); w.Code != http.StatusForbidden {
		t.Fatalf("bearer POST should be forbidden: %d", w.Code)
	}
	if w := call(h, "GET", "/api/keys", "", bearer); w.Code != http.StatusForbidden {
		t.Fatalf("bearer must not manage keys: %d", w.Code)
	}
	if w := call(h, "POST", "/api/keys", `{"label":"x","kind":"bogus"}`, withCookie); w.Code != http.StatusBadRequest {
		t.Fatalf("bad kind: %d", w.Code)
	}
	w = call(h, "GET", "/api/keys", "", withCookie)
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), created.ID) || strings.Contains(w.Body.String(), created.Key) {
		t.Fatalf("list keys: %d %s", w.Code, w.Body)
	}
	if w := call(h, "DELETE", "/api/keys/"+created.ID, "", withCookie); w.Code != http.StatusNoContent {
		t.Fatalf("revoke: %d", w.Code)
	}
	w = call(h, "GET", "/api/sessions", "", bearer)
	if w.Code != http.StatusUnauthorized || w.Header().Get("WWW-Authenticate") == "" {
		t.Fatalf("revoked bearer: %d %q", w.Code, w.Header().Get("WWW-Authenticate"))
	}

	// Logout clears the session.
	if w := call(h, "POST", "/auth/logout", "", withCookie); w.Code != http.StatusNoContent {
		t.Fatalf("logout: %d", w.Code)
	}
	if w := call(h, "GET", "/api/sessions", "", withCookie); w.Code != http.StatusUnauthorized {
		t.Fatalf("after logout: %d", w.Code)
	}
	joined := strings.Join(audited, ",")
	for _, want := range []string{"auth.login_failed", "auth.login", "key.created", "key.revoked", "auth.logout"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("audit missing %s in %s", want, joined)
		}
	}
}

func TestLoginRateLimit(t *testing.T) {
	st := newAuthStore(t, "code")
	h, _ := gatedHandler(t, Options{Auth: st})
	var last *httptest.ResponseRecorder
	for i := 0; i < 6; i++ {
		last = call(h, "POST", "/auth/login", `{"code":"wrong"}`, nil)
	}
	if last.Code != http.StatusTooManyRequests || last.Header().Get("Retry-After") == "" {
		t.Fatalf("expected 429 with Retry-After, got %d", last.Code)
	}
}

func TestOpenMode(t *testing.T) {
	st := newAuthStore(t, "")
	h, _ := gatedHandler(t, Options{Auth: st})
	w := call(h, "GET", "/api/me", "", nil)
	var me map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &me)
	if me["auth_required"] != false || me["authenticated"] != true || me["principal"] != "open" {
		t.Fatalf("open mode me: %v", me)
	}
	if w := call(h, "POST", "/auth/login", `{"code":"x"}`, nil); w.Code != http.StatusConflict {
		t.Fatalf("login in open mode: %d", w.Code)
	}
	if w := call(h, "GET", "/api/sessions", "", nil); w.Code != http.StatusOK {
		t.Fatalf("open mode API: %d", w.Code)
	}
}

func TestKeyReadsAPI(t *testing.T) {
	st := newAuthStore(t, "code")
	h, _ := gatedHandler(t, Options{Auth: st})
	_, secret, _ := st.CreateKey(context.Background(), "script", auth.KindMCP)
	bearer := func(r *http.Request) { r.Header.Set("Authorization", "Bearer "+secret) }

	if w := call(h, "GET", "/api/search?q=x", "", bearer); w.Code != http.StatusOK {
		t.Fatalf("key GET: %d %s", w.Code, w.Body)
	}
	if w := call(h, "POST", "/api/sessions", "", bearer); w.Code != http.StatusForbidden {
		t.Fatalf("key POST must be refused: %d", w.Code)
	}
	// No CORS: a page on another origin never gets Access-Control headers.
	w := call(h, "GET", "/api/search?q=x", "", func(r *http.Request) {
		bearer(r)
		r.Header.Set("Origin", "https://evil.example")
	})
	if w.Header().Get("Access-Control-Allow-Origin") != "" {
		t.Fatal("cross-origin reads must not receive CORS headers")
	}
}

func TestSearchTickEndpoints(t *testing.T) {
	h, f := gatedHandler(t, Options{})
	f.put("s1", "connected")
	if w := call(h, "GET", "/api/search", "", nil); w.Code != http.StatusBadRequest {
		t.Fatalf("missing q: %d", w.Code)
	}
	if w := call(h, "GET", "/api/search?q=x&limit=5000", "", nil); w.Code != http.StatusOK {
		t.Fatalf("oversized limit is clamped, expected 200, got %d", w.Code)
	}
	if w := call(h, "GET", "/api/search?q=x", "", nil); w.Code != http.StatusOK || strings.TrimSpace(w.Body.String()) != "[]" {
		t.Fatalf("empty search: %d %s", w.Code, w.Body)
	}
	if w := call(h, "PATCH", "/api/sessions/s1/chats/g1%40g.us", `{}`, nil); w.Code != http.StatusBadRequest {
		t.Fatalf("tick without field: %d", w.Code)
	}
	if w := call(h, "PATCH", "/api/sessions/s1/chats/g1%40g.us", `{"ticked":false}`, nil); w.Code != http.StatusNotFound {
		t.Fatalf("tick unknown chat: %d %s", w.Code, w.Body)
	}
	if w := call(h, "GET", "/api/sessions/s1/chats/g1%40g.us/messages/m1/context", "", nil); w.Code != http.StatusNotFound {
		t.Fatalf("context unknown message: %d", w.Code)
	}
}

func TestExtraRoutesRunBehindMiddleware(t *testing.T) {
	st := newAuthStore(t, "code")
	var seen auth.Principal
	h, _ := gatedHandler(t, Options{Auth: st, ExtraRoutes: func(mux *http.ServeMux) {
		mux.HandleFunc("GET /api/extra", func(w http.ResponseWriter, r *http.Request) {
			seen = auth.FromContext(r.Context())
			w.WriteHeader(http.StatusOK)
		})
	}})
	if w := call(h, "GET", "/api/extra", "", nil); w.Code != http.StatusUnauthorized {
		t.Fatalf("anonymous: %d", w.Code)
	}
	if w := call(h, "GET", "/api/extra", "", func(r *http.Request) { r.Host = "evil.example" }); w.Code != http.StatusForbidden {
		t.Fatalf("foreign Host: %d", w.Code)
	}
	if w := call(h, "POST", "/api/extra", "", nil); w.Code != http.StatusUnauthorized {
		t.Fatalf("unrouted method, anonymous: %d", w.Code)
	}
	tok, _ := st.Login(context.Background(), "code", "", "")
	w := call(h, "GET", "/api/extra", "", func(r *http.Request) { r.AddCookie(auth.SessionCookie(tok, true)) })
	if w.Code != http.StatusOK || seen.Kind != auth.PrincipalSession || w.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("session: %d %+v %v", w.Code, seen, w.Header())
	}
}
