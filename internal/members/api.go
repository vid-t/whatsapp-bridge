package members

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/vid-t/whatsapp-bridge/internal/auth"
)

// ---- operations (members API and the admin command) ---------------------------------

// Invite admits an address as an approved member (approving a pending
// request for it) and, when mail is configured, emails it a sign-in link
// valid one hour. by names the inviter for approved_by and the audit line;
// base is the bridge origin for the link ("" sends no link).
func (s *Server) Invite(ctx context.Context, email, name, by, base string) (auth.Member, error) {
	m, err := s.auth.InviteMember(ctx, email, name, by)
	if err != nil {
		return auth.Member{}, err
	}
	s.audit("member.invited", "id="+m.ID+" email="+m.Email+" by="+by)
	s.sendAccess(ctx, m.Email, base)
	return m, nil
}

// Approve lets a pending member in and emails them a sign-in link valid one
// hour. changed is false (and nothing is sent) when they were approved
// already.
func (s *Server) Approve(ctx context.Context, id, by, base string) (m auth.Member, changed bool, err error) {
	m, changed, err = s.auth.ApproveMember(ctx, id, by)
	if err != nil || !changed {
		return m, changed, err
	}
	s.audit("member.approved", "id="+m.ID+" email="+m.Email+" by="+by)
	s.sendAccess(ctx, m.Email, base)
	return m, true, nil
}

// Remove deletes a member or denies a pending request: their sessions end
// and their outstanding links stop working.
func (s *Server) Remove(ctx context.Context, id, by string) (auth.Member, error) {
	m, err := s.auth.Member(ctx, id)
	if err != nil {
		return auth.Member{}, err
	}
	if err := s.auth.RemoveMember(ctx, id); err != nil {
		return auth.Member{}, err
	}
	if err := s.forget(ctx, m.Email); err != nil {
		s.log.Warn("forget links", "err", err)
	}
	s.audit("member.removed", "id="+m.ID+" email="+m.Email+" status="+m.Status+" by="+by)
	return m, nil
}

// ---- members API ------------------------------------------------------------------------

func (s *Server) sessionOnly(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		p := auth.FromContext(r.Context())
		switch {
		case p.IsSession():
			w.Header().Set("Cache-Control", "no-store")
			next(w, r)
		case p.Authenticated():
			writeError(w, http.StatusForbidden, "members can be managed only from a signed-in browser session")
		default:
			writeError(w, http.StatusUnauthorized, http.StatusText(http.StatusUnauthorized))
		}
	}
}

func (s *Server) changeLimited(w http.ResponseWriter, r *http.Request) bool {
	if ok, wait := s.limiter.Allow("members:"+auth.ClientIP(r), 60, time.Hour); !ok {
		retryAfter(w, wait)
		writeError(w, http.StatusTooManyRequests, "too many changes; try later")
		return true
	}
	return false
}

func (s *Server) listMembers(w http.ResponseWriter, r *http.Request) {
	ms, err := s.auth.ListMembers(r.Context())
	if err != nil {
		s.log.Error("list members", "err", err)
		writeError(w, http.StatusInternalServerError, http.StatusText(http.StatusInternalServerError))
		return
	}
	writeJSON(w, http.StatusOK, ms)
}

func (s *Server) inviteMember(w http.ResponseWriter, r *http.Request) {
	if s.changeLimited(w, r) {
		return
	}
	if mt, _, _ := strings.Cut(r.Header.Get("Content-Type"), ";"); !strings.EqualFold(strings.TrimSpace(mt), "application/json") {
		writeError(w, http.StatusUnsupportedMediaType, "content type must be application/json")
		return
	}
	var body struct {
		Email string `json:"email"`
		Name  string `json:"name"`
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxBody)
	dec := json.NewDecoder(r.Body)
	if err := dec.Decode(&body); err != nil || dec.Decode(&struct{}{}) != io.EOF {
		writeError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	m, err := s.Invite(r.Context(), body.Email, body.Name, auth.FromContext(r.Context()).Actor(), s.base(r))
	switch {
	case errors.Is(err, auth.ErrBadEmail):
		writeError(w, http.StatusBadRequest, "email must be a valid address")
	case errors.Is(err, auth.ErrMemberExists):
		writeError(w, http.StatusConflict, "this email is already a member")
	case err != nil:
		s.log.Error("invite member", "err", err)
		writeError(w, http.StatusInternalServerError, http.StatusText(http.StatusInternalServerError))
	default:
		writeJSON(w, http.StatusCreated, m)
	}
}

func (s *Server) approveMember(w http.ResponseWriter, r *http.Request) {
	if s.changeLimited(w, r) {
		return
	}
	m, _, err := s.Approve(r.Context(), r.PathValue("id"), auth.FromContext(r.Context()).Actor(), s.base(r))
	switch {
	case errors.Is(err, auth.ErrNoMember):
		writeError(w, http.StatusNotFound, "member not found")
	case err != nil:
		s.log.Error("approve member", "err", err)
		writeError(w, http.StatusInternalServerError, http.StatusText(http.StatusInternalServerError))
	default:
		writeJSON(w, http.StatusOK, m)
	}
}

func (s *Server) removeMember(w http.ResponseWriter, r *http.Request) {
	_, err := s.Remove(r.Context(), r.PathValue("id"), auth.FromContext(r.Context()).Actor())
	switch {
	case errors.Is(err, auth.ErrNoMember):
		writeError(w, http.StatusNotFound, "member not found")
	case err != nil:
		s.log.Error("remove member", "err", err)
		writeError(w, http.StatusInternalServerError, http.StatusText(http.StatusInternalServerError))
	default:
		w.WriteHeader(http.StatusNoContent)
	}
}
