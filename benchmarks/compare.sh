#!/usr/bin/env bash
# Measures what the access guard adds: the same agentload traffic against the
# unguarded baseline (GUARD=off) and the guarded service, repeated REPS times
# (default 5), alternating which side runs first, each run on a fresh DB.
# Prints the median and range of the guarded-minus-baseline delta.
#
#   REPS=7 benchmarks/compare.sh [agentload flags...]
#
# Defaults use REST and no denied flow so both sides see identical requests.
# Run from anywhere; everything is local and temp files are removed on exit.
set -euo pipefail

reps=${REPS:-5}
root=$(cd "$(dirname "$0")/.." && pwd)
tmp=$(mktemp -d)
pid=""
cleanup() { [ -n "$pid" ] && kill "$pid" 2>/dev/null || true; rm -rf "$tmp"; }
trap cleanup EXIT

cd "$root"
go build -o "$tmp/service" ./examples/wallet-service
go build -o "$tmp/agentload" ./benchmarks/agentload

args=(-api rest -mix topup=4,spend=3,hold=1,browse=4 -seed 1 "$@")

# run_side <base|guarded> <rep>: fresh service and DB, one agentload run.
run_side() {
  local name=$1 i=$2 port=18081 guard=off baseline=(-baseline)
  if [ "$name" = guarded ]; then port=18082 guard=on baseline=(); fi
  PORT=$port DB_PATH="$tmp/$name-$i.db" GUARD=$guard "$tmp/service" >"$tmp/$name-$i.svc.log" 2>&1 &
  pid=$!
  disown
  for _ in $(seq 50); do curl -sf "localhost:$port/health" >/dev/null && break; sleep 0.1; done
  if ! "$tmp/agentload" -url "http://localhost:$port" ${baseline[@]+"${baseline[@]}"} -save "$tmp/$name-$i.json" "${args[@]}" >"$tmp/$name-$i.log" 2>&1; then
    echo "$name run $i failed:"; cat "$tmp/$name-$i.log"; exit 1
  fi
  kill "$pid" 2>/dev/null || true
  pid=""
  printf '  rep %s %-8s %s\n' "$i" "$name" "$(sed -n '2p' "$tmp/$name-$i.log")"
}

for i in $(seq "$reps"); do
  if [ $((i % 2)) -eq 1 ]; then run_side base "$i"; run_side guarded "$i"
  else run_side guarded "$i"; run_side base "$i"; fi
done
echo
"$tmp/agentload" -aggregate "$tmp"
