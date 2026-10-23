package wa

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"regexp"
	"sort"
	"sync"
	"time"

	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/proto/waHistorySync"
	"go.mau.fi/whatsmeow/store"
	"go.mau.fi/whatsmeow/store/sqlstore"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
	"google.golang.org/protobuf/proto"
)

// Options configures the Manager.
type Options struct {
	// DeviceName is shown on the phone under Linked devices.
	DeviceName string
	// MaxConcurrentPairings caps sessions in the pairing state (default 3).
	// Every pairing opens a WhatsApp socket from this server's address.
	MaxConcurrentPairings int
}

var (
	ErrNoSession       = errors.New("wa: no such session")
	ErrNotPaired       = errors.New("wa: session is not paired")
	ErrNotPairing      = errors.New("wa: session is not pairing")
	ErrBadPhone        = errors.New("wa: phone number must have at least 7 digits")
	ErrStillLinked     = errors.New("wa: session is still linked; log it out instead of removing it")
	ErrUnlinkFailed    = errors.New("wa: removed locally but WhatsApp did not confirm the unlink")
	ErrTooManyPairings = errors.New("wa: too many pairings in progress")
	nonDigits          = regexp.MustCompile(`\D+`)
	codePairExpiry     = 3 * time.Minute
	defaultMaxPairings = 3
)

type pairMode string

const (
	pairNone  pairMode = ""
	pairQR    pairMode = "qr"
	pairPhone pairMode = "code"
)

type session struct {
	mu          sync.Mutex
	id          string
	label       string
	createdAt   time.Time
	dev         *store.Device
	cli         *whatsmeow.Client
	state       State
	lastErr     string
	connectedAt *time.Time
	mode        pairMode
	pairCancel  context.CancelFunc
	pairCh      chan PairEvent
	pairClosed  bool
	dirMu       sync.Mutex // serialises refreshDirectory
}

func (s *session) getState() State {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.state
}

func (s *session) getMode() pairMode {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.mode
}

func (s *session) paired() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.dev != nil && s.dev.ID != nil
}

// Manager owns all sessions and the whatsmeow store container.
type Manager struct {
	db        *sql.DB
	container *sqlstore.Container
	log       *slog.Logger
	opts      Options

	mu       sync.RWMutex
	sessions map[string]*session

	subMu   sync.Mutex
	subs    map[int]chan Event
	nextSub int

	baseCtx context.Context
	cancel  context.CancelFunc
	fts     bool // full-text index available
}

// NewManager prepares the whatsmeow store (running its migrations), creates
// the bridge tables and loads existing devices. It does not connect; call Start.
func NewManager(ctx context.Context, db *sql.DB, log *slog.Logger, opts Options) (*Manager, error) {
	if opts.DeviceName == "" {
		opts.DeviceName = "whatsapp-bridge"
	}
	if opts.MaxConcurrentPairings <= 0 {
		opts.MaxConcurrentPairings = defaultMaxPairings
	}
	store.DeviceProps.Os = proto.String(opts.DeviceName)

	baseCtx, cancel := context.WithCancel(context.Background())
	m := &Manager{
		db:       db,
		log:      log,
		opts:     opts,
		sessions: map[string]*session{},
		subs:     map[int]chan Event{},
		baseCtx:  baseCtx,
		cancel:   cancel,
	}
	m.container = sqlstore.NewWithDB(db, "sqlite3", newWALog(log, "sqlstore"))
	if err := m.container.Upgrade(ctx); err != nil {
		cancel()
		return nil, fmt.Errorf("wa: upgrade whatsmeow store: %w", err)
	}
	if err := m.ensureSchema(ctx); err != nil {
		cancel()
		return nil, err
	}

	rows, err := m.loadSessions(ctx)
	if err != nil {
		cancel()
		return nil, fmt.Errorf("wa: load sessions: %w", err)
	}
	byJID := map[string]sessionRow{}
	for _, r := range rows {
		if r.jid == "" {
			// Abandoned pairing from a previous run.
			_ = m.deleteSessionData(ctx, r.id)
			continue
		}
		byJID[r.jid] = r
	}
	devs, err := m.container.GetAllDevices(ctx)
	if err != nil {
		cancel()
		return nil, fmt.Errorf("wa: load devices: %w", err)
	}
	now := time.Now().UTC()
	for _, d := range devs {
		if d.ID == nil {
			continue
		}
		jid := d.ID.String()
		r, ok := byJID[jid]
		if !ok {
			r = sessionRow{id: newID(), jid: jid, createdAt: now}
			_ = m.insertSession(ctx, r.id, "", now)
			_ = m.setSessionJID(ctx, r.id, jid)
		}
		delete(byJID, jid)
		m.sessions[r.id] = &session{id: r.id, label: r.label, createdAt: r.createdAt, dev: d, state: StateDisconnected}
	}
	for _, r := range byJID {
		// Session row survives but whatsmeow removed the device: it was logged out.
		m.sessions[r.id] = &session{id: r.id, label: r.label, createdAt: r.createdAt, state: StateLoggedOut,
			lastErr: "device was logged out; relink or remove"}
	}
	return m, nil
}

