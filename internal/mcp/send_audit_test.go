package mcp

import "testing"

// A send's audit event names the account it went to, whether the send went
// through or was refused after the chat resolved, so the account page shows
// it. A refusal before the chat resolves stays bridge-wide.
func TestSendAuditNamesTheAccount(t *testing.T) {
	h, _ := newConsentHarness(t, Options{})
	cs := h.sessionFor(sendGrant)
	args := map[string]any{"chat_jid": dmJID, "text": "Running late"}
	mustOK(t, cs, "send_message", args)
	mustErr(t, cs, "send_message", args, "so it was not sent again")
	mustErr(t, cs, "send_message", map[string]any{"chat_jid": "41790000000@s.whatsapp.net", "text": "Hello"}, "No ticked chat")

	ev := h.audit()
	if len(ev) < 3 {
		t.Fatalf("want 3 audited calls, got %d", len(ev))
	}
	sent, dup, unknown := ev[len(ev)-3], ev[len(ev)-2], ev[len(ev)-1]
	if sent.Tool != "send_message" || sent.Error != "" || sent.SessionID != "s1" {
		t.Fatalf("a send must be filed under its account: %+v", sent)
	}
	if dup.Error != "duplicate" || dup.SessionID != "s1" {
		t.Fatalf("a refusal after the chat resolved must be filed under the account: %+v", dup)
	}
	if unknown.Error != "unknown_chat" || unknown.SessionID != "" {
		t.Fatalf("a refusal before the chat resolved stays bridge-wide: %+v", unknown)
	}
}
