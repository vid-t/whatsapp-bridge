package web

import (
	"context"
	"errors"
	"net/http"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"go.mau.fi/whatsmeow/types"

	"github.com/vid-t/whatsapp-bridge/internal/auth"
	"github.com/vid-t/whatsapp-bridge/internal/wa"
)

var (
	idRe  = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)
	jidRe = regexp.MustCompile(`^[0-9A-Za-z._:-]{1,128}@[0-9A-Za-z.-]{1,64}$`)
)

const maxLabelRunes = 64

// sessionID validates the {id} path value. The manager stays the authority on
// whether the session exists.
func sessionID(w http.ResponseWriter, r *http.Request) (string, bool) {
	id := r.PathValue("id")
	if !idRe.MatchString(id) {
		writeError(w, http.StatusBadRequest, "invalid session id")
		return "", false
	}
	return id, true
}

func queryLimit(w http.ResponseWriter, r *http.Request, def, max int) (int, bool) {
	v := r.URL.Query().Get("limit")
	if v == "" {
		return def, true
	}
	n, err := strconv.Atoi(v)
	if err != nil || n < 1 {
		writeError(w, http.StatusBadRequest, "limit must be a positive integer")
		return 0, false
	}
	return min(n, max), true
}

func cleanLabel(w http.ResponseWriter, label string) (string, bool) {
	label = strings.TrimSpace(label)
	if utf8.RuneCountInString(label) > maxLabelRunes {
		writeError(w, http.StatusBadRequest, "label is too long (max 64 characters)")
		return "", false
	}
	if strings.IndexFunc(label, unicode.IsControl) >= 0 {
		writeError(w, http.StatusBadRequest, "label contains control characters")
		return "", false
	}
	return label, true
}

// detached keeps an operation running when the browser disconnects, so that
// logout, remove and passkey steps never stop halfway.
func detached(r *http.Request, d time.Duration) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(r.Context()), d)
}

// orEmpty makes nil slices encode as [] rather than null.
func orEmpty[T any](v []T) []T {
	if v == nil {
		return []T{}
	}
	return v
}

// ---- sessions ------------------------------------------------------------------

func (s *server) listSessions(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, orEmpty(s.mgr.Sessions()))
}

