package wa

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"

	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/types"
	"google.golang.org/protobuf/proto"
)

// Sending is off unless the operator turns it on (Options.AllowSend). When it
// is on, the bridge sends plain text, and only to a chat that is ticked at the
// moment it sends. Ticking or unticking a chat waits for a send to that chat
// in progress, so once an untick returns, nothing more can be sent to it.
// Other chats are not held up. Sending never sends presence ("online",
// typing) or read receipts.

// MaxSendRunes caps one outgoing message. WhatsApp allows more, but a longer
// text from an assistant is almost always a mistake.
const MaxSendRunes = 4096

// SendTimeout bounds one send from the moment it holds the chat's lock: the
// tick check, whatsmeow's lookups and the wait for WhatsApp to confirm.
const SendTimeout = 45 * time.Second

// sendAckTimeout bounds the wait for WhatsApp to acknowledge a message once
// it is on the wire (whatsmeow's default is 75 seconds).
const sendAckTimeout = 30 * time.Second

var (
	ErrSendDisabled    = errors.New("wa: sending is off on this bridge")
	ErrNotTicked       = errors.New("wa: chat is not ticked")
	ErrNotConnected    = errors.New("wa: account is not connected")
	ErrUnsupportedChat = errors.New("wa: the bridge cannot send to this kind of chat")
	ErrEmptyText       = errors.New("wa: message text is empty")
	ErrTextTooLong     = fmt.Errorf("wa: message text is longer than %d characters", MaxSendRunes)
	ErrBadText         = errors.New("wa: message text has control characters or invalid UTF-8")
	ErrNoPermission    = errors.New("wa: only admins may send messages to this group")
	// ErrSendFailed means WhatsApp did not take the message: it was not sent.
	ErrSendFailed = errors.New("wa: WhatsApp did not accept the message")
	// ErrSendUnconfirmed means the message may have gone out, but WhatsApp
	// did not confirm it in time. Sending it again may deliver it twice.
	ErrSendUnconfirmed = errors.New("wa: WhatsApp did not confirm the message in time")
	// ErrStore means the bridge could not read the chat from its database,
	// so it could not check the tick. Nothing was sent.
	ErrStore = errors.New("wa: the bridge could not read its database")
)

// Outgoing is a text message to send.
type Outgoing struct {
	SessionID string // the linked account that sends it
	ChatJID   string // a ticked chat of that account
	Text      string
	KeyID     string // the key or OAuth grant that asked; logged, never the text
}

// sent is what a transport reports for a message WhatsApp accepted. On an
// error it still carries the message id when there is one, so a message
// that was not confirmed can be found later.
type sent struct {
	id   string
	at   time.Time
	from string // sender JID, phone-number form where known
}

// transport sends one text message through a session's client. Tests
// replace it.
type transport func(ctx context.Context, s *session, to types.JID, text string) (sent, error)

// OutgoingText normalises and checks a message text. It trims surrounding
// whitespace and turns CRLF into LF. The result must be valid UTF-8, not
// empty, at most MaxSendRunes long and free of control characters other
// than newline and tab.
func OutgoingText(text string) (string, error) {
	if !utf8.ValidString(text) {
		return "", ErrBadText
	}
	text = strings.TrimSpace(strings.ReplaceAll(text, "\r\n", "\n"))
	if text == "" {
		return "", ErrEmptyText
	}
	if utf8.RuneCountInString(text) > MaxSendRunes {
		return "", ErrTextTooLong
	}
	for _, r := range text {
		if r != '\n' && r != '\t' && unicode.IsControl(r) {
			return "", ErrBadText
		}
	}
	return text, nil
}

// sendTarget parses a stored chat id into an address the bridge may send to:
// a person (by phone number or hidden LID address) or a group. Status
// updates, broadcast lists, channels, service chats and non-canonical ids are
// refused.
func sendTarget(chatJID string) (types.JID, error) {
	jid, err := types.ParseJID(chatJID)
	if err != nil || jid.User == "" || jid.ToNonAD().String() != chatJID || skipChat(jid) {
		return types.JID{}, ErrUnsupportedChat
	}
	switch jid.Server {
	case types.DefaultUserServer, types.HiddenUserServer, types.GroupServer:
		return jid, nil
	}
	return types.JID{}, ErrUnsupportedChat
}

