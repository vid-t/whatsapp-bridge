package wa

import (
	"context"
	"database/sql"
	"fmt"
	"time"
)

// Tables are prefixed bridge_ so they never collide with whatsmeow_* tables
// that live in the same database file.
const schema = `
CREATE TABLE IF NOT EXISTS bridge_sessions (
  id         TEXT PRIMARY KEY,
  jid        TEXT NOT NULL DEFAULT '',
  label      TEXT NOT NULL DEFAULT '',
  created_at TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS bridge_chats (
  session_id      TEXT NOT NULL,
  jid             TEXT NOT NULL,
  name            TEXT NOT NULL DEFAULT '',
  is_group        INTEGER NOT NULL DEFAULT 0,
  last_message_at TEXT,
  message_count   INTEGER NOT NULL DEFAULT 0,
  ticked          INTEGER NOT NULL DEFAULT 1,
  PRIMARY KEY (session_id, jid)
);
CREATE TABLE IF NOT EXISTS bridge_messages (
  session_id  TEXT NOT NULL,
  chat_jid    TEXT NOT NULL,
  id          TEXT NOT NULL,
  sender_jid  TEXT NOT NULL DEFAULT '',
  sender_name TEXT NOT NULL DEFAULT '',
  ts          TEXT NOT NULL,
  from_me     INTEGER NOT NULL DEFAULT 0,
  kind        TEXT NOT NULL,
  text        TEXT NOT NULL DEFAULT '',
  PRIMARY KEY (session_id, chat_jid, id)
);
CREATE INDEX IF NOT EXISTS bridge_messages_chat_ts ON bridge_messages (session_id, chat_jid, ts);
CREATE TABLE IF NOT EXISTS bridge_events (
  seq        INTEGER PRIMARY KEY AUTOINCREMENT,
  ts         TEXT NOT NULL,
  session_id TEXT NOT NULL DEFAULT '',
  type       TEXT NOT NULL,
  detail     TEXT NOT NULL DEFAULT ''
);
`

const tsLayout = time.RFC3339Nano

func fmtTime(t time.Time) string { return t.UTC().Format(tsLayout) }

func parseTime(s string) time.Time {
	t, err := time.Parse(tsLayout, s)
	if err != nil {
		return time.Time{}
	}
	return t
}

func parseTimePtr(s sql.NullString) *time.Time {
	if !s.Valid || s.String == "" {
		return nil
	}
	t := parseTime(s.String)
	if t.IsZero() {
		return nil
	}
	return &t
}

type execer interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
}

func (m *Manager) ensureSchema(ctx context.Context) error {
	if _, err := m.db.ExecContext(ctx, schema); err != nil {
		return fmt.Errorf("wa: create schema: %w", err)
	}
	// Additive migrations for databases created before a column existed.
	if !m.hasColumn(ctx, "bridge_chats", "ticked") {
		if _, err := m.db.ExecContext(ctx, `ALTER TABLE bridge_chats ADD COLUMN ticked INTEGER NOT NULL DEFAULT 1`); err != nil {
			return fmt.Errorf("wa: migrate ticked: %w", err)
		}
	}
	m.ensureFTS(ctx)
	return nil
}

func (m *Manager) hasColumn(ctx context.Context, table, column string) bool {
	rows, err := m.db.QueryContext(ctx, `PRAGMA table_info(`+table+`)`)
	if err != nil {
		return false
	}
	defer rows.Close()
	for rows.Next() {
		var cid int
		var name, typ string
		var notnull, pk int
		var dflt sql.NullString
		if err := rows.Scan(&cid, &name, &typ, &notnull, &dflt, &pk); err == nil && name == column {
			return true
		}
	}
	return false
}

