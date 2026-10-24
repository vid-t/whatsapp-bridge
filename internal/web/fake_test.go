package web

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"go.mau.fi/whatsmeow/types"

	"github.com/vid-t/whatsapp-bridge/internal/wa"
)

// fakeManager stands in for *wa.Manager so tests can publish events and
// script outcomes without touching WhatsApp.
type fakeManager struct {
	mu       sync.Mutex
	sessions map[string]wa.SessionInfo
	chats    map[string][]wa.Chat // per session
	subs     map[int]chan wa.Event
	next     int
	pairErr  error
}

func newFakeManager() *fakeManager {
	return &fakeManager{sessions: map[string]wa.SessionInfo{}, chats: map[string][]wa.Chat{}, subs: map[int]chan wa.Event{}}
}

func (f *fakeManager) put(id string, st wa.State) {
	f.mu.Lock()
	f.sessions[id] = wa.SessionInfo{ID: id, State: st}
	f.mu.Unlock()
}

func (f *fakeManager) publish(ev wa.Event) {
	if ev.Time.IsZero() {
		ev.Time = time.Now().UTC()
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, ch := range f.subs {
		ch <- ev
	}
}

func (f *fakeManager) subscribers() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.subs)
}

func (f *fakeManager) Subscribe() (<-chan wa.Event, func()) {
	f.mu.Lock()
	defer f.mu.Unlock()
	id, ch := f.next, make(chan wa.Event, 64)
	f.next++
	f.subs[id] = ch
	return ch, func() {
		f.mu.Lock()
		delete(f.subs, id)
		f.mu.Unlock()
	}
}

func (f *fakeManager) Sessions() []wa.SessionInfo { return nil }

func (f *fakeManager) Session(id string) (wa.SessionInfo, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	s, ok := f.sessions[id]
	if !ok {
		return wa.SessionInfo{}, wa.ErrNoSession
	}
	return s, nil
}

func (f *fakeManager) Chats(_ context.Context, sessionID string) ([]wa.Chat, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]wa.Chat(nil), f.chats[sessionID]...), nil
}
func (f *fakeManager) RecentMessages(context.Context, string, string, int) ([]wa.Message, error) {
	return nil, nil
}
func (f *fakeManager) RecentLogs(context.Context, string, int) ([]wa.LogEntry, error) {
	return nil, nil
}
func (f *fakeManager) Reconnect(string) error { return nil }
func (f *fakeManager) PairWithCode(context.Context, string, string) (string, string, error) {
	return "", "", wa.ErrBadPhone
}
func (f *fakeManager) SubmitPasskeyResponse(context.Context, string, *types.WebAuthnResponse) error {
	return nil
}
func (f *fakeManager) ConfirmPasskey(context.Context, string) error { return nil }

func (f *fakeManager) StartPairing(string) (string, <-chan wa.PairEvent, error) {
	if f.pairErr != nil {
		return "", nil, f.pairErr
	}
	f.put("fakepair", wa.StatePairing)
	ch := make(chan wa.PairEvent)
	close(ch)
	return "fakepair", ch, nil
}

func (f *fakeManager) CancelPairing(id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, ok := f.sessions[id]; !ok {
		return wa.ErrNoSession
	}
	delete(f.sessions, id)
	return nil
}

func (f *fakeManager) Logout(_ context.Context, id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, ok := f.sessions[id]; !ok {
		return wa.ErrNoSession
	}
	delete(f.sessions, id)
	if id == "unlinkfail" {
		return fmt.Errorf("%w: websocket closed", wa.ErrUnlinkFailed)
	}
	return nil
}

func (f *fakeManager) Remove(_ context.Context, id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	s, ok := f.sessions[id]
	if !ok {
		return wa.ErrNoSession
	}
	if s.State == wa.StateConnected {
		return wa.ErrStillLinked
	}
	delete(f.sessions, id)
	return nil
}

func newFakeServer(t *testing.T, opts Options) (*server, *fakeManager) {
	t.Helper()
	f := newFakeManager()
	return newServer(f, slog.New(slog.NewTextHandler(io.Discard, nil)), opts), f
}

func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func qrEvent(id string) wa.Event {
	return wa.Event{SessionID: id, Type: wa.EventPair, Pair: &wa.PairEvent{Type: wa.PairQR, Code: "2@secret-ref,noise,identity,adv-secret", TimeoutSeconds: 60}}
}

