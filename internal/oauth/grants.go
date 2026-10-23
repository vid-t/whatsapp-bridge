package oauth

import (
	"context"
	"database/sql"
	"net/http"
	"time"

	"github.com/vid-t/whatsapp-bridge/internal/auth"
)

// Grant is one approval of an OAuth client, as listed by the grants API.
type Grant struct {
	ID         string     `json:"id"`
	ClientName string     `json:"client_name"`
	ClientID   string     `json:"client_id"`
	CreatedAt  time.Time  `json:"created_at"`
	LastUsedAt *time.Time `json:"last_used_at"`
}

// RegisterAPI mounts the grants API on the web layer's inner mux (through
// web.Options.ExtraRoutes), so it runs behind the web middleware: Host
// allowlist, headers, cross-origin protection and principal resolution.
// Like /api/keys it is session-only; bearer keys get 403.
//
//	GET    /api/oauth/grants       -> 200 [Grant] (active grants, oldest first)
//	DELETE /api/oauth/grants/{id}  -> 204, revoking every token of the grant
func (s *Server) RegisterAPI(mux *http.ServeMux) {
	mux.HandleFunc("GET "+GrantsPath, s.sessionOnly(s.listGrants))
	mux.HandleFunc("DELETE "+GrantsPath+"/{id}", s.sessionOnly(s.deleteGrant))
}

func (s *Server) sessionOnly(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		p := auth.FromContext(r.Context())
		switch {
		case p.IsSession():
			w.Header().Set("Cache-Control", "no-store")
			next(w, r)
		case p.Authenticated():
			apiError(w, http.StatusForbidden, "grants can be managed only from a signed-in browser session")
		default:
			apiError(w, http.StatusUnauthorized, http.StatusText(http.StatusUnauthorized))
		}
	}
}

// Grants returns the active (not revoked, not expired) grants, oldest first.
func (s *Server) Grants(ctx context.Context) ([]Grant, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id, client_name, client_id, created_at, last_used_at FROM bridge_oauth_grants
		WHERE revoked_at IS NULL AND expires_at > ? ORDER BY created_at, id`, ms(s.now()))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Grant{}
	for rows.Next() {
		var g Grant
		var created int64
		var used sql.NullInt64
		if err := rows.Scan(&g.ID, &g.ClientName, &g.ClientID, &created, &used); err != nil {
			return nil, err
		}
		g.CreatedAt = fromMS(created)
		if used.Valid {
			t := fromMS(used.Int64)
			g.LastUsedAt = &t
		}
		out = append(out, g)
	}
	return out, rows.Err()
}

func (s *Server) listGrants(w http.ResponseWriter, r *http.Request) {
	grants, err := s.Grants(r.Context())
	if err != nil {
		s.log.Error("list grants", "err", err)
		apiError(w, http.StatusInternalServerError, http.StatusText(http.StatusInternalServerError))
		return
	}
	writeJSON(w, http.StatusOK, grants)
}

func (s *Server) deleteGrant(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if len(id) > 64 {
		apiError(w, http.StatusNotFound, "grant not found")
		return
	}
	revoked, err := s.revokeGrant(r.Context(), id)
	if err != nil {
		s.log.Error("revoke grant", "err", err)
		apiError(w, http.StatusInternalServerError, http.StatusText(http.StatusInternalServerError))
		return
	}
	if !revoked {
		apiError(w, http.StatusNotFound, "grant not found")
		return
	}
	s.audit("oauth.revoke", "grant="+id+" via=ui")
	w.WriteHeader(http.StatusNoContent)
}