// Start connects every paired session in the background.
func (m *Manager) Start() {
	m.mu.RLock()
	defer m.mu.RUnlock()
	for _, s := range m.sessions {
		if s.dev != nil && s.dev.ID != nil {
			go m.connect(s)
		}
	}
}

// Close disconnects all clients. The database is left to the caller.
func (m *Manager) Close() {
	m.cancel()
	m.mu.RLock()
	defer m.mu.RUnlock()
	for _, s := range m.sessions {
		s.mu.Lock()
		if s.pairCancel != nil {
			s.pairCancel()
		}
		cli := s.cli
		s.mu.Unlock()
		if cli != nil {
			cli.Disconnect()
		}
	}
}

// ---- subscriptions -------------------------------------------------------

// Subscribe returns a channel of events and an unsubscribe function. Slow
// consumers drop events rather than block the manager.
func (m *Manager) Subscribe() (<-chan Event, func()) {
	ch := make(chan Event, 128)
	m.subMu.Lock()
	id := m.nextSub
	m.nextSub++
	m.subs[id] = ch
	m.subMu.Unlock()
	return ch, func() {
		m.subMu.Lock()
		if c, ok := m.subs[id]; ok {
			delete(m.subs, id)
			close(c)
		}
		m.subMu.Unlock()
	}
}

func (m *Manager) publish(ev Event) {
	if ev.Time.IsZero() {
		ev.Time = time.Now().UTC()
	}
	m.subMu.Lock()
	defer m.subMu.Unlock()
	for _, ch := range m.subs {
		select {
		case ch <- ev:
		default:
		}
	}
}

func (m *Manager) publishPair(s *session, pe PairEvent) {
	s.mu.Lock()
	if s.pairCh != nil && !s.pairClosed {
		select {
		case s.pairCh <- pe:
		default:
		}
	}
	s.mu.Unlock()
	detail := string(pe.Type)
	if pe.Error != "" {
		detail += ": " + pe.Error
	}
	m.appendLog(m.ctx(10*time.Second), s.id, "pair."+string(pe.Type), pe.Error)
	m.publish(Event{SessionID: s.id, Type: EventPair, Pair: &pe, Text: detail})
}

func (m *Manager) closePairCh(s *session) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.pairCh != nil && !s.pairClosed {
		close(s.pairCh)
		s.pairClosed = true
	}
}

func (m *Manager) setState(s *session, st State, note string) {
	s.mu.Lock()
	s.state = st
	switch st {
	case StateConnected:
		now := time.Now().UTC()
		s.connectedAt = &now
		s.lastErr = ""
	case StatePairing, StateConnecting:
		// Notes for these states are progress, not errors.
		s.lastErr = ""
	default:
		if note != "" {
			s.lastErr = note
		}
	}
	s.mu.Unlock()
	m.appendLog(m.ctx(10*time.Second), s.id, "state."+string(st), note)
	m.publish(Event{SessionID: s.id, Type: EventState, State: st, Text: note})
}

func (m *Manager) ctx(d time.Duration) context.Context {
	ctx, cancel := context.WithTimeout(m.baseCtx, d)
	// The cancel is intentionally dropped: callers are short-lived DB ops and
	// the timeout bounds them. Leaks are limited to the timer.
	_ = cancel
	return ctx
}

// ---- session registry ----------------------------------------------------

func (m *Manager) get(id string) (*session, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	s, ok := m.sessions[id]
	if !ok {
		return nil, ErrNoSession
	}
	return s, nil
}

func (m *Manager) add(s *session) {
	m.mu.Lock()
	m.sessions[s.id] = s
	m.mu.Unlock()
}

func (m *Manager) remove(id string) {
	m.mu.Lock()
	delete(m.sessions, id)
	m.mu.Unlock()
}

func (m *Manager) snapshot(s *session) SessionInfo {
	s.mu.Lock()
	info := SessionInfo{
		ID:          s.id,
		Label:       s.label,
		State:       s.state,
		LastError:   s.lastErr,
		CreatedAt:   s.createdAt,
		ConnectedAt: s.connectedAt,
	}
	if s.dev != nil && s.dev.ID != nil {
		info.JID = s.dev.ID.String()
		info.Phone = s.dev.ID.User
		info.PushName = s.dev.PushName
		info.Platform = s.dev.Platform
		info.BusinessName = s.dev.BusinessName
	}
	s.mu.Unlock()
	info.MessageCount, info.ChatCount, info.LastMessageAt = m.counts(m.ctx(5*time.Second), s.id)
	return info
}