type pairingResp struct {
	Session          wa.SessionInfo  `json:"session"`
	Step             json.RawMessage `json:"step"`
	RemainingSeconds int             `json:"remaining_seconds"`
}

func getPairing(t *testing.T, h http.Handler, id string) (int, pairingResp) {
	t.Helper()
	rec := do(t, h, "GET", "/api/sessions/"+id+"/pairing", "", "")
	var p pairingResp
	if rec.Code == http.StatusOK {
		if err := json.Unmarshal(rec.Body.Bytes(), &p); err != nil {
			t.Fatal(err)
		}
	}
	return rec.Code, p
}

// ---- pairing step cache -------------------------------------------------------

func TestStepCache(t *testing.T) {
	s, f := newFakeServer(t, Options{})
	h := s.handler()
	f.put("s1", wa.StatePairing)

	// Stored, served with remaining seconds, and without the raw payload.
	f.publish(qrEvent("s1"))
	eventually(t, "qr step cached", func() bool { ev, _ := s.step("s1"); return ev != nil })
	code, p := getPairing(t, h, "s1")
	if code != http.StatusOK || !strings.Contains(string(p.Step), `"qr_png":"data:image/png;base64,`) {
		t.Fatalf("pairing: %d %s", code, p.Step)
	}
	if strings.Contains(string(p.Step), "adv-secret") || strings.Contains(string(p.Step), `"code"`) {
		t.Fatalf("cached qr step leaks the payload: %s", p.Step)
	}
	if p.RemainingSeconds < 58 || p.RemainingSeconds > 60 {
		t.Fatalf("remaining_seconds = %d, want ~60", p.RemainingSeconds)
	}

	// Hints do not replace the screen step.
	f.publish(wa.Event{SessionID: "s1", Type: wa.EventPair, Pair: &wa.PairEvent{Type: wa.PairScannedNoMultiDev}})
	f.publish(wa.Event{SessionID: "s1", Type: wa.EventLog, Text: "barrier"})
	time.Sleep(20 * time.Millisecond)
	if ev, _ := s.step("s1"); ev == nil || ev.Pair.Type != wa.PairQR {
		t.Fatalf("hint replaced the cached qr step: %+v", ev)
	}

	// Dropped when the session leaves the pairing state.
	f.publish(wa.Event{SessionID: "s1", Type: wa.EventState, State: wa.StateConnecting})
	eventually(t, "step dropped on state change", func() bool { ev, _ := s.step("s1"); return ev == nil })

	// Not returned (and dropped) when the session is no longer pairing.
	s.storeStep(s.toWire(qrEvent("s1")), time.Now())
	f.put("s1", wa.StateConnected)
	if code, p := getPairing(t, h, "s1"); code != http.StatusOK || string(p.Step) != "null" {
		t.Fatalf("non-pairing session returned a step: %d %s", code, p.Step)
	}
	if ev, _ := s.step("s1"); ev != nil {
		t.Fatal("step kept for a non-pairing session")
	}

	// Dropped on 404.
	s.storeStep(s.toWire(qrEvent("gone")), time.Now())
	if code, _ := getPairing(t, h, "gone"); code != http.StatusNotFound {
		t.Fatalf("gone session: %d", code)
	}
	if ev, _ := s.step("gone"); ev != nil {
		t.Fatal("step kept after 404")
	}

	// Dropped on cancel, which accepts an empty, untyped body like sendBeacon's.
	f.put("s2", wa.StatePairing)
	s.storeStep(s.toWire(qrEvent("s2")), time.Now())
	if rec := do(t, h, "POST", "/api/sessions/s2/cancel", "", ""); rec.Code != http.StatusNoContent {
		t.Fatalf("cancel: %d %s", rec.Code, rec.Body)
	}
	if ev, _ := s.step("s2"); ev != nil {
		t.Fatal("step kept after cancel")
	}
}

