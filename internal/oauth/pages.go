package oauth

import (
	"bytes"
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"html/template"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
	"unicode"
)

// The sign-in, consent and error pages are plain HTML forms with one
// stylesheet: no scripts and no inline styles, so they pass a CSP as strict
// as the web UI's.
//
//go:embed assets/pages.html assets/oauth.css
var assetFS embed.FS

type asset struct {
	data  []byte
	ctype string
	etag  string
	ver   string
}

// staticTypes are the files served under StaticPath, with their types.
var staticTypes = map[string]string{
	"oauth.css": "text/css; charset=utf-8",
}

// shell is parsed once: the authorization server and RenderNotice share it.
var shell = sync.OnceValues(func() (shellAssets, error) {
	t, err := template.New("pages.html").Funcs(template.FuncMap{"breakable": breakable}).ParseFS(assetFS, "assets/pages.html")
	if err != nil {
		return shellAssets{}, err
	}
	files := make(map[string]asset, len(staticTypes))
	for name, ctype := range staticTypes {
		data, err := assetFS.ReadFile("assets/" + name)
		if err != nil {
			return shellAssets{}, err
		}
		sum := sha256.Sum256(data)
		ver := hex.EncodeToString(sum[:6])
		files[name] = asset{data: data, ctype: ctype, etag: `"` + ver + `"`, ver: ver}
	}
	return shellAssets{pages: t, css: files["oauth.css"], files: files}, nil
})

type shellAssets struct {
	pages *template.Template
	css   asset
	files map[string]asset
}

func loadPages() (*template.Template, asset, error) {
	a, err := shell()
	return a.pages, a.css, err
}

func (s *Server) static(w http.ResponseWriter, r *http.Request) {
	a, err := shell()
	name := r.PathValue("file")
	f, ok := a.files[name]
	if err != nil || !ok {
		http.NotFound(w, r)
		return
	}
	h := w.Header()
	h.Set("Content-Type", f.ctype)
	h.Set("Cache-Control", "public, max-age=86400")
	h.Set("ETag", f.etag)
	http.ServeContent(w, r, name, time.Time{}, bytes.NewReader(f.data))
}

// breakable escapes s and lets long addresses in a heading wrap after "@"
// and after a dot inside a name ("max@<wbr>broker.<wbr>example"), not in
// the middle of a word, on a 360px screen.
func breakable(s string) template.HTML {
	var b strings.Builder
	rs := []rune(s)
	for i, r := range rs {
		b.WriteString(template.HTMLEscapeString(string(r)))
		if i+1 < len(rs) && (r == '@' || r == '.' && unicode.IsLetter(rs[i+1])) {
			b.WriteString("<wbr>")
		}
	}
	return template.HTML(b.String())
}

// page is the data every template receives.
type page struct {
	Title string
	Name  string // the bridge's display name
	CSS   string

	Req          authzRequest
	ClientName   string
	ClientHost   string // CIMD: host that published the identity document
	Document     bool
	RedirectHost string
	Loopback     bool
	Error        string
	Send         bool // the app can send to ticked chats, not only read them

	Heading string
	Message string

	// Providers are "Continue with ..." links on the sign-in step.
	Providers []providerLink
	// Email offers "Email me a sign-in link" on the sign-in step; Next is
	// the authorization request the link returns to.
	Email bool
	Next  string

	Notice Notice
}

type providerLink struct {
	Name  string // "google" or "microsoft": picks the logo
	Label string
	URL   string
}

var providerLabels = map[string]string{"google": "Continue with Google", "microsoft": "Continue with Microsoft"}

func (s *Server) base(title string) page {
	name := s.auth.Name()
	if name == "" {
		name = "whatsapp-bridge"
	}
	return page{Title: title, Name: name, CSS: StaticPath + "oauth.css?v=" + s.css.ver}
}

func (s *Server) forClient(p page, req authzRequest, cl client) page {
	p.Req = req
	p.Send = s.send
	p.ClientName = cl.Name
	p.Document = cl.Document
	p.ClientHost = cl.Host
	if u, err := url.Parse(req.RedirectURI); err == nil {
		p.RedirectHost = u.Host
		p.Loopback = u.Scheme == "http" && isLoopbackHost(u.Hostname())
	}
	return p
}

