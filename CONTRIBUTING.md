# Contributing to whatsapp-bridge

Thank you for helping. Small, focused pull requests get reviewed fastest.

## Build and test

You need Go 1.27 or newer, and Node 22 only if you change the web UI.

```sh
CGO_ENABLED=0 go build -o bin/whatsapp-bridge ./cmd/whatsapp-bridge
gofmt -l .                 # must print nothing
go vet ./...
go test ./...
```

The web UI is a React app in `web/`. Its production build lives in
`internal/web/assets/dist/` and is embedded in the binary. After changing anything in
`web/`, rebuild it and commit the result:

```sh
npm --prefix web install       # once
go generate ./internal/web     # runs npm run build in web/
npm --prefix web run dev       # live UI on :5173, proxied to a bridge on :8080 (BRIDGE_URL)
npm --prefix web run preview:mock   # the built UI against a mock bridge, no WhatsApp needed
```

## Where things live

- `cmd/whatsapp-bridge/`: the commands (serve, pair, pair-code, sessions, keygen, members)
- `internal/wa/`: whatsmeow sessions, pairing, history sync, message capture, search
- `internal/store/`: SQLite with optional Adiantum encryption
- `internal/web/`, `internal/mcp/`, `internal/oauth/`: the UI and API, the MCP tools, OAuth
- `internal/auth/`, `internal/members/`, `internal/mail/`, `internal/sso/`: sign-in and members
- `web/`: the web UI source (React, Vite); `deploy/`: self-hosting files; `docs/`: guides

## The sending rule

whatsapp-bridge sends nothing to WhatsApp unless the operator sets `BRIDGE_ALLOW_SEND=1`.
Then it sends plain text to ticked chats only, through `send_message`. It never sends
presence ("online") or read receipts, and its MCP tools cannot change or delete messages.
All sending goes through `SendText` in `internal/wa/send.go`, behind those checks. Open an
issue before working on anything that would widen that.

## Dependencies

- `go.mau.fi/whatsmeow` (MPL-2.0) speaks the WhatsApp Web protocol. It has no tagged
  releases, so it is pinned by commit: bump it monthly and re-test pairing.
- `go.mau.fi/libsignal` (GPL-3.0) does the end-to-end encryption. Linking it is why the
  project needs a GPL-compatible licence.
- `github.com/ncruces/go-sqlite3` (MIT) is a pure-Go SQLite, with `vfs/adiantum` for
  encryption at rest, so the build needs no cgo.
- `github.com/modelcontextprotocol/go-sdk` (MIT and Apache-2.0) serves MCP;
  `github.com/skip2/go-qrcode` (MIT) draws the pairing QR code.

## Licence of contributions

whatsapp-bridge is AGPL-3.0-only. By opening a pull request you agree that your contribution
is licensed under the same terms (inbound = outbound). There is no separate contributor
agreement. Only submit code you have the right to license this way.

## Pull requests

- Keep each pull request to one change, with tests for new behaviour.
- Run `gofmt`, `go vet ./...` and `go test ./...` before you push.
- Say in the description what you changed and how you tested it.
- Report security issues privately, as described in [`SECURITY.md`](SECURITY.md).
