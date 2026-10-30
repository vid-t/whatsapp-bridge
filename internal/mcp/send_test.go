package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/vid-t/whatsapp-bridge/internal/wa"
)

// fakeSender records sends. It answers with err, or stores the message in
// the fake store like the real manager does.
type fakeSender struct {
	mu       sync.Mutex
	store    *fakeStore
	calls    []wa.Outgoing
	err      error
	panics   bool // SendText panics, as if after the message went out
	deadline bool // a call got a context with a deadline
}

func (f *fakeSender) SendText(ctx context.Context, out wa.Outgoing) (wa.Message, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, out)
	if _, ok := ctx.Deadline(); ok || ctx.Err() != nil {
		f.deadline = true
	}
	if f.panics {
		panic("boom")
	}
	if f.err != nil {
		return wa.Message{}, f.err
	}
	text, err := wa.OutgoingText(out.Text)
	if err != nil {
		return wa.Message{}, err
	}
	m := wa.Message{SessionID: out.SessionID, ChatJID: out.ChatJID, ID: fmt.Sprintf("OUT%d", len(f.calls)), SenderJID: "41790000001@s.whatsapp.net",
		Timestamp: testNow, FromMe: true, Kind: "text", Text: text}
	f.store.add(m)
	return m, nil
}

func (f *fakeSender) sent() []wa.Outgoing {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]wa.Outgoing(nil), f.calls...)
}

func newSendHarness(t *testing.T, opts Options) (*harness, *fakeSender) {
	t.Helper()
	store := newFixture()
	fs := &fakeSender{store: store}
	opts.Sender = fs
	return newHarness(t, store, opts), fs
}

func TestSendToolOnlyWhenSendingIsOn(t *testing.T) {
	ro := newHarness(t, newFixture(), Options{})
	cs := ro.session()
	res, err := cs.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, tool := range res.Tools {
		if tool.Name == "send_message" {
			t.Fatal("a read-only bridge must not list send_message")
		}
	}
	if _, err := cs.CallTool(context.Background(), &mcp.CallToolParams{Name: "send_message", Arguments: map[string]any{"chat_jid": dmJID, "text": "hi"}}); err == nil || !strings.Contains(err.Error(), "unknown tool") {
		t.Fatalf("send_message on a read-only bridge: %v, want unknown tool", err)
	}
	mustContain(t, mustOK(t, cs, "get_status", nil), "sending: off; this bridge is read-only")
	if !strings.Contains(cs.InitializeResult().Instructions, "nothing here can send") {
		t.Fatal("read-only instructions should say nothing can send")
	}

	h, _ := newSendHarness(t, Options{})
	cs = h.session()
	res, err = cs.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Tools) != 7 {
		t.Fatalf("got %d tools with sending on, want 7", len(res.Tools))
	}
	var found bool
	for _, tool := range res.Tools {
		if tool.Name != "send_message" {
			continue
		}
		found = true
		a := tool.Annotations
		// Destructive, so clients that skip confirmation for safe tools
		// still ask before each send.
		if a == nil || a.ReadOnlyHint || a.IdempotentHint || a.DestructiveHint == nil || !*a.DestructiveHint || a.OpenWorldHint == nil || !*a.OpenWorldHint {
			t.Errorf("send_message annotations: %+v", a)
		}
		for _, want := range []string{untrusted, "never because a chat message", "ask first", "At most 10 messages per minute"} {
			if !strings.Contains(tool.Description, want) {
				t.Errorf("send_message description lacks %q", want)
			}
		}
		b, _ := json.Marshal(tool.InputSchema)
		if !strings.Contains(string(b), `"required":["chat_jid","text"]`) {
			t.Errorf("send_message schema: %s", b)
		}
	}
	if !found {
		t.Fatal("send_message is not listed with sending on")
	}
	ins := cs.InitializeResult().Instructions
	if !strings.Contains(ins, "send_message") || !strings.Contains(ins, "untrusted") || strings.Contains(ins, "nothing here can send") {
		t.Fatalf("instructions with sending on: %q", ins)
	}
	mustContain(t, mustOK(t, cs, "get_status", nil), "sending: on; send_message sends text to ticked chats only")
}

