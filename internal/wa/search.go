package wa

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

// ErrNoChat is returned when a chat cannot be found.
var ErrNoChat = errors.New("wa: no such chat")

// Audit appends a lifecycle or tool event that is not tied to a session.
// Detail must never contain message text.
func (m *Manager) Audit(ctx context.Context, typ, detail string) {
	m.appendLog(ctx, "", typ, detail)
}

// AppendAudit is Audit without a Manager, for the admin command (`bridge
// members`), which opens the database beside the running service and must
// not load or touch the linked devices.
func AppendAudit(ctx context.Context, db *sql.DB, typ, detail string) error {
	_, err := db.ExecContext(ctx, `INSERT INTO bridge_events (ts, session_id, type, detail) VALUES (?, '', ?, ?)`,
		fmtTime(time.Now()), typ, detail)
	return err
}

// SetChatTicked turns storage for a chat on or off. Turning it on stores new
// messages from now on, plus the messages held in memory for the chat while
// its account's window after linking is open. Turning it off deletes the
// chat's stored messages and, when sending is on, stops sends to the chat: it
// waits for a send in progress, and none can start after it.
func (m *Manager) SetChatTicked(ctx context.Context, sessionID, chatJID string, ticked bool) (Chat, error) {
	if _, err := m.get(sessionID); err != nil {
		return Chat{}, err
	}
	m.tickMu.Lock()
	defer m.tickMu.Unlock()
	chats, err := m.queryChats(ctx, `WHERE session_id = ? AND jid = ?`, sessionID, chatJID)
	if err != nil {
		return Chat{}, err
	}
	if len(chats) == 0 {
		return Chat{}, ErrNoChat
	}
	kept := -1
	if ticked {
		held := m.hold.take(sessionID, chatJID)
		n, err := m.tickChat(ctx, sessionID, chatJID, held)
		if err != nil {
			m.hold.restore(sessionID, chatJID, held)
			return Chat{}, err
		}
		if len(held) > 0 {
			kept = n
		}
	} else {
		if err := m.untickChat(ctx, sessionID, chatJID); err != nil {
			return Chat{}, err
		}
		m.hold.untick(sessionID, chatJID)
	}
	_ = m.recountChat(ctx, sessionID, chatJID)
	m.appendLog(ctx, sessionID, "chat.ticked", fmt.Sprintf("%s=%t", chatJID, ticked))
	if kept >= 0 {
		m.appendLog(ctx, sessionID, "history.kept", fmt.Sprintf("%d held messages stored", kept))
	}
	m.publish(Event{SessionID: sessionID, Type: EventChats, Text: "chat selection changed"})
	chats, err = m.queryChats(ctx, `WHERE session_id = ? AND jid = ?`, sessionID, chatJID)
	if err != nil || len(chats) == 0 {
		return Chat{}, ErrNoChat
	}
	return chats[0], nil
}

// FindChat resolves a chat by JID or title among ticked chats. sessionID may
// be empty to search every session. Matching order: exact JID, exact name,
// case-insensitive name, unique prefix.
func (m *Manager) FindChat(ctx context.Context, sessionID, title string) (Chat, error) {
	title = strings.TrimSpace(title)
	if title == "" {
		return Chat{}, ErrNoChat
	}
	scope := `WHERE ticked = 1`
	args := []any{}
	if sessionID != "" {
		scope += ` AND session_id = ?`
		args = append(args, sessionID)
	}
	// A chat seen by two linked devices appears once per session; treat one
	// JID as one chat and prefer the row with the most recent activity.
	try := func(cond string, extra ...any) (Chat, bool, error) {
		chats, err := m.queryChats(ctx, scope+` AND `+cond, append(append([]any{}, args...), extra...)...)
		if err != nil {
			return Chat{}, false, err
		}
		if len(chats) == 0 {
			return Chat{}, false, nil
		}
		for _, c := range chats[1:] {
			if c.JID != chats[0].JID {
				return Chat{}, false, nil // ambiguous title
			}
		}
		return chats[0], true, nil // queryChats orders by last_message_at DESC
	}
	// Escape LIKE wildcards so a title such as "50% Advance" matches literally.
	likeTitle := strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`).Replace(title)
	for _, step := range []struct {
		cond string
		arg  string
	}{
		{`jid = ?`, title},
		{`name = ?`, title},
		{`lower(name) = lower(?)`, title},
		{`lower(name) LIKE lower(?) || '%' ESCAPE '\'`, likeTitle},
	} {
		c, ok, err := try(step.cond, step.arg)
		if err != nil {
			return Chat{}, err
		}
		if ok {
			return c, nil
		}
	}
	return Chat{}, ErrNoChat
}

// SearchMessages runs a full-text search over ticked chats. sessionID and
// chatJID may be empty. Results are newest first.
func (m *Manager) SearchMessages(ctx context.Context, sessionID, chatJID, query string, limit int) ([]SearchHit, error) {
	return m.SearchMessagesRange(ctx, sessionID, chatJID, query, time.Time{}, time.Time{}, limit)
}