// ensureFTS creates the full-text index over message text if the SQLite
// build has FTS5. External-content mode keys on bridge_messages' rowid; the
// database is never vacuumed, so rowids are stable. On failure the manager
// falls back to LIKE search.
func (m *Manager) ensureFTS(ctx context.Context) {
	var exists int
	_ = m.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = 'bridge_messages_fts'`).Scan(&exists)
	stmts := []string{
		`CREATE VIRTUAL TABLE IF NOT EXISTS bridge_messages_fts USING fts5(
		   text, content='bridge_messages', content_rowid='rowid',
		   tokenize='unicode61 remove_diacritics 2')`,
		`CREATE TRIGGER IF NOT EXISTS bridge_messages_ai AFTER INSERT ON bridge_messages BEGIN
		   INSERT INTO bridge_messages_fts(rowid, text) VALUES (new.rowid, new.text);
		 END`,
		`CREATE TRIGGER IF NOT EXISTS bridge_messages_ad AFTER DELETE ON bridge_messages BEGIN
		   INSERT INTO bridge_messages_fts(bridge_messages_fts, rowid, text) VALUES ('delete', old.rowid, old.text);
		 END`,
		`CREATE TRIGGER IF NOT EXISTS bridge_messages_au AFTER UPDATE OF text ON bridge_messages BEGIN
		   INSERT INTO bridge_messages_fts(bridge_messages_fts, rowid, text) VALUES ('delete', old.rowid, old.text);
		   INSERT INTO bridge_messages_fts(rowid, text) VALUES (new.rowid, new.text);
		 END`,
	}
	for _, st := range stmts {
		if _, err := m.db.ExecContext(ctx, st); err != nil {
			m.log.Warn("full-text index unavailable; using LIKE search", "err", err)
			m.fts = false
			return
		}
	}
	if exists == 0 {
		if _, err := m.db.ExecContext(ctx, `INSERT INTO bridge_messages_fts(bridge_messages_fts) VALUES ('rebuild')`); err != nil {
			m.log.Warn("full-text index rebuild failed", "err", err)
		}
	}
	m.fts = true
}

type sessionRow struct {
	id, jid, label string
	createdAt      time.Time
}

func (m *Manager) loadSessions(ctx context.Context) ([]sessionRow, error) {
	rows, err := m.db.QueryContext(ctx, `SELECT id, jid, label, created_at FROM bridge_sessions ORDER BY created_at`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []sessionRow
	for rows.Next() {
		var r sessionRow
		var created string
		if err := rows.Scan(&r.id, &r.jid, &r.label, &created); err != nil {
			return nil, err
		}
		r.createdAt = parseTime(created)
		out = append(out, r)
	}
	return out, rows.Err()
}

func (m *Manager) insertSession(ctx context.Context, id, label string, createdAt time.Time) error {
	_, err := m.db.ExecContext(ctx, `INSERT OR IGNORE INTO bridge_sessions (id, jid, label, created_at) VALUES (?, '', ?, ?)`,
		id, label, fmtTime(createdAt))
	return err
}

func (m *Manager) setSessionJID(ctx context.Context, id, jid string) error {
	_, err := m.db.ExecContext(ctx, `UPDATE bridge_sessions SET jid = ? WHERE id = ?`, jid, id)
	return err
}

func (m *Manager) deleteSessionData(ctx context.Context, id string) error {
	for _, q := range []string{
		`DELETE FROM bridge_messages WHERE session_id = ?`,
		`DELETE FROM bridge_chats WHERE session_id = ?`,
		`DELETE FROM bridge_sessions WHERE id = ?`,
	} {
		if _, err := m.db.ExecContext(ctx, q, id); err != nil {
			return err
		}
	}
	return nil
}

// upsertChat creates or updates a chat row. An empty name never overwrites a
// known name; last_message_at only moves forward.
func (m *Manager) upsertChat(ctx context.Context, sessionID, jid, name string, isGroup bool, lastAt *time.Time) error {
	var last any
	if lastAt != nil {
		last = fmtTime(*lastAt)
	}
	_, err := m.db.ExecContext(ctx, `
INSERT INTO bridge_chats (session_id, jid, name, is_group, last_message_at)
VALUES (?, ?, ?, ?, ?)
ON CONFLICT(session_id, jid) DO UPDATE SET
  name = CASE WHEN excluded.name <> '' THEN excluded.name ELSE bridge_chats.name END,
  is_group = excluded.is_group,
  last_message_at = CASE
    WHEN excluded.last_message_at IS NULL THEN bridge_chats.last_message_at
    WHEN bridge_chats.last_message_at IS NULL THEN excluded.last_message_at
    WHEN excluded.last_message_at > bridge_chats.last_message_at THEN excluded.last_message_at
    ELSE bridge_chats.last_message_at END`,
		sessionID, jid, name, boolInt(isGroup), last)
	return err
}

// recountChat refreshes message_count and last_message_at from the messages table.
func (m *Manager) recountChat(ctx context.Context, sessionID, jid string) error {
	_, err := m.db.ExecContext(ctx, `
UPDATE bridge_chats SET
  message_count   = (SELECT COUNT(*) FROM bridge_messages WHERE session_id = ?1 AND chat_jid = ?2),
  last_message_at = COALESCE((SELECT MAX(ts) FROM bridge_messages WHERE session_id = ?1 AND chat_jid = ?2), last_message_at)
WHERE session_id = ?1 AND jid = ?2`, sessionID, jid)
	return err
}

func insertMessage(ctx context.Context, ex execer, msg Message) (bool, error) {
	res, err := ex.ExecContext(ctx, `
INSERT OR IGNORE INTO bridge_messages (session_id, chat_jid, id, sender_jid, sender_name, ts, from_me, kind, text)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		msg.SessionID, msg.ChatJID, msg.ID, msg.SenderJID, msg.SenderName, fmtTime(msg.Timestamp), boolInt(msg.FromMe), msg.Kind, msg.Text)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

