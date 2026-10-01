#!/usr/bin/env bash
# Public-offline watchdog for search-relay only (never touches search-service/babeldoc).
set -euo pipefail
cd "$(dirname "$0")"

TOKEN_FILE="${TOKEN_FILE:-./proxy.token}"
PROXY_TUNNEL="${PROXY_TUNNEL:-hub.maclaw.top:18781}"
SEARCH_BACKEND="${SEARCH_BACKEND:-http://127.0.0.1:18765}"
PUBLIC_URL="${PUBLIC_URL:-https://papers.maclaw.top/}"
# Must hit the tunnel-backed catalog, not the heavy live /papers/api (~2MB) or
# a cacheable HTML shell that can 200 while the backend is unreachable.
PUBLIC_API_URL="${PUBLIC_API_URL:-https://papers.maclaw.top/papers/api/catalog?offset=0&limit=1}"
LOCAL_HEALTH="${LOCAL_HEALTH:-http://127.0.0.1:18765/health}"
RELAY_BIN="${RELAY_BIN:-./search-relay}"
LOG_FILE="${LOG_FILE:-/tmp/search-relay-new.log}"
PID_FILE="${PID_FILE:-/tmp/search-relay.pid}"
INTERVAL_SEC="${INTERVAL_SEC:-20}"
CURL_MAX_SEC="${CURL_MAX_SEC:-8}"

log() { printf '%s %s\n' "$(date -u '+%Y-%m-%dT%H:%M:%SZ')" "$*"; }

need_token() {
  if [[ ! -f "$TOKEN_FILE" ]]; then
    log "ERROR missing $TOKEN_FILE"
    return 1
  fi
  SEARCH_TOKEN="$(tr -d '[:space:]' < "$TOKEN_FILE")"
  export SEARCH_TOKEN PROXY_TUNNEL SEARCH_BACKEND
  if [[ -z "$SEARCH_TOKEN" ]]; then
    log "ERROR empty SEARCH_TOKEN"
    return 1
  fi
}

relay_pids() {
  pgrep -f '[.]/search-relay$' 2>/dev/null || pgrep -x search-relay 2>/dev/null || true
}

ensure_single_relay() {
  local pids
  pids="$(relay_pids | tr '\n' ' ' | xargs || true)"
  if [[ -z "${pids// }" ]]; then
    return 1
  fi
  local arr=($pids)
  if ((${#arr[@]} > 1)); then
    local keep="${arr[-1]}"
    log "WARN multiple relay pids ($pids); keeping $keep"
    for p in "${arr[@]}"; do
      if [[ "$p" != "$keep" ]]; then
        kill "$p" 2>/dev/null || true
      fi
    done
    echo "$keep" >"$PID_FILE"
    return 0
  fi
  echo "${arr[0]}" >"$PID_FILE"
  return 0
}

start_relay() {
  need_token || return 1
  if [[ ! -x "$RELAY_BIN" ]]; then
    log "ERROR missing executable $RELAY_BIN"
    return 1
  fi
  local p
  for p in $(relay_pids); do
    log "stopping old search-relay pid=$p"
    kill "$p" 2>/dev/null || true
  done
  sleep 1
  for p in $(relay_pids); do
    kill -9 "$p" 2>/dev/null || true
  done
  nohup env PROXY_TUNNEL="$PROXY_TUNNEL" SEARCH_BACKEND="$SEARCH_BACKEND" SEARCH_TOKEN="$SEARCH_TOKEN" \
    "$RELAY_BIN" >>"$LOG_FILE" 2>&1 &
  local newpid=$!
  echo "$newpid" >"$PID_FILE"
  sleep 2
  if kill -0 "$newpid" 2>/dev/null; then
    log "started search-relay pid=$newpid log=$LOG_FILE"
    return 0
  fi
  log "ERROR search-relay failed to stay up (pid=$newpid)"
  return 1
}

# Returns 0 when public path looks offline / not tunnel-backed.
public_offline() {
  local tmp body code bytes
  tmp="$(mktemp)"
  code="$(curl -sS -m "$CURL_MAX_SEC" -o "$tmp" -w '%{http_code}' "$PUBLIC_API_URL" || echo 000)"
  body="$(cat "$tmp" 2>/dev/null || true)"
  bytes="$(wc -c <"$tmp" | tr -d ' ')"
  rm -f "$tmp"

  if [[ "$code" != "200" ]]; then
    log "public catalog check fail http=$code"
    return 0
  fi
  if printf '%s' "$body" | grep -q '"code":"offline"'; then
    log "public catalog check offline JSON"
    return 0
  fi
  # Require a real catalog JSON slice (proves search-service behind the tunnel).
  if ! printf '%s' "$body" | grep -q '"papers"'; then
    log "public catalog check missing papers field (bytes=$bytes)"
    return 0
  fi
  if ! printf '%s' "$body" | grep -qE '"generated_at"|"snapshot_etag"|"count"'; then
    log "public catalog check missing catalog fields (bytes=$bytes)"
    return 0
  fi
  # Tiny/empty bodies are not a healthy first-page response.
  if [[ "${bytes:-0}" -lt 80 ]]; then
    log "public catalog check body too small bytes=$bytes"
    return 0
  fi
  return 1
}

check_local() {
  local code
  code="$(curl -sS -m 5 -o /dev/null -w '%{http_code}' "$LOCAL_HEALTH" || echo 000)"
  if [[ "$code" != "200" ]]; then
    log "local backend health down http=$code (not restarting babeldoc/search-service)"
  fi
}

once() {
  check_local
  if public_offline; then
    log "restarting search-relay due to public offline/bad catalog probe"
    start_relay || true
    # Give the tunnel a moment, then re-probe once (helps after remote wake).
    sleep 3
    if public_offline; then
      log "still offline after relay restart (remote may be waking; will retry next interval)"
    else
      log "public catalog probe OK after relay restart"
    fi
    return
  fi
  if ! ensure_single_relay; then
    log "no search-relay running; starting"
    start_relay || true
  fi
}

case "${1:-watch}" in
  once) once ;;
  start) start_relay ;;
  watch)
    log "watch-relay started interval=${INTERVAL_SEC}s probe=$PUBLIC_API_URL"
    while true; do
      once || true
      sleep "$INTERVAL_SEC"
    done
    ;;
  *)
    echo "usage: $0 [watch|once|start]" >&2
    exit 2
    ;;
esac