func TestStepRemainingSecondsUsesServerClock(t *testing.T) {
	s, f := newFakeServer(t, Options{})
	f.put("s1", wa.StatePairing)
	s.storeStep(s.toWire(qrEvent("s1")), time.Now().Add(-50*time.Second))
	if _, p := getPairing(t, s.handler(), "s1"); p.RemainingSeconds != 10 {
		t.Fatalf("remaining_seconds = %d, want 10", p.RemainingSeconds)
	}
	s.storeStep(s.toWire(qrEvent("s1")), time.Now().Add(-5*time.Minute))
	if _, p := getPairing(t, s.handler(), "s1"); p.RemainingSeconds != 0 {
		t.Fatalf("expired step: remaining_seconds = %d, want 0", p.RemainingSeconds)
	}
}

func TestStepCacheEvictsOldest(t *testing.T) {
	s, _ := newFakeServer(t, Options{})
	base := time.Now()
	for i := range maxSteps {
		s.storeStep(s.toWire(qrEvent(fmt.Sprintf("s%03d", i))), base.Add(time.Duration(i)*time.Second))
	}
	s.storeStep(s.toWire(qrEvent("newest")), base.Add(time.Hour))
	if ev, _ := s.step("s000"); ev != nil {
		t.Fatal("oldest entry was not evicted")
	}
	for _, id := range []string{"s001", fmt.Sprintf("s%03d", maxSteps-1), "newest"} {
		if ev, _ := s.step(id); ev == nil {
			t.Fatalf("%s was evicted", id)
		}
	}
	if len(s.steps) != maxSteps {
		t.Fatalf("len = %d, want %d", len(s.steps), maxSteps)
	}
}

// ---- server-sent events --------------------------------------------------------

// sseLines streams the lines of an open event stream.
func sseLines(t *testing.T, base string) (<-chan string, context.CancelFunc) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	req, _ := http.NewRequestWithContext(ctx, "GET", base+"/api/events", nil)
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	if res.StatusCode != http.StatusOK {
		cancel()
		t.Fatalf("events: status %d", res.StatusCode)
	}
	lines := make(chan string, 64)
	go func() {
		defer close(lines)
		defer res.Body.Close()
		sc := bufio.NewScanner(res.Body)
		sc.Buffer(make([]byte, 0, 64<<10), 1<<20)
		for sc.Scan() {
			lines <- sc.Text()
		}
	}()
	return lines, cancel
}

func nextLine(t *testing.T, lines <-chan string) string {
	t.Helper()
	select {
	case l, ok := <-lines:
		if !ok {
			t.Fatal("stream ended")
		}
		return l
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for a stream line")
	}
	return ""
}

// readFrame returns the event name and data of the next SSE frame.
func readFrame(t *testing.T, lines <-chan string) (string, string) {
	t.Helper()
	var name, data string
	for {
		l := nextLine(t, lines)
		switch {
		case l == "" && name != "":
			return name, data
		case strings.HasPrefix(l, "event: "):
			name = strings.TrimPrefix(l, "event: ")
		case strings.HasPrefix(l, "data: "):
			data = strings.TrimPrefix(l, "data: ")
		}
	}
}

func TestEventsFraming(t *testing.T) {
	s, f := newFakeServer(t, Options{})
	srv := httptest.NewServer(s.handler())
	defer srv.Close()
	lines, cancel := sseLines(t, srv.URL)
	defer cancel()
	if l := nextLine(t, lines); l != "retry: 3000" {
		t.Fatalf("first line %q", l)
	}
	if l := nextLine(t, lines); l != ": connected" {
		t.Fatalf("second line %q", l)
	}

	f.publish(wa.Event{SessionID: "s1", Type: wa.EventState, State: wa.StateConnected, Text: "ok"})
	name, data := readFrame(t, lines)
	var ev wa.Event
	if err := json.Unmarshal([]byte(data), &ev); err != nil {
		t.Fatal(err)
	}
	if name != "state" || ev.Type != wa.EventState || ev.SessionID != "s1" || ev.State != wa.StateConnected || ev.Time.IsZero() {
		t.Fatalf("frame %q %s", name, data)
	}

	f.publish(qrEvent("s1"))
	name, data = readFrame(t, lines)
	if name != "pair" || !strings.Contains(data, `"qr_png":"data:image/png;base64,`) || strings.Contains(data, "adv-secret") {
		t.Fatalf("qr frame %q %.120s", name, data)
	}
}