// Sessions returns a snapshot of all sessions, oldest first.
func (m *Manager) Sessions() []SessionInfo {
	m.mu.RLock()
	list := make([]*session, 0, len(m.sessions))
	for _, s := range m.sessions {
		list = append(list, s)
	}
	m.mu.RUnlock()
	sort.Slice(list, func(i, j int) bool { return list[i].createdAt.Before(list[j].createdAt) })
	out := make([]SessionInfo, 0, len(list))
	for _, s := range list {
		out = append(out, m.snapshot(s))
	}
	return out
}

// Session returns one session snapshot.
func (m *Manager) Session(id string) (SessionInfo, error) {
	s, err := m.get(id)
	if err != nil {
		return SessionInfo{}, err
	}
	return m.snapshot(s), nil
}

// Chats lists the chats known for a session, most recent first.
func (m *Manager) Chats(ctx context.Context, sessionID string) ([]Chat, error) {
	if _, err := m.get(sessionID); err != nil {
		return nil, err
	}
	return m.chats(ctx, sessionID)
}

// RecentMessages returns up to limit messages of a chat, oldest first.
func (m *Manager) RecentMessages(ctx context.Context, sessionID, chatJID string, limit int) ([]Message, error) {
	if _, err := m.get(sessionID); err != nil {
		return nil, err
	}
	return m.recentMessages(ctx, sessionID, chatJID, limit)
}

// RecentLogs returns persisted lifecycle events, newest first. Empty
// sessionID returns events for all sessions.
func (m *Manager) RecentLogs(ctx context.Context, sessionID string, limit int) ([]LogEntry, error) {
	return m.recentLogs(ctx, sessionID, limit)
}

// ---- connecting -----------------------------------------------------------

func (m *Manager) newClient(s *session) {
	s.cli = whatsmeow.NewClient(s.dev, newWALog(m.log, "wa."+s.id))
	s.cli.AddEventHandler(m.eventHandler(s))
}

func (m *Manager) connect(s *session) {
	s.mu.Lock()
	if s.cli == nil {
		m.newClient(s)
	}
	cli := s.cli
	s.mu.Unlock()
	m.setState(s, StateConnecting, "")
	if err := cli.Connect(); err != nil {
		m.setState(s, StateDisconnected, "connect failed: "+err.Error())
	}
}

// Reconnect forces a connection attempt for a paired session.
func (m *Manager) Reconnect(id string) error {
	s, err := m.get(id)
	if err != nil {
		return err
	}
	if !s.paired() {
		return ErrNotPaired
	}
	s.mu.Lock()
	cli := s.cli
	s.mu.Unlock()
	if cli != nil && cli.IsConnected() {
		return nil
	}
	go m.connect(s)
	return nil
}

// ---- pairing ---------------------------------------------------------------

func (m *Manager) pairingSlotFree() bool {
	m.mu.RLock()
	defer m.mu.RUnlock()
	n := 0
	for _, s := range m.sessions {
		if s.getState() == StatePairing {
			n++
		}
	}
	return n < m.opts.MaxConcurrentPairings
}

// StartPairing creates a new device and starts the QR flow. QR payloads and
// progress arrive on the returned channel (and on Subscribe). The channel is
// closed when pairing ends, successfully or not.
func (m *Manager) StartPairing(label string) (string, <-chan PairEvent, error) {
	if !m.pairingSlotFree() {
		return "", nil, ErrTooManyPairings
	}
	s := &session{
		id:        newID(),
		label:     label,
		createdAt: time.Now().UTC(),
		dev:       m.container.NewDevice(),
		state:     StatePairing,
		mode:      pairQR,
		pairCh:    make(chan PairEvent, 32),
	}
	m.newClient(s)
	ctx, cancel := context.WithCancel(m.baseCtx)
	s.pairCancel = cancel
	qrCh, err := s.cli.GetQRChannel(ctx)
	if err != nil {
		cancel()
		return "", nil, fmt.Errorf("wa: qr channel: %w", err)
	}
	m.add(s)
	_ = m.insertSession(m.ctx(5*time.Second), s.id, label, s.createdAt)
	if err := s.cli.Connect(); err != nil {
		cancel()
		m.abandonPairing(s, "connect failed: "+err.Error())
		return "", nil, fmt.Errorf("wa: connect: %w", err)
	}
	m.setState(s, StatePairing, "waiting for QR scan")
	go m.runQRLoop(s, qrCh)
	return s.id, s.pairCh, nil
}

