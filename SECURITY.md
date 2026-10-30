# Security policy

## Supported versions

Only the latest release of whatsapp-bridge gets security fixes. If you run an older build,
upgrade before you report, and say which version you run (`/api/me` shows it).

## Reporting a vulnerability

Please report privately. Do not open a public issue.

Use GitHub private vulnerability reporting on this repository: **Security > Report a
vulnerability** at <https://github.com/vid-t/whatsapp-bridge/security/advisories/new>.

Include what you found, how to reproduce it, the version, and what an attacker gains. Never
send real WhatsApp messages, access codes, database keys or API keys; redact them.

## In scope

- The `whatsapp-bridge` binary: pairing, message storage, encryption at rest, the HTTP API, the
  MCP endpoint, OAuth, email and Google or Microsoft sign-in, members and keys.
- The web UI embedded in the binary, and the server-rendered sign-in pages.
- The deploy files in `deploy/` (install script, cloud-init, Docker Compose and the Caddy
  configuration).

Out of scope: WhatsApp itself and whatsmeow (report those upstream), your own server, proxy
or AI provider, and the fact that WhatsApp may restrict accounts linked to unofficial
clients (see the README).

## What happens after you report

1. We confirm that we received the report.
2. We reproduce it and agree the severity with you.
3. We fix it, release a new version and publish an advisory on GitHub.
4. We credit you in the advisory, unless you prefer not to be named.

Please give us the chance to ship the fix before you disclose. We keep you informed at
each step.

## Security model

The README's Privacy and safety section lists the defaults: chats start unticked,
encryption at rest, the access code, loopback-only listening and read-only access to ticked
chats. With `BRIDGE_ALLOW_SEND=1`, assistants may also send text messages to ticked chats, and
to no other chat ([`docs/CONFIGURATION.md`](docs/CONFIGURATION.md#sending-messages)). Only
assistants approved on a page that said they could send, and keys created with sending,
may send; those approved or created before stay read-only. For an hour after a phone is linked, messages for unticked chats are held in the
bridge's memory, never in the database or a log, so that ticking a chat keeps its history.
[`docs/MCP-SETUP.md`](docs/MCP-SETUP.md) describes how assistants sign in.
