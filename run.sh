#!/usr/bin/env bash
set -euo pipefail
cd "$(dirname "$0")"

export DISPLAY="${DISPLAY:-:1}"
export HOME="${HOME:-/home/box}"

mkdir -p chrome-profile debug

need_build=0
if [[ ! -x ./search-service ]]; then
  need_build=1
else
  # Rebuild if any Go source is newer than the binary.
  if find . -name '*.go' -newer ./search-service | grep -q .; then
    need_build=1
  fi
fi

if [[ "$need_build" -eq 1 ]]; then
  echo "building search-service..."
  go build -o search-service .
fi

echo "starting search-service on 127.0.0.1:18765 (DISPLAY=${DISPLAY})"
exec ./search-service