func TestEventsUnsubscribeOnDisconnect(t *testing.T) {
	s, f := newFakeServer(t, Options{})
	srv := httptest.NewServer(s.handler())
	defer srv.Close()
	base := f.subscribers() // the pairing-step tracker
	lines, cancel := sseLines(t, srv.URL)
	nextLine(t, lines)
	nextLine(t, lines)
	if got := f.subscribers(); got != base+1 {
		t.Fatalf("subscribers = %d, want %d", got, base+1)
	}
	cancel()
	eventually(t, "unsubscribe after disconnect", func() bool { return f.subscribers() == base })
	eventually(t, "stream slot released", func() bool { return s.streams.Load() == 0 })
}

func TestEventsEndOnShutdown(t *testing.T) {
	s, _ := newFakeServer(t, Options{})
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	hs := &http.Server{Handler: s.handler()}
	go func() { _ = hs.Serve(ln) }()
	lines, cancel := sseLines(t, "http://"+ln.Addr().String())
	defer cancel()
	nextLine(t, lines)
	nextLine(t, lines)

	ctx, stop := context.WithTimeout(context.Background(), 5*time.Second)
	defer stop()
	start := time.Now()
	if err := hs.Shutdown(ctx); err != nil {
		t.Fatalf("shutdown: %v", err)
	}
	if d := time.Since(start); d > 2*time.Second {
		t.Fatalf("shutdown took %v with a stream open", d)
	}
	for range lines { // drains until the server closes the stream
	}
}

func TestEventsStreamLimit(t *testing.T) {
	s, _ := newFakeServer(t, Options{})
	srv := httptest.NewServer(s.handler())
	defer srv.Close()
	var cancels []context.CancelFunc
	defer func() {
		for _, c := range cancels {
			c()
		}
	}()
	for range maxStreams {
		_, cancel := sseLines(t, srv.URL)
		cancels = append(cancels, cancel)
	}
	res, err := http.Get(srv.URL + "/api/events")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(res.Body)
	res.Body.Close()
	if res.StatusCode != http.StatusServiceUnavailable || !strings.Contains(string(body), `"error"`) {
		t.Fatalf("stream %d: %d %s", maxStreams+1, res.StatusCode, body)
	}
}

// ---- request checks ---------------------------------------------------------------

func TestCrossOriginByOriginHeader(t *testing.T) {
	s, f := newFakeServer(t, Options{})
	h := s.handler()
	f.put("s1", wa.StateLoggedOut)
	send := func(method, target, origin string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, target, nil)
		req.Host = "localhost:8080"
		req.Header.Set("Origin", origin)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec
	}
	// Only Origin, no Sec-Fetch-Site: an older browser or a stripped request.
	wantJSONError(t, send("POST", "/api/sessions/pair", "https://evil.example"), http.StatusForbidden)
	wantJSONError(t, send("DELETE", "/api/sessions/s1", "https://evil.example"), http.StatusForbidden)
	if _, err := f.Session("s1"); err != nil {
		t.Fatal("cross-origin DELETE reached the manager")
	}
	if rec := send("POST", "/api/sessions/pair", "http://localhost:8080"); rec.Code != http.StatusCreated {
		t.Fatalf("same-origin POST: %d %s", rec.Code, rec.Body)
	}
	if rec := send("DELETE", "/api/sessions/s1", "http://localhost:8080"); rec.Code != http.StatusNoContent {
		t.Fatalf("same-origin DELETE: %d %s", rec.Code, rec.Body)
	}
}

func TestHostAllowlist(t *testing.T) {
	s, _ := newFakeServer(t, Options{AllowedHosts: []string{"bridge.example.com:8443", "Intranet.Example"}})
	h := s.handler()
	cases := map[string]int{
		"localhost":               http.StatusOK,
		"localhost:8080":          http.StatusOK,
		"127.0.0.1:8080":          http.StatusOK,
		"[::1]":                   http.StatusOK,
		"[::1]:8080":              http.StatusOK,
		"bridge.example.com:8443": http.StatusOK,
		"intranet.example":        http.StatusOK,
		"intranet.example:80":     http.StatusOK,
		"bridge.example.com":      http.StatusForbidden,
		"bridge.example.com:9999": http.StatusForbidden,
		"evil.example":            http.StatusForbidden,
		"localhost.evil.example":  http.StatusForbidden,
		"127.0.0.2":               http.StatusForbidden,
	}
	for host, want := range cases {
		req := httptest.NewRequest("GET", "/api/sessions", nil)
		req.Host = host
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != want {
			t.Errorf("Host %q: status %d, want %d", host, rec.Code, want)
		}
	}
	// Register (no options) accepts loopback names only.
	def, _ := newFakeServer(t, Options{})
	req := httptest.NewRequest("GET", "/api/sessions", nil)
	req.Host = "bridge.example.com:8443"
	rec := httptest.NewRecorder()
	def.handler().ServeHTTP(rec, req)
	wantJSONError(t, rec, http.StatusForbidden)
}

