#!/bin/bash
# Remote installer for search-proxy. Invoked via deploy-proxy.sh / deploy-proxy.cmd
# through a single ssh session. Expects a linux amd64 binary at /tmp/search-proxy.new.
# Idempotent. Never prints token or env secret values.
set -euo pipefail

INSTALL_DIR=/opt/search-proxy
BIN="${INSTALL_DIR}/search-proxy"
TOKEN_FILE="${INSTALL_DIR}/proxy.token"
ENV_FILE="${INSTALL_DIR}/proxy.env"
UNIT=/etc/systemd/system/search-proxy.service
NEW=/tmp/search-proxy.new
HUB_DEFAULT='https://hub.mypapers.top,https://hub.maclaw.top'
SNIPPET=/etc/nginx/snippets/search-proxy.conf

if [[ ! -f "$NEW" || ! -s "$NEW" ]]; then
  echo "error: missing or empty $NEW" >&2
  exit 1
fi

mkdir -p "$INSTALL_DIR"

if [[ -f "$BIN" ]]; then
  ts=$(date +%Y%m%d%H%M%S)
  cp -a "$BIN" "${INSTALL_DIR}/search-proxy.bak.${ts}"
  echo "backed up existing binary to ${INSTALL_DIR}/search-proxy.bak.${ts}"
fi

install -m 755 "$NEW" "$BIN"
rm -f "$NEW" /tmp/deploy-proxy-remote.sh
echo "installed ${BIN}"

if [[ ! -f "$TOKEN_FILE" ]]; then
  umask 077
  openssl rand -hex 32 > "$TOKEN_FILE"
  chmod 600 "$TOKEN_FILE"
  echo "generated proxy.token"
else
  chmod 600 "$TOKEN_FILE"
  echo "keeping existing proxy.token"
fi

if [[ ! -f "$ENV_FILE" ]]; then
  umask 077
  token=$(tr -d '[:space:]' < "$TOKEN_FILE")
  cat > "$ENV_FILE" <<EOF
SEARCH_TOKEN=${token}
PROXY_LISTEN=127.0.0.1:18780
TUNNEL_LISTEN=0.0.0.0:18781
HUB_AUTH_BASES=${HUB_DEFAULT}
EOF
  chmod 600 "$ENV_FILE"
  echo "wrote proxy.env"
  unset token
else
  chmod 600 "$ENV_FILE"
  if grep -q '^HUB_AUTH_BASES=' "$ENV_FILE"; then
    echo "keeping existing proxy.env"
  else
    echo "HUB_AUTH_BASES=${HUB_DEFAULT}" >> "$ENV_FILE"
    echo "appended HUB_AUTH_BASES to existing proxy.env"
  fi
fi

if [[ ! -f "$UNIT" ]]; then
  cat > "$UNIT" <<'UNIT'
[Unit]
Description=RapidSearch public proxy
After=network.target
[Service]
Type=simple
WorkingDirectory=/opt/search-proxy
EnvironmentFile=/opt/search-proxy/proxy.env
ExecStart=/opt/search-proxy/search-proxy
Restart=always
RestartSec=2
[Install]
WantedBy=multi-user.target
UNIT
  echo "installed systemd unit ${UNIT}"
else
  echo "keeping existing systemd unit"
fi

systemctl daemon-reload
systemctl enable --now search-proxy.service
systemctl restart search-proxy.service

echo "status: $(systemctl is-active search-proxy.service)"
if command -v ss >/dev/null 2>&1; then
  ss -lnt | grep -E ':18780|:18781' || true
elif command -v netstat >/dev/null 2>&1; then
  netstat -lnt | grep -E ':18780|:18781' || true
fi

code=$(curl -s -o /dev/null -w '%{http_code}' http://127.0.0.1:18780/health || true)
echo "GET /health => HTTP ${code} (expect 401 without token)"

if [[ -f "$SNIPPET" ]]; then
  echo "nginx snippet already present; leaving nginx unchanged"
else
  mkdir -p /etc/nginx/snippets
  cat > "$SNIPPET" <<'NGX'
location /searchproxy/ {
    proxy_pass http://127.0.0.1:18780/;
    proxy_http_version 1.1;
    proxy_set_header Host $host;
    proxy_set_header Authorization $http_authorization;
    proxy_set_header X-Real-IP $remote_addr;
    proxy_set_header X-Forwarded-For $proxy_add_x_forwarded_for;
    proxy_set_header X-Forwarded-Proto $scheme;
    proxy_read_timeout 180s;
    proxy_send_timeout 180s;
}
NGX
  echo "wrote ${SNIPPET}"
  if grep -RqsE 'searchproxy|127\.0\.0\.1:18780' /etc/nginx --include='*.conf' 2>/dev/null; then
    echo "nginx already routes search-proxy; not adding include"
  else
    inserted=0
    while IFS= read -r -d '' f; do
      [[ -f "$f" ]] || continue
      if grep -q 'snippets/search-proxy.conf' "$f"; then
        continue
      fi
      if grep -qE 'server_name[^;]*hub\.maclaw\.top' "$f"; then
        cp -a "$f" "${f}.bak.searchproxy"
        sed -i '/server_name[^;]*hub\.maclaw\.top/a \    include /etc/nginx/snippets/search-proxy.conf;' "$f"
        inserted=1
      fi
    done < <(find /etc/nginx -name '*.conf' -print0 2>/dev/null)
    if [[ "$inserted" -eq 1 ]]; then
      if nginx -t; then
        systemctl reload nginx 2>/dev/null || service nginx reload || true
        echo "nginx reloaded with search-proxy snippet include"
        find /etc/nginx -name '*.bak.searchproxy' -delete
      else
        echo "nginx -t failed; reverting vhost edits"
        find /etc/nginx -name '*.bak.searchproxy' | while read -r b; do
          mv -f "$b" "${b%.bak.searchproxy}"
        done
      fi
    else
      echo "snippet installed; include it inside the HTTPS server block if needed:"
      echo "    include /etc/nginx/snippets/search-proxy.conf;"
    fi
  fi
fi