func (m *Manager) runQRLoop(s *session, qrCh <-chan whatsmeow.QRChannelItem) {
	defer m.closePairCh(s)
	for item := range qrCh {
		switch item.Event {
		case "code":
			m.publishPair(s, PairEvent{Type: PairQR, Code: item.Code, TimeoutSeconds: int(item.Timeout.Seconds())})
		case whatsmeow.QRChannelEventPasskeyRequest:
			var pk *types.WebAuthnPublicKey
			if item.PasskeyRequest != nil {
				pk = item.PasskeyRequest.PublicKey
			}
			m.publishPair(s, PairEvent{Type: PairPasskeyRequest, PasskeyRequest: pk})
		case whatsmeow.QRChannelEventPasskeyResponse:
			if item.PasskeyConfirmation != nil && !item.PasskeyConfirmation.SkipHandoffUX {
				m.publishPair(s, PairEvent{Type: PairPasskeyCode, Code: item.PasskeyConfirmation.Code})
			}
		case whatsmeow.QRChannelScannedWithoutMultidevice.Event:
			m.publishPair(s, PairEvent{Type: PairScannedNoMultiDev})
		case whatsmeow.QRChannelSuccess.Event:
			m.publishPair(s, PairEvent{Type: PairSuccess})
			return
		case whatsmeow.QRChannelTimeout.Event:
			m.publishPair(s, PairEvent{Type: PairTimeout})
			m.abandonPairing(s, "QR codes expired")
			return
		default:
			msg := item.Event
			if item.Error != nil {
				msg = item.Error.Error()
			}
			m.publishPair(s, PairEvent{Type: PairError, Error: msg})
			m.abandonPairing(s, msg)
			return
		}
	}
}

// PairWithCode creates a new device and requests an 8-character pairing code
// for the given phone number (digits, with country code). The code is also
// delivered as a PairCode event.
func (m *Manager) PairWithCode(ctx context.Context, phone, label string) (string, string, error) {
	digits := nonDigits.ReplaceAllString(phone, "")
	if len(digits) < 7 {
		return "", "", ErrBadPhone
	}
	if !m.pairingSlotFree() {
		return "", "", ErrTooManyPairings
	}
	s := &session{
		id:        newID(),
		label:     label,
		createdAt: time.Now().UTC(),
		dev:       m.container.NewDevice(),
		state:     StatePairing,
		mode:      pairPhone,
		pairCh:    make(chan PairEvent, 32),
	}
	m.newClient(s)
	pctx, cancel := context.WithCancel(m.baseCtx)
	s.pairCancel = cancel
	m.add(s)
	_ = m.insertSession(m.ctx(5*time.Second), s.id, label, s.createdAt)
	if err := s.cli.Connect(); err != nil {
		m.abandonPairing(s, "connect failed: "+err.Error())
		return "", "", fmt.Errorf("wa: connect: %w", err)
	}
	if !s.cli.WaitForConnection(20 * time.Second) {
		m.abandonPairing(s, "could not reach WhatsApp servers")
		return "", "", errors.New("wa: could not reach WhatsApp servers")
	}
	code, err := s.cli.PairPhone(ctx, digits, true, whatsmeow.PairClientChrome, "Chrome (Linux)")
	if err != nil {
		m.abandonPairing(s, "pair code request failed: "+err.Error())
		return "", "", fmt.Errorf("wa: pair phone: %w", err)
	}
	m.setState(s, StatePairing, "enter the code on the phone")
	m.publishPair(s, PairEvent{Type: PairCode, Code: code, TimeoutSeconds: int(codePairExpiry.Seconds())})
	go func() {
		select {
		case <-pctx.Done():
			return
		case <-time.After(codePairExpiry):
			if !s.paired() {
				m.publishPair(s, PairEvent{Type: PairTimeout})
				m.abandonPairing(s, "pairing code expired")
			}
		}
	}()
	return s.id, code, nil
}

// SubmitPasskeyResponse forwards a WebAuthn assertion produced by an
// authenticator for a pending passkey request.
func (m *Manager) SubmitPasskeyResponse(ctx context.Context, id string, resp *types.WebAuthnResponse) error {
	s, err := m.get(id)
	if err != nil {
		return err
	}
	if s.getState() != StatePairing {
		return ErrNotPairing
	}
	return s.cli.SendPasskeyResponse(ctx, resp)
}

// ConfirmPasskey tells WhatsApp the user verified the passkey confirmation code.
func (m *Manager) ConfirmPasskey(ctx context.Context, id string) error {
	s, err := m.get(id)
	if err != nil {
		return err
	}
	if s.getState() != StatePairing {
		return ErrNotPairing
	}
	return s.cli.SendPasskeyConfirmation(ctx)
}