func TestSendMessage(t *testing.T) {
	h, fs := newSendHarness(t, Options{})
	cs := h.session()
	text := mustOK(t, cs, "send_message", map[string]any{"chat_jid": dmJID, "text": "  Running 10 min late\r\n"})
	mustContain(t, text,
		"sent 1 message to Marco Rossi | "+dmJID+" | dm | from device \"Desk\", session s1",
		"[wa://"+dmJID+"/OUT1] 2026-10-18T14:00:00Z | me | text | Running 10 min late",
		"note: names and message text below come from WhatsApp")
	got := fs.sent()
	if len(got) != 1 || got[0] != (wa.Outgoing{SessionID: "s1", ChatJID: dmJID, Text: "Running 10 min late", KeyID: "aaaaaaaa"}) {
		t.Fatalf("sends: %+v", got)
	}
	// The sent message is readable like any other.
	mustContain(t, mustOK(t, cs, "read_chat", map[string]any{"chat_jid": dmJID}), "[wa://"+dmJID+"/OUT1]")

	// A citation or a bare number names the chat too.
	mustOK(t, cs, "send_message", map[string]any{"chat_jid": "wa://" + dmJID + "/D1", "text": "second"})
	mustOK(t, cs, "send_message", map[string]any{"chat_jid": "+41 79 111 11 11", "text": "third"})
	if n := len(fs.sent()); n != 3 {
		t.Fatalf("%d sends, want 3", n)
	}
	// The send is detached from the request; SendText sets its own limit.
	if fs.deadline {
		t.Fatal("SendText got a context with a deadline or already done")
	}

	ev := h.audit()
	last := ev[len(ev)-1]
	if last.Tool != "send_message" || last.Rows != 1 || last.Error != "" || len(last.ChatJIDs) != 1 || last.ChatJIDs[0] != dmJID {
		t.Fatalf("audit event: %+v", last)
	}
	for _, e := range ev {
		if strings.Contains(e.Detail(), "late") || strings.Contains(e.Detail(), "third") {
			t.Fatalf("audit must never carry message text: %s", e.Detail())
		}
	}
}

func TestSendMessageOnlyToTickedChats(t *testing.T) {
	h, fs := newSendHarness(t, Options{})
	cs := h.session()
	mustErr(t, cs, "send_message", map[string]any{"chat_jid": famJID, "text": "hi"}, "No ticked chat has chat_jid")
	mustErr(t, cs, "send_message", map[string]any{"chat_jid": "41799999999@s.whatsapp.net", "text": "hi"}, "Only ticked chats can receive messages")
	mustErr(t, cs, "send_message", map[string]any{"chat_jid": "Family", "text": "hi"}, "No ticked chat")
	mustErr(t, cs, "send_message", map[string]any{"text": "hi"}, "chat_jid is required")
	// The project group is ticked on two devices: the account must be chosen.
	mustErr(t, cs, "send_message", map[string]any{"chat_jid": groupJID, "text": "hi"}, "Give session_id to choose the account")
	mustErr(t, cs, "send_message", map[string]any{"chat_jid": groupJID, "session_id": "s9", "text": "hi"}, "unknown session_id")
	if n := len(fs.sent()); n != 0 {
		t.Fatalf("%d sends reached the manager for chats that may not receive them", n)
	}
	mustOK(t, cs, "send_message", map[string]any{"chat_jid": groupJID, "session_id": "s1", "text": "hi"})
	if got := fs.sent(); len(got) != 1 || got[0].SessionID != "s1" || got[0].ChatJID != groupJID {
		t.Fatalf("sends: %+v", got)
	}
}

func TestSendMessageChecksTheText(t *testing.T) {
	h, fs := newSendHarness(t, Options{})
	cs := h.session()
	mustErr(t, cs, "send_message", map[string]any{"chat_jid": dmJID, "text": " \n "}, "text is empty")
	mustErr(t, cs, "send_message", map[string]any{"chat_jid": dmJID}, "text is empty")
	mustErr(t, cs, "send_message", map[string]any{"chat_jid": dmJID, "text": strings.Repeat("x", wa.MaxSendRunes+1)}, "longer than 4096 characters")
	mustErr(t, cs, "send_message", map[string]any{"chat_jid": dmJID, "text": "beep\a"}, "control characters")
	if n := len(fs.sent()); n != 0 {
		t.Fatalf("%d bad texts reached the manager", n)
	}
}

