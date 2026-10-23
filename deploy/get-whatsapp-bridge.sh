#!/usr/bin/env bash
# Installs or upgrades whatsapp-bridge on Ubuntu 24.04 or Debian 12, behind Caddy
# (automatic HTTPS). Run it as root on the server:
#
#   curl -fsSL https://raw.githubusercontent.com/vid-t/whatsapp-bridge/main/deploy/get-whatsapp-bridge.sh | DOMAIN=bridge.example.com sudo -E bash
#
# DOMAIN   the public hostname. Its DNS record must point at this server so
#          Caddy can get a certificate. Required on the first install; on an
#          upgrade, leave it out to keep the Caddy setup as it is.
# VERSION  a release tag such as v0.2.0 (default: the latest release).
#
# Re-running it upgrades the binary and the systemd unit. It never overwrites
# /etc/whatsapp-bridge/whatsapp-bridge-env, which holds the access code and the
# database key. Guide: deploy/SELF-HOSTING.md
set -euo pipefail

REPO="vid-t/whatsapp-bridge"
BIN=/usr/local/bin/whatsapp-bridge
DATA_DIR=/var/lib/whatsapp-bridge
CONF_DIR=/etc/whatsapp-bridge
ENV_FILE=$CONF_DIR/whatsapp-bridge-env
UNIT=/etc/systemd/system/whatsapp-bridge.service
CADDYFILE=/etc/caddy/Caddyfile
DOCS="https://github.com/$REPO/blob/main/docs/MCP-SETUP.md"
TMP_DIR=""
trap 'rm -rf "$TMP_DIR"' EXIT

say() { printf '==> %s\n' "$*"; }
die() { printf 'get-whatsapp-bridge: %s\n' "$*" >&2; exit 1; }

APT_UPDATED=0
apt_install() {
  export DEBIAN_FRONTEND=noninteractive
  if [ "$APT_UPDATED" -eq 0 ]; then
    apt-get -o DPkg::Lock::Timeout=300 update -q
    APT_UPDATED=1
  fi
  apt-get -o DPkg::Lock::Timeout=300 install -y -q --no-install-recommends "$@"
}

install_caddy() {
  say "Installing Caddy from its official apt repository"
  apt_install debian-keyring debian-archive-keyring apt-transport-https curl gnupg
  curl -1sLf 'https://dl.cloudsmith.io/public/caddy/stable/gpg.key' |
    gpg --batch --yes --dearmor -o /usr/share/keyrings/caddy-stable-archive-keyring.gpg
  curl -1sLf 'https://dl.cloudsmith.io/public/caddy/stable/debian.deb.txt' > /etc/apt/sources.list.d/caddy-stable.list
  chmod o+r /usr/share/keyrings/caddy-stable-archive-keyring.gpg /etc/apt/sources.list.d/caddy-stable.list
  APT_UPDATED=0
  apt_install caddy
}

