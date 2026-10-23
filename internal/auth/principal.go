package auth

import (
	"context"
	"net/http"
	"strings"
)

// PrincipalKind says how a request was authenticated.
type PrincipalKind string

const (
	PrincipalOpen      PrincipalKind = "open"      // no access code configured
	PrincipalSession   PrincipalKind = "session"   // UI cookie
	PrincipalKey       PrincipalKind = "key"       // bearer key
	PrincipalAnonymous PrincipalKind = "anonymous" // nothing valid presented
)

// Principal is the resolved identity of a request.
type Principal struct {
	Kind PrincipalKind
	Key  KeyInfo // set when Kind == PrincipalKey
	// User is set for a member session (email link, Google, Microsoft); nil
	// for access-code sessions and open mode.
	User *User
}

// User is the member behind a member session. Provider is how this session
// signed in: "email", "google" or "microsoft".
type User struct {
	ID       string `json:"-"`
	Email    string `json:"email"`
	Name     string `json:"name"`
	Provider string `json:"provider"`
}

// Actor names the principal for audit lines: the member's email, "session"
// for an access-code session, "open", "key:<id>", or "" when anonymous.
func (p Principal) Actor() string {
	switch {
	case p.User != nil:
		return p.User.Email
	case p.Kind == PrincipalSession:
		return "session"
	case p.Kind == PrincipalOpen:
		return "open"
	case p.Kind == PrincipalKey:
		return "key:" + p.Key.ID
	}
	return ""
}

// Authenticated reports whether the request may use protected routes.
func (p Principal) Authenticated() bool {
	return p.Kind == PrincipalOpen || p.Kind == PrincipalSession || p.Kind == PrincipalKey
}

// IsSession reports whether the principal is a UI session (or open mode).
func (p Principal) IsSession() bool {
	return p.Kind == PrincipalOpen || p.Kind == PrincipalSession
}

type ctxKey struct{}

// WithPrincipal stores p in ctx.
func WithPrincipal(ctx context.Context, p Principal) context.Context {
	return context.WithValue(ctx, ctxKey{}, p)
}

// FromContext returns the principal stored by the middleware, or anonymous.
func FromContext(ctx context.Context) Principal {
	if p, ok := ctx.Value(ctxKey{}).(Principal); ok {
		return p
	}
	return Principal{Kind: PrincipalAnonymous}
}

// Resolve determines the principal of a request from its bearer header or
// session cookie. It never writes to the response.
func (s *Store) Resolve(r *http.Request) Principal {
	if h := r.Header.Get("Authorization"); strings.HasPrefix(h, "Bearer ") {
		if k, err := s.VerifyKey(r.Context(), h); err == nil {
			return Principal{Kind: PrincipalKey, Key: k}
		}
		return Principal{Kind: PrincipalAnonymous}
	}
	return s.ResolveSession(r)
}

// ResolveSession is Resolve without the bearer header: open mode, or the
// session cookie (with its member, if any), or anonymous. Pages that a
// browser opens (OAuth authorize) use it.
func (s *Store) ResolveSession(r *http.Request) Principal {
	if s.OpenMode() {
		return Principal{Kind: PrincipalOpen}
	}
	tok := CookieValue(r)
	if tok == "" {
		return Principal{Kind: PrincipalAnonymous}
	}
	memberID, via, ok := s.verifySession(r.Context(), tok)
	if !ok {
		return Principal{Kind: PrincipalAnonymous}
	}
	if memberID == "" {
		return Principal{Kind: PrincipalSession}
	}
	m, err := s.Member(r.Context(), memberID)
	if err != nil || !m.Approved() {
		// The member was removed (or is no longer approved); the session goes with it.
		s.Logout(r.Context(), tok)
		return Principal{Kind: PrincipalAnonymous}
	}
	if via == "" {
		via = m.Provider
	}
	return Principal{Kind: PrincipalSession, User: &User{ID: m.ID, Email: m.Email, Name: m.Name, Provider: via}}
}

// SessionCookie builds the cookie for a login token. secure must be true
// behind TLS; the __Host- prefix requires it, so on plain loopback the name
// falls back to a plain name.
func SessionCookie(token string, secure bool) *http.Cookie {
	name := CookieName
	if !secure {
		name = "bridge_session"
	}
	return &http.Cookie{
		Name:     name,
		Value:    token,
		Path:     "/",
		HttpOnly: true,
		Secure:   secure,
		SameSite: http.SameSiteLaxMode,
		MaxAge:   int(sessionMax.Seconds()),
	}
}

// ClearCookie expires the session cookie.
func ClearCookie(secure bool) *http.Cookie {
	c := SessionCookie("", secure)
	c.MaxAge = -1
	return c
}

// CookieValue returns the session token from either cookie name.
func CookieValue(r *http.Request) string {
	for _, n := range []string{CookieName, "bridge_session"} {
		if c, err := r.Cookie(n); err == nil {
			return c.Value
		}
	}
	return ""
}