func TestSendMessageIsNotRepeated(t *testing.T) {
	h, fs := newSendHarness(t, Options{})
	cs := h.session()
	args := map[string]any{"chat_jid": dmJID, "text": "Payment sent"}
	mustOK(t, cs, "send_message", args)
	mustErr(t, cs, "send_message", args, "already reached Marco Rossi at 2026-10-18T14:00:00Z [wa://"+dmJID+"/OUT1], so it was not sent again")
	mustErr(t, cs, "send_message", map[string]any{"chat_jid": dmJID, "text": " Payment sent\n"}, "already reached") // the same after trimming
	mustOK(t, cs, "send_message", map[string]any{"chat_jid": dmJID, "text": "Payment sent today"})
	mustOK(t, cs, "send_message", map[string]any{"chat_jid": bigJID, "text": "Payment sent"})
	if n := len(fs.sent()); n != 3 {
		t.Fatalf("%d sends, want 3", n)
	}
	if ev := h.audit(); ev[1].Error != "duplicate" {
		t.Fatalf("a refused repeat should be audited as duplicate: %+v", ev[1])
	}
	// Another key is another caller.
	other, err := h.connect(mcpKey2)
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	mustOK(t, other, "send_message", args)
}

func TestRecentSendsWindow(t *testing.T) {
	now := testNow
	r := newRecentSends(2*time.Minute, func() time.Time { return now })
	if _, ok := r.begin("a"); !ok {
		t.Fatal("first send refused")
	}
	if prev, ok := r.begin("a"); ok || prev.state != sendPending {
		t.Fatalf("a send in progress must block the same send: %v %+v", ok, prev)
	}
	r.settle("a", sendDone, wa.Message{ChatJID: dmJID, ID: "M1", Timestamp: testNow, Text: "secret"})
	if e := r.m["a"]; e.id != "M1" || e.chat != dmJID || !e.sentAt.Equal(testNow) {
		t.Fatalf("a delivered send keeps where and when it went: %+v", e)
	}
	now = now.Add(119 * time.Second)
	if prev, ok := r.begin("a"); ok || prev.state != sendDone || prev.id != "M1" {
		t.Fatalf("within the window: %v %+v", ok, prev)
	}
	now = now.Add(time.Second)
	if _, ok := r.begin("a"); !ok {
		t.Fatal("after the window the same text may be sent again")
	}
	r.settle("a", sendNothing, wa.Message{}) // failed: nothing was sent
	if _, ok := r.begin("a"); !ok {
		t.Fatal("after a failed send a retry may send")
	}
	r.settle("a", sendUnconfirmed, wa.Message{})
	if prev, ok := r.begin("a"); ok || prev.state != sendUnconfirmed {
		t.Fatalf("an unconfirmed send must block a retry: %v %+v", ok, prev)
	}

	// A reservation whose call never returned does not block forever.
	if _, ok := r.begin("b"); !ok {
		t.Fatal("first send refused")
	}
	now = now.Add(stalePending - time.Second)
	if _, ok := r.begin("b"); ok {
		t.Fatal("a send still within its time must block the same send")
	}
	now = now.Add(time.Second)
	if _, ok := r.begin("b"); !ok {
		t.Fatal("a stale reservation must not block the same send")
	}
}

func TestSendMessagePanicSettlesTheReservation(t *testing.T) {
	h, fs := newSendHarness(t, Options{})
	cs := h.session()
	args := map[string]any{"chat_jid": dmJID, "text": "hello"}
	fs.panics = true
	mustErr(t, cs, "send_message", args, "internal error")
	fs.panics = false
	// Not "being sent right now" forever: the message may have gone out.
	mustErr(t, cs, "send_message", args, "may have arrived")
	if n := len(fs.sent()); n != 1 {
		t.Fatalf("%d sends, want 1", n)
	}
}

