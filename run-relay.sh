#!/usr/bin/env bash
set -euo pipefail
cd "$(dirname "$0")"

TOKEN_FILE="${TOKEN_FILE:-./proxy.token}"
if [[ ! -f "$TOKEN_FILE" ]]; then
  echo "missing $TOKEN_FILE (copy it from the proxy host, or run run-proxy.sh once)" >&2
  exit 1
fi
export SEARCH_TOKEN
SEARCH_TOKEN="$(tr -d '[:space:]' < "$TOKEN_FILE")"
export SEARCH_BACKEND="${SEARCH_BACKEND:-http://127.0.0.1:18765}"
if [[ -z "${PROXY_TUNNEL:-}" ]]; then
  echo "set PROXY_TUNNEL to the public proxy tunnel (host:port, e.g. 203.0.113.10:18781)" >&2
  exit 1
fi

if [[ ! -x ./search-relay ]]; then
  echo "building search-relay..."
  go build -o search-relay ./cmd/relay
fi

echo "starting search-relay tunnel=${PROXY_TUNNEL} backend=${SEARCH_BACKEND}"
exec ./search-relay
