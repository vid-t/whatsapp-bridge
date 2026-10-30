package oauth

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"
	"time"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/vid-t/whatsapp-bridge/internal/auth"
	"github.com/vid-t/whatsapp-bridge/internal/store"
)

// restart is the same bridge started again on the same database, with
// sending on or off: grants made before keep the consent they were given.
func (h *harness) restart(send bool) *Server {
	h.t.Helper()
	srv, err := newServer(Deps{DB: h.db, Auth: h.auth, Log: slog.New(slog.DiscardHandler)}, Options{PublicURL: h.url, SendEnabled: send, now: h.now})
	if err != nil {
		h.t.Fatal(err)
	}
	return srv
}

// callToolArgs runs one tool and returns its text and error flag.
func (h *harness) callToolArgs(token, name string, args map[string]any) (string, bool) {
	h.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	c := sdk.NewClient(&sdk.Implementation{Name: "oauth-test", Version: "1"}, nil)
	cs, err := c.Connect(ctx, &sdk.StreamableClientTransport{
		Endpoint:             h.url + "/mcp",
		HTTPClient:           &http.Client{Transport: bearerRT{token}, Timeout: 10 * time.Second},
		DisableStandaloneSSE: true,
		MaxRetries:           -1,
	}, nil)
	if err != nil {
		h.t.Fatalf("connect: %v", err)
	}
	defer cs.Close()
	res, err := cs.CallTool(ctx, &sdk.CallToolParams{Name: name, Arguments: args})
	if err != nil {
		h.t.Fatalf("%s: %v", name, err)
	}
	var b strings.Builder
	for _, c := range res.Content {
		if tc, ok := c.(*sdk.TextContent); ok {
			b.WriteString(tc.Text)
		}
	}
	return b.String(), res.IsError
}

func TestGrantRecordsConsentToSend(t *testing.T) {
	for _, send := range []bool{false, true} {
		h := newHarness(t, harnessOpts{code: testCode, send: send})
		f := h.fullFlow()
		grants := h.grants(f.cookie)
		if len(grants) != 1 || grants[0].CanSend != send {
			t.Fatalf("send=%v: grants: %+v", send, grants)
		}
		_, raw := h.get(GrantsPath, withCookie(f.cookie))
		var shape []map[string]any
		_ = json.Unmarshal([]byte(raw), &shape)
		if shape[0]["can_send"] != send {
			t.Fatalf("send=%v: grant JSON: %s", send, raw)
		}
		k, err := h.srv.VerifyAccessToken(context.Background(), f.access)
		if err != nil || k.CanSend != send || !k.OAuth {
			t.Fatalf("send=%v: verify: %+v %v", send, k, err)
		}
		// A refreshed token keeps the grant's consent.
		status, tok := h.refresh(f.clientID, f.refresh)
		if status != http.StatusOK {
			t.Fatalf("refresh: %d %v", status, tok)
		}
		if k, err := h.srv.VerifyAccessToken(context.Background(), tok["access_token"].(string)); err != nil || k.CanSend != send {
			t.Fatalf("send=%v: refreshed token: %+v %v", send, k, err)
		}
		want := "send=off"
		if send {
			want = "send=on"
		}
		var audited bool
		for _, a := range h.auditLog() {
			audited = audited || strings.HasPrefix(a, "oauth.consent_granted ") && strings.HasSuffix(a, want)
		}
		if !audited {
			t.Fatalf("send=%v: consent audit should say %s: %v", send, want, h.auditLog())
		}
	}
}

func TestReadOnlyGrantStaysReadOnlyWhenSendingIsTurnedOn(t *testing.T) {
	// Approved on the read-only consent page; the MCP endpoint now sends.
	h := newHarness(t, harnessOpts{code: testCode, mcpSend: true})
	f := h.fullFlow()

	text, isErr := h.callToolArgs(f.access, "send_message", map[string]any{"chat_jid": "41791111111@s.whatsapp.net", "text": "hi"})
	if !isErr || !strings.Contains(text, "This connection was approved read-only, so nothing was sent.") ||
		!strings.Contains(text, "connect this assistant to the bridge again and click Allow") {
		t.Fatalf("a read-only grant must be refused send_message: %v %s", isErr, text)
	}
	ev := h.toolEvents()
	if last := ev[len(ev)-1]; last.Tool != "send_message" || last.Error != "send_not_allowed" || last.KeyID != h.grants(f.cookie)[0].ID {
		t.Fatalf("audit: %+v", last)
	}
	if text, _ := h.callToolArgs(f.access, "get_status", nil); !strings.Contains(text, "sending: off for this connection") {
		t.Fatalf("get_status should say this connection cannot send: %s", text)
	}

	// Restarting with sending on changes nothing for the grant, nor for
	// tokens refreshed after.
	srv := h.restart(true)
	if k, err := srv.VerifyAccessToken(context.Background(), f.access); err != nil || k.CanSend {
		t.Fatalf("after the restart: %+v %v", k, err)
	}
	if g, err := srv.Grants(context.Background()); err != nil || len(g) != 1 || g[0].CanSend {
		t.Fatalf("grants after the restart: %+v %v", g, err)
	}
	resp, err := srv.refresh(context.Background(), url.Values{"refresh_token": {f.refresh}}, f.clientID, h.url)
	if err != nil {
		t.Fatal(err)
	}
	if k, err := srv.VerifyAccessToken(context.Background(), resp.AccessToken); err != nil || k.CanSend {
		t.Fatalf("token refreshed with sending on: %+v %v", k, err)
	}
}