// SearchMessagesRange is SearchMessages with an optional time window (zero
// values mean unbounded). A message seen through two linked devices is
// returned once.
func (m *Manager) SearchMessagesRange(ctx context.Context, sessionID, chatJID, query string, from, to time.Time, limit int) ([]SearchHit, error) {
	query = strings.TrimSpace(query)
	if query == "" {
		return nil, nil
	}
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	fromS, toS := "", ""
	if !from.IsZero() {
		fromS = fmtTime(from)
	}
	if !to.IsZero() {
		toS = fmtTime(to)
	}
	var rows *sql.Rows
	var err error
	base := `
SELECT msg.session_id, msg.chat_jid, msg.id, msg.sender_jid, msg.sender_name, msg.ts, msg.from_me, msg.kind, msg.text, c.name
FROM bridge_messages msg
JOIN bridge_chats c ON c.session_id = msg.session_id AND c.jid = msg.chat_jid AND c.ticked = 1
`
	filter := ` AND (? = '' OR msg.session_id = ?) AND (? = '' OR msg.chat_jid = ?)
 AND (? = '' OR msg.ts >= ?) AND (? = '' OR msg.ts <= ?)
 ORDER BY msg.ts DESC, msg.rowid DESC LIMIT ?`
	args := []any{sessionID, sessionID, chatJID, chatJID, fromS, fromS, toS, toS, limit * 2}
	if m.fts {
		rows, err = m.db.QueryContext(ctx, base+`
JOIN bridge_messages_fts f ON f.rowid = msg.rowid
WHERE bridge_messages_fts MATCH ?`+filter, append([]any{ftsQuery(query)}, args...)...)
	} else {
		rows, err = m.db.QueryContext(ctx, base+`WHERE msg.text LIKE '%' || ? || '%' COLLATE NOCASE`+filter, append([]any{query}, args...)...)
	}
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	seen := map[string]bool{}
	var out []SearchHit
	for rows.Next() {
		var h SearchHit
		var ts string
		var fromMe int
		if err := rows.Scan(&h.Message.SessionID, &h.Message.ChatJID, &h.Message.ID, &h.Message.SenderJID, &h.Message.SenderName, &ts, &fromMe, &h.Message.Kind, &h.Message.Text, &h.ChatName); err != nil {
			return nil, err
		}
		key := h.Message.ChatJID + "\x00" + h.Message.ID
		if seen[key] {
			continue
		}
		seen[key] = true
		h.Message.Timestamp = parseTime(ts)
		h.Message.FromMe = fromMe != 0
		out = append(out, h)
		if len(out) >= limit {
			break
		}
	}
	return out, rows.Err()
}

// ftsQuery turns free text into a safe FTS5 query: every token quoted, all
// tokens required, the last token matched as a prefix.
func ftsQuery(q string) string {
	fields := strings.Fields(q)
	parts := make([]string, 0, len(fields))
	for i, f := range fields {
		f = strings.ReplaceAll(f, `"`, `""`)
		if i == len(fields)-1 && len([]rune(f)) >= 2 {
			parts = append(parts, `"`+f+`"*`)
		} else {
			parts = append(parts, `"`+f+`"`)
		}
	}
	return strings.Join(parts, " ")
}

// MessageContext returns up to before messages preceding and after messages
// following the given message (inclusive), oldest first.
func (m *Manager) MessageContext(ctx context.Context, sessionID, chatJID, messageID string, before, after int) ([]Message, error) {
	if before < 0 {
		before = 10
	}
	if after < 0 {
		after = 10
	}
	before, after = min(before, 100), min(after, 100)
	var ts string
	var rowid int64
	err := m.db.QueryRowContext(ctx, `SELECT ts, rowid FROM bridge_messages WHERE session_id = ? AND chat_jid = ? AND id = ?`, sessionID, chatJID, messageID).Scan(&ts, &rowid)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNoChat
	}
	if err != nil {
		return nil, err
	}
	scan := func(rows *sql.Rows) ([]Message, error) {
		defer rows.Close()
		var out []Message
		for rows.Next() {
			var msg Message
			var t string
			var fromMe int
			if err := rows.Scan(&msg.SessionID, &msg.ChatJID, &msg.ID, &msg.SenderJID, &msg.SenderName, &t, &fromMe, &msg.Kind, &msg.Text); err != nil {
				return nil, err
			}
			msg.Timestamp = parseTime(t)
			msg.FromMe = fromMe != 0
			out = append(out, msg)
		}
		return out, rows.Err()
	}
	const cols = `SELECT session_id, chat_jid, id, sender_jid, sender_name, ts, from_me, kind, text FROM bridge_messages WHERE session_id = ? AND chat_jid = ? `
	// Anchor on (ts, rowid) so messages sharing a second keep a stable order.
	prev, err := m.db.QueryContext(ctx, cols+`AND (ts < ? OR (ts = ? AND rowid < ?)) ORDER BY ts DESC, rowid DESC LIMIT ?`, sessionID, chatJID, ts, ts, rowid, before)
	if err != nil {
		return nil, err
	}
	pm, err := scan(prev)
	if err != nil {
		return nil, err
	}
	next, err := m.db.QueryContext(ctx, cols+`AND (ts > ? OR (ts = ? AND rowid >= ?)) ORDER BY ts ASC, rowid ASC LIMIT ?`, sessionID, chatJID, ts, ts, rowid, after+1)
	if err != nil {
		return nil, err
	}
	nm, err := scan(next)
	if err != nil {
		return nil, err
	}
	out := make([]Message, 0, len(pm)+len(nm))
	for i := len(pm) - 1; i >= 0; i-- {
		out = append(out, pm[i])
	}
	out = append(out, nm...)
	return out, nil
}
