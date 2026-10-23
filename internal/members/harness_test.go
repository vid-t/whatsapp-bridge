package members

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/vid-t/whatsapp-bridge/internal/auth"
	"github.com/vid-t/whatsapp-bridge/internal/mail"
	"github.com/vid-t/whatsapp-bridge/internal/oauth"
	"github.com/vid-t/whatsapp-bridge/internal/store"
	"github.com/vid-t/whatsapp-bridge/internal/wa"
	"github.com/vid-t/whatsapp-bridge/internal/web"
)

const (
	accessCode = "bootstrap-code-1"
	bridgeName = "Acme Team"
	approverA  = "boss@example.org"
	approverB  = "ops@example.org"
)

// fakeMail records messages instead of sending them.
type fakeMail struct {
	mu   sync.Mutex
	msgs []mail.Message
}

func (f *fakeMail) Send(_ context.Context, m mail.Message) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.msgs = append(f.msgs, m)
	return nil
}

// take returns and clears the messages sent so far.
func (f *fakeMail) take() []mail.Message {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := f.msgs
	f.msgs = nil
	return out
}

// harness wires web + members + oauth on one database, as cmd/whatsapp-bridge does.
type harness struct {
	t      *testing.T
	url    string
	db     *sql.DB
	auth   *auth.Store
	srv    *Server
	mail   *fakeMail
	client *http.Client
	logs   *bytes.Buffer

	mu     sync.Mutex
	audits []string
	clock  time.Time
}

type hopts struct {
	noMail    bool
	domains   []string
	approvers []string
}

func newHarness(t *testing.T, o hopts) *harness {
	t.Helper()
	h := &harness{t: t, mail: &fakeMail{}, logs: &bytes.Buffer{}, clock: time.Now().UTC()}
	db, err := store.Open(filepath.Join(t.TempDir(), "bridge.db"), "")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	h.db = db
	ctx := context.Background()
	// Debug level on purpose: no level may log a token or a link.
	log := slog.New(slog.NewTextHandler(&lockedWriter{w: h.logs, mu: &h.mu}, &slog.HandlerOptions{Level: slog.LevelDebug}))
	h.auth, err = auth.New(ctx, db, auth.Options{AccessCode: accessCode, Name: bridgeName})
	if err != nil {
		t.Fatal(err)
	}
	mgr, err := wa.NewManager(ctx, db, slog.New(slog.DiscardHandler), wa.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(mgr.Close)
	audit := func(typ, detail string) {
		h.mu.Lock()
		defer h.mu.Unlock()
		h.audits = append(h.audits, typ+" "+detail)
	}
	var sender mail.Sender = h.mail
	if o.noMail {
		sender = nil
	}
	approvers := o.approvers
	if approvers == nil {
		approvers = []string{approverA, approverB}
	}
	h.srv, err = New(Deps{Auth: h.auth, DB: db, Mail: sender, Log: log, Audit: audit},
		Config{AllowedDomains: o.domains, Approvers: approvers, now: h.now})
	if err != nil {
		t.Fatal(err)
	}
	var providers []string
	if h.srv.EmailEnabled() {
		providers = []string{"email"}
	}
	mux := http.NewServeMux()
	oauthMux := http.NewServeMux()
	osrv, err := oauth.Register(oauthMux, oauth.Deps{DB: db, Auth: h.auth, Log: log, Audit: audit}, oauth.Options{SignInProviders: providers})
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range oauth.Paths() {
		mux.Handle(p, oauthMux)
	}
	web.RegisterWithOptions(mux, mgr, log, web.Options{Auth: h.auth, Audit: audit, SignInProviders: providers,
		ExtraRoutes: func(m *http.ServeMux) { h.srv.RegisterRoutes(m); osrv.RegisterAPI(m) }})
	ts := httptest.NewServer(mux)
	t.Cleanup(ts.Close)
	h.url = ts.URL
	h.client = &http.Client{Timeout: 10 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	return h
}

type lockedWriter struct {
	w  io.Writer
	mu *sync.Mutex
}

func (l *lockedWriter) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.w.Write(p)
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

func (h *harness) auditLog() string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return strings.Join(h.audits, "\n")
}

func (h *harness) logText() string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.logs.String()
}

