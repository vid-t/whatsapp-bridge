package web

import (
	"bytes"
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"io/fs"
	"net/http"
	"path"
	"regexp"
	"strings"
	"time"
)

// The UI is a single-page app built from ../../web (Vite, React) into
// assets/dist: index.html plus flat, content-hashed files under static/.
// Rebuild it with `npm --prefix web run build` from the bridge directory.
//
//go:generate npm --prefix ../../web run build
//go:embed assets/dist
var assetFS embed.FS

type asset struct {
	data      []byte
	ctype     string
	etag      string
	immutable bool // content-hashed name: cache for a year
}

// Explicit types, so the result never depends on the host's mime tables.
var assetTypes = map[string]string{
	".html":  "text/html; charset=utf-8",
	".css":   "text/css; charset=utf-8",
	".js":    "text/javascript; charset=utf-8",
	".svg":   "image/svg+xml",
	".png":   "image/png",
	".woff2": "font/woff2",
	".txt":   "text/plain; charset=utf-8",
}

// hashedName matches Vite's output names, e.g. index-Cl0fLVrz.js.
var hashedName = regexp.MustCompile(`-[A-Za-z0-9_-]{8}\.[a-z0-9]+$`)

// loadAssets reads the embedded build: "index.html" and "static/<file>".
func loadAssets() map[string]asset {
	out := map[string]asset{}
	root, err := fs.Sub(assetFS, "assets/dist")
	if err != nil {
		panic(err) // embedded at build time; cannot fail at runtime
	}
	err = fs.WalkDir(root, ".", func(name string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		ctype, ok := assetTypes[path.Ext(name)]
		if !ok {
			return nil
		}
		data, err := fs.ReadFile(root, name)
		if err != nil {
			return err
		}
		sum := sha256.Sum256(data)
		out[name] = asset{
			data:      data,
			ctype:     ctype,
			etag:      `"` + hex.EncodeToString(sum[:8]) + `"`,
			immutable: path.Dir(name) == "static" && hashedName.MatchString(name),
		}
		return nil
	})
	if err != nil {
		panic(err)
	}
	if _, ok := out["index.html"]; !ok {
		panic("web: assets/dist/index.html missing; run `npm --prefix web run build`")
	}
	return out
}

func (s *server) serveAsset(w http.ResponseWriter, r *http.Request, name string) {
	a, ok := s.assets[name]
	if !ok {
		http.NotFound(w, r)
		return
	}
	h := w.Header()
	h.Set("Content-Type", a.ctype)
	if a.immutable {
		h.Set("Cache-Control", "public, max-age=31536000, immutable")
	} else {
		h.Set("Cache-Control", "no-cache")
	}
	h.Set("ETag", a.etag)
	http.ServeContent(w, r, path.Base(name), time.Time{}, bytes.NewReader(a.data))
}

// spaPattern is the catch-all route of the app shell.
const spaPattern = "GET /"

// notSPA are the path prefixes the shell is never served for: they belong
// to the API, the sign-in routes, OAuth, MCP, the build files and the
// well-known documents, and an unknown path there is a plain 404.
var notSPA = []string{"/api", "/auth", "/oauth", "/mcp", "/static", "/.well-known"}

// spaFallback serves the app shell for any path without a dot outside
// notSPA; the script picks the view from the path, renders its own
// not-found view and asks /api/me whether to show the sign-in step. Paths
// under /sessions/ other than /sessions/{id} (handled by sessionPage) are
// 404, as before.
func (s *server) spaFallback(w http.ResponseWriter, r *http.Request) {
	if !spaPath(r.URL.Path) {
		http.NotFound(w, r)
		return
	}
	s.serveAsset(w, r, "index.html")
}

func spaPath(p string) bool {
	if strings.Contains(p, ".") || strings.HasPrefix(p, "/sessions/") || !strings.HasPrefix(p, "/") {
		return false
	}
	for _, prefix := range notSPA {
		if p == prefix || strings.HasPrefix(p, prefix+"/") {
			return false
		}
	}
	return true
}

func (s *server) sessionPage(w http.ResponseWriter, r *http.Request) {
	if !idRe.MatchString(r.PathValue("id")) {
		http.NotFound(w, r)
		return
	}
	s.serveAsset(w, r, "index.html")
}

// static serves the hashed build files from assets/dist/static.
func (s *server) static(w http.ResponseWriter, r *http.Request) {
	s.serveAsset(w, r, "static/"+r.PathValue("file"))
}