func (s *Server) renderSignIn(w http.ResponseWriter, r *http.Request, status int, req authzRequest, cl client, msg string) {
	p := s.forClient(s.base("Sign in"), req, cl)
	p.Error = msg
	next := AuthorizePath + "?" + req.query().Encode()
	for _, name := range s.providers {
		if name == "email" {
			p.Email, p.Next = true, next
			continue
		}
		label, ok := providerLabels[name]
		if !ok {
			continue
		}
		p.Providers = append(p.Providers, providerLink{Name: name, Label: label, URL: "/auth/" + name + "/start?" + url.Values{"next": {next}}.Encode()})
	}
	s.render(w, status, "signin", p, req.RedirectURI)
}

func (s *Server) renderConsent(w http.ResponseWriter, r *http.Request, req authzRequest, cl client) {
	p := s.forClient(s.base("Allow access"), req, cl)
	s.render(w, http.StatusOK, "consent", p, req.RedirectURI)
}

func (s *Server) renderError(w http.ResponseWriter, r *http.Request, status int, heading, msg string) {
	p := s.base(heading)
	p.Heading, p.Message = heading, msg
	s.render(w, status, "error", p, "")
}

func (s *Server) render(w http.ResponseWriter, status int, name string, p page, formTarget string) {
	var buf bytes.Buffer
	if err := s.pages.ExecuteTemplate(&buf, name, p); err != nil {
		s.log.Error("render page", "page", name, "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	h := w.Header()
	h.Set("Content-Type", "text/html; charset=utf-8")
	h.Set("Content-Security-Policy", pageCSP(formTarget))
	w.WriteHeader(status)
	_, _ = w.Write(buf.Bytes())
}

// pageCSP is the web UI's policy without scripts, fonts or connections. The consent
// form posts to this origin, which answers with a redirect to the client;
// browsers apply form-action to that redirect, so the trusted redirect_uri's
// origin is allowed as a form target on the sign-in and consent pages.
func pageCSP(formTarget string) string {
	action := "'self'"
	if formTarget != "" {
		if u, err := url.Parse(formTarget); err == nil && u.Host != "" {
			origin := u.Scheme + "://" + u.Host
			switch {
			case strings.ContainsAny(origin, " ;,'\"[]"):
				action += " " + u.Scheme + ":" // e.g. http://[::1]:port: CSP has no IPv6 host syntax
			default:
				action += " " + origin
			}
		}
	}
	return "default-src 'none'; style-src 'self'; img-src 'self' data:; form-action " + action +
		"; frame-ancestors 'none'; base-uri 'none'"
}

// Notice is a message page in the OAuth page shell, for server-rendered
// pages outside this package (email sign-in and approval in
// internal/members). Plain HTML: no scripts, no inline styles.
type Notice struct {
	Name    string // the bridge's display name; "" = whatsapp-bridge
	Eyebrow string // small line above the heading
	Heading string
	Message string
	Detail  string // muted line below the message
	// Action, when set, renders a same-origin POST form with one button.
	Action      string
	ActionLabel string
	// Link, when set, renders a link button (a same-origin path).
	Link      string
	LinkLabel string
}

// RenderNotice writes a notice page. When the response has no
// Content-Security-Policy yet (the web layer sets one), the pages' own
// policy is applied.
func RenderNotice(w http.ResponseWriter, status int, n Notice) {
	a, err := shell()
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	if n.Name == "" {
		n.Name = "whatsapp-bridge"
	}
	p := page{Title: n.Heading, Name: n.Name, CSS: StaticPath + "oauth.css?v=" + a.css.ver, Notice: n}
	var buf bytes.Buffer
	if err := a.pages.ExecuteTemplate(&buf, "notice", p); err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	h := w.Header()
	h.Set("Content-Type", "text/html; charset=utf-8")
	h.Set("Cache-Control", "no-store")
	if h.Get("Content-Security-Policy") == "" {
		h.Set("Content-Security-Policy", pageCSP(""))
	}
	w.WriteHeader(status)
	_, _ = w.Write(buf.Bytes())
}
