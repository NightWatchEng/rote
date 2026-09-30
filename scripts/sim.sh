#!/usr/bin/env bash
# Start or stop the two MeridianCore simulators the demo uses:
#   pinecrest on 127.0.0.1:8080, lakeshore (same product, different labels) on 127.0.0.1:8081.
set -euo pipefail
cd "$(dirname "$0")/.."

case "${1:-}" in
start)
  [ -x bin/legacybank ] || go build -o bin/ ./cmd/legacybank
  for port in 8080 8081; do
    if lsof -nP -iTCP:$port -sTCP:LISTEN >/dev/null 2>&1; then
      echo "port $port is already in use (a simulator from an earlier run? try: $0 stop)" >&2
      exit 1
    fi
  done
  mkdir -p runs
  ./bin/legacybank --addr 127.0.0.1:8080 --variant pinecrest >runs/sim-pinecrest.log 2>&1 &
  echo $! >runs/sim-pinecrest.pid
  ./bin/legacybank --addr 127.0.0.1:8081 --variant lakeshore >runs/sim-lakeshore.log 2>&1 &
  echo $! >runs/sim-lakeshore.pid
  sleep 0.5
  echo "pinecrest: http://127.0.0.1:8080/   lakeshore: http://127.0.0.1:8081/   (sign on: TELLER01 / demo-teller-pass)"
  ;;
stop)
  for f in runs/sim-*.pid; do
    [ -f "$f" ] && kill "$(cat "$f")" 2>/dev/null || true
    rm -f "$f"
  done
  ;;
*)
  echo "usage: $0 start|stop" >&2
  exit 2
  ;;
esac