// CancelPairing aborts an in-progress pairing and discards the device.
func (m *Manager) CancelPairing(id string) error {
	s, err := m.get(id)
	if err != nil {
		return err
	}
	if s.getState() != StatePairing || s.paired() {
		return ErrNotPairing
	}
	m.publishPair(s, PairEvent{Type: PairCancelled})
	m.abandonPairing(s, "cancelled")
	return nil
}

// abandonPairing tears down a session that never completed pairing.
func (m *Manager) abandonPairing(s *session, reason string) {
	if s.paired() {
		// Pairing actually succeeded; keep the session.
		return
	}
	s.mu.Lock()
	if s.pairCancel != nil {
		s.pairCancel()
	}
	cli := s.cli
	s.mu.Unlock()
	if cli != nil {
		cli.Disconnect()
	}
	m.closePairCh(s)
	m.remove(s.id)
	_ = m.deleteSessionData(m.ctx(5*time.Second), s.id)
	m.appendLog(m.ctx(5*time.Second), s.id, "pair.abandoned", reason)
	m.publish(Event{SessionID: s.id, Type: EventState, State: StateUnpaired, Text: reason})
}

// ---- logout / remove --------------------------------------------------------

// Logout unlinks the device on the phone side (when connected) and removes
// everything stored for the session.
func (m *Manager) Logout(ctx context.Context, id string) error {
	s, err := m.get(id)
	if err != nil {
		return err
	}
	s.mu.Lock()
	cli := s.cli
	s.mu.Unlock()
	var unlinkErr error
	if cli != nil && cli.IsLoggedIn() {
		if err := cli.Logout(ctx); err != nil {
			m.log.Warn("logout", "session", id, "err", err)
			unlinkErr = fmt.Errorf("%w: %v", ErrUnlinkFailed, err)
		}
	}
	if err := m.teardown(ctx, s, "logout"); err != nil {
		return err
	}
	return unlinkErr
}

// Remove deletes a session locally without contacting WhatsApp. It refuses
// sessions that are currently logged in; use Logout for those so the phone
// side is unlinked too.
func (m *Manager) Remove(ctx context.Context, id string) error {
	s, err := m.get(id)
	if err != nil {
		return err
	}
	s.mu.Lock()
	cli := s.cli
	s.mu.Unlock()
	if cli != nil && cli.IsLoggedIn() {
		return ErrStillLinked
	}
	return m.teardown(ctx, s, "remove")
}

func (m *Manager) teardown(ctx context.Context, s *session, how string) error {
	s.mu.Lock()
	if s.pairCancel != nil {
		s.pairCancel()
	}
	cli := s.cli
	dev := s.dev
	s.mu.Unlock()
	if cli != nil {
		cli.Disconnect()
	}
	if dev != nil && dev.ID != nil {
		if err := m.container.DeleteDevice(ctx, dev); err != nil {
			// Already deleted by whatsmeow's Logout, or never saved.
			m.log.Debug("delete device", "session", s.id, "err", err)
		}
	}
	m.closePairCh(s)
	m.remove(s.id)
	if err := m.deleteSessionData(ctx, s.id); err != nil {
		return err
	}
	m.appendLog(ctx, s.id, "session."+how, "")
	m.publish(Event{SessionID: s.id, Type: EventState, State: StateLoggedOut, Text: "removed (" + how + ")"})
	return nil
}

// ---- event handling ----------------------------------------------------------

