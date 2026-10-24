# Configuration and commands

Every setting works as an environment variable or a flag. Every command also accepts
`-env-file <path>`, a systemd-style file read before the flags. `whatsapp-bridge keygen`
prints the two secrets you need: a database key and an access code.

## Commands

```sh
whatsapp-bridge serve                    # web UI, API and MCP on http://127.0.0.1:8080 (the default command)
whatsapp-bridge pair                     # pair a device from the terminal (QR in the terminal)
whatsapp-bridge pair-code -phone 15551234567   # pair with an 8-character code instead
whatsapp-bridge sessions                 # list stored sessions
whatsapp-bridge keygen                   # print a fresh BRIDGE_DB_KEY and BRIDGE_ACCESS_CODE
whatsapp-bridge members list             # members and pending requests (also invite|approve|remove <email>)
```

`pair`, `pair-code` and `members` can run next to a running `serve` on the same data
directory: they do not connect the other stored devices. At most 3 pairings may be in
progress at once; extra requests get HTTP 429.

Chats start unticked. For an hour after linking, the bridge holds the history WhatsApp
sends in the memory of the process that linked the phone, and ticking a chat in that
process's web UI keeps it. `pair` and `pair-code` have no web UI, so they keep none of
that history. Link from the web UI if you want it.

## Core settings

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

Off loopback, `serve` refuses to start unless both `BRIDGE_ACCESS_CODE` and
`BRIDGE_DB_KEY` are set.

## Sign-in for other people

The access code is the owner's bootstrap and emergency path. Named members sign in by
email link, Google or Microsoft.

| Variable | Default | Meaning |
|---|---|---|
| `BRIDGE_SMTP_HOST`, `BRIDGE_SMTP_PORT` | unset, `587` | mail for email sign-in; `587` STARTTLS, `465` implicit TLS. When set, the sign-in page offers email links |
| `BRIDGE_SMTP_USER`, `BRIDGE_SMTP_PASSWORD` | unset | PLAIN authentication, over TLS only |
| `BRIDGE_MAIL_FROM` | `BRIDGE_SMTP_USER` | sender, e.g. `whatsapp-bridge <bridge@example.com>` |
| `BRIDGE_APPROVERS` | unset | comma-separated addresses that receive access requests (and may sign in without approval) |
| `BRIDGE_ALLOWED_DOMAINS` | unset | email domains admitted without approval or a member entry |
| `BRIDGE_MAIL_DEV_FILE` | unset | development: append mails to this file instead of sending; refused off loopback |
| `BRIDGE_GOOGLE_CLIENT_ID`, `BRIDGE_GOOGLE_CLIENT_SECRET` | unset | enable "Continue with Google"; redirect URI `<BRIDGE_PUBLIC_URL>/auth/google/callback` |
| `BRIDGE_MICROSOFT_CLIENT_ID`, `BRIDGE_MICROSOFT_CLIENT_SECRET` | unset | enable "Continue with Microsoft"; redirect URI `<BRIDGE_PUBLIC_URL>/auth/microsoft/callback` |
| `BRIDGE_MICROSOFT_TENANT` | `common` | `organizations`, `consumers` or a tenant id |

Members are invited, approved and removed in the web UI or with `whatsapp-bridge members`.
Every member sees all ticked chats; per-member scoping is not built yet.
