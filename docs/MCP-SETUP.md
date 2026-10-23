# Connect Claude, ChatGPT or Codex to whatsapp-bridge (MCP)

The bridge exposes one read-only MCP endpoint, `https://<host>/mcp` (streamable HTTP).
An assistant connects once, lists six tools, and from then on decides itself when to
call them while it answers you. Nothing reachable through MCP can send, change or delete
a WhatsApp message.

There are two ways to connect:

- **Sign in (OAuth), the normal way.** Paste the URL into the assistant, click connect,
  sign in on the bridge's own page and click **Allow**. No key to
  copy. Works with claude.ai, Claude Desktop, Claude Code, Codex and ChatGPT.
- **A key** (`wb_live_...`) sent as a header, for scripts and clients that cannot sign in
  (section 6).

In the commands below, replace `<host>` with the address you reach the bridge at, for
example `bridge.example.com`.

---

## 1. What happens when you sign in

1. The assistant calls `https://<host>/mcp`, gets "401, sign in first", and reads where to
   sign in from the bridge's discovery documents.
2. It identifies itself to the bridge, either with its published identity (an https
   address describing the app, for example `https://claude.ai/oauth/claude-code-client-metadata`)
   or by registering automatically. Both work; nothing needs setting up on the bridge.
3. Your browser opens the bridge's sign-in page. If this browser is not signed in to the
   bridge yet, sign in: with **Continue with Google** or **Continue with Microsoft** where
   they are switched on, with an emailed link, or with the access code. Then the bridge
   shows which app is asking, where it will send you back to, and what it gets: read-only access to the chats this
   bridge stores; it can never send. Click **Allow** (or **Deny**).
4. The browser returns to the assistant, which receives a token valid for one hour and
   renews it by itself. Access ends when you revoke it (section 7) or after 30 days
   without use.

Each approval is a separate *grant* with its own entries in the bridge's event log, so you
can see which assistant read what and revoke one without touching the others.

---

## 2. Claude on the web and in the desktop app

Claude.ai connects to the bridge **from Anthropic's servers**, not from your computer. The
bridge must therefore be reachable on the public internet over HTTPS with a valid
certificate (a self-hosted `https://bridge.example.com` behind Caddy is). `localhost`, a LAN address or a Tailscale (tailnet) name
will **not** work here; those work only with Claude Code and Codex.

1. Open **Customize > Connectors** (`https://claude.ai/customize/connectors`; older
   versions of the app call this **Settings > Connectors**).
2. Click **Add custom connector**.
3. **Name:** `whatsapp-bridge`. **MCP server URL:** `https://<host>/mcp`.
4. **Authentication:** choose OAuth sign-in. Either timing works:
   - **Sign in now** asks you to sign in straight away.
   - **Sign in when needed** asks the first time Claude talks to the bridge. Every bridge
     tool reads chat data, so in practice that is the first time you use the connector.
5. **OAuth client:** leave the default. Both **Use Claude's published identity** and
   **Register automatically** work; the bridge accepts either. **Use your own OAuth
   client** also works if you want a fixed client id (see below).
6. Click **Add** (or **Connect**). A window opens on the bridge's sign-in page: enter the
   access code if asked, check that it names Claude and says you will be sent back to
   `claude.ai`, then click **Allow**.
7. In a chat, click **+**, then **Connectors**, and make sure whatsapp-bridge is switched on.
   Ask a first question (see section 8).

To use **your own OAuth client**, register one once and paste its `client_id` into the
dialog, leaving the secret empty (the bridge has public clients only):

```sh
curl -X POST https://<host>/oauth/register -H 'Content-Type: application/json' \
  -d '{"client_name":"Claude","redirect_uris":["https://claude.ai/api/mcp/auth_callback"]}'
# -> 201 {"client_id":"wbc_...","token_endpoint_auth_method":"none",...}
```

Notes:

- **Team and Enterprise:** an Owner adds the connector for the organisation, and each
  member then signs in themselves. Each member's approval is its own grant, so the event
  log is per person and per assistant.
