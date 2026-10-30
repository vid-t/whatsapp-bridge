package wa

import (
	"context"
	"errors"
	"fmt"
	"strings"
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
// moment it sends. Ticking and unticking wait for a send in progress, so once
// an untick returns, nothing more can be sent to that chat. Sending never
// sends presence ("online", typing) or read receipts.

// MaxSendRunes caps one outgoing message. WhatsApp allows more, but a longer
// text from an assistant is almost always a mistake.
const MaxSendRunes = 4096

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
)

// Outgoing is a text message to send.
type Outgoing struct {
	SessionID string // the linked account that sends it
	ChatJID   string // a ticked chat of that account
	Text      string
	KeyID     string // the key or OAuth grant that asked; logged, never the text
}

// sent is what a transport reports for a message WhatsApp accepted.
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
// ErrSendDisabled unless Options.AllowSend is set, and with ErrNotTicked for
// a chat that is unticked or unknown. Errors other than ErrSendUnconfirmed
// mean nothing was sent.
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

	// Hold the tick lock, shared, from the tick check until the message is
	// stored. An untick waits for this send to end, and no send can pass the
	// check once an untick has started.
	m.tickMu.RLock()
	defer m.tickMu.RUnlock()
	if ticked, _ := m.chatTicked(ctx, s.id, out.ChatJID); !ticked {
		return Message{}, ErrNotTicked
	}
	if s.getState() != StateConnected {
		return Message{}, ErrNotConnected
	}
	res, err := m.send(ctx, s, to, text)
	if err != nil {
		typ := "message.failed"
		if errors.Is(err, ErrSendUnconfirmed) {
			typ = "message.unconfirmed"
		}
		m.appendLog(m.ctx(5*time.Second), s.id, typ, fmt.Sprintf("chat=%s key=%s", out.ChatJID, out.KeyID))
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
	resp, err := cli.SendMessage(ctx, to, &waE2E.Message{Conversation: proto.String(text)},
		whatsmeow.SendRequestExtra{ID: cli.GenerateMessageID(), Timeout: sendAckTimeout, CheckPermissions: true})
	if err != nil {
		m.log.Warn("send message", "session", s.id, "err", err)
		return sent{}, sendError(err)
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
	var disc *whatsmeow.DisconnectedError
	switch {
	case errors.Is(err, whatsmeow.ErrNoPermissionInGroup):
		return ErrNoPermission
	case errors.Is(err, whatsmeow.ErrNotLoggedIn):
		return ErrNotConnected
	case errors.Is(err, whatsmeow.ErrMessageTimedOut), errors.Is(err, whatsmeow.ErrNotConnected), errors.As(err, &disc),
		errors.Is(err, context.DeadlineExceeded), errors.Is(err, context.Canceled):
		// The connection dropped or the wait ended, perhaps after the
		// message went out.
		return fmt.Errorf("%w: %v", ErrSendUnconfirmed, err)
	default:
		return fmt.Errorf("%w: %v", ErrSendFailed, err)
	}
}