func (m *Manager) chats(ctx context.Context, sessionID string) ([]Chat, error) {
	return m.queryChats(ctx, `WHERE session_id = ?`, sessionID)
}

func (m *Manager) queryChats(ctx context.Context, where string, args ...any) ([]Chat, error) {
	rows, err := m.db.QueryContext(ctx, `
SELECT session_id, jid, name, is_group, last_message_at, message_count, ticked
FROM bridge_chats `+where+`
ORDER BY last_message_at DESC NULLS LAST, name`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Chat
	for rows.Next() {
		var c Chat
		var isGroup, ticked int
		var last sql.NullString
		if err := rows.Scan(&c.SessionID, &c.JID, &c.Name, &isGroup, &last, &c.MessageCount, &ticked); err != nil {
			return nil, err
		}
		c.IsGroup = isGroup != 0
		c.Ticked = ticked != 0
		c.LastMessageAt = parseTimePtr(last)
		out = append(out, c)
	}
	return out, rows.Err()
}

// chatTicked reports whether messages for a chat should be stored. Unknown
// chats are stored (they get a row with the default on first upsert).
func (m *Manager) chatTicked(ctx context.Context, sessionID, jid string) bool {
	var ticked int
	err := m.db.QueryRowContext(ctx, `SELECT ticked FROM bridge_chats WHERE session_id = ? AND jid = ?`, sessionID, jid).Scan(&ticked)
	if err != nil {
		return true
	}
	return ticked != 0
}

func (m *Manager) setTicked(ctx context.Context, sessionID, jid string, ticked bool) error {
	_, err := m.db.ExecContext(ctx, `UPDATE bridge_chats SET ticked = ? WHERE session_id = ? AND jid = ?`, boolInt(ticked), sessionID, jid)
	return err
}

func (m *Manager) deleteChatMessages(ctx context.Context, sessionID, jid string) error {
	_, err := m.db.ExecContext(ctx, `DELETE FROM bridge_messages WHERE session_id = ? AND chat_jid = ?`, sessionID, jid)
	return err
}

func (m *Manager) recentMessages(ctx context.Context, sessionID, chatJID string, limit int) ([]Message, error) {
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	rows, err := m.db.QueryContext(ctx, `
SELECT session_id, chat_jid, id, sender_jid, sender_name, ts, from_me, kind, text
FROM bridge_messages WHERE session_id = ? AND chat_jid = ?
ORDER BY ts DESC LIMIT ?`, sessionID, chatJID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Message
	for rows.Next() {
		var msg Message
		var ts string
		var fromMe int
		if err := rows.Scan(&msg.SessionID, &msg.ChatJID, &msg.ID, &msg.SenderJID, &msg.SenderName, &ts, &fromMe, &msg.Kind, &msg.Text); err != nil {
			return nil, err
		}
		msg.Timestamp = parseTime(ts)
		msg.FromMe = fromMe != 0
		out = append(out, msg)
	}
	// Return oldest first.
	for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
		out[i], out[j] = out[j], out[i]
	}
	return out, rows.Err()
}

// counts reads aggregates from bridge_chats, which recountChat keeps current,
// so it stays cheap as message volume grows.
func (m *Manager) counts(ctx context.Context, sessionID string) (msgs, chats int64, lastAt *time.Time) {
	var last sql.NullString
	var sum sql.NullInt64
	_ = m.db.QueryRowContext(ctx, `SELECT COUNT(*), COALESCE(SUM(message_count), 0), MAX(last_message_at) FROM bridge_chats WHERE session_id = ?`, sessionID).Scan(&chats, &sum, &last)
	return sum.Int64, chats, parseTimePtr(last)
}

func (m *Manager) appendLog(ctx context.Context, sessionID, typ, detail string) {
	if _, err := m.db.ExecContext(ctx, `INSERT INTO bridge_events (ts, session_id, type, detail) VALUES (?, ?, ?, ?)`,
		fmtTime(time.Now()), sessionID, typ, detail); err != nil {
		m.log.Warn("append event log", "err", err)
	}
}

func (m *Manager) recentLogs(ctx context.Context, sessionID string, limit int) ([]LogEntry, error) {
	if limit <= 0 || limit > 1000 {
		limit = 200
	}
	var rows *sql.Rows
	var err error
	if sessionID == "" {
		rows, err = m.db.QueryContext(ctx, `SELECT seq, ts, session_id, type, detail FROM bridge_events ORDER BY seq DESC LIMIT ?`, limit)
	} else {
		rows, err = m.db.QueryContext(ctx, `SELECT seq, ts, session_id, type, detail FROM bridge_events WHERE session_id = ? ORDER BY seq DESC LIMIT ?`, sessionID, limit)
	}
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []LogEntry
	for rows.Next() {
		var e LogEntry
		var ts string
		if err := rows.Scan(&e.Seq, &ts, &e.SessionID, &e.Type, &e.Detail); err != nil {
			return nil, err
		}
		e.Time = parseTime(ts)
		out = append(out, e)
	}
	return out, rows.Err()
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

// ---- identity repair helpers ---------------------------------------------

// lidChats lists chats keyed by a hidden (LID) address.
func (m *Manager) lidChats(ctx context.Context, sessionID string) ([]string, error) {
	rows, err := m.db.QueryContext(ctx, `SELECT jid FROM bridge_chats WHERE session_id = ? AND jid LIKE '%@lid'`, sessionID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var j string
		if err := rows.Scan(&j); err != nil {
			return nil, err
		}
		out = append(out, j)
	}
	return out, rows.Err()
}

// mergeChat moves a chat's messages under another JID and drops the old row.
// Messages that already exist under the target are discarded as duplicates.
func (m *Manager) mergeChat(ctx context.Context, sessionID, from, to string) error {
	tx, err := m.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	steps := []struct {
		q    string
		args []any
	}{
		{`UPDATE OR IGNORE bridge_messages SET chat_jid = ? WHERE session_id = ? AND chat_jid = ?`, []any{to, sessionID, from}},
		{`DELETE FROM bridge_messages WHERE session_id = ? AND chat_jid = ?`, []any{sessionID, from}},
		{`INSERT INTO bridge_chats (session_id, jid, name, is_group, last_message_at)
		  SELECT session_id, ?, name, is_group, last_message_at FROM bridge_chats WHERE session_id = ? AND jid = ?
		  ON CONFLICT(session_id, jid) DO UPDATE SET
		    name = CASE WHEN bridge_chats.name = '' THEN excluded.name ELSE bridge_chats.name END`, []any{to, sessionID, from}},
		{`DELETE FROM bridge_chats WHERE session_id = ? AND jid = ?`, []any{sessionID, from}},
	}
	for _, s := range steps {
		if _, err := tx.ExecContext(ctx, s.q, s.args...); err != nil {
			return err
		}
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	return m.recountChat(ctx, sessionID, to)
}

// lidSenders lists distinct sender JIDs that are hidden (LID) addresses.
func (m *Manager) lidSenders(ctx context.Context, sessionID string) ([]string, error) {
	rows, err := m.db.QueryContext(ctx, `SELECT DISTINCT sender_jid FROM bridge_messages WHERE session_id = ? AND sender_jid LIKE '%@lid'`, sessionID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var j string
		if err := rows.Scan(&j); err != nil {
			return nil, err
		}
		out = append(out, j)
	}
	return out, rows.Err()
}

func (m *Manager) replaceSender(ctx context.Context, sessionID, from, to string) error {
	_, err := m.db.ExecContext(ctx, `UPDATE bridge_messages SET sender_jid = ? WHERE session_id = ? AND sender_jid = ?`, to, sessionID, from)
	return err
}

// unnamedSenders lists distinct sender JIDs that have messages without a name.
func (m *Manager) unnamedSenders(ctx context.Context, sessionID string) ([]string, error) {
	rows, err := m.db.QueryContext(ctx, `SELECT DISTINCT sender_jid FROM bridge_messages WHERE session_id = ? AND sender_name = '' AND from_me = 0`, sessionID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var j string
		if err := rows.Scan(&j); err != nil {
			return nil, err
		}
		out = append(out, j)
	}
	return out, rows.Err()
}

func (m *Manager) fillSenderName(ctx context.Context, sessionID, sender, name string) error {
	_, err := m.db.ExecContext(ctx, `UPDATE bridge_messages SET sender_name = ? WHERE session_id = ? AND sender_jid = ? AND sender_name = ''`, name, sessionID, sender)
	return err
}

// lastIncomingName returns the most recent push name seen from the other
// party in a direct chat, or "".
func (m *Manager) lastIncomingName(ctx context.Context, sessionID, chatJID string) string {
	var name string
	_ = m.db.QueryRowContext(ctx, `SELECT sender_name FROM bridge_messages WHERE session_id = ? AND chat_jid = ? AND from_me = 0 AND sender_name <> '' ORDER BY ts DESC LIMIT 1`, sessionID, chatJID).Scan(&name)
	return name
}

func (m *Manager) setChatName(ctx context.Context, sessionID, jid, name string) error {
	_, err := m.db.ExecContext(ctx, `UPDATE bridge_chats SET name = ? WHERE session_id = ? AND jid = ?`, name, sessionID, jid)
	return err
}

// deleteChat removes one chat and its messages.
func (m *Manager) deleteChat(ctx context.Context, sessionID, jid string) error {
	if _, err := m.db.ExecContext(ctx, `DELETE FROM bridge_messages WHERE session_id = ? AND chat_jid = ?`, sessionID, jid); err != nil {
		return err
	}
	_, err := m.db.ExecContext(ctx, `DELETE FROM bridge_chats WHERE session_id = ? AND jid = ?`, sessionID, jid)
	return err
}
