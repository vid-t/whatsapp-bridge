package wa

import (
	"bytes"
	"context"
	"database/sql"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"go.mau.fi/whatsmeow/proto/waCommon"
	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/proto/waHistorySync"
	"go.mau.fi/whatsmeow/proto/waWeb"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
	"google.golang.org/protobuf/proto"

	"github.com/vid-t/whatsapp-bridge/internal/store"
)

// lockedBuffer collects log output from several goroutines.
type lockedBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *lockedBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *lockedBuffer) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

type optinEnv struct {
	m    *Manager
	db   *sql.DB
	path string
	logs *lockedBuffer
	s    *session
}

const optinSID = "s1"

// newOptinEnv opens a manager on an unencrypted database file (so tests can
// look for message text in its bytes) with one registered account.
func newOptinEnv(t *testing.T) *optinEnv {
	t.Helper()
	path := filepath.Join(t.TempDir(), "bridge.db")
	return openOptinEnv(t, path)
}

func openOptinEnv(t *testing.T, path string) *optinEnv {
	t.Helper()
	db, err := store.Open(path, "")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	logs := &lockedBuffer{}
	m, err := NewManager(context.Background(), db, slog.New(slog.NewTextHandler(logs, nil)), Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(m.Close)
	s := &session{id: optinSID, createdAt: time.Now(), state: StateConnected}
	m.add(s)
	if err := m.insertSession(context.Background(), optinSID, "", time.Now()); err != nil {
		t.Fatal(err)
	}
	return &optinEnv{m: m, db: db, path: path, logs: logs, s: s}
}

var (
	groupA = types.NewJID("120363000000000001", types.GroupServer)
	groupB = types.NewJID("120363000000000002", types.GroupServer)
	dmC    = types.NewJID("15550100003", types.DefaultUserServer)
	ana    = types.NewJID("15550100001", types.DefaultUserServer)
	t0     = time.Date(2026, 10, 20, 9, 0, 0, 0, time.UTC)
)

func liveMsg(chat types.JID, id, text string, ts time.Time) *events.Message {
	return &events.Message{
		Info: types.MessageInfo{
			MessageSource: types.MessageSource{Chat: chat, Sender: ana, IsGroup: chat.Server == types.GroupServer},
			ID:            types.MessageID(id),
			PushName:      "Ana",
			Timestamp:     ts,
		},
		Message: &waE2E.Message{Conversation: proto.String(text)},
	}
}

type histMsg struct {
	id, text string
	ts       time.Time
}

func conversation(chat types.JID, name string, msgs ...histMsg) *waHistorySync.Conversation {
	c := &waHistorySync.Conversation{ID: proto.String(chat.String()), Name: proto.String(name)}
	for _, hm := range msgs {
		c.Messages = append(c.Messages, &waHistorySync.HistorySyncMsg{Message: &waWeb.WebMessageInfo{
			Key:              &waCommon.MessageKey{RemoteJID: proto.String(chat.String()), ID: proto.String(hm.id), Participant: proto.String(ana.String())},
			Message:          &waE2E.Message{Conversation: proto.String(hm.text)},
			MessageTimestamp: proto.Uint64(uint64(hm.ts.Unix())),
			PushName:         proto.String("Ana"),
		}})
	}
	return c
}

// sync runs one history batch and waits for the directory refresh it starts.
func (e *optinEnv) sync(t *testing.T, convs ...*waHistorySync.Conversation) {
	t.Helper()
	ch, unsub := e.m.Subscribe()
	defer unsub()
	e.m.ingestHistory(e.s, &waHistorySync.HistorySync{SyncType: waHistorySync.HistorySync_RECENT.Enum(), Conversations: convs})
	deadline := time.After(5 * time.Second)
	for {
		select {
		case ev := <-ch:
			if ev.Type == EventChats && ev.Text == "directory refreshed" {
				return
			}
		case <-deadline:
			t.Fatal("no directory refresh after the history batch")
		}
	}
}

func (e *optinEnv) chat(t *testing.T, jid types.JID) Chat {
	t.Helper()
	chats, err := e.m.Chats(context.Background(), optinSID)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range chats {
		if c.JID == jid.String() {
			return c
		}
	}
	t.Fatalf("chat %s is not listed", jid)
	return Chat{}
}

func (e *optinEnv) storedMessages(t *testing.T) int {
	t.Helper()
	var n int
	if err := e.db.QueryRow(`SELECT COUNT(*) FROM bridge_messages`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// onDisk reports whether text appears in the database file or its WAL.
func (e *optinEnv) onDisk(t *testing.T, text string) bool {
	t.Helper()
	for _, p := range []string{e.path, e.path + "-wal"} {
		b, err := os.ReadFile(p)
		if err != nil && !os.IsNotExist(err) {
			t.Fatal(err)
		}
		if bytes.Contains(b, []byte(text)) {
			return true
		}
	}
	return false
}

func (e *optinEnv) events(t *testing.T) string {
	t.Helper()
	logs, err := e.m.RecentLogs(context.Background(), "", 1000)
	if err != nil {
		t.Fatal(err)
	}
	var b strings.Builder
	for _, l := range logs {
		b.WriteString(l.Type + " " + l.Detail + "\n")
	}
	return b.String()
}

func TestNewChatsStartUnticked(t *testing.T) {
	e := newOptinEnv(t)
	ctx := context.Background()

	// No window is open (no phone was linked in this process): a new chat
	// gets a listable row, and nothing else.
	e.m.ingestMessage(e.s, liveMsg(groupA, "live-1", "QuietMarker live text", t0))
	e.sync(t, conversation(dmC, "Chris", histMsg{"h-1", "QuietMarker history text", t0.Add(-time.Hour)}))

	if n := e.storedMessages(t); n != 0 {
		t.Fatalf("%d messages stored for chats nobody ticked", n)
	}
	a := e.chat(t, groupA)
	if a.Ticked || !a.IsGroup || a.MessageCount != 0 || a.LastMessageAt == nil || !a.LastMessageAt.Equal(t0) {
		t.Fatalf("live chat row: %+v", a)
	}
	c := e.chat(t, dmC)
	if c.Ticked || c.Name != "Chris" || c.IsGroup || c.MessageCount != 0 || c.LastMessageAt == nil {
		t.Fatalf("history chat row: %+v", c)
	}
	if hits, _ := e.m.SearchMessages(ctx, "", "", "QuietMarker", 10); len(hits) != 0 {
		t.Fatalf("unticked chats are searchable: %+v", hits)
	}
	if e.onDisk(t, "QuietMarker") || strings.Contains(e.logs.String(), "QuietMarker") || strings.Contains(e.events(t), "QuietMarker") {
		t.Fatal("text of an unticked chat reached the database, its WAL or a log")
	}
	if info, _ := e.m.Session(optinSID); info.MessageCount != 0 || info.ChatCount != 2 || info.LastMessageAt != nil || info.HoldUntil != nil {
		t.Fatalf("session counts: %+v", info)
	}

	// The chat can be ticked; from then on its messages are stored.
	if got, err := e.m.SetChatTicked(ctx, optinSID, groupA.String(), true); err != nil || !got.Ticked {
		t.Fatalf("tick: %+v %v", got, err)
	}
	e.m.ingestMessage(e.s, liveMsg(groupA, "live-2", "stored after the tick", t0.Add(time.Minute)))
	if got := e.chat(t, groupA); got.MessageCount != 1 {
		t.Fatalf("message after the tick not stored: %+v", got)
	}
}

func TestTickInWindowStoresHeldHistory(t *testing.T) {
	e := newOptinEnv(t)
	ctx := context.Background()
	e.m.openHold(optinSID)
	info, _ := e.m.Session(optinSID)
	if info.HoldUntil == nil || info.HoldUntil.Sub(time.Now()) < holdWindow-time.Minute {
		t.Fatalf("hold_until after linking: %v", info.HoldUntil)
	}

	e.sync(t,
		conversation(groupA, "Project Atlas",
			histMsg{"a-1", "HeldMarkerA kick-off notes", t0.Add(-2 * time.Hour)},
			histMsg{"a-2", "HeldMarkerA review on Thursday", t0.Add(-time.Hour)}),
		conversation(groupB, "Book club", histMsg{"b-1", "HeldMarkerB next book", t0.Add(-3 * time.Hour)}),
	)
	if n := e.storedMessages(t); n != 0 {
		t.Fatalf("held history written before any tick: %d messages", n)
	}
	if e.onDisk(t, "HeldMarker") || strings.Contains(e.logs.String(), "HeldMarker") || strings.Contains(e.events(t), "HeldMarker") {
		t.Fatal("held text reached the database, its WAL or a log")
	}
	if a := e.chat(t, groupA); a.Ticked || a.Name != "Project Atlas" || a.LastMessageAt == nil || !a.LastMessageAt.Equal(t0.Add(-time.Hour)) {
		t.Fatalf("listable row for a held chat: %+v", a)
	}

	// Ticking inside the window stores that chat's held history, and only that.
	a, err := e.m.SetChatTicked(ctx, optinSID, groupA.String(), true)
	if err != nil || !a.Ticked || a.MessageCount != 2 {
		t.Fatalf("tick in window: %+v %v", a, err)
	}
	msgs, _ := e.m.RecentMessages(ctx, optinSID, groupA.String(), 10)
	if len(msgs) != 2 || msgs[0].Text != "HeldMarkerA kick-off notes" || msgs[0].SenderJID != ana.String() || msgs[0].SenderName != "Ana" {
		t.Fatalf("stored history: %+v", msgs)
	}
	if !e.onDisk(t, "HeldMarkerA") {
		t.Fatal("sanity: stored text should be in the database file or WAL")
	}
	if e.onDisk(t, "HeldMarkerB") {
		t.Fatal("history of a chat still unticked was written")
	}
	if !strings.Contains(e.events(t), "history.kept 2 held messages stored") {
		t.Fatalf("no history.kept event:\n%s", e.events(t))
	}

	// A new message to an unticked chat inside the window is held too, and
	// stored with the chat's history when it is ticked.
	e.m.ingestMessage(e.s, liveMsg(groupB, "b-2", "HeldMarkerB live", t0))
	if b := e.chat(t, groupB); b.MessageCount != 0 {
		t.Fatalf("live message for an unticked chat stored: %+v", b)
	}
	if b, err := e.m.SetChatTicked(ctx, optinSID, groupB.String(), true); err != nil || b.MessageCount != 2 {
		t.Fatalf("tick B: %+v %v", b, err)
	}

	// Unticking deletes; ticking again inside the window does not bring the
	// deleted messages back.
	if b, err := e.m.SetChatTicked(ctx, optinSID, groupB.String(), false); err != nil || b.MessageCount != 0 {
		t.Fatalf("untick B: %+v %v", b, err)
	}
	if b, err := e.m.SetChatTicked(ctx, optinSID, groupB.String(), true); err != nil || b.MessageCount != 0 {
		t.Fatalf("tick B again: %+v %v", b, err)
	}
}

func TestHeldMessagesDroppedWhenWindowEnds(t *testing.T) {
	e := newOptinEnv(t)
	ctx := context.Background()
	now := time.Now()
	e.m.hold.now = func() time.Time { return now }
	e.m.openHold(optinSID)
	e.sync(t, conversation(groupA, "Atlas", histMsg{"a-1", "ExpiredMarker", t0}))

	// Past the window, held messages are neither kept nor offered, and new
	// messages are not held.
	now = now.Add(holdWindow)
	if info, _ := e.m.Session(optinSID); info.HoldUntil != nil {
		t.Fatalf("hold_until after the window: %v", info.HoldUntil)
	}
	e.m.ingestMessage(e.s, liveMsg(groupA, "a-2", "ExpiredMarker live", t0))
	if a, err := e.m.SetChatTicked(ctx, optinSID, groupA.String(), true); err != nil || a.MessageCount != 0 {
		t.Fatalf("tick after the window kept held messages: %+v %v", a, err)
	}

	// The window's timer drops them and logs a count, never the text.
	e.m.endHold(optinSID, "the window ended")
	if e.m.hold.bytes != 0 || len(e.m.hold.sessions) != 0 {
		t.Fatalf("held memory not freed: %d bytes, %d accounts", e.m.hold.bytes, len(e.m.hold.sessions))
	}
	ev := e.events(t)
	if !strings.Contains(ev, "history.dropped 1 held messages of 1 unticked chats dropped") {
		t.Fatalf("no history.dropped event:\n%s", ev)
	}
	if strings.Contains(ev, "ExpiredMarker") || strings.Contains(e.logs.String(), "ExpiredMarker") || e.onDisk(t, "ExpiredMarker") {
		t.Fatal("held text reached a log or the database")
	}
}

func TestHoldTimerDropsHeldMessages(t *testing.T) {
	e := newOptinEnv(t)
	e.m.openHold(optinSID)
	e.sync(t, conversation(groupA, "Atlas", histMsg{"a-1", "TimerMarker", t0}))
	// Bring the end of the window forward to now.
	e.m.hold.mu.Lock()
	e.m.hold.sessions[optinSID].timer.Reset(time.Millisecond)
	e.m.hold.mu.Unlock()
	deadline := time.Now().Add(5 * time.Second)
	for {
		e.m.hold.mu.Lock()
		left := len(e.m.hold.sessions)
		e.m.hold.mu.Unlock()
		if left == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the window's timer did not drop the held messages")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if !strings.Contains(e.events(t), "history.dropped 1 held messages") {
		t.Fatalf("no history.dropped event:\n%s", e.events(t))
	}
}

func TestHoldMemoryCap(t *testing.T) {
	e := newOptinEnv(t)
	ctx := context.Background()
	one := heldSize(Message{SessionID: optinSID, ChatJID: groupA.String(), ID: "a-1", SenderJID: ana.String(), SenderName: "Ana", Kind: "text", Text: "cap 1"})
	e.m.hold.maxBytes = 2 * one
	e.m.openHold(optinSID)
	e.sync(t, conversation(groupA, "Atlas",
		histMsg{"a-1", "cap 1", t0}, histMsg{"a-2", "cap 2", t0.Add(time.Minute)}, histMsg{"a-3", "cap 3", t0.Add(2 * time.Minute)}))
	if e.m.hold.bytes > e.m.hold.maxBytes {
		t.Fatalf("held %d bytes, over the %d cap", e.m.hold.bytes, e.m.hold.maxBytes)
	}
	if !strings.Contains(e.logs.String(), "memory cap reached") || !strings.Contains(e.events(t), "held=2") || !strings.Contains(e.events(t), "not_held=1") {
		t.Fatalf("cap not reported:\n%s\n%s", e.logs.String(), e.events(t))
	}
	if strings.Contains(e.logs.String(), "cap 3") {
		t.Fatal("a refused message's text reached the log")
	}
	// The first messages held are the ones kept.
	if a, err := e.m.SetChatTicked(ctx, optinSID, groupA.String(), true); err != nil || a.MessageCount != 2 {
		t.Fatalf("tick: %+v %v", a, err)
	}
	if e.m.hold.bytes != 0 {
		t.Fatalf("stored messages still counted as held: %d bytes", e.m.hold.bytes)
	}
}

func TestHoldBufferTickedMeanwhile(t *testing.T) {
	h := newHoldBuffer()
	h.open("s", func() {})
	msg := Message{SessionID: "s", ChatJID: "c", ID: "1", Text: "x"}
	if res, kept, _ := h.add("s", "c", []Message{msg}); res != holdKept || kept != 1 {
		t.Fatalf("add: %v %d", res, kept)
	}
	if got := h.take("s", "c"); len(got) != 1 {
		t.Fatalf("take: %+v", got)
	}
	// The caller read "unticked" before the tick landed: it must write instead.
	if res, _, _ := h.add("s", "c", []Message{msg}); res != holdTicked {
		t.Fatalf("add after tick: %v", res)
	}
	h.untick("s", "c")
	if res, _, _ := h.add("s", "c", []Message{msg}); res != holdKept {
		t.Fatalf("add after untick: %v", res)
	}
	h.restore("s", "c", []Message{msg})
	if got := h.take("s", "c"); len(got) != 2 {
		t.Fatalf("restore: %+v", got)
	}
	if h.drop("s"); h.bytes != 0 {
		t.Fatalf("bytes after drop: %d", h.bytes)
	}
	if res, _, _ := h.add("s", "c", []Message{msg}); res != holdClosed {
		t.Fatalf("add without a window: %v", res)
	}
}

func TestRemovingAnAccountDropsHeldMessages(t *testing.T) {
	e := newOptinEnv(t)
	e.s.state = StateLoggedOut
	e.m.openHold(optinSID)
	e.sync(t, conversation(groupA, "Atlas", histMsg{"a-1", "RemovedMarker", t0}))
	if err := e.m.Remove(context.Background(), optinSID); err != nil {
		t.Fatal(err)
	}
	if len(e.m.hold.sessions) != 0 || e.m.hold.bytes != 0 {
		t.Fatal("held messages survived removing the account")
	}
}

// The schema of the release before opt-in chats: ticked defaults to 1.
const tickedDefaultOneSchema = `
CREATE TABLE bridge_chats (
  session_id      TEXT NOT NULL,
  jid             TEXT NOT NULL,
  name            TEXT NOT NULL DEFAULT '',
  is_group        INTEGER NOT NULL DEFAULT 0,
  last_message_at TEXT,
  message_count   INTEGER NOT NULL DEFAULT 0,
  ticked          INTEGER NOT NULL DEFAULT 1,
  PRIMARY KEY (session_id, jid)
);`

// The schema before the ticked column existed.
const noTickedSchema = `
CREATE TABLE bridge_chats (
  session_id      TEXT NOT NULL,
  jid             TEXT NOT NULL,
  name            TEXT NOT NULL DEFAULT '',
  is_group        INTEGER NOT NULL DEFAULT 0,
  last_message_at TEXT,
  message_count   INTEGER NOT NULL DEFAULT 0,
  PRIMARY KEY (session_id, jid)
);`

const oldMessagesSchema = `
CREATE TABLE bridge_messages (
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
INSERT INTO bridge_messages VALUES ('s1', '120363000000000001@g.us', 'old-1', '15550100001@s.whatsapp.net', 'Ana', '2026-10-01T09:00:00Z', 0, 'text', 'UpgradeMarker kept');`

// oldDatabase writes a database as an earlier release left it.
func oldDatabase(t *testing.T, stmts ...string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "bridge.db")
	db, err := store.Open(path, "")
	if err != nil {
		t.Fatal(err)
	}
	for _, st := range stmts {
		if _, err := db.Exec(st); err != nil {
			t.Fatal(err)
		}
	}
	db.Close()
	return path
}

func TestUpgradeKeepsExistingTicks(t *testing.T) {
	path := oldDatabase(t, tickedDefaultOneSchema, oldMessagesSchema,
		`INSERT INTO bridge_chats (session_id, jid, name, is_group, message_count, ticked) VALUES
		   ('s1', '120363000000000001@g.us', 'Atlas', 1, 1, 1),
		   ('s1', '120363000000000002@g.us', 'Book club', 1, 0, 0)`)
	e := openOptinEnv(t, path)
	ctx := context.Background()

	if a := e.chat(t, groupA); !a.Ticked || a.MessageCount != 1 {
		t.Fatalf("ticked chat changed on upgrade: %+v", a)
	}
	if b := e.chat(t, groupB); b.Ticked {
		t.Fatalf("unticked chat changed on upgrade: %+v", b)
	}
	if hits, _ := e.m.SearchMessages(ctx, "", "", "UpgradeMarker", 10); len(hits) != 1 {
		t.Fatalf("stored message not visible after upgrade: %+v", hits)
	}
	// The column still defaults to 1 here; new chats must start unticked anyway.
	e.m.ingestMessage(e.s, liveMsg(dmC, "c-1", "NewChatMarker", t0))
	if c := e.chat(t, dmC); c.Ticked || c.MessageCount != 0 {
		t.Fatalf("new chat on an upgraded database: %+v", c)
	}
	e.m.ingestMessage(e.s, liveMsg(groupA, "a-2", "still stored", t0))
	if a := e.chat(t, groupA); a.MessageCount != 2 {
		t.Fatalf("ticked chat stopped storing after upgrade: %+v", a)
	}
}

func TestUpgradeFromBeforeTheTickedColumn(t *testing.T) {
	path := oldDatabase(t, noTickedSchema, oldMessagesSchema,
		`INSERT INTO bridge_chats (session_id, jid, name, is_group, message_count) VALUES
		   ('s1', '120363000000000001@g.us', 'Atlas', 1, 1),
		   ('s1', '120363000000000002@g.us', 'Book club', 1, 0)`)
	e := openOptinEnv(t, path)
	ctx := context.Background()

	// That bridge stored every chat, so every chat stays ticked and visible.
	for _, j := range []types.JID{groupA, groupB} {
		if c := e.chat(t, j); !c.Ticked {
			t.Fatalf("chat hidden by the migration: %+v", c)
		}
	}
	if hits, _ := e.m.SearchMessages(ctx, "", "", "UpgradeMarker", 10); len(hits) != 1 {
		t.Fatalf("stored message hidden by the migration: %+v", hits)
	}
	var dflt string
	if err := e.db.QueryRow(`SELECT dflt_value FROM pragma_table_info('bridge_chats') WHERE name = 'ticked'`).Scan(&dflt); err != nil || dflt != "0" {
		t.Fatalf("ticked column default after migration: %q %v", dflt, err)
	}
	e.m.ingestMessage(e.s, liveMsg(dmC, "c-1", "new", t0))
	if c := e.chat(t, dmC); c.Ticked {
		t.Fatalf("new chat after migration: %+v", c)
	}
}

func TestDatabaseErrorFailsClosed(t *testing.T) {
	e := newOptinEnv(t)
	ctx := context.Background()
	seedChat(t, e.m, optinSID, groupA.String(), "Atlas")
	if ticked, known := e.m.chatTicked(ctx, optinSID, groupA.String()); !ticked || !known {
		t.Fatalf("seeded chat: %v %v", ticked, known)
	}
	if ticked, known := e.m.chatTicked(ctx, optinSID, "unknown@g.us"); ticked || known {
		t.Fatalf("unknown chat: %v %v", ticked, known)
	}

	// The chat list cannot be read: the chat counts as unticked and nothing
	// is stored, even though its row says ticked.
	if _, err := e.db.Exec(`ALTER TABLE bridge_chats RENAME TO bridge_chats_away`); err != nil {
		t.Fatal(err)
	}
	if ticked, _ := e.m.chatTicked(ctx, optinSID, groupA.String()); ticked {
		t.Fatal("a database error must mean not ticked")
	}
	e.m.ingestMessage(e.s, liveMsg(groupA, "a-1", "FailClosedMarker", t0))
	if ok, err := insertMessage(ctx, e.db, Message{SessionID: optinSID, ChatJID: groupA.String(), ID: "a-2", Timestamp: t0, Kind: "text", Text: "FailClosedMarker"}); ok || err == nil {
		t.Fatalf("insert without the chat list: ok=%v err=%v", ok, err)
	}
	if _, err := e.db.Exec(`ALTER TABLE bridge_chats_away RENAME TO bridge_chats`); err != nil {
		t.Fatal(err)
	}
	if n := e.storedMessages(t); n != 0 {
		t.Fatalf("%d messages stored during the database error", n)
	}
	if !strings.Contains(e.logs.String(), "treating the chat as unticked") {
		t.Fatalf("the error was not logged:\n%s", e.logs.String())
	}
}

func TestMergeKeepsATick(t *testing.T) {
	e := newOptinEnv(t)
	ctx := context.Background()
	lid := "123456789@lid"
	seedChat(t, e.m, optinSID, lid, "Ana", "merged message")
	if err := e.m.upsertChat(ctx, optinSID, dmC.String(), "Ana", false, nil); err != nil {
		t.Fatal(err)
	}
	if err := e.m.mergeChat(ctx, optinSID, lid, dmC.String()); err != nil {
		t.Fatal(err)
	}
	if c := e.chat(t, dmC); !c.Ticked || c.MessageCount != 1 {
		t.Fatalf("merged chat lost its tick or messages: %+v", c)
	}

	// Two unticked rows merge into an unticked chat with no messages.
	if err := e.m.upsertChat(ctx, optinSID, "987@lid", "Bo", false, nil); err != nil {
		t.Fatal(err)
	}
	other := types.NewJID("15550100009", types.DefaultUserServer)
	if err := e.m.upsertChat(ctx, optinSID, other.String(), "", false, nil); err != nil {
		t.Fatal(err)
	}
	if err := e.m.mergeChat(ctx, optinSID, "987@lid", other.String()); err != nil {
		t.Fatal(err)
	}
	if c := e.chat(t, other); c.Ticked || c.Name != "Bo" {
		t.Fatalf("merged unticked chat: %+v", c)
	}
}
