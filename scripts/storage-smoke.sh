#!/usr/bin/env bash
# Deploys, redeploys, uploads, restarts and deletes against a running eze binary using the
# given EZE_STORAGE, checking each step. Pass "" to test local disk.
#
#   scripts/storage-smoke.sh ./eze 's3://eze?endpoint=http://127.0.0.1:9000&region=us-east-1&use_path_style=true'
set -euo pipefail

EZE=$(cd "$(dirname "$1")" && pwd)/$(basename "$1")
STORAGE=$2
PORT=${PORT:-18080}
WORK=$(mktemp -d)
PID=

start() {
  EZE_DOMAIN=localhost:$PORT EZE_SCHEME=http EZE_AUTH=dev EZE_ADDR=127.0.0.1:$PORT \
    EZE_DATA_DIR=$WORK/data EZE_STORAGE=$STORAGE "$EZE" serve >>"$WORK/log" 2>&1 &
  PID=$!
  for _ in $(seq 1 50); do
    curl -fsS "http://127.0.0.1:$PORT/healthz" >/dev/null 2>&1 && return
    sleep 0.2
  done
  cat "$WORK/log"
  exit 1
}
stop() { kill "$PID"; wait "$PID" 2>/dev/null || true; }
trap 'kill $PID 2>/dev/null || true; rm -rf "$WORK"' EXIT

expect() { # name, got, want
  if [ "$2" != "$3" ]; then
    echo "FAIL $1: got '$2', want '$3'"
    cat "$WORK/log"
    exit 1
  fi
  echo "ok   $1"
}
site() { curl -sS -H "Host: blog.localhost:$PORT" "$@"; }
deploy() {
  rm -rf "$WORK/src" && mkdir -p "$WORK/src/docs"
  echo "$1" >"$WORK/src/index.html"
  echo docs >"$WORK/src/docs/index.html"
  if [ -n "${2:-}" ]; then echo old >"$WORK/src/old.html"; fi
  tar -czf "$WORK/site.tgz" -C "$WORK/src" .
  curl -sS -o /dev/null -w '%{http_code}' -X PUT -H "Origin: http://localhost:$PORT" \
    --data-binary @"$WORK/site.tgz" "http://localhost:$PORT/_eze/sites/blog"
}

start
expect "deploy v1" "$(deploy v1 with-old-file)" 200
expect "serve index" "$(site "http://127.0.0.1:$PORT/")" v1
expect "folder redirect" "$(site -o /dev/null -w '%{http_code}' "http://127.0.0.1:$PORT/docs")" 301
expect "folder index" "$(site "http://127.0.0.1:$PORT/docs/")" docs
expect "deploy v2" "$(deploy v2)" 200
expect "redeploy replaces" "$(site "http://127.0.0.1:$PORT/")" v2
expect "old file gone" "$(site -o /dev/null -w '%{http_code}' "http://127.0.0.1:$PORT/old.html")" 404

head -c 200000 /dev/urandom >"$WORK/clip.mp4"
URL=$(site -X POST -H "Origin: http://blog.localhost:$PORT" -F "file=@$WORK/clip.mp4;type=video/mp4" \
  "http://127.0.0.1:$PORT/_eze/files" | sed -E 's/.*"url":"([^"]+)".*/\1/')
site -o "$WORK/got" "http://127.0.0.1:$PORT$URL"
expect "upload round trip" "$(cmp -s "$WORK/got" "$WORK/clip.mp4" && echo same)" same
CODE=$(site -o "$WORK/part" -w '%{http_code}' -H 'Range: bytes=1000-1099' "http://127.0.0.1:$PORT$URL")
expect "range status" "$CODE" 206
expect "range bytes" "$(cmp -s "$WORK/part" <(tail -c +1001 "$WORK/clip.mp4" | head -c 100) && echo same)" same

stop
start
expect "site after restart" "$(site "http://127.0.0.1:$PORT/")" v2
expect "upload after restart" "$(site -o /dev/null -w '%{http_code}' "http://127.0.0.1:$PORT$URL")" 200
expect "delete" "$(curl -sS -o /dev/null -w '%{http_code}' -X DELETE -H "Origin: http://localhost:$PORT" "http://localhost:$PORT/_eze/sites/blog")" 204
expect "gone after delete" "$(site -o /dev/null -w '%{http_code}' "http://127.0.0.1:$PORT/")" 404
if [ -n "$STORAGE" ]; then
  expect "nothing on local disk" "$(ls "$WORK/data/sites" "$WORK/data/uploads" 2>/dev/null | wc -l | tr -d ' ')" 0
fi
