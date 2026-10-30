package mcp

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/vid-t/whatsapp-bridge/internal/auth"
)

// Credentials with and without the consent to send.
const (
	readOnlyKey   = "wb_live_dddddddd_000000000000000000000000000000000000000000000004"
	readOnlyGrant = "wb_at_readonly-grant"
	sendGrant     = "wb_at_send-grant"
)

func consentVerifier() fakeVerifier {
	v := testVerifier()
	v[readOnlyKey] = auth.KeyInfo{ID: "dddddddd", Label: "Old script", Kind: auth.KindMCP}
	v[readOnlyGrant] = auth.KeyInfo{ID: "oa_0000000000000001", Label: "Claude", Kind: auth.KindMCP, OAuth: true}
	v[sendGrant] = auth.KeyInfo{ID: "oa_0000000000000002", Label: "Claude", Kind: auth.KindMCP, OAuth: true, CanSend: true}
	return v
}

// newConsentHarness is newSendHarness with consentVerifier: sending is on
// for the bridge, and each credential has its own consent.
func newConsentHarness(t *testing.T, opts Options) (*harness, *fakeSender) {
	t.Helper()
	store := newFixture()
	fs := &fakeSender{store: store}
	h := &harness{t: t, store: store}
	opts.Name, opts.Version, opts.Sender = "Acme Team", "abc1234", fs
	opts.now = func() time.Time { return testNow }
	opts.Audit = func(ev AuditEvent) {
		h.mu.Lock()
		defer h.mu.Unlock()
		h.events = append(h.events, ev)
	}
	mux := http.NewServeMux()
	Register(mux, store, consentVerifier(), nil, opts)
	ts := httptest.NewServer(mux)
	t.Cleanup(ts.Close)
	h.url = ts.URL
	return h, fs
}

func (h *harness) sessionFor(token string) *mcp.ClientSession {
	h.t.Helper()
	cs, err := h.connect(token)
	if err != nil {
		h.t.Fatalf("connect: %v", err)
	}
	h.t.Cleanup(func() { _ = cs.Close() })
	return cs
}

func TestSendRefusedWithoutConsent(t *testing.T) {
	args := map[string]any{"chat_jid": dmJID, "text": "Running late"}
	for _, tc := range []struct {
		name, token string
		want        []string
		status      string
	}{
		{"key", readOnlyKey,
			[]string{"This key was created read-only, so nothing was sent.", "create a new key with sending allowed, under Keys"},
			"sending: off for this key; it was created read-only"},
		{"grant", readOnlyGrant,
			[]string{"This connection was approved read-only, so nothing was sent.", "connect this assistant to the bridge again and click Allow"},
			"sending: off for this connection; it was approved read-only"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, fs := newConsentHarness(t, Options{})
			cs := h.sessionFor(tc.token)

			// The tool stays listed: the list is the same for every caller.
			res, err := cs.ListTools(context.Background(), nil)
			if err != nil {
				t.Fatal(err)
			}
			var listed bool
			for _, tool := range res.Tools {
				listed = listed || tool.Name == "send_message"
			}
			if !listed {
				t.Fatal("send_message should be listed when the bridge has sending on")
			}

			for _, want := range tc.want {
				mustErr(t, cs, "send_message", args, want)
			}
			if n := len(fs.sent()); n != 0 {
				t.Fatalf("%d sends reached the manager without the consent to send", n)
			}
			ev := h.audit()
			last := ev[len(ev)-1]
			if last.Tool != "send_message" || last.Error != "send_not_allowed" || last.Rows != 0 || len(last.ChatJIDs) != 0 {
				t.Fatalf("a refused send must be audited as send_not_allowed: %+v", last)
			}
			status := mustOK(t, cs, "get_status", nil)
			mustContain(t, status, tc.status)
			mustNotContain(t, status, "sending: on")
			mustOK(t, cs, "list_chats", nil) // reading still works
		})
	}
}

func TestSendAllowedWithConsent(t *testing.T) {
	h, fs := newConsentHarness(t, Options{})
	for _, tok := range []string{mcpKey, sendGrant} {
		cs := h.sessionFor(tok)
		mustContain(t, mustOK(t, cs, "get_status", nil), "sending: on; send_message sends text to ticked chats only")
		mustOK(t, cs, "send_message", map[string]any{"chat_jid": dmJID, "text": "On my way " + tok[:6]})
	}
	if got := fs.sent(); len(got) != 2 || got[1].KeyID != "oa_0000000000000002" {
		t.Fatalf("sends: %+v", got)
	}
	ins := h.sessionFor(sendGrant).InitializeResult().Instructions
	if !strings.Contains(ins, "approved with sending") || !strings.Contains(ins, "get_status says whether this one can send") {
		t.Fatalf("instructions should say sending needs consent: %q", ins)
	}
}

func TestRefusedSendsCountAgainstNoSendLimit(t *testing.T) {
	// One send for the whole bridge per minute: refusals must not use it up,
	// nor reserve the repeat window for the same text.
	h, fs := newConsentHarness(t, Options{Limits: Limits{SendOverall: 1}})
	ro := h.sessionFor(readOnlyKey)
	args := map[string]any{"chat_jid": dmJID, "text": "Payment sent"}
	for i := 0; i < 3; i++ {
		mustErr(t, ro, "send_message", args, "nothing was sent")
	}
	ok := h.sessionFor(mcpKey)
	mustOK(t, ok, "send_message", args)
	if n := len(fs.sent()); n != 1 {
		t.Fatalf("%d sends, want 1", n)
	}
}

func TestReadOnlyBridgeIgnoresConsent(t *testing.T) {
	// Without a sender there is no send_message at all, whatever the key says.
	h := newHarness(t, newFixture(), Options{})
	cs := h.session() // mcpKey has the consent to send
	if _, err := cs.CallTool(context.Background(), &mcp.CallToolParams{Name: "send_message", Arguments: map[string]any{"chat_jid": dmJID, "text": "hi"}}); err == nil || !strings.Contains(err.Error(), "unknown tool") {
		t.Fatalf("send_message on a read-only bridge: %v, want unknown tool", err)
	}
	mustContain(t, mustOK(t, cs, "get_status", nil), "sending: off; this bridge is read-only")
}