- **Free** plans can add one custom connector in total.
- The **Request headers** route with a key (section 6) still works where Anthropic has
  enabled that beta, but sign-in is simpler and needs no key handling.

---

## 3. Claude Code

Claude Code connects from your own machine, so a bridge on `localhost` or a tailnet host
works as well as a public one.

```sh
claude mcp add --transport http whatsapp-bridge https://<host>/mcp
claude mcp list
# whatsapp-bridge: https://<host>/mcp (HTTP) - ! Needs authentication

claude mcp login whatsapp-bridge        # opens the browser on the bridge's sign-in page
claude mcp list
# whatsapp-bridge: https://<host>/mcp (HTTP) - ✔ Connected
```

Inside a running `claude` session, `/mcp` offers the same sign-in. On a machine without a
browser (SSH), `claude mcp login whatsapp-bridge --no-browser` prints the sign-in URL; open it
on any computer, sign in and allow, then paste the address the browser lands on
(`http://localhost:<port>/callback?code=...`) back into the terminal.

- By default the server is added for the current project only (`--scope local`). Add
  `--scope user` to make it available in every project.
- Claude Code keeps the tokens in its own credential store, not in `~/.claude.json`.
- `claude mcp logout whatsapp-bridge` forgets the tokens on this machine; revoke the grant on
  the bridge (section 7) to cut access off everywhere.
- To use a key instead: `claude mcp add --transport http whatsapp-bridge https://<host>/mcp
  --header "Authorization: Bearer wb_live_..."`. With a header configured Claude Code does
  not try to sign in.

Then start `claude` and try:

> Use whatsapp-bridge: what is the bridge's status, and what are the three most recent
> messages in my family group? Cite them.

Claude asks you to allow each tool the first time. All six are read-only, so allowing
them for the session is safe.

---

## 4. Codex CLI

```sh
codex mcp add whatsapp-bridge --url https://<host>/mcp   # starts the sign-in straight away
codex mcp login whatsapp-bridge                          # sign in again later, if needed
codex mcp list
# whatsapp-bridge  https://<host>/mcp  -  enabled  OAuth
```

`codex mcp login whatsapp-bridge --no-browser` prints the URL and accepts the callback address
when there is no browser. `codex mcp logout whatsapp-bridge` forgets the tokens locally.
`codex mcp list` shows only the configuration; the first prompt tests the connection, for
example *"Call the whatsapp-bridge get_status tool and tell me how fresh the data is."*

To use a key instead, Codex reads it from an environment variable, so the key never lands
in its config file:

```sh
codex mcp add whatsapp-bridge --url https://<host>/mcp --bearer-token-env-var WHATSAPP_BRIDGE_MCP_KEY
export WHATSAPP_BRIDGE_MCP_KEY=wb_live_...   # e.g. in ~/.zshrc
```

---

## 5. ChatGPT (developer mode)

ChatGPT's developer mode accepts MCP servers that use OAuth, so it can now connect.
Like claude.ai it calls from OpenAI's servers, so the bridge needs its public HTTPS
address.

1. In ChatGPT, open **Settings > Apps** (called **Apps & Connectors** in some versions),
   turn on **Developer mode** under **Advanced**, and choose **Create** (add an app by
   URL).
2. **MCP server URL:** `https://<host>/mcp`. **Authentication:** OAuth. Leave the client
   settings empty; ChatGPT registers itself.
3. Sign in on the bridge's page and click **Allow**. In a chat, enable the app from the
   tools menu (developer mode).

Developer mode is for testing and may show warnings about unverified apps; the bridge's
tools are read-only either way.

---

## 6. Keys for scripts and header-only setups

Sign-in (sections 2 to 5) is the normal way to connect an assistant. Keys remain for
scripts and for clients that can only send a fixed header. A key can call `/mcp` and read
the bridge's `/api/*` endpoints (GET only); it can never manage keys or change anything.

Each person and each assistant gets its own key, so it can be revoked on its own and the
event log shows who read what.