// SendText sends a text message to a ticked chat, stores it with the chat's
// messages and logs that it was sent (never the text). It fails with
// ErrSendDisabled unless Options.AllowSend is set, with ErrNotTicked for a
// chat that is unticked or unknown, and with ErrStore when it cannot read
// the tick. Errors other than ErrSendUnconfirmed mean nothing was sent. The
// send is bounded by SendTimeout once it holds the chat's lock; the caller's
// ctx can end it sooner.
func (m *Manager) SendText(ctx context.Context, out Outgoing) (Message, error) {
	if !m.opts.AllowSend {
		return Message{}, ErrSendDisabled
	}
	text, err := OutgoingText(out.Text)
	if err != nil {
		return Message{}, err
	}
	s, err := m.get(out.SessionID)
	if err != nil {
		return Message{}, err
	}
	to, err := sendTarget(out.ChatJID)
	if err != nil {
		return Message{}, err
	}

	// Hold the chat's lock, shared, from the tick check until the message is
	// stored. An untick of the chat waits for this send to end, and no send
	// can pass the check once an untick has started. The time limit starts
	// now, so a send that waited for the lock still gets its full time.
	unlock := m.tickLocks.rlock(s.id, out.ChatJID)
	defer unlock()
	ctx, cancel := context.WithTimeout(ctx, SendTimeout)
	defer cancel()
	ticked, _, err := m.readTick(ctx, s.id, out.ChatJID)
	if err != nil {
		m.log.Warn("read chat tick before sending; nothing sent", "session", s.id, "err", err)
		return Message{}, fmt.Errorf("%w: %v", ErrStore, err)
	}
	if !ticked {
		return Message{}, ErrNotTicked
	}
	if s.getState() != StateConnected {
		return Message{}, ErrNotConnected
	}
	res, err := m.send(ctx, s, to, text)
	if err != nil {
		typ, detail := "message.failed", fmt.Sprintf("chat=%s key=%s", out.ChatJID, out.KeyID)
		if errors.Is(err, ErrSendUnconfirmed) {
			// WhatsApp may still deliver it. The id lets someone match it
			// with the chat on the phone.
			typ, detail = "message.unconfirmed", fmt.Sprintf("chat=%s id=%s key=%s", out.ChatJID, res.id, out.KeyID)
		}
		m.appendLog(m.ctx(5*time.Second), s.id, typ, detail)
		return Message{}, err
	}

	msg := Message{SessionID: s.id, ChatJID: out.ChatJID, ID: res.id, SenderJID: res.from, Timestamp: res.at,
		FromMe: true, Kind: "text", Text: text}
	// WhatsApp has the message now, so record it even if the caller's
	// request has gone away meanwhile.
	sctx := m.ctx(10 * time.Second)
	if _, err := insertMessage(sctx, m.db, msg); err != nil {
		m.log.Warn("store sent message", "session", s.id, "err", err)
	}
	_ = m.upsertChat(sctx, s.id, msg.ChatJID, "", to.Server == types.GroupServer, &msg.Timestamp)
	_ = m.recountChat(sctx, s.id, msg.ChatJID)
	m.appendLog(sctx, s.id, "message.sent", fmt.Sprintf("chat=%s id=%s chars=%d key=%s",
		msg.ChatJID, msg.ID, utf8.RuneCountInString(text), out.KeyID))
	summary := msg
	summary.Text = preview(text, 120)
	m.publish(Event{SessionID: s.id, Type: EventMessage, Message: &summary})
	return msg, nil
}

// sendViaClient is the real transport: one plain-text message through the
// session's whatsmeow client. It asks whatsmeow to check group permissions
// first, so a group where only admins may write fails before anything is
// sent.
func (m *Manager) sendViaClient(ctx context.Context, s *session, to types.JID, text string) (sent, error) {
	s.mu.Lock()
	cli, dev := s.cli, s.dev
	s.mu.Unlock()
	if cli == nil || !cli.IsConnected() || !cli.IsLoggedIn() {
		return sent{}, ErrNotConnected
	}
	id := cli.GenerateMessageID()
	resp, err := cli.SendMessage(ctx, to, &waE2E.Message{Conversation: proto.String(text)},
		whatsmeow.SendRequestExtra{ID: id, Timeout: sendAckTimeout, CheckPermissions: true})
	if err != nil {
		m.log.Warn("send message", "session", s.id, "id", id, "err", err)
		return sent{id: string(id)}, sendError(err)
	}
	out := sent{id: string(resp.ID), at: resp.Timestamp.UTC()}
	if resp.Timestamp.IsZero() {
		out.at = time.Now().UTC()
	}
	if dev != nil && dev.ID != nil {
		out.from = dev.ID.ToNonAD().String()
	}
	return out, nil
}