// settle waits for the background admissions and mails.
func (h *harness) settle() {
	h.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := h.srv.Wait(ctx); err != nil {
		h.t.Fatal("background work did not finish:", err)
	}
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

func (h *harness) do(method, target, contentType, body string, j jar, mods ...func(*http.Request)) (*http.Response, string) {
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
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
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

func (h *harness) json(method, target, body string, j jar, mods ...func(*http.Request)) (*http.Response, string) {
	h.t.Helper()
	ct := ""
	if body != "" {
		ct = "application/json"
	}
	return h.do(method, target, ct, body, j, mods...)
}

// start asks for a link and waits for the background work.
func (h *harness) start(email, next string) (*http.Response, string) {
	h.t.Helper()
	b, _ := json.Marshal(map[string]string{"email": email, "next": next})
	resp, body := h.json("POST", "/auth/email/start", string(b), nil)
	h.settle()
	return resp, body
}

func (h *harness) codeSession() jar {
	h.t.Helper()
	j := jar{}
	resp, body := h.json("POST", "/auth/login", `{"code":"`+accessCode+`"}`, j)
	if resp.StatusCode != http.StatusNoContent {
		h.t.Fatalf("access-code login: %d %s", resp.StatusCode, body)
	}
	return j
}

var linkRe = regexp.MustCompile(`https?://\S+`)

// linkIn returns the only link of a mail, failing when there is not exactly one.
func (h *harness) linkIn(m mail.Message) string {
	h.t.Helper()
	links := linkRe.FindAllString(m.Body, -1)
	if len(links) != 1 {
		h.t.Fatalf("mail %q has %d links, want 1:\n%s", m.Subject, len(links), m.Body)
	}
	return links[0]
}

// mailTo returns the one mail of this kind to this address among msgs.
func (h *harness) mailTo(msgs []mail.Message, kind, to string) mail.Message {
	h.t.Helper()
	var found []mail.Message
	for _, m := range msgs {
		if m.Kind == kind && m.To == to {
			found = append(found, m)
		}
	}
	if len(found) != 1 {
		h.t.Fatalf("want one %s mail to %s, got %d among %d mails", kind, to, len(found), len(msgs))
	}
	return found[0]
}

// open follows an emailed link the way a browser does: the token moves into
// a cookie (303), then the page with the button. It returns the page.
func (h *harness) open(link string, j jar) (*http.Response, string) {
	h.t.Helper()
	resp, body := h.do("GET", link, "", "", j)
	if resp.StatusCode != http.StatusSeeOther {
		return resp, body
	}
	loc := resp.Header.Get("Location")
	if !strings.HasPrefix(loc, "/auth/email/") || strings.Contains(loc, "token") {
		return resp, body
	}
	return h.do("GET", loc, "", "", j)
}

// press submits the page's single form (same origin, like the browser).
func (h *harness) press(page string, j jar) (*http.Response, string) {
	h.t.Helper()
	m := regexp.MustCompile(`<form method="post" action="([^"]+)"`).FindStringSubmatch(page)
	if m == nil {
		h.t.Fatalf("page has no form:\n%s", page)
	}
	return h.do("POST", m[1], "application/x-www-form-urlencoded", "", j, func(r *http.Request) {
		r.Header.Set("Origin", h.url)
	})
}

// signIn opens a sign-in link and presses Continue.
func (h *harness) signIn(link string, j jar) (*http.Response, string) {
	h.t.Helper()
	resp, page := h.open(link, j)
	if resp.StatusCode != http.StatusOK || !strings.Contains(page, `action="/auth/email/callback"`) {
		return resp, page
	}
	return h.press(page, j)
}

func (h *harness) me(j jar) map[string]any {
	h.t.Helper()
	_, body := h.json("GET", "/api/me", "", j)
	var m map[string]any
	if err := json.Unmarshal([]byte(body), &m); err != nil {
		h.t.Fatal(err)
	}
	return m
}

func (h *harness) member(email string) auth.Member {
	h.t.Helper()
	m, err := h.auth.MemberByEmail(context.Background(), email)
	if err != nil {
		h.t.Fatalf("member %s: %v", email, err)
	}
	return m
}

func tokenOf(link string) string {
	u, _ := url.Parse(link)
	return u.Query().Get("token")
}
