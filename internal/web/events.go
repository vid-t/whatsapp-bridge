package web

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"strconv"
	"sync"
	"time"

	qrcode "github.com/skip2/go-qrcode"

	"github.com/vid-t/whatsapp-bridge/internal/wa"
)

// wireEvent is wa.Event as sent to the browser. Its Pair field shadows the
// embedded one so QR steps can carry a rendered PNG.
type wireEvent struct {
	wa.Event
	Pair *wirePair `json:"pair,omitempty"`
}

type wirePair struct {
	wa.PairEvent
	QRPNG string `json:"qr_png,omitempty"` // data:image/png;base64,... for qr steps
}

// toWire prepares an event for the browser. QR steps carry only the rendered
// image: the raw payload includes the device's ADV secret and stays server-side.
func (s *server) toWire(ev wa.Event) wireEvent {
	we := wireEvent{Event: ev}
	if ev.Pair != nil {
		wp := &wirePair{PairEvent: *ev.Pair}
		if ev.Pair.Type == wa.PairQR {
			if ev.Pair.Code != "" {
				wp.QRPNG = s.qr.dataURL(ev.Pair.Code)
			}
			wp.Code = ""
		}
		we.Pair = wp
	}
	return we
}

// qrCache renders each QR payload once, however many browsers are watching.
type qrCache struct {
	mu sync.Mutex
	m  map[string]string
}

func (c *qrCache) dataURL(code string) string {
	c.mu.Lock()
	defer c.mu.Unlock()
	if v, ok := c.m[code]; ok {
		return v
	}
	png, err := qrcode.Encode(code, qrcode.Medium, 320)
	if err != nil {
		return ""
	}
	v := "data:image/png;base64," + base64.StdEncoding.EncodeToString(png)
	if len(c.m) >= 16 {
		clear(c.m)
	}
	c.m[code] = v
	return v
}

// ---- pairing steps (for page reloads) -------------------------------------------

// screenSteps are the pairing steps that decide what the link page shows.
// Hints (scanned_without_md, passkey_request) and terminal steps are not
// kept: a reload should bring back the QR or code, not a transient message.
var screenSteps = map[wa.PairEventType]bool{wa.PairQR: true, wa.PairCode: true, wa.PairPasskeyCode: true}

type cachedStep struct {
	ev wireEvent
	at time.Time // when it was cached, for remaining-time calculations
}

// trackPairing remembers the latest screen step of every session that is
// still pairing. It runs for the life of the process.
func (s *server) trackPairing(events <-chan wa.Event) {
	for ev := range events {
		switch {
		case ev.Type == wa.EventPair && ev.Pair != nil && screenSteps[ev.Pair.Type]:
			s.storeStep(s.toWire(ev), time.Now())
		case ev.Type == wa.EventState && ev.State != wa.StatePairing:
			s.dropStep(ev.SessionID)
		}
	}
}

func (s *server) storeStep(ev wireEvent, at time.Time) {
	s.stepMu.Lock()
	defer s.stepMu.Unlock()
	if _, ok := s.steps[ev.SessionID]; !ok && len(s.steps) >= maxSteps {
		oldest := ""
		for id, c := range s.steps {
			if oldest == "" || c.at.Before(s.steps[oldest].at) {
				oldest = id
			}
		}
		delete(s.steps, oldest)
	}
	s.steps[ev.SessionID] = cachedStep{ev: ev, at: at}
}

// step returns the cached step and the seconds left on its timer, counted
// on the server clock so the browser never has to compare clocks.
func (s *server) step(id string) (*wireEvent, int) {
	s.stepMu.Lock()
	defer s.stepMu.Unlock()
	c, ok := s.steps[id]
	if !ok {
		return nil, 0
	}
	left := 0
	if c.ev.Pair != nil && c.ev.Pair.TimeoutSeconds > 0 {
		elapsed := time.Since(c.at).Seconds()
		left = max(0, int(math.Ceil(float64(c.ev.Pair.TimeoutSeconds)-elapsed)))
	}
	return &c.ev, left
}

func (s *server) dropStep(id string) {
	s.stepMu.Lock()
	delete(s.steps, id)
	s.stepMu.Unlock()
}

// ---- server-sent events --------------------------------------------------------------

// events streams every manager event as SSE until the client disconnects or
// the server shuts down.
func (s *server) events(w http.ResponseWriter, r *http.Request) {
	if s.streams.Add(1) > maxStreams {
		s.streams.Add(-1)
		w.Header().Set("Retry-After", strconv.Itoa(int(heartbeat.Seconds())))
		writeError(w, http.StatusServiceUnavailable, "too many event streams open")
		return
	}
	defer s.streams.Add(-1)
	s.watchShutdown(r)
	rc := http.NewResponseController(w)
	sub, unsubscribe := s.mgr.Subscribe()
	defer unsubscribe()

	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-store")
	h.Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)

	// Every write gets its own deadline, so a stalled client cannot pin the
	// handler while the stream otherwise stays open indefinitely.
	send := func(chunk string) bool {
		_ = rc.SetWriteDeadline(time.Now().Add(writeTimeout))
		if _, err := io.WriteString(w, chunk); err != nil {
			return false
		}
		return rc.Flush() == nil
	}
	if !send("retry: 3000\n: connected\n\n") {
		return
	}
	tick := time.NewTicker(heartbeat)
	defer tick.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case <-s.done:
			return
		case <-tick.C:
			if !send(": ping\n\n") {
				return
			}
		case ev, ok := <-sub:
			if !ok {
				return
			}
			data, err := json.Marshal(s.toWire(ev))
			if err != nil {
				s.log.Warn("encode event", "err", err)
				continue
			}
			if !send(fmt.Sprintf("event: %s\ndata: %s\n\n", ev.Type, data)) {
				return
			}
		}
	}
}

// watchShutdown hooks the serving http.Server once, so open SSE streams end
// promptly on Shutdown instead of holding it until its timeout.
func (s *server) watchShutdown(r *http.Request) {
	srv, ok := r.Context().Value(http.ServerContextKey).(*http.Server)
	if !ok {
		return
	}
	s.shutdownOnce.Do(func() {
		srv.RegisterOnShutdown(func() { close(s.done) })
	})
}