**In the web UI:** open `https://<host>/keys`, click **New key**, give it a label such as
`Claude - Anna`. The key is shown **once**. Copy it straight
into the assistant's settings. The bridge stores only a hash of it, so nobody can show it
to you again. If you lose it, revoke it and mint a new one.

**With the API** (needs a logged-in UI session, never a key):

```sh
# log in once; the cookie jar keeps the session
curl -c jar.txt -X POST https://<host>/auth/login \
  -H 'Content-Type: application/json' -d '{"code":"<BRIDGE_ACCESS_CODE>"}'

curl -b jar.txt -X POST https://<host>/api/keys \
  -H 'Content-Type: application/json' -d '{"label":"Claude Code - Anna"}'
# -> 201 {"id":"0a1b2c3d","key":"wb_live_0a1b2c3d_<48 hex characters>","label":"Claude Code - Anna","kind":"mcp","created_at":"..."}

curl -b jar.txt https://<host>/api/keys                    # list keys (no secrets)
curl -b jar.txt -X DELETE https://<host>/api/keys/0a1b2c3d # revoke
```

On a local bridge in open mode (no access code set), skip the login and drop `-b jar.txt`.

**What a key looks like:** `wb_live_` + an 8-character id + `_` + a 48-character secret,
all lowercase hex, for example `wb_live_0a1b2c3d_4e5f...` (65 characters in total).

**claude.ai with a key** (only where Anthropic has enabled the Request headers beta): in
**Add custom connector** choose **No sign-in**, open **Request headers**, pick the header
name `authorization` and enter `Bearer wb_live_...` (the word `Bearer`, a space, then the
key; Claude adds no prefix), mark it **Required** and click **Add**. A header is one key
shared by everyone who uses the connector, and it cannot be edited after saving: to
rotate it, remove the connector and add it again.

---

## 7. See and revoke what is connected

Every approval is a grant. The bridge lists the active ones (needs a signed-in browser
session, never a key; log in as in section 6):

```sh
curl -b jar.txt https://<host>/api/oauth/grants
# -> [{"id":"oa_0d8efbf4838d04f2","client_name":"Claude Code",
#      "client_id":"https://claude.ai/oauth/claude-code-client-metadata",
#      "created_at":"...","last_used_at":"..."}]

curl -b jar.txt -X DELETE https://<host>/api/oauth/grants/oa_0d8efbf4838d04f2   # -> 204
```

Revoking a grant ends its tokens at once, including the one the assistant would use to
renew itself; the assistant then asks you to sign in again. The **Connect AI** page in the
web UI lists the same grants with a **Revoke** button. Keys are listed and revoked at
`/keys`.

---

## 8. Five prompts that use the tools well

Each result carries citations of the form `wa://<chat_jid>/<message_id>`. A good answer
names the sender and time and gives the citation for every fact it takes from a chat.

1. **"What did Sam say about invoice 1042 and when it is due? Cite the messages."**
   Calls `search_messages` (`invoice 1042`), then `get_message_context` around the best
   hit. A good answer gives the amount and due date, who said it and when, plus any open
   question, each with its `wa://` citation.
2. **"Summarise today's Project Atlas group: decisions, open questions, and who owes a
   reply."** Calls `read_chat` with `chat_name`, and `before` to page back if needed. A
   good answer gives a short list with one citation per point and says which time window
   it covered.
3. **"Who sent a quote for the venue since 1 October, and at what price?"** Calls
   `search_messages` with `from: 2026-10-01`. A good answer is a table of sender, price,
   time and citation, and says when the search limit may have hidden older quotes.
