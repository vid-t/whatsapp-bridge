package web

import (
	"errors"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/vid-t/whatsapp-bridge/internal/auth"
)

// clientIP returns the peer address, honouring the first X-Forwarded-For
// entry only when the request arrived on loopback (from the reverse proxy).
func clientIP(r *http.Request) string {
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

func (s *server) secureCookies(r *http.Request) bool {
	return r.TLS != nil || strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https")
}

func (s *server) login(w http.ResponseWriter, r *http.Request) {
	if s.auth == nil || s.auth.OpenMode() {
		writeError(w, http.StatusConflict, "server runs in open mode; no login required")
		return
	}
	if ok, wait := s.limiter.Allow("login:"+clientIP(r), 5, time.Minute); !ok {
		retryAfter(w, wait)
		writeError(w, http.StatusTooManyRequests, "too many login attempts")
		return
	}
	var body struct {
		Code string `json:"code"`
	}
	if !decodeJSON(w, r, &body, true) {
		return
	}
	token, err := s.auth.Login(r.Context(), body.Code, r.UserAgent(), clientIP(r))
	if err != nil {
		if errors.Is(err, auth.ErrBadCode) {
			s.audit(r, "auth.login_failed", "ip="+clientIP(r))
			writeError(w, http.StatusUnauthorized, "wrong access code")
			return
		}
		s.fail(w, r, err, http.StatusInternalServerError)
		return
	}
	http.SetCookie(w, auth.SessionCookie(token, s.secureCookies(r)))
	s.audit(r, "auth.login", "ip="+clientIP(r))
	w.WriteHeader(http.StatusNoContent)
}

func (s *server) logoutUI(w http.ResponseWriter, r *http.Request) {
	if s.auth != nil {
		s.auth.Logout(r.Context(), auth.CookieValue(r))
	}
	http.SetCookie(w, auth.ClearCookie(s.secureCookies(r)))
	s.audit(r, "auth.logout", "")
	w.WriteHeader(http.StatusNoContent)
}

func (s *server) me(w http.ResponseWriter, r *http.Request) {
	p := auth.FromContext(r.Context())
	name := ""
	required := false
	if s.auth != nil {
		name = s.auth.Name()
		required = !s.auth.OpenMode()
	}
	// Sign-in methods the UI may offer: the access code when one is set,
	// then email links (when mail is configured) and the identity providers.
	providers := []string{}
	if required {
		providers = append(providers, "code")
	}
	providers = append(providers, s.providers...)
	out := map[string]any{
		"auth_required": required,
		"authenticated": p.Authenticated(),
		"principal":     string(p.Kind),
		"name":          name,
		"version":       s.version,
		"providers":     providers,
		"send_enabled":  s.send,
	}
	if p.User != nil {
		out["user"] = p.User
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *server) listKeys(w http.ResponseWriter, r *http.Request) {
	keys, err := s.auth.ListKeys(r.Context())
	if err != nil {
		s.fail(w, r, err, http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, orEmpty(keys))
}

func (s *server) createKey(w http.ResponseWriter, r *http.Request) {
	if ok, wait := s.limiter.Allow("keys:"+clientIP(r), 20, time.Hour); !ok {
		retryAfter(w, wait)
		writeError(w, http.StatusTooManyRequests, "too many keys created; try later")
		return
	}
	var body struct {
		Label string `json:"label"`
		Kind  string `json:"kind"`
	}
	if !decodeJSON(w, r, &body, true) {
		return
	}
	label, ok := cleanLabel(w, body.Label)
	if !ok {
		return
	}
	kind := auth.KindMCP
	if body.Kind != "" {
		kind = auth.KeyKind(body.Kind)
	}
	info, secret, err := s.auth.CreateKey(r.Context(), label, kind)
	if err != nil {
		if errors.Is(err, auth.ErrBadKind) {
			writeError(w, http.StatusBadRequest, `kind must be "mcp"`)
			return
		}
		s.fail(w, r, err, http.StatusInternalServerError)
		return
	}
	s.audit(r, "key.created", "id="+info.ID+" kind="+string(info.Kind))
	writeJSON(w, http.StatusCreated, map[string]any{
		"id": info.ID, "key": secret, "label": info.Label, "kind": info.Kind, "created_at": info.CreatedAt,
	})
}

func (s *server) revokeKey(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if err := s.auth.RevokeKey(r.Context(), id); err != nil {
		if errors.Is(err, auth.ErrNoKey) {
			writeError(w, http.StatusNotFound, "key not found")
			return
		}
		s.fail(w, r, err, http.StatusInternalServerError)
		return
	}
	s.audit(r, "key.revoked", "id="+id)
	w.WriteHeader(http.StatusNoContent)
}

func retryAfter(w http.ResponseWriter, wait time.Duration) {
	secs := int(wait.Seconds()) + 1
	if secs < 1 {
		secs = 1
	}
	w.Header().Set("Retry-After", itoa(secs))
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b [20]byte
	i := len(b)
	neg := n < 0
	if neg {
		n = -n
	}
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		b[i] = '-'
	}
	return string(b[i:])
}

// audit records an auth or admin event without message text, naming who did
// it: the member's email for a provider session, else "session", "open" or
// the key.
func (s *server) audit(r *http.Request, typ, detail string) {
	if s.auditFn == nil {
		return
	}
	if actor := auth.FromContext(r.Context()).Actor(); actor != "" {
		if detail != "" {
			detail += " "
		}
		detail += "by=" + actor
	}
	s.auditFn(typ, detail)
}
