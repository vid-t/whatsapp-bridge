package web

import (
	"errors"
	"net/http"
	"strconv"

	"github.com/vid-t/whatsapp-bridge/internal/wa"
)

func (s *server) setChatTicked(w http.ResponseWriter, r *http.Request) {
	id, ok := sessionID(w, r)
	if !ok {
		return
	}
	jid := r.PathValue("jid")
	var body struct {
		Ticked *bool `json:"ticked"`
	}
	if !decodeJSON(w, r, &body, true) {
		return
	}
	if body.Ticked == nil {
		writeError(w, http.StatusBadRequest, `"ticked" is required`)
		return
	}
	chat, err := s.mgr.SetChatTicked(r.Context(), id, jid, *body.Ticked)
	if err != nil {
		if errors.Is(err, wa.ErrNoChat) {
			writeError(w, http.StatusNotFound, "chat not found")
			return
		}
		s.fail(w, r, err, http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, chat)
}

func (s *server) search(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query().Get("q")
	if q == "" {
		writeError(w, http.StatusBadRequest, "q is required")
		return
	}
	limit, ok := queryLimit(w, r, 50, 200)
	if !ok {
		return
	}
	hits, err := s.mgr.SearchMessages(r.Context(), r.URL.Query().Get("session"), r.URL.Query().Get("chat"), q, limit)
	if err != nil {
		s.fail(w, r, err, http.StatusInternalServerError)
		return
	}
	type hit struct {
		Message   wa.Message `json:"message"`
		ChatName  string     `json:"chat_name"`
		SessionID string     `json:"session_id"`
	}
	out := make([]hit, 0, len(hits))
	for _, h := range hits {
		out = append(out, hit{Message: h.Message, ChatName: h.ChatName, SessionID: h.Message.SessionID})
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *server) messageContext(w http.ResponseWriter, r *http.Request) {
	id, ok := sessionID(w, r)
	if !ok {
		return
	}
	before, after := 10, 10
	if v := r.URL.Query().Get("before"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			before = n
		}
	}
	if v := r.URL.Query().Get("after"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			after = n
		}
	}
	msgs, err := s.mgr.MessageContext(r.Context(), id, r.PathValue("jid"), r.PathValue("mid"), before, after)
	if err != nil {
		if errors.Is(err, wa.ErrNoChat) {
			writeError(w, http.StatusNotFound, "message not found")
			return
		}
		s.fail(w, r, err, http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, orEmpty(msgs))
}