func (s *server) getSession(w http.ResponseWriter, r *http.Request) {
	id, ok := sessionID(w, r)
	if !ok {
		return
	}
	info, err := s.mgr.Session(id)
	if err != nil {
		s.fail(w, r, err, http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, info)
}

func (s *server) removeSession(w http.ResponseWriter, r *http.Request) {
	id, ok := sessionID(w, r)
	if !ok {
		return
	}
	ctx, cancel := detached(r, 30*time.Second)
	defer cancel()
	if err := s.mgr.Remove(ctx, id); err != nil {
		s.fail(w, r, err, http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *server) logout(w http.ResponseWriter, r *http.Request) {
	id, ok := sessionID(w, r)
	if !ok {
		return
	}
	ctx, cancel := detached(r, 30*time.Second)
	defer cancel()
	err := s.mgr.Logout(ctx, id)
	switch {
	case err == nil:
		w.WriteHeader(http.StatusNoContent)
	case errors.Is(err, wa.ErrUnlinkFailed):
		// The session is gone locally; only the phone-side unlink is unconfirmed.
		s.log.Warn("logout: unlink not confirmed", "route", r.Pattern, "err", err)
		writeJSON(w, http.StatusOK, map[string]string{"warning": "Removed from the bridge, but WhatsApp did not confirm the unlink. " +
			"Remove the device under WhatsApp > Linked devices."})
	default:
		s.fail(w, r, err, http.StatusInternalServerError)
	}
}

func (s *server) reconnect(w http.ResponseWriter, r *http.Request) {
	id, ok := sessionID(w, r)
	if !ok {
		return
	}
	if err := s.mgr.Reconnect(id); err != nil {
		s.fail(w, r, err, http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// ---- pairing ---------------------------------------------------------------------

type pairRequest struct {
	Label string `json:"label"`
	Phone string `json:"phone"`
}

func (s *server) startPairQR(w http.ResponseWriter, r *http.Request) {
	var req pairRequest
	if !decodeJSON(w, r, &req, false) {
		return
	}
	label, ok := cleanLabel(w, req.Label)
	if !ok {
		return
	}
	// Steps also arrive via Subscribe; the dedicated channel is not read.
	// The manager never blocks on it.
	id, _, err := s.mgr.StartPairing(label)
	if err != nil {
		s.fail(w, r, err, http.StatusBadGateway)
		return
	}
	w.Header().Set("Location", "/api/sessions/"+id)
	writeJSON(w, http.StatusCreated, map[string]string{"id": id})
}

func (s *server) startPairCode(w http.ResponseWriter, r *http.Request) {
	var req pairRequest
	if !decodeJSON(w, r, &req, true) {
		return
	}
	label, ok := cleanLabel(w, req.Label)
	if !ok {
		return
	}
	if len(req.Phone) > 32 {
		writeError(w, http.StatusBadRequest, "phone number is too long")
		return
	}
	// PairWithCode waits up to ~20s for the WhatsApp socket, then asks for a code.
	ctx, cancel := context.WithTimeout(r.Context(), 45*time.Second)
	defer cancel()
	id, code, err := s.mgr.PairWithCode(ctx, req.Phone, label)
	if err != nil {
		s.fail(w, r, err, http.StatusBadGateway)
		return
	}
	w.Header().Set("Location", "/api/sessions/"+id)
	writeJSON(w, http.StatusCreated, map[string]string{"id": id, "code": code})
}

// pairingState lets the link page recover after a reload or reconnect: the
// session plus, while it is still pairing, the latest screen step and the
// seconds left on that step's timer (server clock).
func (s *server) pairingState(w http.ResponseWriter, r *http.Request) {
	id, ok := sessionID(w, r)
	if !ok {
		return
	}
	info, err := s.mgr.Session(id)
	if err != nil {
		s.dropStep(id)
		s.fail(w, r, err, http.StatusInternalServerError)
		return
	}
	resp := struct {
		Session          wa.SessionInfo `json:"session"`
		Step             *wireEvent     `json:"step"`
		RemainingSeconds int            `json:"remaining_seconds"`
	}{Session: info}
	if info.State == wa.StatePairing {
		resp.Step, resp.RemainingSeconds = s.step(id)
	} else {
		s.dropStep(id)
	}
	writeJSON(w, http.StatusOK, resp)
}

// cancelPairing takes no body, so it also accepts the empty POST that
// navigator.sendBeacon sends when the link page is left.
func (s *server) cancelPairing(w http.ResponseWriter, r *http.Request) {
	id, ok := sessionID(w, r)
	if !ok {
		return
	}
	err := s.mgr.CancelPairing(id)
	if err == nil || errors.Is(err, wa.ErrNoSession) {
		s.dropStep(id)
	}
	if err != nil {
		s.fail(w, r, err, http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *server) passkeyConfirm(w http.ResponseWriter, r *http.Request) {
	id, ok := sessionID(w, r)
	if !ok {
		return
	}
	ctx, cancel := detached(r, 30*time.Second)
	defer cancel()
	if err := s.mgr.ConfirmPasskey(ctx, id); err != nil {
		s.fail(w, r, err, http.StatusBadGateway)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *server) passkeyResponse(w http.ResponseWriter, r *http.Request) {
	id, ok := sessionID(w, r)
	if !ok {
		return
	}
	var resp types.WebAuthnResponse
	if !decodeJSON(w, r, &resp, true) {
		return
	}
	if resp.ID == "" || len(resp.Response.Signature) == 0 {
		writeError(w, http.StatusBadRequest, "incomplete WebAuthn assertion")
		return
	}
	ctx, cancel := detached(r, 30*time.Second)
	defer cancel()
	if err := s.mgr.SubmitPasskeyResponse(ctx, id, &resp); err != nil {
		s.fail(w, r, err, http.StatusBadGateway)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// ---- chats, messages, logs ----------------------------------------------------------

func (s *server) listChats(w http.ResponseWriter, r *http.Request) {
	id, ok := sessionID(w, r)
	if !ok {
		return
	}
	chats, err := s.mgr.Chats(r.Context(), id)
	if err != nil {
		s.fail(w, r, err, http.StatusInternalServerError)
		return
	}
	// A key (a script or an assistant) sees ticked chats only, like the MCP
	// tools. The full list is for the people choosing what to share.
	if auth.FromContext(r.Context()).Kind == auth.PrincipalKey {
		chats = slices.DeleteFunc(chats, func(c wa.Chat) bool { return !c.Ticked })
	}
	writeJSON(w, http.StatusOK, orEmpty(chats))
}

func (s *server) listMessages(w http.ResponseWriter, r *http.Request) {
	id, ok := sessionID(w, r)
	if !ok {
		return
	}
	jid := r.PathValue("jid")
	if !jidRe.MatchString(jid) {
		writeError(w, http.StatusBadRequest, "invalid chat id")
		return
	}
	limit, ok := queryLimit(w, r, 100, 500)
	if !ok {
		return
	}
	msgs, err := s.mgr.RecentMessages(r.Context(), id, jid, limit)
	if err != nil {
		s.fail(w, r, err, http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, orEmpty(msgs))
}

func (s *server) sessionLogs(w http.ResponseWriter, r *http.Request) {
	id, ok := sessionID(w, r)
	if !ok {
		return
	}
	if _, err := s.mgr.Session(id); err != nil {
		s.fail(w, r, err, http.StatusInternalServerError)
		return
	}
	s.writeLogs(w, r, id)
}

func (s *server) allLogs(w http.ResponseWriter, r *http.Request) {
	s.writeLogs(w, r, "")
}

func (s *server) writeLogs(w http.ResponseWriter, r *http.Request, id string) {
	limit, ok := queryLimit(w, r, 200, 1000)
	if !ok {
		return
	}
	logs, err := s.mgr.RecentLogs(r.Context(), id, limit)
	if err != nil {
		s.fail(w, r, err, http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, orEmpty(logs))
}
