# Self-hosting whatsapp-bridge

whatsapp-bridge runs on your own server as one process behind [Caddy](https://caddyserver.com),
which gets and renews the HTTPS certificate. Pick one of three paths: a fresh VPS
configured on first boot (A), Docker Compose (B), or the installer on a server you
already have (C). All three end at the same place: `https://<your domain>` with the web
UI, and `https://<your domain>/mcp` for your AI assistant.

## What you need

- **A server** with a public IPv4 address: Ubuntu 24.04 or Debian 12 for paths A and C,
  any Linux with Docker Engine and the Compose plugin for path B. amd64 or arm64.
- **A domain or subdomain**, for example `bridge.yourdomain.com`, with a DNS `A` record
  (and `AAAA` if the server has IPv6) pointing at the server. Create it before you
  install: Caddy requests the certificate when it first starts.
- **Ports 80 and 443** reachable from the internet. Caddy needs 80 for the certificate
  check and serves the bridge on 443.
- **A phone with WhatsApp.** The bridge becomes one of its linked devices. It reads; it
  sends nothing unless you set `BRIDGE_ALLOW_SEND=1`.

## Path A: one-click VPS with cloud-init

1. Download [`cloud-init.yaml`](cloud-init.yaml) and edit the two values marked `EDIT`:
   your domain and your SSH public key.
2. Create the DNS record for the domain.
3. Create an Ubuntu 24.04 server at Hetzner, DigitalOcean, Vultr, Linode or any
   provider that accepts user data. Paste the file into the
   field called "user data" or "cloud-init".
4. Boot it. First boot hardens the box (an `admin` user with your key, key-only SSH, no
   root login, unattended security updates, a firewall open on 22, 80 and 443), installs
   Caddy, then runs the installer from path C. Follow it with:
   ```sh
   ssh admin@<server> sudo tail -f /var/log/cloud-init-output.log
   ```
   When it prints "Next steps", open `https://<your domain>`.

## Path B: Docker Compose

In an empty folder on the server:

```sh
b=https://raw.githubusercontent.com/vid-t/whatsapp-bridge/main/deploy
curl -fsSL -O "$b/docker-compose.yml" -O "$b/Caddyfile.compose" -o whatsapp-bridge-env "$b/whatsapp-bridge-env.example"
```

Then three commands. Use your domain in the first:

```sh
sed -i 's/bridge.example.com/bridge.yourdomain.com/' whatsapp-bridge-env
docker run --rm ghcr.io/vid-t/whatsapp-bridge keygen >> whatsapp-bridge-env && chmod 600 whatsapp-bridge-env
docker compose up -d
```

`whatsapp-bridge-env` now holds your domain, the access code and the database key. Both
containers read it; the bridge is reachable only through Caddy. Logs:
`docker compose logs -f whatsapp-bridge`.

## Path C: a server you already have

On Ubuntu 24.04 or Debian 12, as a user with sudo:

```sh
curl -fsSL https://raw.githubusercontent.com/vid-t/whatsapp-bridge/main/deploy/get-whatsapp-bridge.sh | sudo DOMAIN=bridge.yourdomain.com bash
```

To read the script first, download it with `curl -fsSLO <same URL>`, then run
`sudo DOMAIN=bridge.yourdomain.com bash get-whatsapp-bridge.sh`. Add `VERSION=v1.2.3` to pin
a release.

The installer:

- downloads the latest release for your CPU and checks it against the release's
  `checksums.txt`, then installs `/usr/local/bin/whatsapp-bridge`;
- creates the `whatsapp-bridge` system user and `/var/lib/whatsapp-bridge` (mode 0700);
- on the first install, generates an access code and a database key into
  `/etc/whatsapp-bridge/whatsapp-bridge-env` (mode 0600). It never overwrites that file;
- installs a hardened `whatsapp-bridge.service` that listens on `127.0.0.1:8080` only;
- installs Caddy from its official apt repository if it is missing, and adds a site for
  your domain to `/etc/caddy/Caddyfile` (a stock welcome page is replaced; anything else
  is kept, and the old file is saved beside it);
- waits for `/healthz` and prints the next steps.

Port 8080 on the server must be free. Logs: `journalctl -u whatsapp-bridge -f`.

## First use

1. Open `https://<your domain>` and sign in with the access code. Read it with:
   - paths A and C: `sudo grep '^BRIDGE_ACCESS_CODE=' /etc/whatsapp-bridge/whatsapp-bridge-env`
   - path B: `grep '^BRIDGE_ACCESS_CODE=' whatsapp-bridge-env`
2. **Pair WhatsApp.** The web UI shows a QR code. On your phone open WhatsApp >
   Settings > Linked devices > Link a device, and scan it. The bridge appears there as
   "whatsapp-bridge" (`BRIDGE_DEVICE_NAME`).
3. **Tick the chats** the bridge may share. Chats start unticked, and only ticked chats are
   stored and reach your assistant. Tick them within an hour of pairing: until then the
   bridge holds the history WhatsApp sent at pairing in memory, and a ticked chat keeps it.
4. **Connect your AI.** Add `https://<your domain>/mcp` as a connector in Claude,
   ChatGPT, Codex or Claude Code: [docs/MCP-SETUP.md](../docs/MCP-SETUP.md).

WhatsApp logs out linked devices when the phone stays offline for 14 days. Open
WhatsApp on the phone now and then.

## Optional settings

Add these to the settings file (`/etc/whatsapp-bridge/whatsapp-bridge-env` or `whatsapp-bridge-env`),
then restart the bridge (`sudo systemctl restart whatsapp-bridge` or `docker compose up -d`):

- `BRIDGE_NAME`: the name shown in the web UI, sign-in pages, emails and to assistants.
- **Email sign-in** for other people: `BRIDGE_SMTP_HOST`, `BRIDGE_SMTP_PORT`,
  `BRIDGE_SMTP_USER`, `BRIDGE_SMTP_PASSWORD`, `BRIDGE_MAIL_FROM` and `BRIDGE_APPROVERS`
  (who approves access requests).
- **Google or Microsoft sign-in:** `BRIDGE_GOOGLE_CLIENT_ID` and
  `BRIDGE_GOOGLE_CLIENT_SECRET`, or the `BRIDGE_MICROSOFT_*` pair. Register
  `https://<your domain>/auth/google/callback` (or `/auth/microsoft/callback`) as the
  redirect URI with the provider.

[`whatsapp-bridge-env.example`](whatsapp-bridge-env.example) lists them with examples, and
[`docs/CONFIGURATION.md`](../docs/CONFIGURATION.md) describes every setting.

## Upgrade

- **Paths A and C:** run the installer command again. `DOMAIN` is optional on an
  upgrade. Settings and data stay; the previous binary is kept as
  `/usr/local/bin/whatsapp-bridge.prev`, and the installer prints the rollback command.
- **Path B:** `docker compose pull && docker compose up -d`.

## Backup

Back up two things:

- **The data:** `/var/lib/whatsapp-bridge` (paths A and C) or the `whatsapp-bridge_whatsapp-bridge-data`
  volume (path B). It holds the linked-device session, the messages and the settings.
- **The settings file:** `/etc/whatsapp-bridge/whatsapp-bridge-env` or `whatsapp-bridge-env`.
  It holds `BRIDGE_DB_KEY`. The database is encrypted with it: **losing
  `BRIDGE_DB_KEY` makes the data unreadable**, and anyone with both the key and the
  data can read it. Keep the key in a password manager, apart from the data copies.

Stop the bridge while you copy, so the SQLite file and its write-ahead log match. The
encryption also means the stock `sqlite3` tool and its `.backup` command cannot open the
file; copy it as a whole.

```sh
# Paths A and C
sudo systemctl stop whatsapp-bridge
sudo tar -czf whatsapp-bridge-data-$(date +%F).tar.gz -C /var/lib whatsapp-bridge
sudo systemctl start whatsapp-bridge

# Path B
docker compose stop whatsapp-bridge
docker run --rm -v whatsapp-bridge_whatsapp-bridge-data:/data:ro -v "$PWD":/backup alpine \
  tar -czf /backup/whatsapp-bridge-data-$(date +%F).tar.gz -C /data .
docker compose start whatsapp-bridge
```

To restore, install as usual, stop the bridge, put the data back (owned by the
`whatsapp-bridge` user on paths A and C) together with the same settings file, and start
it. Never run two copies of the same data at once: both would use the same linked
device.

## Uninstall

First unlink the bridge on your phone: WhatsApp > Settings > Linked devices >
whatsapp-bridge > Log out.

```sh
# Paths A and C. The last two lines delete the data and the key for good.
sudo systemctl disable --now whatsapp-bridge
sudo rm -f /etc/systemd/system/whatsapp-bridge.service /usr/local/bin/whatsapp-bridge /usr/local/bin/whatsapp-bridge.prev
sudo systemctl daemon-reload
sudo rm -rf /var/lib/whatsapp-bridge /etc/whatsapp-bridge
sudo userdel whatsapp-bridge
```

Then remove your domain's block from `/etc/caddy/Caddyfile` and run
`sudo systemctl reload caddy`, or remove Caddy with `sudo apt remove caddy`.

Path B: `docker compose down -v` removes the containers, the data and the certificates.

## When something is wrong

- **No certificate, or the browser cannot connect:** check that DNS points at the server
  (`dig +short <your domain>`) and that ports 80 and 443 are open, then read the Caddy
  log: `journalctl -u caddy` or `docker compose logs caddy`.
- **`unexpected Host header`:** `BRIDGE_ALLOWED_HOSTS` must be exactly your domain.
  Restart the bridge after changing it.
- **The bridge does not start:** read its log. Off loopback it refuses to start without
  both `BRIDGE_ACCESS_CODE` and `BRIDGE_DB_KEY`.
