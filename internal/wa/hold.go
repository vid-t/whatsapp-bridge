package wa

import (
	"sync"
	"time"
)

// Chats start unticked, but WhatsApp sends its history sync seconds after a
// phone is linked, before anyone can tick a chat, and never sends it again.
// So for a while after linking, messages for unticked chats (history and new
// ones) are held here, in memory only: never in the database, its WAL or a
// log. Ticking a chat while its account's window is open writes the chat's
// held messages. When the window ends, the account is removed or the process
// stops, they are dropped.
const (
	// holdWindow starts when a phone is linked. It must cover the whole
	// initial history sync, which runs for many minutes on a large account,
	// and the time a person needs to go through a long chat list, perhaps
	// after a break. An hour does both; it is not extended.
	holdWindow = time.Hour
	// holdMaxBytes caps the estimated memory of all held messages across
	// accounts. Past it, further messages are not held: the first batches
	// WhatsApp sends carry the most recent messages, so they are kept.
	holdMaxBytes = 64 << 20
)

// holdResult says what happened to messages offered to the hold.
type holdResult int

const (
	holdClosed holdResult = iota // no open window for this account: not held
	holdKept                     // held in memory (up to the cap)
	holdTicked                   // the chat was ticked meanwhile: write them instead
)

// holdBuffer holds messages for unticked chats per account. It is safe for
// concurrent use.
type holdBuffer struct {
	mu       sync.Mutex
	window   time.Duration
	maxBytes int
	now      func() time.Time
	bytes    int // estimated, across accounts
	sessions map[string]*heldSession
}

type heldSession struct {
	until   time.Time
	timer   *time.Timer
	chats   map[string][]Message
	count   int             // messages held
	ticked  map[string]bool // chats ticked while the window is open
	refused int             // messages not held because of the cap
}

// heldStats counts what a window held when it closed. Never content.
type heldStats struct {
	messages, chats, refused int
}

func newHoldBuffer() *holdBuffer {
	return &holdBuffer{window: holdWindow, maxBytes: holdMaxBytes, now: time.Now, sessions: map[string]*heldSession{}}
}

// heldSize estimates the memory a held message takes.
func heldSize(m Message) int {
	return 160 + len(m.SessionID) + len(m.ChatJID) + len(m.ID) + len(m.SenderJID) + len(m.SenderName) + len(m.Kind) + len(m.Text)
}

// open starts the window for an account, unless one was already started.
// onEnd runs when the window ends; it should call drop. It returns when the
// window ends.
func (h *holdBuffer) open(sessionID string, onEnd func()) time.Time {
	h.mu.Lock()
	defer h.mu.Unlock()
	if hs := h.sessions[sessionID]; hs != nil {
		return hs.until
	}
	hs := &heldSession{until: h.now().Add(h.window), chats: map[string][]Message{}, ticked: map[string]bool{}}
	hs.timer = time.AfterFunc(h.window, onEnd)
	h.sessions[sessionID] = hs
	return hs.until
}

// live returns the account's window if it is open. A window past its end
// never accepts or yields messages, even before its timer has fired and
// dropped them. The caller holds h.mu.
func (h *holdBuffer) live(sessionID string) *heldSession {
	hs := h.sessions[sessionID]
	if hs == nil || !h.now().Before(hs.until) {
		return nil
	}
	return hs
}

// add offers messages for an unticked chat. It returns holdTicked, without
// holding them, when the chat was ticked after the caller read its state;
// the caller then writes them. kept and refused count the messages held and
// those turned away by the memory cap.
func (h *holdBuffer) add(sessionID, chatJID string, msgs []Message) (res holdResult, kept, refused int) {
	h.mu.Lock()
	defer h.mu.Unlock()
	hs := h.live(sessionID)
	if hs == nil {
		return holdClosed, 0, 0
	}
	if hs.ticked[chatJID] {
		return holdTicked, 0, 0
	}
	for _, m := range msgs {
		size := heldSize(m)
		if h.bytes+size > h.maxBytes {
			refused++
			continue
		}
		h.bytes += size
		hs.chats[chatJID] = append(hs.chats[chatJID], m)
		kept++
	}
	hs.count += kept
	hs.refused += refused
	return holdKept, kept, refused
}

// take removes and returns a chat's held messages as the chat is ticked, and
// remembers the tick so messages offered later are written instead.
func (h *holdBuffer) take(sessionID, chatJID string) []Message {
	h.mu.Lock()
	defer h.mu.Unlock()
	hs := h.live(sessionID)
	if hs == nil {
		return nil
	}
	hs.ticked[chatJID] = true
	msgs := hs.chats[chatJID]
	delete(hs.chats, chatJID)
	for _, m := range msgs {
		h.bytes -= heldSize(m)
	}
	hs.count -= len(msgs)
	return msgs
}

// restore puts back messages that take returned but could not be written,
// and forgets the tick.
func (h *holdBuffer) restore(sessionID, chatJID string, msgs []Message) {
	h.mu.Lock()
	defer h.mu.Unlock()
	hs := h.live(sessionID)
	if hs == nil {
		return
	}
	delete(hs.ticked, chatJID)
	for _, m := range msgs {
		h.bytes += heldSize(m)
	}
	hs.chats[chatJID] = append(msgs, hs.chats[chatJID]...)
	hs.count += len(msgs)
}

// untick forgets a tick, so the chat's messages are held again while the
// window is open.
func (h *holdBuffer) untick(sessionID, chatJID string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if hs := h.live(sessionID); hs != nil {
		delete(hs.ticked, chatJID)
	}
}

// rekey moves held messages when two chat rows are merged into one.
func (h *holdBuffer) rekey(sessionID, from, to string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	hs := h.live(sessionID)
	if hs == nil {
		return
	}
	for i := range hs.chats[from] {
		hs.chats[from][i].ChatJID = to
	}
	if msgs := hs.chats[from]; len(msgs) > 0 {
		hs.chats[to] = append(hs.chats[to], msgs...)
	}
	delete(hs.chats, from)
	if hs.ticked[from] {
		hs.ticked[to] = true
	}
	delete(hs.ticked, from)
}

// until reports when the account's window ends, or nil when none is open.
func (h *holdBuffer) until(sessionID string) *time.Time {
	h.mu.Lock()
	defer h.mu.Unlock()
	if hs := h.live(sessionID); hs != nil {
		t := hs.until.UTC()
		return &t
	}
	return nil
}

// drop ends an account's window and forgets everything it held.
func (h *holdBuffer) drop(sessionID string) heldStats {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.dropLocked(sessionID)
}

func (h *holdBuffer) dropLocked(sessionID string) heldStats {
	hs := h.sessions[sessionID]
	if hs == nil {
		return heldStats{}
	}
	hs.timer.Stop()
	st := heldStats{messages: hs.count, chats: len(hs.chats), refused: hs.refused}
	for _, msgs := range hs.chats {
		for _, m := range msgs {
			h.bytes -= heldSize(m)
		}
	}
	delete(h.sessions, sessionID)
	return st
}

// dropAll forgets every held message (the manager is closing).
func (h *holdBuffer) dropAll() {
	h.mu.Lock()
	defer h.mu.Unlock()
	for id := range h.sessions {
		h.dropLocked(id)
	}
}
