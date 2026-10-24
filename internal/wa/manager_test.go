package wa

import (
	"context"
	"errors"
	"log/slog"
	"path/filepath"
	"testing"
	"time"

	"github.com/vid-t/whatsapp-bridge/internal/store"
)

func newTestManager(t *testing.T) *Manager {
	t.Helper()
	db, err := store.Open(filepath.Join(t.TempDir(), "t.db"), "")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	m, err := NewManager(context.Background(), db, slog.New(slog.NewTextHandler(testWriter{t}, nil)), Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(m.Close)
	return m
}

type testWriter struct{ t *testing.T }

func (w testWriter) Write(p []byte) (int, error) { w.t.Log(string(p)); return len(p), nil }

func TestNewManagerEmpty(t *testing.T) {
	m := newTestManager(t)
	if got := m.Sessions(); len(got) != 0 {
		t.Fatalf("expected no sessions, got %d", len(got))
	}
	if _, err := m.Session("nope"); !errors.Is(err, ErrNoSession) {
		t.Fatalf("expected ErrNoSession, got %v", err)
	}
	if _, err := m.Chats(context.Background(), "nope"); !errors.Is(err, ErrNoSession) {
		t.Fatalf("expected ErrNoSession, got %v", err)
	}
}

func TestPairWithCodeRejectsShortPhone(t *testing.T) {
	m := newTestManager(t)
	if _, _, err := m.PairWithCode(context.Background(), "+41 12", ""); !errors.Is(err, ErrBadPhone) {
		t.Fatalf("expected ErrBadPhone, got %v", err)
	}
}

func TestPersistenceRoundTrip(t *testing.T) {
	m := newTestManager(t)
	ctx := context.Background()
	const sid = "s1"
	if err := m.insertSession(ctx, sid, "label", time.Now()); err != nil {
		t.Fatal(err)
	}
	// Register the session so the public accessors accept the id.
	m.add(&session{id: sid, createdAt: time.Now(), state: StateDisconnected})

	ts := time.Date(2026, 10, 18, 12, 0, 0, 0, time.UTC)
	msg := Message{SessionID: sid, ChatJID: "123@g.us", ID: "m1", SenderJID: "1@s.whatsapp.net", SenderName: "A", Timestamp: ts, Kind: "text", Text: "80 MT early November"}
	if err := m.upsertChat(ctx, sid, "123@g.us", "Atlas Team", true, &ts); err != nil {
		t.Fatal(err)
	}
	ok, err := insertMessage(ctx, m.db, msg) // a new chat starts unticked
	if err != nil || ok {
		t.Fatalf("insert into an unticked chat: ok=%v err=%v", ok, err)
	}
	if _, err := m.tickChat(ctx, sid, "123@g.us", nil); err != nil {
		t.Fatal(err)
	}
	ok, err = insertMessage(ctx, m.db, msg)
	if err != nil || !ok {
		t.Fatalf("insert: ok=%v err=%v", ok, err)
	}
	ok, err = insertMessage(ctx, m.db, msg) // duplicate is ignored
	if err != nil || ok {
		t.Fatalf("duplicate insert: ok=%v err=%v", ok, err)
	}
	if err := m.upsertChat(ctx, sid, "123@g.us", "", true, nil); err != nil { // empty name must not overwrite
		t.Fatal(err)
	}
	if err := m.recountChat(ctx, sid, "123@g.us"); err != nil {
		t.Fatal(err)
	}

	chats, err := m.Chats(ctx, sid)
	if err != nil || len(chats) != 1 {
		t.Fatalf("chats: %v %v", chats, err)
	}
	if chats[0].Name != "Atlas Team" || !chats[0].IsGroup || chats[0].MessageCount != 1 || chats[0].LastMessageAt == nil || !chats[0].LastMessageAt.Equal(ts) {
		t.Fatalf("unexpected chat: %+v", chats[0])
	}
	msgs, err := m.RecentMessages(ctx, sid, "123@g.us", 10)
	if err != nil || len(msgs) != 1 || msgs[0].Text != msg.Text || !msgs[0].Timestamp.Equal(ts) {
		t.Fatalf("messages: %+v %v", msgs, err)
	}
	info, err := m.Session(sid)
	if err != nil || info.MessageCount != 1 || info.ChatCount != 1 {
		t.Fatalf("session counts: %+v %v", info, err)
	}
	m.appendLog(ctx, sid, "state.connected", "")
	logs, err := m.RecentLogs(ctx, sid, 10)
	if err != nil || len(logs) != 1 || logs[0].Type != "state.connected" {
		t.Fatalf("logs: %+v %v", logs, err)
	}
	if err := m.deleteSessionData(ctx, sid); err != nil {
		t.Fatal(err)
	}
	if chats, _ := m.chats(ctx, sid); len(chats) != 0 {
		t.Fatal("chats survived delete")
	}
}

func TestSubscribePublish(t *testing.T) {
	m := newTestManager(t)
	ch, unsub := m.Subscribe()
	defer unsub()
	m.publish(Event{Type: EventLog, Text: "hi"})
	select {
	case ev := <-ch:
		if ev.Type != EventLog || ev.Text != "hi" || ev.Time.IsZero() {
			t.Fatalf("unexpected event %+v", ev)
		}
	case <-time.After(time.Second):
		t.Fatal("no event received")
	}
}

func TestPairingCap(t *testing.T) {
	m := newTestManager(t)
	for i := 0; i < m.opts.MaxConcurrentPairings; i++ {
		m.add(&session{id: newID(), createdAt: time.Now(), state: StatePairing})
	}
	if _, _, err := m.StartPairing("x"); !errors.Is(err, ErrTooManyPairings) {
		t.Fatalf("StartPairing: want ErrTooManyPairings, got %v", err)
	}
	if _, _, err := m.PairWithCode(context.Background(), "41791234567", "x"); !errors.Is(err, ErrTooManyPairings) {
		t.Fatalf("PairWithCode: want ErrTooManyPairings, got %v", err)
	}
}

func TestRemoveUnpairedSession(t *testing.T) {
	m := newTestManager(t)
	ctx := context.Background()
	s := &session{id: newID(), createdAt: time.Now(), state: StateLoggedOut}
	m.add(s)
	if err := m.insertSession(ctx, s.id, "", time.Now()); err != nil {
		t.Fatal(err)
	}
	if err := m.Remove(ctx, s.id); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	if _, err := m.Session(s.id); !errors.Is(err, ErrNoSession) {
		t.Fatalf("session still present after Remove: %v", err)
	}
	logs, _ := m.RecentLogs(ctx, s.id, 5)
	if len(logs) == 0 || logs[0].Type != "session.remove" {
		t.Fatalf("expected session.remove log entry, got %+v", logs)
	}
}

func TestStateNotesAreNotErrors(t *testing.T) {
	m := newTestManager(t)
	s := &session{id: newID(), createdAt: time.Now(), state: StateUnpaired}
	m.add(s)
	m.setState(s, StatePairing, "waiting for QR scan")
	if info, _ := m.Session(s.id); info.LastError != "" {
		t.Fatalf("pairing note leaked into LastError: %q", info.LastError)
	}
	m.setState(s, StateDisconnected, "connect failure: 503")
	if info, _ := m.Session(s.id); info.LastError != "connect failure: 503" {
		t.Fatalf("disconnect reason missing from LastError: %q", info.LastError)
	}
	m.setState(s, StateConnecting, "")
	if info, _ := m.Session(s.id); info.LastError != "" {
		t.Fatalf("LastError should clear on connecting: %q", info.LastError)
	}
}
