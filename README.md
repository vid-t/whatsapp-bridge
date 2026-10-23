# whatsapp-bridge

whatsapp-bridge lets your own AI assistant read the WhatsApp chats you choose. It links to your
WhatsApp as a read-only extra device, like WhatsApp Web, and keeps the chats you tick in an
encrypted SQLite file on your server. Claude, Claude Code, Codex, ChatGPT or any other MCP
client reads them over MCP. whatsapp-bridge never sends messages, presence ("online") or read
receipts. It is one Go binary with a web UI to manage it.

## What it does

- **Pairing** by QR code or 8-character phone code, from the web UI or the terminal,
  including WhatsApp's passkey confirmation step. Several WhatsApp accounts per instance.
- **Ticked chats.** You choose which chats the bridge keeps. Unticking a chat deletes its
  stored messages and stops storing new ones. New chats start ticked.
- **Full-text search** across ticked chats (SQLite FTS5; case and accents ignored).
- **Documents, as references.** The file name and caption of documents, images, videos and
  voice notes are stored and searchable.
- **Six read-only MCP tools:** `get_status`, `list_chats`, `search_messages`, `read_chat`,
  `get_message_context` and `get_document`. Messages come with `wa://` citations, and
  every result says how fresh the data is.
- **OAuth 2.1 sign-in for assistants**, with dynamic client registration (RFC 7591) and
  client metadata documents, so Claude, Claude Code, Codex and ChatGPT connect by signing
  in. API keys (`wb_live_...`) for scripts and clients that cannot sign in.
- **People.** An access code for the owner, plus optional email sign-in links with
  approvals, and Google or Microsoft sign-in. Members are invited, approved and removed
  from the web UI or the CLI.
- **Audit of every tool call:** which key or grant, which tool, which chats, how many rows
  and how long it took, never the message text.

Not built yet: downloading attachments and extracting their text, passkeys, and
per-member data scoping (today every member sees all ticked chats).

## Install on a server

[`deploy/SELF-HOSTING.md`](deploy/SELF-HOSTING.md) covers three ways to run it behind
Caddy with automatic HTTPS:

- **cloud-init** for a fresh VPS at any provider that accepts user data (Hetzner,
  DigitalOcean, Vultr, Linode and others);
- **Docker Compose** with Caddy;
- **an install script** for an existing Ubuntu or Debian server.

Each ends at `https://<your domain>` with the web UI, where you link WhatsApp, tick
chats and connect your AI. Release archives for Linux and macOS (amd64 and arm64) are on
the Releases page; the container image is `ghcr.io/vid-t/whatsapp-bridge`.

## Run it locally

You need Go 1.27 or newer. There is no cgo: the SQLite driver is pure Go. The web UI is
already built and embedded in the binary.

```sh
git clone https://github.com/vid-t/whatsapp-bridge.git
cd whatsapp-bridge
CGO_ENABLED=0 go build -o bin/whatsapp-bridge ./cmd/whatsapp-bridge

./bin/whatsapp-bridge keygen > whatsapp-bridge.env   # a database key and an access code
chmod 600 whatsapp-bridge.env                    # losing the key makes the database unreadable
./bin/whatsapp-bridge serve -env-file whatsapp-bridge.env
```

Open <http://127.0.0.1:8080> and enter the access code from `whatsapp-bridge.env`. Link
WhatsApp (on the phone: **Settings > Linked devices > Link a device**, then scan the code),
tick the chats the bridge may keep, and connect your AI. The phone lists the bridge as
"whatsapp-bridge" under Linked devices.

All commands:

```sh
./bin/whatsapp-bridge serve                    # web UI, API and MCP on http://127.0.0.1:8080 (the default command)
./bin/whatsapp-bridge pair                     # pair a device from the terminal (QR in the terminal)
./bin/whatsapp-bridge pair-code -phone 15551234567   # pair with an 8-character code instead
./bin/whatsapp-bridge sessions                 # list stored sessions
./bin/whatsapp-bridge keygen                   # print a fresh BRIDGE_DB_KEY and BRIDGE_ACCESS_CODE
./bin/whatsapp-bridge members list             # members and pending requests (also invite|approve|remove <email>)
```

Every command accepts `-env-file <path>`, a systemd-style file read before the flags.
`pair`, `pair-code` and `members` can run next to a running `serve` on the same data
directory: they do not connect the other stored devices. At most 3 pairings may be in
progress at once; extra requests get HTTP 429.

## Connect your AI

Add `https://<host>/mcp` in your assistant and sign in on the bridge's page when it opens.
[`docs/MCP-SETUP.md`](docs/MCP-SETUP.md) has the steps for claude.ai, Claude Desktop,
Claude Code, Codex and ChatGPT, plus keys for scripts. Claude Code, for example:

```sh
claude mcp add --transport http whatsapp-bridge https://<host>/mcp
claude mcp login whatsapp-bridge
```

claude.ai and ChatGPT call the bridge from their own servers, so they need a public HTTPS
address. Claude Code and Codex also work with `localhost` or a private network.

## Configuration

By flag or environment variable:

| Variable | Flag | Default | Meaning |
|---|---|---|---|
| `BRIDGE_ADDR` | `-addr` | `127.0.0.1:8080` | listen address; put Caddy or another TLS proxy in front for remote access |
| `BRIDGE_DATA_DIR` | `-data` | `./data` | holds `bridge.db` (sessions, chats, messages, event log) |
| `BRIDGE_DB_KEY` | `-db-key` | empty | 64 hex characters; enables Adiantum encryption at rest. Generate with `whatsapp-bridge keygen`. Changing or losing the key makes the file unreadable |
| `BRIDGE_ACCESS_CODE` | `-access-code` | unset | gates the UI; unset means open mode, allowed on loopback only. Generate with `whatsapp-bridge keygen` |
| `BRIDGE_NAME` | `-name` | `whatsapp-bridge` | name shown in the web UI, sign-in pages, emails and to MCP clients |
| `BRIDGE_DEVICE_NAME` | `-device-name` | `whatsapp-bridge` | name shown under WhatsApp > Linked devices |
| `BRIDGE_ALLOWED_HOSTS` | `-allowed-hosts` | loopback names | comma-separated `Host` headers to accept, e.g. `bridge.example.com`. Required behind a reverse proxy |
| `BRIDGE_PUBLIC_URL` | `-public-url` | from the request | external origin, e.g. `https://bridge.example.com`: OAuth issuer, MCP metadata, provider redirect URIs, emailed links |
| `BRIDGE_LOG_LEVEL` | `-log-level` | `info` | `debug`, `info`, `warn`, `error`. Debug prints pairing payloads and protocol frames; do not use it on a shared machine |
| `BRIDGE_INSECURE_BIND` | `-insecure-bind` | off | allow a non-loopback `-addr` without an access code. Private networks only |
| `BRIDGE_INSECURE_PLAIN` | `-insecure-plain` | off | allow a non-loopback `-addr` without `BRIDGE_DB_KEY` (or with `BRIDGE_MAIL_DEV_FILE`). Testing only |
| `BRIDGE_SMTP_HOST`, `BRIDGE_SMTP_PORT` | | unset, `587` | mail for email sign-in; `587` STARTTLS, `465` implicit TLS. When set, the sign-in page offers email links |
| `BRIDGE_SMTP_USER`, `BRIDGE_SMTP_PASSWORD` | | unset | PLAIN authentication, over TLS only |
| `BRIDGE_MAIL_FROM` | | `BRIDGE_SMTP_USER` | sender, e.g. `whatsapp-bridge <bridge@example.com>` |
| `BRIDGE_APPROVERS` | | unset | comma-separated addresses that receive access requests (and may sign in without approval) |
| `BRIDGE_ALLOWED_DOMAINS` | | unset | email domains admitted without approval or a member entry |
| `BRIDGE_MAIL_DEV_FILE` | | unset | development: append mails to this file instead of sending; refused off loopback |
| `BRIDGE_GOOGLE_CLIENT_ID`, `BRIDGE_GOOGLE_CLIENT_SECRET` | | unset | enable "Continue with Google"; redirect URI `<BRIDGE_PUBLIC_URL>/auth/google/callback` |
| `BRIDGE_MICROSOFT_CLIENT_ID`, `BRIDGE_MICROSOFT_CLIENT_SECRET` | | unset | enable "Continue with Microsoft"; redirect URI `<BRIDGE_PUBLIC_URL>/auth/microsoft/callback` |
| `BRIDGE_MICROSOFT_TENANT` | | `common` | `organizations`, `consumers` or a tenant id |

Off loopback, `serve` refuses to start unless both `BRIDGE_ACCESS_CODE` and
`BRIDGE_DB_KEY` are set.

## Unofficial client

whatsapp-bridge uses [whatsmeow](https://github.com/tulir/whatsmeow), an unofficial
implementation of the WhatsApp Web protocol. It is not affiliated with, endorsed by or
connected to WhatsApp or Meta. WhatsApp does not permit unofficial clients, and an account
linked to one can be restricted. whatsapp-bridge stays silent and read-only to keep that risk
low, but it cannot remove it. You accept that risk when you link an account.

whatsapp-bridge is not an AI chatbot on WhatsApp. No model writes into any chat, and nothing
reaches an AI provider unless your own assistant asks for it.

The phone stays in control. Unlinking the device in WhatsApp cuts the bridge off at once.
WhatsApp also logs out linked devices when the phone is offline for 14 days.

## Security

Report vulnerabilities privately as described in [`SECURITY.md`](SECURITY.md).

- **Encryption at rest.** With `BRIDGE_DB_KEY` set, the database file is encrypted with
  Adiantum. This protects the file and its backups, not a running process: a linked device
  must decrypt messages in memory.
- **Sign-in.** `BRIDGE_ACCESS_CODE` gates the UI as the bootstrap and emergency path;
  named members sign in by email link or Google or Microsoft.
- **Loopback by default.** The server listens on `127.0.0.1` and refuses a public address
  without an access code and a database key. The UI ships under a strict Content Security
  Policy and accepts only the `Host` names you allow.
- **Only ticked chats reach assistants.** Assistants see nothing else, and they can only
  read.
- **Who can read it.** Whoever operates the server can technically read what it holds while
  it runs. Run it on a machine you control.

To contribute, see [`CONTRIBUTING.md`](CONTRIBUTING.md).

## Licence

whatsapp-bridge is licensed under the GNU Affero General Public License, version 3 only
(AGPL-3.0-only); see [`LICENSE`](LICENSE). The binary links `go.mau.fi/libsignal`
(GPL-3.0) and `go.mau.fi/whatsmeow` (MPL-2.0), so it needs a GPL-compatible licence. The
AGPL also means that anyone who runs a modified whatsapp-bridge as a service must offer its
source to the people who use it.

Other dependencies worth knowing: `github.com/ncruces/go-sqlite3` (MIT, pure Go, with
`vfs/adiantum` for encryption), `github.com/modelcontextprotocol/go-sdk` (MIT and
Apache-2.0) and `github.com/skip2/go-qrcode` (MIT). whatsmeow has no tagged releases, so it
is pinned by commit; bump it monthly and re-test pairing.
