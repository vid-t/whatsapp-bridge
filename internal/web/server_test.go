package web

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"image/png"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/vid-t/whatsapp-bridge/internal/store"
	"github.com/vid-t/whatsapp-bridge/internal/wa"
)

// newHandler builds the web layer on a real, offline manager. Nothing here
// may call Start, StartPairing or PairWithCode with a valid phone: those
// contact WhatsApp.
func newHandler(t *testing.T) http.Handler {
	t.Helper()
	db, err := store.Open(filepath.Join(t.TempDir(), "t.db"), "")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	mgr, err := wa.NewManager(context.Background(), db, log, wa.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(mgr.Close)
	mux := http.NewServeMux()
	Register(mux, mgr, log)
	return mux
}

func do(t *testing.T, h http.Handler, method, target, contentType, body string) *httptest.ResponseRecorder {
	t.Helper()
	var rd io.Reader
	if body != "" {
		rd = strings.NewReader(body)
	}
	req := httptest.NewRequest(method, target, rd)
	req.Host = "localhost:8080"
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func wantJSONError(t *testing.T, rec *httptest.ResponseRecorder, status int) {
	t.Helper()
	if rec.Code != status {
		t.Fatalf("status = %d, want %d (body %q)", rec.Code, status, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Fatalf("Content-Type = %q, want application/json", ct)
	}
	var e struct {
		Error string `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &e); err != nil || e.Error == "" {
		t.Fatalf("body %q is not a JSON error (%v)", rec.Body.String(), err)
	}
}

func TestListSessionsEmpty(t *testing.T) {
	rec := do(t, newHandler(t), "GET", "/api/sessions", "", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	if got := strings.TrimSpace(rec.Body.String()); got != "[]" {
		t.Fatalf("body = %q, want []", got)
	}
	if cc := rec.Header().Get("Cache-Control"); cc != "no-store" {
		t.Fatalf("Cache-Control = %q, want no-store", cc)
	}
}

func TestEmptyListsAreArrays(t *testing.T) {
	h := newHandler(t)
	for _, target := range []string{"/api/logs", "/api/logs?limit=5"} {
		rec := do(t, h, "GET", target, "", "")
		if rec.Code != http.StatusOK || strings.TrimSpace(rec.Body.String()) != "[]" {
			t.Fatalf("%s: %d %q", target, rec.Code, rec.Body.String())
		}
	}
}

func TestUnknownSessionIs404(t *testing.T) {
	h := newHandler(t)
	cases := []struct{ method, target string }{
		{"GET", "/api/sessions/nope"},
		{"GET", "/api/sessions/nope/pairing"},
		{"GET", "/api/sessions/nope/chats"},
		{"GET", "/api/sessions/nope/chats/123%40s.whatsapp.net/messages"},
		{"GET", "/api/sessions/nope/logs"},
		{"POST", "/api/sessions/nope/cancel"},
		{"POST", "/api/sessions/nope/reconnect"},
		{"POST", "/api/sessions/nope/logout"},
		{"POST", "/api/sessions/nope/passkey/confirm"},
		{"DELETE", "/api/sessions/nope"},
	}
	for _, c := range cases {
		t.Run(c.method+" "+c.target, func(t *testing.T) {
			wantJSONError(t, do(t, h, c.method, c.target, "", ""), http.StatusNotFound)
		})
	}
}

func TestInvalidPathParams(t *testing.T) {
	h := newHandler(t)
	wantJSONError(t, do(t, h, "GET", "/api/sessions/bad%20id", "", ""), http.StatusBadRequest)
	wantJSONError(t, do(t, h, "GET", "/api/sessions/nope/chats/not-a-jid/messages", "", ""), http.StatusBadRequest)
	wantJSONError(t, do(t, h, "GET", "/api/logs?limit=abc", "", ""), http.StatusBadRequest)
	wantJSONError(t, do(t, h, "GET", "/api/logs?limit=0", "", ""), http.StatusBadRequest)
}

func TestPairCodeValidation(t *testing.T) {
	h := newHandler(t)
	// ErrBadPhone is returned before the manager touches the network.
	wantJSONError(t, do(t, h, "POST", "/api/sessions/pair-code", "application/json", `{"phone":"12"}`), http.StatusBadRequest)
	wantJSONError(t, do(t, h, "POST", "/api/sessions/pair-code", "", ""), http.StatusBadRequest)
	wantJSONError(t, do(t, h, "POST", "/api/sessions/pair-code", "text/plain", `{"phone":"12"}`), http.StatusUnsupportedMediaType)
	wantJSONError(t, do(t, h, "POST", "/api/sessions/pair-code", "application/json", `{"phone":`), http.StatusBadRequest)
	long := `{"phone":"12","label":"` + strings.Repeat("x", 65) + `"}`
	wantJSONError(t, do(t, h, "POST", "/api/sessions/pair-code", "application/json", long), http.StatusBadRequest)
	wantJSONError(t, do(t, h, "POST", "/api/sessions/pair-code", "application/json", `{"phone":"12"} {"x":1}`), http.StatusBadRequest)
	huge := `{"phone":"` + strings.Repeat("1", 2<<20) + `"}`
	wantJSONError(t, do(t, h, "POST", "/api/sessions/pair-code", "application/json", huge), http.StatusRequestEntityTooLarge)
}

func TestPasskeyResponseValidation(t *testing.T) {
	h := newHandler(t)
	wantJSONError(t, do(t, h, "POST", "/api/sessions/nope/passkey/response", "", ""), http.StatusBadRequest)
	wantJSONError(t, do(t, h, "POST", "/api/sessions/nope/passkey/response", "application/json", `{"id":""}`), http.StatusBadRequest)
	body := `{"id":"abc","rawId":"AQID","type":"public-key","response":{"clientDataJSON":"AQID","authenticatorData":"AQID","signature":"AQID"}}`
	wantJSONError(t, do(t, h, "POST", "/api/sessions/nope/passkey/response", "application/json", body), http.StatusNotFound)
}

func TestUnroutedAPIIsJSON(t *testing.T) {
	h := newHandler(t)
	wantJSONError(t, do(t, h, "GET", "/api/nope", "", ""), http.StatusNotFound)
	rec := do(t, h, "PUT", "/api/sessions", "", "")
	wantJSONError(t, rec, http.StatusMethodNotAllowed)
	if allow := rec.Header().Get("Allow"); allow != "GET" {
		t.Fatalf("Allow = %q, want GET", allow)
	}
}

func TestCrossOriginMutationRejected(t *testing.T) {
	h := newHandler(t)
	req := httptest.NewRequest("POST", "/api/sessions/pair", nil)
	req.Host = "localhost:8080"
	req.Header.Set("Sec-Fetch-Site", "cross-site")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	wantJSONError(t, rec, http.StatusForbidden)
}

func TestPages(t *testing.T) {
	h := newHandler(t)
	for _, target := range []string{"/", "/welcome", "/link", "/keys", "/security", "/sessions/0123456789abcdef",
		"/signin", "/members", "/assistants", "/nope", "/a/b/c", "/apix", "/sessions"} {
		rec := do(t, h, "GET", target, "", "")
		if rec.Code != http.StatusOK {
			t.Fatalf("%s: status %d", target, rec.Code)
		}
		hdr := rec.Header()
		if ct := hdr.Get("Content-Type"); ct != "text/html; charset=utf-8" {
			t.Fatalf("%s: Content-Type %q", target, ct)
		}
		if got := hdr.Get("Content-Security-Policy"); got != csp {
			t.Fatalf("%s: CSP %q", target, got)
		}
		if hdr.Get("X-Content-Type-Options") != "nosniff" || hdr.Get("Referrer-Policy") != "no-referrer" {
			t.Fatalf("%s: missing security headers: %v", target, hdr)
		}
		if cc := hdr.Get("Cache-Control"); cc != "no-cache" {
			t.Fatalf("%s: Cache-Control %q, want no-cache for the shell", target, cc)
		}
		body := rec.Body.String()
		if !strings.Contains(body, "whatsapp-bridge") || strings.Contains(body, "<script>") || strings.Contains(body, "style=") ||
			strings.Contains(body, "<style") {
			t.Fatalf("%s: unexpected shell content", target)
		}
		if len(shellAssets(t, body)) == 0 {
			t.Fatalf("%s: shell references no /static/ assets", target)
		}
	}
	for _, target := range []string{"/sessions/bad%20id", "/sessions/a/b", "/favicon.ico", "/index.html", "/x.js", "/signin.html",
		"/auth", "/auth/nope", "/oauth/nope", "/mcp/x", "/static", "/static/a/b", "/.well-known/nope", "/a%2eb"} {
		if rec := do(t, h, "GET", target, "", ""); rec.Code != http.StatusNotFound || strings.Contains(rec.Body.String(), "<html") {
			t.Fatalf("%s: status %d", target, rec.Code)
		}
	}
}

// The SPA fallback must not hide unknown API routes or methods.
func TestSPAFallbackLeavesAPIAlone(t *testing.T) {
	h := newHandler(t)
	wantJSONError(t, do(t, h, "GET", "/api/nope", "", ""), http.StatusNotFound)
	if rec := do(t, h, "GET", "/api", "", ""); rec.Code != http.StatusNotFound {
		t.Fatalf("/api: %d", rec.Code)
	}
	rec := do(t, h, "PUT", "/api/sessions", "", "")
	wantJSONError(t, rec, http.StatusMethodNotAllowed)
	if allow := rec.Header().Get("Allow"); allow != "GET" {
		t.Fatalf("Allow = %q, want only GET (the fallback is not a route of /api/sessions)", allow)
	}
	if rec := do(t, h, "POST", "/members", "", ""); rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("POST to a page: %d", rec.Code)
	}
	if rec := do(t, h, "HEAD", "/members", "", ""); rec.Code != http.StatusOK {
		t.Fatalf("HEAD a page: %d", rec.Code)
	}
}

var assetRef = regexp.MustCompile(`(?:src|href)="(/[^"]*)"`)

// shellAssets returns the asset paths the shell references, all under /static/.
func shellAssets(t *testing.T, html string) []string {
	t.Helper()
	var out []string
	for _, m := range assetRef.FindAllStringSubmatch(html, -1) {
		if !strings.HasPrefix(m[1], "/static/") {
			t.Fatalf("shell references %q outside /static/", m[1])
		}
		out = append(out, m[1])
	}
	return out
}

func TestStaticAssets(t *testing.T) {
	h := newHandler(t)
	shell := do(t, h, "GET", "/", "", "").Body.String()
	want := map[string]string{
		".js":  "text/javascript; charset=utf-8",
		".css": "text/css; charset=utf-8",
		".svg": "image/svg+xml",
	}
	seen := map[string]bool{}
	for _, target := range shellAssets(t, shell) {
		ext := target[strings.LastIndex(target, "."):]
		seen[ext] = true
		rec := do(t, h, "GET", target, "", "")
		if rec.Code != http.StatusOK || rec.Body.Len() == 0 {
			t.Fatalf("%s: status %d", target, rec.Code)
		}
		if ct, ok := want[ext]; ok && rec.Header().Get("Content-Type") != ct {
			t.Fatalf("%s: Content-Type %q, want %q", target, rec.Header().Get("Content-Type"), ct)
		}
		if cc := rec.Header().Get("Cache-Control"); cc != "public, max-age=31536000, immutable" {
			t.Fatalf("%s: Cache-Control %q, want a long immutable cache for hashed files", target, cc)
		}
		etag := rec.Header().Get("ETag")
		req := httptest.NewRequest("GET", target, nil)
		req.Host = "localhost"
		req.Header.Set("If-None-Match", etag)
		again := httptest.NewRecorder()
		h.ServeHTTP(again, req)
		if etag == "" || again.Code != http.StatusNotModified {
			t.Fatalf("%s: revalidation got %d (etag %q)", target, again.Code, etag)
		}
	}
	for _, ext := range []string{".js", ".css"} {
		if !seen[ext] {
			t.Fatalf("shell has no %s asset", ext)
		}
	}
	// The stylesheet uses system fonts and loads nothing from elsewhere.
	for _, target := range shellAssets(t, shell) {
		if strings.HasSuffix(target, ".css") {
			if css := do(t, h, "GET", target, "", "").Body.String(); regexp.MustCompile(`url\((https?:)?//`).MatchString(css) || strings.Contains(css, "@import") {
				t.Fatalf("%s loads an external resource", target)
			}
		}
	}
	for _, file := range []string{"index.html", "nope.js", "app.js"} {
		if rec := do(t, h, "GET", "/static/"+file, "", ""); rec.Code != http.StatusNotFound {
			t.Fatalf("%s: status %d, want 404", file, rec.Code)
		}
	}
}

func TestEventsStream(t *testing.T) {
	srv := httptest.NewServer(newHandler(t))
	defer srv.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, "GET", srv.URL+"/api/events", nil)
	res, err := srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("status %d", res.StatusCode)
	}
	for k, want := range map[string]string{
		"Content-Type":      "text/event-stream",
		"Cache-Control":     "no-store",
		"X-Accel-Buffering": "no",
	} {
		if got := res.Header.Get(k); got != want {
			t.Fatalf("%s = %q, want %q", k, got, want)
		}
	}
	sc := bufio.NewScanner(res.Body)
	for sc.Scan() {
		if strings.HasPrefix(sc.Text(), ":") {
			return // first comment line arrived while the stream stays open
		}
	}
	t.Fatalf("no comment line before stream ended: %v", sc.Err())
}

func TestQRStepCarriesPNG(t *testing.T) {
	s := &server{qr: qrCache{m: map[string]string{}}}
	ev := wa.Event{SessionID: "abc", Type: wa.EventPair, Pair: &wa.PairEvent{Type: wa.PairQR, Code: "2@payload,key,key,key", TimeoutSeconds: 60}}
	data, err := json.Marshal(s.toWire(ev))
	if err != nil {
		t.Fatal(err)
	}
	var got struct {
		Type string `json:"type"`
		Pair struct {
			Type           string `json:"type"`
			Code           string `json:"code"`
			TimeoutSeconds int    `json:"timeout_seconds"`
			QRPNG          string `json:"qr_png"`
		} `json:"pair"`
	}
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatal(err)
	}
	if got.Type != "pair" || got.Pair.Type != "qr" || got.Pair.TimeoutSeconds != 60 {
		t.Fatalf("unexpected wire event %s", data)
	}
	// The raw QR payload carries the device's ADV secret: only the image goes out.
	if got.Pair.Code != "" || bytes.Contains(data, []byte("2@payload")) {
		t.Fatalf("qr step leaks the payload: %s", data)
	}
	b64, ok := strings.CutPrefix(got.Pair.QRPNG, "data:image/png;base64,")
	if !ok {
		t.Fatalf("qr_png = %.40q", got.Pair.QRPNG)
	}
	raw, err := base64.StdEncoding.DecodeString(b64)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := png.Decode(bytes.NewReader(raw)); err != nil {
		t.Fatalf("qr_png is not a PNG: %v", err)
	}
	// Phone codes and passkey codes keep their code and carry no image.
	for _, typ := range []wa.PairEventType{wa.PairCode, wa.PairPasskeyCode} {
		plain, _ := json.Marshal(s.toWire(wa.Event{Type: wa.EventPair, Pair: &wa.PairEvent{Type: typ, Code: "ABCD1234"}}))
		if bytes.Contains(plain, []byte("qr_png")) || !bytes.Contains(plain, []byte(`"code":"ABCD1234"`)) {
			t.Fatalf("%s step: %s", typ, plain)
		}
	}
}
