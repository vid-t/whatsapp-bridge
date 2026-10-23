package oauth

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestRenderNotice(t *testing.T) {
	w := httptest.NewRecorder()
	RenderNotice(w, http.StatusGone, Notice{Name: "Acme <Team>", Eyebrow: "Access request", Heading: "Approve <b>x</b>?",
		Message: "m", Detail: "d", Action: "/auth/email/approve", ActionLabel: "Approve", Link: "/", LinkLabel: "Open"})
	body := w.Body.String()
	if w.Code != http.StatusGone || w.Header().Get("Content-Type") != "text/html; charset=utf-8" || w.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("response: %d %v", w.Code, w.Header())
	}
	if csp := w.Header().Get("Content-Security-Policy"); !strings.HasPrefix(csp, "default-src 'none'; style-src 'self'") {
		t.Fatalf("default CSP: %q", csp)
	}
	for _, want := range []string{"Approve &lt;b&gt;x&lt;/b&gt;?", "Acme &lt;Team&gt;", `<form method="post" action="/auth/email/approve"`,
		`<a class="button" href="/">Open</a>`, `href="/oauth/static/oauth.css?v=`} {
		if !strings.Contains(body, want) {
			t.Fatalf("page lacks %q:\n%s", want, body)
		}
	}
	if strings.Contains(body, "style=") || strings.Contains(body, "<script") || strings.Contains(body, "<b>") {
		t.Fatalf("page:\n%s", body)
	}
	// A policy set by the caller (the web layer) is kept.
	w = httptest.NewRecorder()
	w.Header().Set("Content-Security-Policy", "default-src 'self'")
	RenderNotice(w, http.StatusOK, Notice{Heading: "h"})
	if w.Header().Get("Content-Security-Policy") != "default-src 'self'" || !strings.Contains(w.Body.String(), "whatsapp-bridge") {
		t.Fatalf("caller CSP replaced: %q", w.Header().Get("Content-Security-Policy"))
	}
}

func TestStaticFiles(t *testing.T) {
	s := &Server{}
	for name, ctype := range map[string]string{"oauth.css": "text/css; charset=utf-8"} {
		r := httptest.NewRequest("GET", StaticPath+name, nil)
		r.SetPathValue("file", name)
		w := httptest.NewRecorder()
		s.static(w, r)
		if w.Code != http.StatusOK || w.Header().Get("Content-Type") != ctype || w.Body.Len() == 0 || w.Header().Get("ETag") == "" {
			t.Fatalf("%s: %d %v", name, w.Code, w.Header())
		}
	}
	r := httptest.NewRequest("GET", StaticPath+"pages.html", nil)
	r.SetPathValue("file", "pages.html")
	w := httptest.NewRecorder()
	s.static(w, r)
	if w.Code != http.StatusNotFound {
		t.Fatalf("pages.html is served: %d", w.Code)
	}
	css, _ := assetFS.ReadFile("assets/oauth.css")
	if strings.Contains(string(css), "url(") || strings.Contains(string(css), "@import") {
		t.Fatal("oauth.css must not load anything")
	}
	// Nothing widens the policy.
	if p := pageCSP(""); strings.Contains(p, "font-src") || strings.Contains(p, "unsafe") || !strings.HasPrefix(p, "default-src 'none'; style-src 'self'") {
		t.Fatalf("pageCSP: %q", p)
	}
}

func TestBreakable(t *testing.T) {
	for in, want := range map[string]string{"Approve max@broker.example?": "Approve max@<wbr>broker.<wbr>example?",
		"Sign in to Acme Team.": "Sign in to Acme Team.", "<b>x</b>": "&lt;b&gt;x&lt;/b&gt;", `a"b'c&d`: "a&#34;b&#39;c&amp;d"} {
		if got := string(breakable(in)); got != want {
			t.Errorf("breakable(%q) = %q, want %q", in, got, want)
		}
	}
}