4. **"Is the WhatsApp data up to date? When did the bridge last receive a message?"** Calls
   `get_status`. A good answer quotes the device state (for example "logged_out since
   2026-10-16") and the newest message time, and says what that means for the answer.
5. **"Find the draft agreement that was sent on WhatsApp last week. What is in it?"** Calls
   `search_messages`, then `get_document`. A good answer gives the file name, the caption,
   who sent it and when (with the citation), and says plainly that this build cannot open
   the file. It must not guess at the contents.

---

## 9. Troubleshooting

| Symptom | Cause and fix |
|---|---|
| **claude.ai: "Couldn't register with whatsapp-bridge's sign-in service"** | The bridge build predates sign-in, or OAuth is switched off. Check that `https://<host>/.well-known/oauth-authorization-server` returns JSON and that the bridge log has no `OAuth disabled` line (it appears when `BRIDGE_PUBLIC_URL` is not an https origin). |
| **The sign-in window says "This sign-in link cannot be used"** | The app's registration expired (automatic registrations without an approval are deleted after 24 hours), the return address is not one the app registered, or the request was altered. Remove the connector in the app and add it again. Nothing was shared. |
| **"That access code is not right"** | Use the bridge's access code (`BRIDGE_ACCESS_CODE`), the same one as for the web UI. After five wrong tries in a minute the page asks you to wait. |
| **Claude Code shows `! Needs authentication`** | Not signed in yet, the grant was revoked, or it was unused for 30 days. Run `claude mcp login whatsapp-bridge` (or `/mcp` in a session). |
| **Asked to sign in again unexpectedly** | The grant was revoked on the bridge, or the same renewal token was used twice (for example a copied credential store on two machines); the bridge then revokes that grant as a precaution. Sign in again. |
| **401** with a key / Claude Code shows `Server rejected the configured Authorization header (HTTP 401)` | The key is missing, mistyped or revoked. The response body says which. In claude.ai, check that the header value starts with `Bearer ` plus a space. In Codex, check that `WHATSAPP_BRIDGE_MCP_KEY` is set in the shell that started Codex. Claude Code does not try OAuth when an Authorization header is configured, so a 401 then always means the key itself. |
| Tool error **"Rate limit reached"** | More than 60 tool calls in a minute for this key or grant, or 600 for the whole bridge. The assistant is told how many seconds to wait. This usually means an agent loop: stop it and ask a narrower question. |
| **HTTP 429** with `Retry-After` | More than 300 HTTP requests a minute for one key or grant (3,000 for the bridge), or too many sign-in attempts from one address. Wait for the number of seconds in `Retry-After`. |
| **Empty results** | (1) The chat is not ticked. Only ticked chats exist for assistants: tick it on the bridge's web page. Storage starts from that moment; older history is not fetched again. (2) The device is logged out: `get_status` says so. (3) Search needs every word to appear: use fewer, distinctive words (a name, a number, a place). (4) Dates are UTC, and a date range applies to the newest 200 matches. |
| **Stale answers** | Every result starts with `as_of` and a `freshness:` line, such as `device "Desk" is disconnected since … reconnecting automatically` or `… is logged_out …, so nothing new arrives until it is relinked`. Relink a logged-out device from the bridge's web page. A phone that stays offline for 14 days logs out its linked devices automatically. |
| **claude.ai or ChatGPT cannot connect, but Claude Code can** | claude.ai and ChatGPT call from their own servers: the host must be public with a valid TLS certificate. A tailnet or `localhost` URL works only with Claude Code and Codex. |
| **"unknown chat_jid"** / **"no ticked chat is named …"** | Ask the assistant to call `list_chats` first. It returns the exact names and ids. |

---

## 10. What reaches the AI provider

The bridge sends nothing on its own initiative. The assistant decides which tools to
call, and only the results it asks for leave the bridge: chat names and the text of the
messages requested, capped at 1 MB per call. They go to the model provider (Anthropic
for Claude, OpenAI for Codex) under your own plan and that provider's data terms. With
claude.ai the request comes from Anthropic's servers. With Claude Code and Codex it comes
from your machine, and the results then travel to the provider's API. No MCP setup avoids
this, short of a local model. Unticked chats are not stored, so no assistant can reach
them. The bridge logs every call (key or grant id, tool, a hash of the arguments, chats
touched, number of rows) but never the message text.