# True when a site address in the Caddyfile is exactly $DOMAIN.
caddy_has_domain() {
  awk -v d="$DOMAIN" '
    /^[[:space:]]*#/ { next }
    { for (i = 1; i <= NF; i++) { t = $i; sub(/^https?:\/\//, "", t); sub(/[,{]$/, "", t); sub(/:443$/, "", t); if (t == d) found = 1 } }
    END { exit !found }' "$CADDYFILE"
}

# True when the Caddyfile is the package's welcome page: one site, ":80".
caddy_is_stock() {
  awk '/^[^#[:space:]].*\{[[:space:]]*$/ { n++; if ($1 != ":80") other = 1 } END { exit !(n == 1 && !other) }' "$CADDYFILE"
}

configure_caddy() {
  local upstream=$1 backup=""
  if [ -f "$CADDYFILE" ] && caddy_has_domain; then
    say "$CADDYFILE already serves $DOMAIN; left as it is"
    return
  fi
  if [ -f "$CADDYFILE" ]; then
    backup="$CADDYFILE.bak.$(date +%Y%m%d%H%M%S)"
    cp -p "$CADDYFILE" "$backup"
  fi
  local block
  block=$(cat <<EOF
# whatsapp-bridge (written by get-whatsapp-bridge.sh). Caddy obtains and renews the
# certificate and forwards the original Host header, which the bridge checks
# against BRIDGE_ALLOWED_HOSTS.
$DOMAIN {
	encode zstd gzip
	header {
		Strict-Transport-Security "max-age=31536000"
		-Server
	}
	request_body {
		max_size 2MB
	}
	reverse_proxy $upstream {
		# Long-lived server-sent events: do not buffer, do not time out early.
		flush_interval -1
		transport http {
			read_timeout 0
		}
	}
}
EOF
)
  install -d -m 0755 /etc/caddy
  if [ ! -f "$CADDYFILE" ] || caddy_is_stock; then
    printf '%s\n' "$block" > "$CADDYFILE"
  else
    printf '\n%s\n' "$block" >> "$CADDYFILE"
  fi
  if ! caddy validate --config "$CADDYFILE" --adapter caddyfile >/dev/null 2>&1; then
    [ -n "$backup" ] && cp -p "$backup" "$CADDYFILE"
    die "the new $CADDYFILE does not validate; restored the previous one. Check with: caddy validate --config $CADDYFILE"
  fi
  systemctl enable --quiet caddy
  systemctl reload caddy 2>/dev/null || systemctl restart caddy
  say "Caddy serves https://$DOMAIN${backup:+ (previous Caddyfile saved as $backup)}"
}

write_unit() {
  cat > "$UNIT" <<'EOF'
[Unit]
Description=whatsapp-bridge (read-only WhatsApp linked device)
Documentation=https://github.com/vid-t/whatsapp-bridge
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
User=whatsapp-bridge
Group=whatsapp-bridge
EnvironmentFile=/etc/whatsapp-bridge/whatsapp-bridge-env
ExecStart=/usr/local/bin/whatsapp-bridge serve
Restart=always
RestartSec=5
TimeoutStopSec=20
UMask=0077

# Hardening: the process may only write its data directory. MemoryDenyWriteExecute
# stays off because the wasm SQLite engine compiles to native code.
NoNewPrivileges=true
PrivateTmp=true
ProtectSystem=strict
ProtectHome=true
ReadWritePaths=/var/lib/whatsapp-bridge
ProtectKernelTunables=true
ProtectKernelModules=true
ProtectControlGroups=true
RestrictSUIDSGID=true
RestrictRealtime=true
LockPersonality=true
CapabilityBoundingSet=
AmbientCapabilities=
SystemCallArchitectures=native
MemoryMax=1500M
LimitNOFILE=65536

[Install]
WantedBy=multi-user.target
EOF
  chmod 0644 "$UNIT"
}

main() {
  local domain="${DOMAIN:-}" version="${VERSION:-}" first=1 arch tag ver asset url tmp addr keys ok hosts
  [ "$(id -u)" -eq 0 ] || die "run as root: curl -fsSL <this script's URL> | DOMAIN=bridge.example.com sudo -E bash"
  command -v systemctl >/dev/null 2>&1 || die "systemd is required"
  command -v apt-get >/dev/null 2>&1 || die "this installer supports Ubuntu 24.04 and Debian 12 (apt-get not found)"
  [ -f "$ENV_FILE" ] && first=0

  domain=$(printf '%s' "$domain" | tr '[:upper:]' '[:lower:]')
  if [ -n "$domain" ]; then
    [[ "$domain" =~ ^[a-z0-9]([a-z0-9-]*[a-z0-9])?(\.[a-z0-9]([a-z0-9-]*[a-z0-9])?)+$ ]] ||
      die "DOMAIN must be a hostname such as bridge.example.com, got: $domain"
    case "$domain" in
      example.com | *.example.com) die "replace the example DOMAIN with your own hostname" ;;
    esac
  elif [ "$first" -eq 1 ]; then
    die "set DOMAIN to the hostname whose DNS points at this server, e.g. DOMAIN=bridge.example.com"
  fi
  DOMAIN=$domain

  case "$(uname -m)" in
    x86_64 | amd64) arch=amd64 ;;
    aarch64 | arm64) arch=arm64 ;;
    *) die "unsupported CPU architecture $(uname -m); releases exist for amd64 and arm64" ;;
  esac

  if [ "$first" -eq 1 ] && command -v ss >/dev/null 2>&1 && [ -n "$(ss -Hltn 'sport = :8080')" ]; then
    die "another program already listens on port 8080; stop it first (whatsapp-bridge listens on 127.0.0.1:8080)"
  fi

  local need=()
  command -v curl >/dev/null 2>&1 || need+=(curl)
  command -v tar >/dev/null 2>&1 || need+=(tar)
  [ -f /etc/ssl/certs/ca-certificates.crt ] || need+=(ca-certificates)
  if [ "${#need[@]}" -gt 0 ]; then
    say "Installing ${need[*]}"
    apt_install "${need[@]}"
  fi

  if [ -z "$version" ]; then
    say "Looking up the latest release of $REPO"
    version=$(curl -fsSL -H 'Accept: application/vnd.github+json' "https://api.github.com/repos/$REPO/releases/latest" |
      sed -n 's/^[[:space:]]*"tag_name":[[:space:]]*"\([^"]*\)".*/\1/p' | head -n 1) || true
    [ -n "$version" ] || die "could not read the latest release from the GitHub API; set VERSION=vX.Y.Z and retry"
  fi
  tag="v${version#v}"
  ver="${tag#v}"
  [[ "$ver" =~ ^[0-9]+\.[0-9]+\.[0-9]+([-+][0-9A-Za-z.-]+)?$ ]] || die "VERSION must look like v1.2.3, got: $version"

  asset="whatsapp-bridge_${ver}_linux_${arch}.tar.gz"
  url="https://github.com/$REPO/releases/download/$tag"
  TMP_DIR=$(mktemp -d)
  tmp=$TMP_DIR
  say "Downloading $asset ($tag)"
  curl -fsSL --retry 3 -o "$tmp/$asset" "$url/$asset" || die "download failed: $url/$asset"
  curl -fsSL --retry 3 -o "$tmp/checksums.txt" "$url/checksums.txt" || die "download failed: $url/checksums.txt"
  awk -v f="$asset" '$2 == f || $2 == "*" f' "$tmp/checksums.txt" > "$tmp/expected.sha256"
  [ -s "$tmp/expected.sha256" ] || die "$asset is not listed in checksums.txt"
  (cd "$tmp" && sha256sum --check --status expected.sha256) || die "checksum mismatch for $asset; not installed"
  say "Checksum verified"
  tar -xzf "$tmp/$asset" -C "$tmp" whatsapp-bridge || die "no whatsapp-bridge binary in $asset"

  id -u whatsapp-bridge >/dev/null 2>&1 ||
    useradd --system --user-group --home-dir "$DATA_DIR" --no-create-home --shell /usr/sbin/nologin whatsapp-bridge
  install -d -o whatsapp-bridge -g whatsapp-bridge -m 0700 "$DATA_DIR"
  install -d -m 0755 "$CONF_DIR"
  # Keep the previous binary for a one-command rollback.
  [ -f "$BIN" ] && cp -p "$BIN" "$BIN.prev"
  install -m 0755 "$tmp/whatsapp-bridge" "$BIN"
  say "Installed $BIN ($tag)"

  if [ "$first" -eq 1 ]; then
    keys=$("$BIN" keygen)
    case "$keys" in
      *BRIDGE_DB_KEY=?*BRIDGE_ACCESS_CODE=?*) ;;
      *) die "whatsapp-bridge keygen did not print a key and an access code" ;;
    esac
    (
      umask 077
      cat > "$ENV_FILE.tmp" <<EOF
# whatsapp-bridge settings, read by whatsapp-bridge.service. Keep this file secret and
# back it up: BRIDGE_DB_KEY decrypts the database (without it the data cannot be
# read) and BRIDGE_ACCESS_CODE opens the web UI. All settings: README.md.
BRIDGE_ADDR=127.0.0.1:8080
BRIDGE_DATA_DIR=$DATA_DIR
BRIDGE_ALLOWED_HOSTS=$DOMAIN
BRIDGE_PUBLIC_URL=https://$DOMAIN
BRIDGE_DEVICE_NAME=whatsapp-bridge
BRIDGE_NAME=whatsapp-bridge
BRIDGE_LOG_LEVEL=info
$keys
EOF
    )
    chown root:root "$ENV_FILE.tmp"
    chmod 0600 "$ENV_FILE.tmp"
    mv "$ENV_FILE.tmp" "$ENV_FILE"
    say "Wrote $ENV_FILE with a fresh access code and database key"
  else
    say "Kept $ENV_FILE"
    hosts=$(sed -n 's/^BRIDGE_ALLOWED_HOSTS=//p' "$ENV_FILE" | tail -n 1)
    if [ -n "$DOMAIN" ] && ! printf '%s\n' "$hosts" | tr ',' '\n' | tr -d ' ' | grep -qxF "$DOMAIN"; then
      say "Note: BRIDGE_ALLOWED_HOSTS in $ENV_FILE does not list $DOMAIN. Add it there (and set BRIDGE_PUBLIC_URL), then: sudo systemctl restart whatsapp-bridge"
    fi
  fi

  write_unit
  systemctl daemon-reload
  systemctl enable --quiet whatsapp-bridge
  systemctl restart whatsapp-bridge

  addr=$(sed -n 's/^BRIDGE_ADDR=//p' "$ENV_FILE" | tail -n 1)
  addr=${addr:-127.0.0.1:8080}
  say "Waiting for http://$addr/healthz"
  ok=0
  for _ in $(seq 1 60); do
    if curl -fsS -o /dev/null --max-time 2 "http://$addr/healthz" 2>/dev/null; then
      ok=1
      break
    fi
    sleep 1
  done
  if [ "$ok" -ne 1 ]; then
    journalctl -u whatsapp-bridge -n 30 --no-pager >&2 || true
    die "whatsapp-bridge did not answer on $addr; see: journalctl -u whatsapp-bridge"
  fi

  if [ -n "$DOMAIN" ]; then
    # Caddy needs 80 for the certificate check and 443 to serve.
    if command -v ufw >/dev/null 2>&1 && ufw status 2>/dev/null | grep -q '^Status: active'; then
      ufw allow 80/tcp >/dev/null
      ufw allow 443/tcp >/dev/null
      say "ufw: allowed 80/tcp and 443/tcp"
    fi
    command -v caddy >/dev/null 2>&1 || install_caddy
    configure_caddy "$addr"
  fi

  if [ "$first" -eq 0 ]; then
    cat <<EOF

whatsapp-bridge $tag is running. Settings and data were kept.
Roll back with: sudo install -m 0755 $BIN.prev $BIN && sudo systemctl restart whatsapp-bridge
EOF
    return
  fi
  cat <<EOF

whatsapp-bridge $tag is running behind Caddy.

Next steps
  1. Open https://$DOMAIN
     DNS for $DOMAIN must point at this server; Caddy keeps retrying the certificate until it does.
  2. Sign in with the access code. Read it on this server with:
       sudo grep '^BRIDGE_ACCESS_CODE=' $ENV_FILE
  3. Pair WhatsApp: in the web UI, scan the QR code from your phone
     (WhatsApp > Settings > Linked devices > Link a device), then tick the chats to share.
  4. Connect Claude or another assistant to https://$DOMAIN/mcp:
       $DOCS

Back up $DATA_DIR and $ENV_FILE. Without BRIDGE_DB_KEY the data cannot be read.
EOF
}

main "$@"