// sendError sorts a whatsmeow send error into the bridge's: refused before
// sending, or possibly sent but not confirmed. When unsure it says
// unconfirmed, because a retry that delivers twice is worse than a user who
// checks their phone.
func sendError(err error) error {
	switch {
	case errors.Is(err, whatsmeow.ErrNoPermissionInGroup):
		return ErrNoPermission
	case errors.Is(err, whatsmeow.ErrNotLoggedIn):
		return ErrNotConnected
	case beforeSending(err):
		return fmt.Errorf("%w: %v", ErrSendFailed, err)
	case errors.Is(err, whatsmeow.ErrMessageTimedOut), errors.Is(err, whatsmeow.ErrNotConnected), errors.As(err, new(*whatsmeow.DisconnectedError)),
		errors.Is(err, context.DeadlineExceeded), errors.Is(err, context.Canceled):
		// The connection dropped or the wait ended, perhaps after the
		// message went out.
		return fmt.Errorf("%w: %v", ErrSendUnconfirmed, err)
	default:
		return fmt.Errorf("%w: %v", ErrSendFailed, err)
	}
}

// preSendSteps are the labels whatsmeow puts on the lookups SendMessage
// makes before the message goes out (checked against the version in go.mod):
// group members, the LID for a phone number, and the recipients' devices.
// A dropped connection or an expired wait inside one of them sent nothing.
// If whatsmeow renames them, such errors read as unconfirmed again, which
// is the safe side.
var preSendSteps = []string{
	"failed to get group members: ",
	"failed to get LID for PN ",
	"failed to get user info for ",
	"failed to get device list: ",
}

// beforeSending reports an error that provably came before the message went
// out: a dropped server query (an "info query" in whatsmeow; the message
// itself is a "message send") or a failure in one of the preSendSteps.
func beforeSending(err error) bool {
	var disc *whatsmeow.DisconnectedError
	if errors.As(err, &disc) && strings.HasPrefix(disc.Action, whatsmeow.ErrIQDisconnected.Action) {
		return true
	}
	for _, step := range preSendSteps {
		if strings.HasPrefix(err.Error(), step) {
			return true
		}
	}
	return false
}

// ---- per-chat locks ---------------------------------------------------------------------

// chatLocks hands out one read-write lock per (session, chat). SendText holds
// a chat's lock shared and SetChatTicked holds it exclusively, so an untick
// waits only for sends to that chat. A lock is dropped when nobody holds or
// waits for it. The zero value is ready to use.
type chatLocks struct {
	mu sync.Mutex
	m  map[chatKey]*chatLock
}

type chatKey struct{ session, chat string }

type chatLock struct {
	sync.RWMutex
	users int // holders and waiters, counted under chatLocks.mu
}

// rlock takes a chat's lock shared and returns its unlock.
func (l *chatLocks) rlock(sessionID, chatJID string) (unlock func()) {
	k, c := l.acquire(sessionID, chatJID)
	c.RLock()
	return func() { c.RUnlock(); l.release(k, c) }
}

// lock takes a chat's lock exclusively and returns its unlock.
func (l *chatLocks) lock(sessionID, chatJID string) (unlock func()) {
	k, c := l.acquire(sessionID, chatJID)
	c.Lock()
	return func() { c.Unlock(); l.release(k, c) }
}

func (l *chatLocks) acquire(sessionID, chatJID string) (chatKey, *chatLock) {
	k := chatKey{sessionID, chatJID}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.m == nil {
		l.m = map[chatKey]*chatLock{}
	}
	c := l.m[k]
	if c == nil {
		c = &chatLock{}
		l.m[k] = c
	}
	c.users++
	return k, c
}

func (l *chatLocks) release(k chatKey, c *chatLock) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if c.users--; c.users == 0 {
		delete(l.m, k)
	}
}
