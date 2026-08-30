#!/usr/bin/env bash
# Deploy search-proxy to the public VPS. ssh asks for the root password once.
# Usage: ./deploy-proxy.sh
# Host override: SEARCH_PROXY_HOST=example.com ./deploy-proxy.sh
set -euo pipefail
ROOT="$(cd "$(dirname "$0")" && pwd)"
cd "$ROOT"

HOST="${SEARCH_PROXY_HOST:-hub.maclaw.top}"

if [[ ! -f "$ROOT/deploy-proxy-remote.sh" ]]; then
  echo "error: deploy-proxy-remote.sh missing next to this script" >&2
  exit 1
fi
if ! command -v go >/dev/null 2>&1; then
  echo "error: go not found on PATH" >&2
  exit 1
fi
if ! command -v ssh >/dev/null 2>&1; then
  echo "error: ssh not found on PATH" >&2
  exit 1
fi

echo "building linux/amd64 search-proxy..."
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -ldflags="-s -w" -o search-proxy ./cmd/proxy

elf_ok=0
if command -v python3 >/dev/null 2>&1; then
  python3 -c "import sys; sys.exit(0 if open('search-proxy','rb').read(4)==b'\x7fELF' else 1)" && elf_ok=1 || elf_ok=0
elif command -v od >/dev/null 2>&1; then
  hdr=$(od -An -tx1 -N4 search-proxy | tr -d ' \n')
  [[ "$hdr" == "7f454c46" ]] && elf_ok=1 || elf_ok=0
fi
if [[ "$elf_ok" -ne 1 ]]; then
  echo "error: search-proxy is not a Linux ELF (GOOS=linux GOARCH=amd64 failed?)" >&2
  exit 1
fi

if base64 -w0 /dev/null >/dev/null 2>&1; then
  SCRIPT_B64=$(base64 -w0 < "$ROOT/deploy-proxy-remote.sh")
else
  SCRIPT_B64=$(base64 < "$ROOT/deploy-proxy-remote.sh" | tr -d '\n')
fi

echo "deploying to root@${HOST} (ssh will prompt for root password once)..."
# Binary on stdin; shared remote installer as base64 in the same ssh (one password).
cat search-proxy | ssh -o StrictHostKeyChecking=accept-new "root@${HOST}" \
  "cat > /tmp/search-proxy.new && printf '%s\n' '${SCRIPT_B64}' | base64 -d | bash"