// ---- manager error mapping ------------------------------------------------------------

func TestManagerErrorMapping(t *testing.T) {
	s, f := newFakeServer(t, Options{})
	h := s.handler()

	f.put("unlinkfail", wa.StateConnected)
	rec := do(t, h, "POST", "/api/sessions/unlinkfail/logout", "", "")
	var warn struct {
		Warning string `json:"warning"`
	}
	if rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &warn) != nil || !strings.Contains(warn.Warning, "Linked devices") {
		t.Fatalf("unlink failure: %d %s", rec.Code, rec.Body)
	}

	f.put("linked", wa.StateConnected)
	rec = do(t, h, "DELETE", "/api/sessions/linked", "", "")
	wantJSONError(t, rec, http.StatusConflict)
	if !strings.Contains(rec.Body.String(), "use Log out") {
		t.Fatalf("still linked: %s", rec.Body)
	}

	f.pairErr = wa.ErrTooManyPairings
	rec = do(t, h, "POST", "/api/sessions/pair", "", "")
	wantJSONError(t, rec, http.StatusTooManyRequests)
	if rec.Header().Get("Retry-After") == "" || !strings.Contains(rec.Body.String(), "too many pairings in progress") {
		t.Fatalf("too many pairings: %v %s", rec.Header(), rec.Body)
	}

	f.pairErr = errors.New("wa: connect: dial tcp 157.240.0.1:443: secret detail")
	rec = do(t, h, "POST", "/api/sessions/pair", "", "")
	wantJSONError(t, rec, http.StatusBadGateway)
	if strings.Contains(rec.Body.String(), "dial") {
		t.Fatalf("502 leaks detail: %s", rec.Body)
	}
}

func (f *fakeManager) SetChatTicked(ctx context.Context, sessionID, chatJID string, ticked bool) (wa.Chat, error) {
	for _, c := range f.chatsFor(sessionID) {
		if c.JID == chatJID {
			c.Ticked = ticked
			return c, nil
		}
	}
	return wa.Chat{}, wa.ErrNoChat
}

func (f *fakeManager) SearchMessages(ctx context.Context, sessionID, chatJID, query string, limit int) ([]wa.SearchHit, error) {
	var out []wa.SearchHit
	for _, c := range f.chatsFor(sessionID) {
		msgs, _ := f.RecentMessages(ctx, c.SessionID, c.JID, limit)
		for _, m := range msgs {
			if strings.Contains(strings.ToLower(m.Text), strings.ToLower(query)) {
				out = append(out, wa.SearchHit{Message: m, ChatName: c.Name})
			}
		}
	}
	return out, nil
}

func (f *fakeManager) MessageContext(ctx context.Context, sessionID, chatJID, messageID string, before, after int) ([]wa.Message, error) {
	msgs, err := f.RecentMessages(ctx, sessionID, chatJID, 500)
	if err != nil {
		return nil, err
	}
	for i, m := range msgs {
		if m.ID == messageID {
			lo, hi := i-before, i+after+1
			if lo < 0 {
				lo = 0
			}
			if hi > len(msgs) {
				hi = len(msgs)
			}
			return msgs[lo:hi], nil
		}
	}
	return nil, wa.ErrNoChat
}

// chatsFor lists the fake's chats for one session or all sessions.
func (f *fakeManager) chatsFor(sessionID string) []wa.Chat {
	var out []wa.Chat
	for _, s := range f.Sessions() {
		if sessionID != "" && s.ID != sessionID {
			continue
		}
		cs, _ := f.Chats(context.Background(), s.ID)
		out = append(out, cs...)
	}
	return out
}
