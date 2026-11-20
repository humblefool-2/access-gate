#!/usr/bin/env bash
# Runs the playground presets, the store-latency samples and the memory run, one after
# another, and saves everything under benchmarks/results/<date>/.
#
#   benchmarks/run-all.sh [presets|samples|memory|all]     (default: all)
#
#   presets  time all 9 playground presets (3 benchmarks x quick/standard/thorough)
#            with the same commands the playground runs; about 17 min
#   samples  BenchmarkStoreLatency at -count 10, so benchstat
#            can print confidence intervals; about 10 min
#   memory   make bench-memory: 10 min of agent load, RSS every 30 s; about 11 min
#
# Run it on an otherwise idle machine, plugged in, and do not build, test or run
# anything else meanwhile: the numbers are latency measurements. Keep the laptop
# awake with:  caffeinate -i benchmarks/run-all.sh
set -euo pipefail

what=${1:-all}
case "$what" in presets | samples | memory | all) ;; *)
  echo "usage: $0 [presets|samples|memory|all]" >&2
  exit 2
  ;;
esac

root=$(cd "$(dirname "$0")/.." && pwd)
cd "$root"
export PATH="$PATH:$(go env GOPATH)/bin"

out="benchmarks/results/$(date +%F)"
mkdir -p "$out/presets"
timings="$out/timings.txt"

{
  echo "# run-all.sh $what, started $(date '+%F %T')"
  echo "# $(uname -sm), $(sysctl -n machdep.cpu.brand_string 2>/dev/null || true), $(go version)"
} | tee -a "$timings"

# timed LABEL FILE COMMAND...: run, save output to FILE, append wall time to timings.txt
timed() {
  local label=$1 file=$2
  shift 2
  echo
  echo "=== $label ==="
  local start=$SECONDS
  "$@" 2>&1 | tee "$file"
  local took=$((SECONDS - start))
  printf '%-40s %4d s (%d min %02d s)\n' "$label" "$took" $((took / 60)) $((took % 60)) | tee -a "$timings"
}

# Compile everything first so build time is not part of any measurement.
go test -run '^$' -bench '^$' ./examples/wallet-service ./lib >/dev/null
go build -o /dev/null ./benchmarks/agentload

# The playground's commands (examples/wallet-service/playground/bench.go).
preset_request() { # size benchtime count
  go test ./examples/wallet-service -run '^$' -bench BenchmarkRequest -benchtime "$2" -count "$3" -benchmem -timeout 30m
}
preset_store() { # size benchtime count delays
  go test ./examples/wallet-service -run '^$' -bench "BenchmarkStoreLatency/^op=.*\$/^entities=.*\$/^delay=($4)\$/^mode=.*\$" -benchtime "$2" -count "$3" -benchmem -timeout 30m
}
preset_compare() { # size reps duration
  REPS=$2 benchmarks/compare.sh -agents 1 -think 0s -duration "$3" -wallets 0
}

run_presets() {
  timed "request quick"     "$out/presets/request-quick.txt"     preset_request quick 100ms 1
  timed "request standard"  "$out/presets/request-standard.txt"  preset_request standard 300ms 5
  timed "request thorough"  "$out/presets/request-thorough.txt"  preset_request thorough 1s 10
  timed "store quick"       "$out/presets/store-quick.txt"       preset_store quick 200ms 1 '0s|5ms|100ms'
  timed "store standard"    "$out/presets/store-standard.txt"    preset_store standard 300ms 1 '0s|1ms|5ms|20ms|100ms'
  timed "store thorough"    "$out/presets/store-thorough.txt"    preset_store thorough 1s 3 '0s|1ms|5ms|20ms|100ms'
  timed "compare quick"     "$out/presets/compare-quick.txt"     preset_compare quick 3 5s
  timed "compare standard"  "$out/presets/compare-standard.txt"  preset_compare standard 5 10s
  timed "compare thorough"  "$out/presets/compare-thorough.txt"  preset_compare thorough 7 10s
}

run_samples() {
  timed "store latency, -count 10" "$out/store-count10.txt" \
    go test ./examples/wallet-service -run '^$' -bench BenchmarkStoreLatency -benchmem -count 10 -timeout 60m
  if command -v benchstat >/dev/null; then
    benchstat -col /mode "$out/store-count10.txt" >"$out/store-count10.benchstat.txt" || true
  else
    echo "(benchstat not installed: go install golang.org/x/perf/cmd/benchstat@latest)" | tee -a "$timings"
  fi
}

run_memory() {
  DURATION=10m SAMPLE=30 timed "memory growth, 10 min" "$out/memory.txt" benchmarks/memrun.sh
}

case "$what" in
presets) run_presets ;;
samples) run_samples ;;
memory) run_memory ;;
all)
  run_presets
  run_samples
  run_memory
  ;;
esac

echo
echo "Done. Results in $out/, wall times in $timings"