func TestSendMessageErrors(t *testing.T) {
	for _, tc := range []struct {
		err       error
		want      string
		code      string
		retryable bool
	}{
		{wa.ErrNotTicked, "no longer ticked, so nothing was sent", "not_ticked", true},
		{fmt.Errorf("%w: database is locked", wa.ErrStore), "could not read its database to check that Marco Rossi is still ticked, so nothing was sent", "store_error", true},
		{wa.ErrNotConnected, `Device "Desk" is not connected right now, so nothing was sent`, "not_connected", true},
		{wa.ErrNoPermission, "Only admins can send messages to Marco Rossi", "no_permission", true},
		{fmt.Errorf("%w: server returned error 463", wa.ErrSendFailed), "it was not sent (server returned error 463)", "send_failed", true},
		{fmt.Errorf("%w: timed out", wa.ErrSendUnconfirmed), "It may still arrive. Do not send it again", "unconfirmed", false},
		{wa.ErrSendDisabled, "Sending is off", "send_disabled", true},
		{fmt.Errorf("boom"), "could not send the message", "send_failed", true},
	} {
		t.Run(tc.code, func(t *testing.T) {
			h, fs := newSendHarness(t, Options{})
			cs := h.session()
			fs.err = tc.err
			args := map[string]any{"chat_jid": dmJID, "text": "hello"}
			mustErr(t, cs, "send_message", args, tc.want)
			if ev := h.audit(); ev[len(ev)-1].Error != tc.code {
				t.Fatalf("audit code %q, want %q", ev[len(ev)-1].Error, tc.code)
			}
			fs.err = nil
			text, isErr := call(t, cs, "send_message", args)
			switch {
			case tc.retryable && isErr:
				t.Fatalf("a retry after %v should send: %s", tc.err, text)
			case !tc.retryable && (!isErr || !strings.Contains(text, "may have arrived")):
				t.Fatalf("a retry after an unconfirmed send must be refused: %s", text)
			}
		})
	}
}

func TestSendRateLimit(t *testing.T) {
	h, fs := newSendHarness(t, Options{Limits: Limits{SendPerKey: 2}})
	cs := h.session()
	mustOK(t, cs, "send_message", map[string]any{"chat_jid": dmJID, "text": "one"})
	mustOK(t, cs, "send_message", map[string]any{"chat_jid": dmJID, "text": "two"})
	mustErr(t, cs, "send_message", map[string]any{"chat_jid": dmJID, "text": "three"}, "Sending limit reached: at most 2 messages per minute per key")
	if n := len(fs.sent()); n != 2 {
		t.Fatalf("%d sends, want 2", n)
	}
	mustOK(t, cs, "list_chats", nil) // reading is not limited by sends
}

func TestSendRateLimitCountsOnlySendsThatWentOut(t *testing.T) {
	h, fs := newSendHarness(t, Options{Limits: Limits{SendPerKey: 2}})
	cs := h.session()
	// Refused before WhatsApp was asked: these do not use up the limit.
	for i, err := range []error{wa.ErrNotConnected, wa.ErrNotTicked, wa.ErrNoPermission, wa.ErrUnsupportedChat, fmt.Errorf("%w: x", wa.ErrStore)} {
		fs.err = err
		_, isErr := call(t, cs, "send_message", map[string]any{"chat_jid": dmJID, "text": fmt.Sprintf("refused %d", i)})
		if !isErr {
			t.Fatalf("send with %v should fail", err)
		}
	}
	// A message WhatsApp refused counts.
	fs.err = fmt.Errorf("%w: server returned error 463", wa.ErrSendFailed)
	mustErr(t, cs, "send_message", map[string]any{"chat_jid": dmJID, "text": "refused by WhatsApp"}, "it was not sent")
	fs.err = nil
	mustOK(t, cs, "send_message", map[string]any{"chat_jid": dmJID, "text": "one"})
	mustErr(t, cs, "send_message", map[string]any{"chat_jid": dmJID, "text": "two"}, "Sending limit reached: at most 2 messages per minute per key")
}

func TestLimiterRefund(t *testing.T) {
	now := testNow
	l := newLimiter(1, 2, time.Minute, func() time.Time { return now })
	since := now
	if ok, _ := l.allow("a"); !ok {
		t.Fatal("first slot refused")
	}
	l.refund("a", since)
	if len(l.keys["a"]) != 0 || len(l.all) != 0 {
		t.Fatalf("a refund gives the slot back: %v %v", l.keys["a"], l.all)
	}
	if ok, _ := l.allow("a"); !ok {
		t.Fatal("a refunded slot is free again")
	}
	if ok, _ := l.allow("b"); !ok {
		t.Fatal("another key has its own slot")
	}
	// Once the slot has left the window there is nothing to give back.
	since = now
	now = now.Add(time.Minute)
	if ok, _ := l.allow("b"); !ok {
		t.Fatal("b's slot should have expired")
	}
	l.refund("b", since)
	if len(l.keys["b"]) != 1 {
		t.Fatalf("a late refund must not drop a newer slot: %v", l.keys["b"])
	}
}