func TestGrantApprovedWithSendingCanSend(t *testing.T) {
	h := newHarness(t, harnessOpts{code: testCode, send: true, mcpSend: true})
	f := h.fullFlow()
	// Past the consent check, the send fails on the chat: nothing is ticked.
	text, isErr := h.callToolArgs(f.access, "send_message", map[string]any{"chat_jid": "41791111111@s.whatsapp.net", "text": "hi"})
	if !isErr || !strings.Contains(text, "No ticked chat has chat_jid") {
		t.Fatalf("a grant approved with sending should reach the send: %s", text)
	}
	if ev := h.toolEvents(); ev[len(ev)-1].Error != "unknown_chat" {
		t.Fatalf("audit: %+v", ev[len(ev)-1])
	}
	if text, _ := h.callToolArgs(f.access, "get_status", nil); !strings.Contains(text, "sending: on;") {
		t.Fatalf("get_status: %s", text)
	}
}

// The consent is what the page showed, checked against the bridge at the
// moment of approval: a page from before a restart counts as it was shown.
func TestConsentNeedsThePageAndTheBridge(t *testing.T) {
	for _, tc := range []struct {
		name              string
		pageSend, nowSend bool
		want              bool
	}{
		{"read-only page, sending turned on", false, true, false},
		{"send page, sending turned off", true, false, false},
		{"send page, sending on", true, true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t, harnessOpts{code: "", send: tc.pageSend}) // open mode: no sign-in step
			id := h.register(loopbackCB)
			p := newPKCE()
			resp, page := h.get(AuthorizePath + "?" + authzQuery(id, loopbackUsed, p, "s").Encode())
			if resp.StatusCode != http.StatusOK || strings.Contains(page, `name="send"`) != tc.pageSend {
				t.Fatalf("consent page: %d, send field %v\n%s", resp.StatusCode, tc.pageSend, page)
			}
			form := hiddenFields(page)
			form.Set("action", "allow")
			srv := h.restart(tc.nowSend)
			req := httptest.NewRequest(http.MethodPost, h.url+AuthorizePath, strings.NewReader(form.Encode()))
			req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
			rec := httptest.NewRecorder()
			srv.wrap(srv.authorize).ServeHTTP(rec, req)
			loc, err := url.Parse(rec.Header().Get("Location"))
			if rec.Code != http.StatusSeeOther || err != nil || loc.Query().Get("code") == "" {
				t.Fatalf("approve: %d %q", rec.Code, rec.Header().Get("Location"))
			}
			status, tok := h.exchange(id, loc.Query().Get("code"), loopbackUsed, p.verifier, nil)
			if status != http.StatusOK {
				t.Fatalf("exchange: %d %v", status, tok)
			}
			k, err := h.srv.VerifyAccessToken(context.Background(), tok["access_token"].(string))
			if err != nil || k.CanSend != tc.want {
				t.Fatalf("verify: %+v %v, want can send %v", k, err, tc.want)
			}
		})
	}
}

func TestOldGrantsStayReadOnly(t *testing.T) {
	db, err := store.Open(filepath.Join(t.TempDir(), "old.db"), "")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	ctx := context.Background()
	// Code and grant tables from before the consent to send existed.
	for _, q := range []string{
		`CREATE TABLE bridge_oauth_codes (hash BLOB PRIMARY KEY, client_id TEXT NOT NULL, client_name TEXT NOT NULL DEFAULT '', redirect_uri TEXT NOT NULL,
			code_challenge TEXT NOT NULL, scope TEXT NOT NULL, resource TEXT NOT NULL DEFAULT '', session_ref TEXT NOT NULL DEFAULT '',
			created_at INTEGER NOT NULL, expires_at INTEGER NOT NULL, used_at INTEGER, grant_id TEXT)`,
		`CREATE TABLE bridge_oauth_grants (id TEXT PRIMARY KEY, client_id TEXT NOT NULL, client_name TEXT NOT NULL DEFAULT '', scope TEXT NOT NULL,
			resource TEXT NOT NULL DEFAULT '', session_ref TEXT NOT NULL DEFAULT '', created_at INTEGER NOT NULL, expires_at INTEGER NOT NULL,
			last_used_at INTEGER, revoked_at INTEGER)`,
	} {
		if _, err := db.ExecContext(ctx, q); err != nil {
			t.Fatal(err)
		}
	}
	now := time.Now().UTC()
	if _, err := db.ExecContext(ctx, `INSERT INTO bridge_oauth_grants (id, client_id, client_name, scope, created_at, expires_at) VALUES ('oa_old', 'c', 'Claude', 'mcp', ?, ?)`,
		ms(now), ms(now.Add(time.Hour))); err != nil {
		t.Fatal(err)
	}
	a, err := auth.New(ctx, db, auth.Options{})
	if err != nil {
		t.Fatal(err)
	}
	srv, err := newServer(Deps{DB: db, Auth: a}, Options{SendEnabled: true})
	if err != nil {
		t.Fatal(err)
	}
	g, err := srv.Grants(ctx)
	if err != nil || len(g) != 1 || g[0].CanSend {
		t.Fatalf("an old grant must not gain sending: %+v %v", g, err)
	}
	if _, err := newServer(Deps{DB: db, Auth: a}, Options{}); err != nil {
		t.Fatalf("migrating twice: %v", err)
	}
}
