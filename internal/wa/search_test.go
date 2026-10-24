package wa

import (
	"context"
	"errors"
	"testing"
	"time"
)

// seedChat creates a ticked chat with the given messages.
func seedChat(t *testing.T, m *Manager, sid, jid, name string, texts ...string) {
	t.Helper()
	ctx := context.Background()
	if err := m.upsertChat(ctx, sid, jid, name, true, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := m.tickChat(ctx, sid, jid, nil); err != nil {
		t.Fatal(err)
	}
	base := time.Date(2026, 10, 1, 9, 0, 0, 0, time.UTC)
	for i, txt := range texts {
		msg := Message{SessionID: sid, ChatJID: jid, ID: jid + "-" + string(rune('a'+i)), SenderJID: "1@s.whatsapp.net", SenderName: "Ana",
			Timestamp: base.Add(time.Duration(i) * time.Minute), Kind: "text", Text: txt}
		if ok, err := insertMessage(ctx, m.db, msg); err != nil || !ok {
			t.Fatalf("insert into a ticked chat: ok=%v err=%v", ok, err)
		}
	}
	if err := m.recountChat(ctx, sid, jid); err != nil {
		t.Fatal(err)
	}
}

func TestSearchFindTickContext(t *testing.T) {
	m := newTestManager(t)
	if !m.fts {
		t.Fatal("FTS5 not available in this SQLite build")
	}
	ctx := context.Background()
	const sid = "s1"
	_ = m.insertSession(ctx, sid, "", time.Now())
	m.add(&session{id: sid, createdAt: time.Now(), state: StateDisconnected})
	seedChat(t, m, sid, "g1@g.us", "Atlas Team",
		"For INV-027, November is agreed.",
		"Can you confirm 80 chairs for 1–10 Nov?",
		"Pro forma attached: PI-INV-027-01",
		"Café arrivé à Abidjan hier")
	seedChat(t, m, sid, "g2@g.us", "Project Atlas", "Venue booked for 12 Nov, reference BK1234567")

	// Full-text, diacritics folded, prefix on the last token.
	hits, err := m.SearchMessages(ctx, "", "", "cafe abid", 10)
	if err != nil || len(hits) != 1 || hits[0].ChatName != "Atlas Team" {
		t.Fatalf("diacritics search: %+v %v", hits, err)
	}
	hits, _ = m.SearchMessages(ctx, "", "", "INV-027", 10)
	if len(hits) != 2 {
		t.Fatalf("expected 2 hits for INV-027, got %d", len(hits))
	}
	hits, _ = m.SearchMessages(ctx, sid, "g2@g.us", "nov", 10)
	if len(hits) != 1 || hits[0].ChatName != "Project Atlas" {
		t.Fatalf("scoped search: %+v", hits)
	}

	// FindChat by name, case, prefix, jid.
	for _, q := range []string{"Atlas Team", "atlas team", "Atla", "g1@g.us"} {
		c, err := m.FindChat(ctx, "", q)
		if err != nil || c.JID != "g1@g.us" {
			t.Fatalf("FindChat(%q): %+v %v", q, c, err)
		}
	}
	if _, err := m.FindChat(ctx, "", "nope"); !errors.Is(err, ErrNoChat) {
		t.Fatalf("expected ErrNoChat, got %v", err)
	}
	// LIKE wildcards in a title must not match everything.
	if _, err := m.FindChat(ctx, "", "%"); !errors.Is(err, ErrNoChat) {
		t.Fatalf("wildcard title must not match, got %v", err)
	}
	if _, err := m.FindChat(ctx, "", "A_las"); !errors.Is(err, ErrNoChat) {
		t.Fatalf("underscore wildcard must not match, got %v", err)
	}

	// Context around the second message.
	msgs, err := m.MessageContext(ctx, sid, "g1@g.us", "g1@g.us-b", 1, 1)
	if err != nil || len(msgs) != 3 || msgs[1].ID != "g1@g.us-b" {
		t.Fatalf("context: %+v %v", msgs, err)
	}

	// Untick deletes messages and hides the chat from search.
	c, err := m.SetChatTicked(ctx, sid, "g1@g.us", false)
	if err != nil || c.Ticked || c.MessageCount != 0 {
		t.Fatalf("untick: %+v %v", c, err)
	}
	if hits, _ := m.SearchMessages(ctx, "", "", "INV-027", 10); len(hits) != 0 {
		t.Fatalf("unticked chat still searchable: %+v", hits)
	}
	if _, err := m.FindChat(ctx, "", "Atlas Team"); !errors.Is(err, ErrNoChat) {
		t.Fatal("unticked chat still findable")
	}
	if ticked, known := m.chatTicked(ctx, sid, "g1@g.us"); ticked || !known {
		t.Fatalf("chatTicked = %v, %v; want false, true", ticked, known)
	}
	c, err = m.SetChatTicked(ctx, sid, "g1@g.us", true)
	if err != nil || !c.Ticked {
		t.Fatalf("retick: %+v %v", c, err)
	}
	if _, err := m.SetChatTicked(ctx, sid, "missing@g.us", true); !errors.Is(err, ErrNoChat) {
		t.Fatalf("expected ErrNoChat for unknown chat, got %v", err)
	}
}
