package wa

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/types"
)

// fakeTransport stands in for whatsmeow: it records each send and gives it a
// new message id, then answers with err or with the sent message.
type fakeTransport struct {
	mu      sync.Mutex
	calls   []string // "<to> <text>"
	err     error
	started chan struct{} // when set, receives once per send
	release chan struct{} // when set, a send waits until it is closed
}

func (f *fakeTransport) send(_ context.Context, _ *session, to types.JID, text string) (sent, error) {
	f.mu.Lock()
	f.calls = append(f.calls, to.String()+" "+text)
	n, err, started, release := len(f.calls), f.err, f.started, f.release
	f.mu.Unlock()
	if started != nil {
		started <- struct{}{}
	}
	if release != nil {
		<-release
	}
	id := fmt.Sprintf("OUT%d", n)
	if err != nil {
		return sent{id: id}, err
	}
	return sent{id: id, at: t0.Add(time.Duration(n) * time.Minute), from: "15550100000@s.whatsapp.net"}, nil
}

func (f *fakeTransport) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.calls)
}

// newSendEnv is the opt-in environment with sending on, the fake transport,
// groupA ticked, groupB listed but unticked, and dmC unknown.
func newSendEnv(t *testing.T) (*optinEnv, *fakeTransport) {
	t.Helper()
	e := newOptinEnv(t)
	e.m.opts.AllowSend = true
	ft := &fakeTransport{}
	e.m.send = ft.send
	ctx := context.Background()
	for _, c := range []struct {
		jid  types.JID
		name string
	}{{groupA, "Atlas Team"}, {groupB, "Family"}} {
		if err := e.m.upsertChat(ctx, optinSID, c.jid.String(), c.name, true, &t0); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := e.m.SetChatTicked(ctx, optinSID, groupA.String(), true); err != nil {
		t.Fatal(err)
	}
	return e, ft
}

func sendTo(e *optinEnv, chat, text string) (Message, error) {
	return e.m.SendText(context.Background(), Outgoing{SessionID: optinSID, ChatJID: chat, Text: text, KeyID: "k1"})
}

func TestSendIsOffByDefault(t *testing.T) {
	e, ft := newSendEnv(t)
	e.m.opts.AllowSend = false
	if _, err := sendTo(e, groupA.String(), "hello"); !errors.Is(err, ErrSendDisabled) {
		t.Fatalf("send with sending off: %v, want ErrSendDisabled", err)
	}
	if ft.count() != 0 {
		t.Fatal("nothing may reach WhatsApp when sending is off")
	}
}

func TestSendOnlyToTickedChats(t *testing.T) {
	e, ft := newSendEnv(t)
	for _, chat := range []types.JID{groupB, dmC} { // unticked, unknown
		if _, err := sendTo(e, chat.String(), "hello"); !errors.Is(err, ErrNotTicked) {
			t.Fatalf("send to %s: %v, want ErrNotTicked", chat, err)
		}
	}
	if ft.count() != 0 {
		t.Fatalf("an unticked or unknown chat reached WhatsApp: %v", ft.calls)
	}

	const text = "Loading starts Monday"
	ch, unsub := e.m.Subscribe()
	defer unsub()
	msg, err := sendTo(e, groupA.String(), text)
	if err != nil {
		t.Fatal(err)
	}
	if msg.ID != "OUT1" || !msg.FromMe || msg.Kind != "text" || msg.Text != text || msg.ChatJID != groupA.String() || msg.SenderJID == "" {
		t.Fatalf("sent message: %+v", msg)
	}
	if got := ft.calls; len(got) != 1 || got[0] != groupA.String()+" "+text {
		t.Fatalf("transport calls: %v", got)
	}
	stored, err := e.m.RecentMessages(context.Background(), optinSID, groupA.String(), 10)
	if err != nil || len(stored) != 1 || stored[0].ID != "OUT1" || !stored[0].FromMe {
		t.Fatalf("the sent message should be stored with the chat: %+v %v", stored, err)
	}
	if c := e.chat(t, groupA); c.MessageCount != 1 || c.LastMessageAt == nil || !c.LastMessageAt.Equal(msg.Timestamp) {
		t.Fatalf("chat after send: %+v", c)
	}
	logs := e.events(t)
	if !strings.Contains(logs, "message.sent chat="+groupA.String()+" id=OUT1 chars=21 key=k1") {
		t.Fatalf("no message.sent event:\n%s", logs)
	}
	if strings.Contains(logs, text) {
		t.Fatal("the event log must never hold message text")
	}
	select {
	case ev := <-ch:
		if ev.Type != EventMessage || ev.Message == nil || ev.Message.ID != "OUT1" {
			t.Fatalf("event: %+v", ev)
		}
	case <-time.After(time.Second):
		t.Fatal("no message event after sending")
	}
}

func TestSendChecksTheText(t *testing.T) {
	e, ft := newSendEnv(t)
	for text, want := range map[string]error{
		"":                                  ErrEmptyText,
		" \n\t ":                            ErrEmptyText,
		strings.Repeat("a", MaxSendRunes+1): ErrTextTooLong,
		"bell \a":                           ErrBadText,
		"nul \x00 byte":                     ErrBadText,
		"lone \r return":                    ErrBadText,
		"escape \x1b[31m":                   ErrBadText,
		"c1 \u0085 control":                 ErrBadText,
		"bad \xff\xfe bytes":                ErrBadText,
	} {
		if _, err := sendTo(e, groupA.String(), text); !errors.Is(err, want) {
			t.Errorf("text %q: %v, want %v", text, err, want)
		}
	}
	if ft.count() != 0 {
		t.Fatalf("a refused text reached WhatsApp: %v", ft.calls)
	}
	msg, err := sendTo(e, groupA.String(), "  Hi Ana,\r\n\tthe *BL* is ready ✅  \n")
	if err != nil || msg.Text != "Hi Ana,\n\tthe *BL* is ready ✅" {
		t.Fatalf("normalised text: %q %v", msg.Text, err)
	}
	if _, err := sendTo(e, groupA.String(), strings.Repeat("é", MaxSendRunes)); err != nil {
		t.Fatalf("a text of exactly MaxSendRunes characters: %v", err)
	}
}

func TestSendRefusesOtherKindsOfChat(t *testing.T) {
	e, ft := newSendEnv(t)
	for _, jid := range []string{
		"", "not a jid", "status@broadcast", "1234567@broadcast", "120363000000000003@newsletter",
		"0@s.whatsapp.net", "15550100003:2@s.whatsapp.net", "15550100003.1:2@s.whatsapp.net", "@g.us",
	} {
		if _, err := sendTo(e, jid, "hello"); !errors.Is(err, ErrUnsupportedChat) {
			t.Errorf("send to %q: %v, want ErrUnsupportedChat", jid, err)
		}
	}
	if ft.count() != 0 {
		t.Fatalf("an unsupported chat reached WhatsApp: %v", ft.calls)
	}
	for _, jid := range []string{"15550100003@s.whatsapp.net", "123456789012345@lid", "120363000000000001@g.us"} {
		if _, err := sendTarget(jid); err != nil {
			t.Errorf("sendTarget(%q): %v", jid, err)
		}
	}
}

func TestSendNeedsAConnectedAccount(t *testing.T) {
	e, ft := newSendEnv(t)
	if _, err := e.m.SendText(context.Background(), Outgoing{SessionID: "nope", ChatJID: groupA.String(), Text: "hi"}); !errors.Is(err, ErrNoSession) {
		t.Fatalf("unknown session: %v", err)
	}
	for _, st := range []State{StateDisconnected, StateConnecting, StateLoggedOut, StateBanned} {
		e.s.mu.Lock()
		e.s.state = st
		e.s.mu.Unlock()
		if _, err := sendTo(e, groupA.String(), "hi"); !errors.Is(err, ErrNotConnected) {
			t.Fatalf("state %s: %v, want ErrNotConnected", st, err)
		}
	}
	if ft.count() != 0 {
		t.Fatal("a send went out from an account that is not connected")
	}
	// The real transport refuses a session without a connected client.
	if _, err := e.m.sendViaClient(context.Background(), e.s, groupA, "hi"); !errors.Is(err, ErrNotConnected) {
		t.Fatalf("sendViaClient without a client: %v", err)
	}
}

func TestUntickWaitsForASendInProgress(t *testing.T) {
	e, ft := newSendEnv(t)
	release := make(chan struct{})
	ft.started, ft.release = make(chan struct{}, 1), release
	sendDone := make(chan error, 1)
	go func() {
		_, err := sendTo(e, groupA.String(), "on its way")
		sendDone <- err
	}()
	<-ft.started // the send passed the tick check and is with WhatsApp

	untickDone := make(chan error, 1)
	go func() {
		_, err := e.m.SetChatTicked(context.Background(), optinSID, groupA.String(), false)
		untickDone <- err
	}()
	select {
	case err := <-untickDone:
		t.Fatalf("the untick returned (%v) while a send to the chat was in progress", err)
	case <-time.After(150 * time.Millisecond):
	}
	close(release)
	if err := <-sendDone; err != nil {
		t.Fatalf("the send in progress: %v", err)
	}
	if err := <-untickDone; err != nil {
		t.Fatal(err)
	}
	if c := e.chat(t, groupA); c.Ticked || c.MessageCount != 0 || e.storedMessages(t) != 0 {
		t.Fatalf("after the untick the chat must be unticked and empty: %+v, %d stored", c, e.storedMessages(t))
	}
	if _, err := sendTo(e, groupA.String(), "too late"); !errors.Is(err, ErrNotTicked) {
		t.Fatalf("send after the untick: %v, want ErrNotTicked", err)
	}
	if ft.count() != 1 {
		t.Fatalf("transport calls: %v", ft.calls)
	}
	if n := locksInUse(e.m); n != 0 {
		t.Fatalf("%d chat locks left after all sends and ticks ended", n)
	}
}

// locksInUse counts the chats whose lock someone holds or waits for.
func locksInUse(m *Manager) int {
	m.tickLocks.mu.Lock()
	defer m.tickLocks.mu.Unlock()
	return len(m.tickLocks.m)
}

func TestUntickWaitsOnlyForSendsToThatChat(t *testing.T) {
	e, ft := newSendEnv(t)
	ctx := context.Background()
	if _, err := e.m.SetChatTicked(ctx, optinSID, groupB.String(), true); err != nil {
		t.Fatal(err)
	}
	release := make(chan struct{})
	ft.started, ft.release = make(chan struct{}, 1), release
	sendDone := make(chan error, 1)
	go func() {
		_, err := sendTo(e, groupA.String(), "slow send")
		sendDone <- err
	}()
	<-ft.started // the send to groupA is with WhatsApp

	untickA := make(chan error, 1)
	go func() {
		_, err := e.m.SetChatTicked(ctx, optinSID, groupA.String(), false)
		untickA <- err
	}()
	select {
	case err := <-untickA:
		t.Fatalf("the untick of groupA returned (%v) while a send to it was in progress", err)
	case <-time.After(100 * time.Millisecond):
	}

	// groupA's send and its waiting untick must not hold up another chat.
	untickB := make(chan error, 1)
	go func() {
		_, err := e.m.SetChatTicked(ctx, optinSID, groupB.String(), false)
		untickB <- err
	}()
	select {
	case err := <-untickB:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		close(release)
		t.Fatal("unticking groupB waited for a send to groupA")
	}

	close(release)
	if err := <-sendDone; err != nil {
		t.Fatalf("the send in progress: %v", err)
	}
	if err := <-untickA; err != nil {
		t.Fatal(err)
	}
	if e.chat(t, groupA).Ticked || e.chat(t, groupB).Ticked {
		t.Fatal("both chats should be unticked")
	}
	if n := locksInUse(e.m); n != 0 {
		t.Fatalf("%d chat locks left after all sends and ticks ended", n)
	}
}

func TestSendTimeLimitStartsAfterTheLock(t *testing.T) {
	e, ft := newSendEnv(t)
	var deadline time.Time
	e.m.send = func(ctx context.Context, s *session, to types.JID, text string) (sent, error) {
		deadline, _ = ctx.Deadline()
		return ft.send(ctx, s, to, text)
	}
	unlock := e.m.tickLocks.lock(optinSID, groupA.String()) // an untick in progress
	done := make(chan error, 1)
	go func() {
		_, err := sendTo(e, groupA.String(), "after the wait")
		done <- err
	}()
	time.Sleep(200 * time.Millisecond)
	released := time.Now()
	unlock()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if deadline.IsZero() || deadline.Before(released.Add(SendTimeout)) {
		t.Fatalf("send deadline %v; want SendTimeout from when the lock was free (%v)", deadline, released.Add(SendTimeout))
	}
}

func TestSendReportsADatabaseErrorAsSuch(t *testing.T) {
	e, ft := newSendEnv(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // the tick query fails
	_, err := e.m.SendText(ctx, Outgoing{SessionID: optinSID, ChatJID: groupA.String(), Text: "hello", KeyID: "k1"})
	if !errors.Is(err, ErrStore) || errors.Is(err, ErrNotTicked) {
		t.Fatalf("send when the tick cannot be read: %v, want ErrStore", err)
	}
	if ft.count() != 0 {
		t.Fatal("a send went out without a tick check")
	}
}

func TestFailedSendsStoreNothing(t *testing.T) {
	e, ft := newSendEnv(t)
	ft.err = fmt.Errorf("%w: server returned error 463", ErrSendFailed)
	if _, err := sendTo(e, groupA.String(), "refused text"); !errors.Is(err, ErrSendFailed) {
		t.Fatalf("refused send: %v", err)
	}
	ft.err = fmt.Errorf("%w: timed out", ErrSendUnconfirmed)
	if _, err := sendTo(e, groupA.String(), "unconfirmed text"); !errors.Is(err, ErrSendUnconfirmed) {
		t.Fatalf("unconfirmed send: %v", err)
	}
	if n := e.storedMessages(t); n != 0 {
		t.Fatalf("%d messages stored after failed sends", n)
	}
	logs := e.events(t)
	// The unconfirmed message carries its id: WhatsApp may still deliver it.
	for _, want := range []string{"message.failed chat=" + groupA.String() + " key=k1", "message.unconfirmed chat=" + groupA.String() + " id=OUT2 key=k1"} {
		if !strings.Contains(logs, want) {
			t.Fatalf("event log lacks %q:\n%s", want, logs)
		}
	}
	if strings.Contains(logs, "refused text") || strings.Contains(logs, "unconfirmed text") {
		t.Fatal("the event log must never hold message text")
	}
}

// TestSendErrorSorting uses the error values whatsmeow returns (send.go,
// request.go, errors.go of the version in go.mod).
func TestSendErrorSorting(t *testing.T) {
	pn := types.NewJID("15550100003", types.DefaultUserServer)
	for _, tc := range []struct {
		name string
		err  error
		want error
	}{
		{"admins only", whatsmeow.ErrNoPermissionInGroup, ErrNoPermission},
		{"logged out", whatsmeow.ErrNotLoggedIn, ErrNotConnected},
		{"server refused", fmt.Errorf("%w 463", whatsmeow.ErrServerReturnedError), ErrSendFailed},
		{"group lookup failed", fmt.Errorf("failed to get group members: %w", errors.New("x")), ErrSendFailed},

		// Before the message goes out: whatsmeow's own queries.
		{"query cut off", whatsmeow.ErrIQDisconnected, ErrSendFailed},
		{"query cut off twice", fmt.Errorf("failed to get device list: %w", &whatsmeow.DisconnectedError{Action: "info query (retry)"}), ErrSendFailed},
		{"query timed out", fmt.Errorf("failed to get group members: %w", whatsmeow.ErrIQTimedOut), ErrSendFailed},
		{"group lookup out of time", fmt.Errorf("failed to get group members: %w", context.DeadlineExceeded), ErrSendFailed},
		{"LID lookup out of time", fmt.Errorf("failed to get LID for PN %s: %w", pn, context.DeadlineExceeded), ErrSendFailed},
		{"user info out of time", fmt.Errorf("failed to get user info for %s to fill LID cache: %w", pn, context.Canceled), ErrSendFailed},
		{"device lookup offline", fmt.Errorf("failed to get device list: %w", whatsmeow.ErrNotConnected), ErrSendFailed},

		// The message may be on the wire.
		{"no ack in time", whatsmeow.ErrMessageTimedOut, ErrSendUnconfirmed},
		{"cut off after sending", &whatsmeow.DisconnectedError{Action: "message send"}, ErrSendUnconfirmed},
		{"cut off on the resend", &whatsmeow.DisconnectedError{Action: "message send (retry)"}, ErrSendUnconfirmed},
		{"no socket for the resend", whatsmeow.ErrNotConnected, ErrSendUnconfirmed},
		{"out of time waiting", context.DeadlineExceeded, ErrSendUnconfirmed},
		{"canceled waiting", context.Canceled, ErrSendUnconfirmed},
		{"out of time writing", fmt.Errorf("failed to send message node: %w", context.DeadlineExceeded), ErrSendUnconfirmed},
	} {
		if got := sendError(tc.err); !errors.Is(got, tc.want) {
			t.Errorf("%s: sendError(%v) = %v, want %v", tc.name, tc.err, got, tc.want)
		}
	}
}