func (m *Manager) eventHandler(s *session) whatsmeow.EventHandler {
	return func(evt any) {
		switch e := evt.(type) {
		case *events.PairSuccess:
			_ = m.setSessionJID(m.ctx(5*time.Second), s.id, e.ID.String())
			m.appendLog(m.ctx(5*time.Second), s.id, "pair.success", e.Platform)
			if s.getMode() == pairPhone {
				// The QR loop publishes success for QR pairing; the code flow
				// has no channel loop, so publish it here and end the step stream.
				m.publishPair(s, PairEvent{Type: PairSuccess})
				m.closePairCh(s)
			}
			m.setState(s, StateConnecting, "")
		case *events.PairError:
			if s.getMode() == pairPhone {
				m.publishPair(s, PairEvent{Type: PairError, Error: e.Error.Error()})
			}
			m.setState(s, StateUnpaired, "pair error: "+e.Error.Error())
		case *events.QRScannedWithoutMultidevice:
			m.publishPair(s, PairEvent{Type: PairScannedNoMultiDev})
		case *events.PairPasskeyRequest:
			if s.getMode() == pairPhone {
				m.publishPair(s, PairEvent{Type: PairPasskeyRequest, PasskeyRequest: e.PublicKey})
			}
		case *events.PairPasskeyConfirmation:
			if s.getMode() == pairPhone {
				if e.SkipHandoffUX {
					go func() {
						if err := s.cli.SendPasskeyConfirmation(m.ctx(30 * time.Second)); err != nil {
							m.publishPair(s, PairEvent{Type: PairError, Error: "passkey confirmation: " + err.Error()})
						}
					}()
				} else {
					m.publishPair(s, PairEvent{Type: PairPasskeyCode, Code: e.Code})
				}
			}
		case *events.PairPasskeyError:
			if s.getMode() == pairPhone {
				m.publishPair(s, PairEvent{Type: PairError, Error: "passkey: " + e.Error.Error()})
			}
		case *events.Connected:
			s.mu.Lock()
			s.mode = pairNone
			s.mu.Unlock()
			m.setState(s, StateConnected, "")
			go m.refreshDirectory(s)
		case *events.Disconnected:
			if st := s.getState(); st != StateLoggedOut && st != StateBanned {
				m.setState(s, StateDisconnected, "")
			}
		case *events.ConnectFailure:
			m.setState(s, StateDisconnected, fmt.Sprintf("connect failure: %v %s", e.Reason, e.Message))
		case *events.LoggedOut:
			m.setState(s, StateLoggedOut, fmt.Sprintf("logged out (%v)", e.Reason))
		case *events.TemporaryBan:
			m.setState(s, StateBanned, e.String())
		case *events.ClientOutdated:
			m.setState(s, StateDisconnected, "client outdated: the bridge needs a whatsmeow update")
		case *events.StreamReplaced:
			m.setState(s, StateDisconnected, "stream replaced: another client connected with this session")
		case *events.KeepAliveTimeout:
			m.publish(Event{SessionID: s.id, Type: EventLog, Text: "keepalive timeout"})
		case *events.KeepAliveRestored:
			m.publish(Event{SessionID: s.id, Type: EventLog, Text: "keepalive restored"})
		case *events.OfflineSyncCompleted:
			m.publish(Event{SessionID: s.id, Type: EventLog, Text: fmt.Sprintf("offline sync completed (%d items)", e.Count)})
		case *events.HistorySync:
			go m.ingestHistory(s, e.Data)
		case *events.Message:
			m.ingestMessage(s, e)
		case *events.JoinedGroup:
			_ = m.upsertChat(m.ctx(5*time.Second), s.id, e.JID.String(), e.Name, true, nil)
			m.publish(Event{SessionID: s.id, Type: EventChats, Text: "joined group"})
		case *events.GroupInfo:
			if e.Name != nil {
				_ = m.upsertChat(m.ctx(5*time.Second), s.id, e.JID.String(), e.Name.Name, true, nil)
				m.publish(Event{SessionID: s.id, Type: EventChats, Text: "group renamed"})
			}
		}
	}
}

func (m *Manager) ingestMessage(s *session, e *events.Message) {
	info := e.Info
	if skipChat(info.Chat) {
		return // status updates and WhatsApp service notices
	}
	kind, text := extractText(e.Message)
	if kind == "protocol" || kind == "reaction" {
		return
	}
	chat := m.canonicalChat(s, info)
	ctx := m.ctx(10 * time.Second)
	if !m.chatTicked(ctx, s.id, chat.String()) {
		return
	}
	msg := Message{
		SessionID:  s.id,
		ChatJID:    chat.String(),
		ID:         string(info.ID),
		SenderJID:  m.canonicalSender(s, info).String(),
		SenderName: info.PushName,
		Timestamp:  info.Timestamp.UTC(),
		FromMe:     info.IsFromMe,
		Kind:       kind,
		Text:       text,
	}
	inserted, err := insertMessage(ctx, m.db, msg)
	if err != nil {
		m.log.Warn("store message", "session", s.id, "err", err)
		return
	}
	name := ""
	if !info.IsGroup && !info.IsFromMe {
		name = info.PushName
	}
	_ = m.upsertChat(ctx, s.id, msg.ChatJID, name, info.IsGroup, &msg.Timestamp)
	_ = m.recountChat(ctx, s.id, msg.ChatJID)
	if inserted {
		summary := msg
		summary.Text = preview(text, 120)
		m.publish(Event{SessionID: s.id, Type: EventMessage, Message: &summary})
	}
}

