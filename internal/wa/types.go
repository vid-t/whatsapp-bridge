// Package wa owns the WhatsApp linked-device sessions (whatsmeow clients),
// their lifecycle, pairing flows, and a small persistence layer for chats and
// messages. It is read-only by construction: nothing in this package sends
// messages, presence or read receipts.
package wa

import (
	"time"

	"go.mau.fi/whatsmeow/types"
)

// State is the lifecycle state of one linked device.
type State string

const (
	StateUnpaired     State = "unpaired"     // device created, pairing not finished
	StatePairing      State = "pairing"      // QR or code pairing in progress
	StateConnecting   State = "connecting"   // paired, socket not yet authenticated
	StateConnected    State = "connected"    // authenticated and receiving
	StateDisconnected State = "disconnected" // temporarily offline, auto-reconnecting
	StateLoggedOut    State = "logged_out"   // unlinked on the phone or rejected; needs relink
	StateBanned       State = "banned"       // WhatsApp issued a temporary ban
)

// SessionInfo is the UI-facing snapshot of a session.
type SessionInfo struct {
	ID            string     `json:"id"`
	Label         string     `json:"label,omitempty"`
	JID           string     `json:"jid,omitempty"`
	Phone         string     `json:"phone,omitempty"`
	PushName      string     `json:"push_name,omitempty"`
	Platform      string     `json:"platform,omitempty"`
	BusinessName  string     `json:"business_name,omitempty"`
	State         State      `json:"state"`
	LastError     string     `json:"last_error,omitempty"`
	CreatedAt     time.Time  `json:"created_at"`
	ConnectedAt   *time.Time `json:"connected_at,omitempty"`
	LastMessageAt *time.Time `json:"last_message_at,omitempty"`
	MessageCount  int64      `json:"message_count"`
	ChatCount     int64      `json:"chat_count"`
}

// PairEventType enumerates the pairing sub-events.
type PairEventType string

const (
	PairQR                PairEventType = "qr"                 // Code holds the QR payload to render
	PairCode              PairEventType = "code"               // Code holds the 8-char phone pairing code
	PairTimeout           PairEventType = "timeout"            // QR codes expired; pairing ended
	PairSuccess           PairEventType = "success"            // phone accepted the link
	PairError             PairEventType = "error"              // Error holds the message
	PairPasskeyRequest    PairEventType = "passkey_request"    // PasskeyRequest holds the WebAuthn options
	PairPasskeyCode       PairEventType = "passkey_code"       // Code holds the confirmation code to show
	PairScannedNoMultiDev PairEventType = "scanned_without_md" // phone must enable multi-device
	PairCancelled         PairEventType = "cancelled"
)

// PairEvent is one step of a pairing flow.
type PairEvent struct {
	Type           PairEventType            `json:"type"`
	Code           string                   `json:"code,omitempty"`
	TimeoutSeconds int                      `json:"timeout_seconds,omitempty"`
	Error          string                   `json:"error,omitempty"`
	PasskeyRequest *types.WebAuthnPublicKey `json:"passkey_request,omitempty"`
	SkipHandoffUX  bool                     `json:"skip_handoff_ux,omitempty"`
}

// EventType enumerates broadcast events (used for SSE).
type EventType string

const (
	EventState   EventType = "state"   // State changed; Text has a human-readable note
	EventPair    EventType = "pair"    // Pair holds the step
	EventMessage EventType = "message" // Message holds a summary (text truncated)
	EventChats   EventType = "chats"   // chat list changed (history sync, group refresh)
	EventLog     EventType = "log"     // informational
)

// Event is broadcast to subscribers for every notable change.
type Event struct {
	Time      time.Time  `json:"time"`
	SessionID string     `json:"session_id,omitempty"`
	Type      EventType  `json:"type"`
	State     State      `json:"state,omitempty"`
	Pair      *PairEvent `json:"pair,omitempty"`
	Message   *Message   `json:"message,omitempty"`
	Text      string     `json:"text,omitempty"`
}

// Chat is a conversation the linked account participates in.
type Chat struct {
	SessionID     string     `json:"session_id"`
	JID           string     `json:"jid"`
	Name          string     `json:"name"`
	IsGroup       bool       `json:"is_group"`
	LastMessageAt *time.Time `json:"last_message_at,omitempty"`
	MessageCount  int64      `json:"message_count"`
	// Ticked chats are stored and visible to tools; unticked chats are not
	// stored and their messages are deleted.
	Ticked bool `json:"ticked"`
}

// SearchHit is one full-text search result with its chat name.
type SearchHit struct {
	Message  Message `json:"message"`
	ChatName string  `json:"chat_name"`
}

// Message is a stored message. Text is the extracted body or a short
// description for media ("[image]", "[document: invoice.pdf]").
type Message struct {
	SessionID  string    `json:"session_id"`
	ChatJID    string    `json:"chat_jid"`
	ID         string    `json:"id"`
	SenderJID  string    `json:"sender_jid"`
	SenderName string    `json:"sender_name,omitempty"`
	Timestamp  time.Time `json:"timestamp"`
	FromMe     bool      `json:"from_me"`
	Kind       string    `json:"kind"`
	Text       string    `json:"text"`
}

// LogEntry is a persisted lifecycle event (never message text).
type LogEntry struct {
	Seq       int64     `json:"seq"`
	Time      time.Time `json:"time"`
	SessionID string    `json:"session_id,omitempty"`
	Type      string    `json:"type"`
	Detail    string    `json:"detail"`
}
