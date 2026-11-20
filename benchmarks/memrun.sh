#!/usr/bin/env bash
# Memory growth of the guarded service under sustained agent load.
# History is unbounded in memory, so RSS should climb with the
# number of requests. Starts a fresh guarded service, runs agentload for
# DURATION (default 10m), samples the service's RSS every SAMPLE seconds
# (default 30), then prints the table.
#
#   DURATION=10m SAMPLE=30 benchmarks/memrun.sh [agentload flags...]
#
# Not part of `go test`: it takes as long as DURATION. Everything is local and
# temp files are removed on exit.
set -euo pipefail

duration=${DURATION:-10m}
sample=${SAMPLE:-30}
port=18083
root=$(cd "$(dirname "$0")/.." && pwd)
tmp=$(mktemp -d)
spid=""
lpid=""
cleanup() {
  [ -n "$lpid" ] && kill "$lpid" 2>/dev/null || true
  [ -n "$spid" ] && kill "$spid" 2>/dev/null || true
  rm -rf "$tmp"
}
trap cleanup EXIT

cd "$root"
go build -o "$tmp/service" ./examples/wallet-service
go build -o "$tmp/agentload" ./benchmarks/agentload

PORT=$port DB_PATH="$tmp/mem.db" GUARD=on "$tmp/service" >"$tmp/svc.log" 2>&1 &
spid=$!
disown
for _ in $(seq 50); do curl -sf "localhost:$port/health" >/dev/null && break; sleep 0.1; done

"$tmp/agentload" -url "http://localhost:$port" -duration "$duration" -agents 20 -think 50ms "$@" >"$tmp/load.log" 2>&1 &
lpid=$!

rss_mb() { ps -o rss= -p "$spid" | awk '{printf "%.1f", $1/1024}'; }

printf '%8s  %10s\n' "elapsed" "rss_mb"
printf '%8s  %10s\n' "0s" "$(rss_mb)"
start=$SECONDS
while kill -0 "$lpid" 2>/dev/null; do
  sleep "$sample"
  kill -0 "$lpid" 2>/dev/null || break
  printf '%8s  %10s\n' "$((SECONDS - start))s" "$(rss_mb)"
done
printf '%8s  %10s  (load finished)\n' "$((SECONDS - start))s" "$(rss_mb)"

echo
echo "agentload summary:"
sed -n '1,4p' "$tmp/load.log"
