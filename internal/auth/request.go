package auth

import (
	"net"
	"net/http"
	"net/url"
	"strings"
)

// SafePath keeps only same-origin paths ("/x?y"): never another host, a
// scheme-relative URL ("//host") or a backslash trick; anything else becomes
// "/". Sign-in flows use it for their return address (next).
func SafePath(raw string) string {
	if raw == "" || len(raw) > 4096 || !strings.HasPrefix(raw, "/") || strings.HasPrefix(raw, "//") ||
		strings.ContainsAny(raw, "\\\r\n\t") {
		return "/"
	}
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "" || u.Host != "" || u.User != nil {
		return "/"
	}
	return raw
}

// SafeNext is SafePath that also refuses the sign-in routes themselves
// (/auth/...), so a sign-in never loops into another one.
func SafeNext(raw string) string {
	p := SafePath(raw)
	if u, err := url.Parse(p); err != nil || strings.HasPrefix(u.Path, "/auth/") {
		return "/"
	}
	return p
}

// ClientIP returns the peer address, honouring the first X-Forwarded-For
// entry only when the request arrived on loopback (from the reverse proxy).
// Same rule as internal/web, internal/oauth and internal/sso.
func ClientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	if ip := net.ParseIP(host); ip != nil && ip.IsLoopback() {
		if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
			first, _, _ := strings.Cut(xff, ",")
			if f := strings.TrimSpace(first); f != "" {
				return f
			}
		}
	}
	return host
}

// SecureRequest reports whether the request arrived over TLS, directly or
// through the reverse proxy (X-Forwarded-Proto), which decides the cookie
// flags.
func SecureRequest(r *http.Request) bool {
	return r.TLS != nil || strings.EqualFold(strings.TrimSpace(strings.Split(r.Header.Get("X-Forwarded-Proto"), ",")[0]), "https")
}
