#!/usr/bin/env bash
set -euo pipefail
cd "$(dirname "$0")"

TOKEN_FILE="${TOKEN_FILE:-./proxy.token}"
if [[ ! -f "$TOKEN_FILE" ]]; then
  umask 077
  openssl rand -hex 32 > "$TOKEN_FILE"
  chmod 600 "$TOKEN_FILE"
  echo "generated $TOKEN_FILE"
fi
chmod 600 "$TOKEN_FILE" 2>/dev/null || true

export SEARCH_TOKEN
SEARCH_TOKEN="$(tr -d '[:space:]' < "$TOKEN_FILE")"
export PROXY_LISTEN="${PROXY_LISTEN:-0.0.0.0:18780}"
export TUNNEL_LISTEN="${TUNNEL_LISTEN:-0.0.0.0:18781}"

if [[ ! -x ./search-proxy ]]; then
  echo "building search-proxy..."
  go build -o search-proxy ./cmd/proxy
fi

echo "starting search-proxy public=${PROXY_LISTEN} tunnel=${TUNNEL_LISTEN}"
exec ./search-proxy