func (m *Manager) ingestHistory(s *session, data *waHistorySync.HistorySync) {
	if data == nil {
		return
	}
	ctx := m.ctx(10 * time.Minute)
	convs := data.GetConversations()
	var newMsgs int
	var ownJID string
	s.mu.Lock()
	if s.dev != nil && s.dev.ID != nil {
		ownJID = s.dev.ID.ToNonAD().String()
	}
	s.mu.Unlock()

	for _, c := range convs {
		jid, err := types.ParseJID(c.GetID())
		if err != nil || skipChat(jid) {
			continue
		}
		if jid.Server == types.HiddenUserServer {
			if pn, err := types.ParseJID(c.GetPnJID()); err == nil && !pn.IsEmpty() {
				jid = pn.ToNonAD()
			} else {
				jid = m.lidToPN(s, jid)
			}
		}
		isGroup := jid.Server == types.GroupServer
		name := c.GetName()
		if name == "" {
			name = c.GetDisplayName()
		}
		if !m.chatTicked(ctx, s.id, jid.String()) {
			_ = m.upsertChat(ctx, s.id, jid.String(), name, isGroup, nil)
			continue
		}
		tx, err := m.db.BeginTx(ctx, nil)
		if err != nil {
			m.log.Warn("history tx", "err", err)
			return
		}
		for _, hm := range c.GetMessages() {
			w := hm.GetMessage()
			if w == nil || w.GetMessage() == nil {
				continue
			}
			kind, text := extractText(w.GetMessage())
			if kind == "protocol" || kind == "reaction" {
				continue
			}
			key := w.GetKey()
			sender := key.GetParticipant()
			if sender == "" {
				sender = w.GetParticipant()
			}
			if sender == "" {
				if key.GetFromMe() {
					sender = ownJID
				} else {
					sender = jid.String()
				}
			}
			if sj, err := types.ParseJID(sender); err == nil {
				if sj.Server == types.HiddenUserServer {
					sj = m.lidToPN(s, sj)
				}
				sender = sj.ToNonAD().String()
			}
			msg := Message{
				SessionID:  s.id,
				ChatJID:    jid.String(),
				ID:         key.GetID(),
				SenderJID:  sender,
				SenderName: w.GetPushName(),
				Timestamp:  time.Unix(int64(w.GetMessageTimestamp()), 0).UTC(),
				FromMe:     key.GetFromMe(),
				Kind:       kind,
				Text:       text,
			}
			if ok, err := insertMessage(ctx, tx, msg); err == nil && ok {
				newMsgs++
			}
		}
		if err := tx.Commit(); err != nil {
			m.log.Warn("history commit", "err", err)
		}
		_ = m.upsertChat(ctx, s.id, jid.String(), name, isGroup, nil)
		_ = m.recountChat(ctx, s.id, jid.String())
	}
	detail := fmt.Sprintf("type=%s conversations=%d new_messages=%d progress=%d", data.GetSyncType(), len(convs), newMsgs, data.GetProgress())
	m.appendLog(ctx, s.id, "history_sync", detail)
	m.publish(Event{SessionID: s.id, Type: EventChats, Text: "history sync: " + detail})
	if len(convs) > 0 {
		go m.refreshDirectory(s)
	}
}

// ---- identity and naming ---------------------------------------------------

// skipChat reports chats that are never stored: status updates and
// WhatsApp's own service account.
func skipChat(chat types.JID) bool {
	return chat.Server == types.BroadcastServer || chat.ToNonAD() == types.PSAJID || chat == types.LegacyPSAJID
}

// lidToPN resolves a hidden (LID) address to a phone-number JID when the
// device's LID store knows the mapping; otherwise it returns the LID.
func (m *Manager) lidToPN(s *session, lid types.JID) types.JID {
	if lid.Server != types.HiddenUserServer {
		return lid.ToNonAD()
	}
	s.mu.Lock()
	dev := s.dev
	s.mu.Unlock()
	if dev == nil || dev.LIDs == nil {
		return lid.ToNonAD()
	}
	pn, err := dev.LIDs.GetPNForLID(m.ctx(5*time.Second), lid.ToNonAD())
	if err != nil || pn.IsEmpty() {
		return lid.ToNonAD()
	}
	return pn.ToNonAD()
}

// canonicalChat returns the chat key: groups as-is, direct chats by phone
// number where the alternative address or the LID store provides one.
func (m *Manager) canonicalChat(s *session, info types.MessageInfo) types.JID {
	chat := info.Chat
	if chat.Server != types.HiddenUserServer {
		return chat
	}
	if info.IsFromMe && !info.RecipientAlt.IsEmpty() {
		return info.RecipientAlt.ToNonAD()
	}
	if !info.IsFromMe && !info.SenderAlt.IsEmpty() && info.Sender.ToNonAD().User == chat.User {
		return info.SenderAlt.ToNonAD()
	}
	return m.lidToPN(s, chat)
}

// canonicalSender returns the sender by phone number where possible.
func (m *Manager) canonicalSender(s *session, info types.MessageInfo) types.JID {
	if info.Sender.Server == types.HiddenUserServer {
		if !info.SenderAlt.IsEmpty() {
			return info.SenderAlt.ToNonAD()
		}
		return m.lidToPN(s, info.Sender)
	}
	return info.Sender.ToNonAD()
}

// contactName looks a user up in the device's contact store, trying the
// phone-number and LID forms, and returns the best display name or "".
func (m *Manager) contactName(s *session, jid types.JID) string {
	s.mu.Lock()
	dev := s.dev
	s.mu.Unlock()
	if dev == nil || dev.Contacts == nil {
		return ""
	}
	ctx := m.ctx(5 * time.Second)
	candidates := []types.JID{jid.ToNonAD()}
	if dev.LIDs != nil {
		if jid.Server == types.HiddenUserServer {
			if pn, err := dev.LIDs.GetPNForLID(ctx, jid.ToNonAD()); err == nil && !pn.IsEmpty() {
				candidates = append(candidates, pn.ToNonAD())
			}
		} else if lid, err := dev.LIDs.GetLIDForPN(ctx, jid.ToNonAD()); err == nil && !lid.IsEmpty() {
			candidates = append(candidates, lid.ToNonAD())
		}
	}
	for _, c := range candidates {
		ci, err := dev.Contacts.GetContact(ctx, c)
		if err != nil || !ci.Found {
			continue
		}
		switch {
		case ci.FullName != "":
			return ci.FullName
		case ci.PushName != "":
			return ci.PushName
		case ci.BusinessName != "":
			return ci.BusinessName
		}
	}
	return ""
}

// refreshDirectory runs after connect and after each history batch. It
// refreshes group names, folds LID-keyed chats and senders into their
// phone-number forms, and names direct chats and senders from the contact
// store. It is idempotent and serialised per session.
func (m *Manager) refreshDirectory(s *session) {
	s.dirMu.Lock()
	defer s.dirMu.Unlock()
	ctx := m.ctx(2 * time.Minute)

	// Groups: authoritative names from the server.
	s.mu.Lock()
	cli := s.cli
	s.mu.Unlock()
	if cli != nil && cli.IsLoggedIn() {
		if groups, err := cli.GetJoinedGroups(ctx); err != nil {
			m.log.Warn("joined groups", "session", s.id, "err", err)
		} else {
			for _, g := range groups {
				_ = m.upsertChat(ctx, s.id, g.JID.String(), g.Name, true, nil)
			}
		}
	}

	// Service chats stored before the skip rule existed.
	for _, svc := range []string{types.PSAJID.String(), types.LegacyPSAJID.String(), types.StatusBroadcastJID.String()} {
		_ = m.deleteChat(ctx, s.id, svc)
	}

	// LID-keyed chats and senders stored before a mapping was known.
	if lids, err := m.lidChats(ctx, s.id); err == nil {
		for _, l := range lids {
			lj, err := types.ParseJID(l)
			if err != nil {
				continue
			}
			if pn := m.lidToPN(s, lj); pn.Server != types.HiddenUserServer {
				if err := m.mergeChat(ctx, s.id, l, pn.String()); err != nil {
					m.log.Warn("merge lid chat", "session", s.id, "err", err)
				}
			}
		}
	}
	if lids, err := m.lidSenders(ctx, s.id); err == nil {
		for _, l := range lids {
			lj, err := types.ParseJID(l)
			if err != nil {
				continue
			}
			if pn := m.lidToPN(s, lj); pn.Server != types.HiddenUserServer {
				_ = m.replaceSender(ctx, s.id, l, pn.String())
			}
		}
	}

	// Names for direct chats and for senders without a push name.
	if chats, err := m.chats(ctx, s.id); err == nil {
		for _, c := range chats {
			if c.IsGroup || c.Name != "" {
				continue
			}
			jid, err := types.ParseJID(c.JID)
			if err != nil {
				continue
			}
			name := m.contactName(s, jid)
			if name == "" {
				name = m.lastIncomingName(ctx, s.id, c.JID)
			}
			if name != "" {
				_ = m.setChatName(ctx, s.id, c.JID, name)
			}
		}
	}
	if senders, err := m.unnamedSenders(ctx, s.id); err == nil {
		for _, snd := range senders {
			jid, err := types.ParseJID(snd)
			if err != nil {
				continue
			}
			if name := m.contactName(s, jid); name != "" {
				_ = m.fillSenderName(ctx, s.id, snd, name)
			}
		}
	}
	m.publish(Event{SessionID: s.id, Type: EventChats, Text: "directory refreshed"})
}

func newID() string {
	var b [8]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}
