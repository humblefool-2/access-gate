// SPDX-License-Identifier: Apache-2.0

package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"path/filepath"
)

// The benchmark runner: the UI picks a benchmark and a size, and the server
// maps that pair to a fixed command. No free-text value from the browser ever
// reaches a command line; every argument below comes from this table.

var sizes = []string{"quick", "standard", "thorough"}

type benchDef struct {
	ID    string
	Title string
	Desc  string
	Info  map[string]string // size -> what runs and roughly how long
	// build returns the command for a size.
	build func(p *pg, size string) (name string, args, env []string)
}

// goBench runs `go test -bench` and, when benchstat is installed, prints the
// baseline-vs-guarded comparison afterwards.
func goBench(pkg, pattern, benchtime string, count int, withStat bool) (string, []string, []string) {
	script := fmt.Sprintf(`set -o pipefail
export PATH="$PATH:$(go env GOPATH)/bin"
out=$(mktemp)
trap 'rm -f "$out"' EXIT
go test %s -run '^$' -bench '%s' -benchtime %s -count %d -benchmem -timeout 30m | tee "$out"
`, pkg, pattern, benchtime, count)
	if withStat {
		script += `echo
if command -v benchstat >/dev/null; then
  echo '--- benchstat (baseline vs guarded) ---'
  benchstat -col /mode "$out"
else
  echo '(benchstat not installed: go install golang.org/x/perf/cmd/benchstat@latest)'
fi
`
	}
	return "bash", []string{"-c", script}, nil
}

var catalog = []benchDef{
	{
		ID: "request", Title: "Request overhead",
		Desc: "Per-request cost of the guard: 3 routes x 3 roles, with and without the lib, no network.",
		Info: map[string]string{
			"quick":    "1 run each, short, about 3 s",
			"standard": "5 runs each, about 45 s",
			"thorough": "10 runs each (benchstat-ready), about 4 min",
		},
		build: func(_ *pg, size string) (string, []string, []string) {
			bt, n := map[string]string{"quick": "100ms", "standard": "300ms", "thorough": "1s"}[size], map[string]int{"quick": 1, "standard": 5, "thorough": 10}[size]
			return goBench("./examples/wallet-service", "BenchmarkRequest", bt, n, true)
		},
	},
	{
		ID: "store", Title: "Datastore latency",
		Desc: "Throughput when the datastore is slow. A stub with an injected delay stands in for any store (SQLite, Postgres, Kafka, OpenSearch); 1 vs 64 entities, reads and writes. Shows that writes to one entity serialise while writes to different entities run in parallel.",
		Info: map[string]string{
			"quick":    "delays 0, 5 and 100 ms, about 15 s",
			"standard": "delays 0, 1, 5, 20 and 100 ms, about 25 s",
			"thorough": "all 5 delays, 3 runs each, about 3 min",
		},
		build: func(_ *pg, size string) (string, []string, []string) {
			delays := map[string]string{"quick": "0s|5ms|100ms", "standard": "0s|1ms|5ms|20ms|100ms", "thorough": "0s|1ms|5ms|20ms|100ms"}[size]
			bt, n := map[string]string{"quick": "200ms", "standard": "300ms", "thorough": "1s"}[size], map[string]int{"quick": 1, "standard": 1, "thorough": 3}[size]
			return goBench("./examples/wallet-service", "BenchmarkStoreLatency/^op=.*$/^entities=.*$/^delay=("+delays+")$/^mode=.*$", bt, n, true)
		},
	},
	{
		ID: "compare", Title: "Compare Performance",
		Desc: "Paired runs of the same agent traffic with and without the lib, then the median latency difference.",
		Info: map[string]string{
			"quick":    "3 pairs of 5 s, about 30 s",
			"standard": "5 pairs of 10 s, about 100 s",
			"thorough": "7 pairs of 10 s (the headline run), about 2.5 min",
		},
		build: func(p *pg, size string) (string, []string, []string) {
			reps := map[string]string{"quick": "3", "standard": "5", "thorough": "7"}[size]
			dur := map[string]string{"quick": "5s", "standard": "10s", "thorough": "10s"}[size]
			script := filepath.Join(p.root, "benchmarks", "compare.sh")
			return script, []string{"-agents", "1", "-think", "0s", "-duration", dur, "-wallets", "0"}, []string{"REPS=" + reps}
		},
	},
}

func findBench(id string) *benchDef {
	for i := range catalog {
		if catalog[i].ID == id {
			return &catalog[i]
		}
	}
	return nil
}

// handleBenchOptions lists what the UI can offer.
func (p *pg) handleBenchOptions(w http.ResponseWriter, _ *http.Request) {
	type opt struct {
		ID    string            `json:"id"`
		Title string            `json:"title"`
		Desc  string            `json:"desc"`
		Info  map[string]string `json:"info"`
	}
	out := make([]opt, len(catalog))
	for i, b := range catalog {
		out[i] = opt{b.ID, b.Title, b.Desc, b.Info}
	}
	writeJSON(w, http.StatusOK, map[string]any{"sizes": sizes, "benchmarks": out})
}

// handleBenchRun runs one catalog entry and streams its output.
func (p *pg) handleBenchRun(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Bench string `json:"bench"`
		Size  string `json:"size"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		httpErr(w, http.StatusBadRequest, "invalid JSON")
		return
	}
	def := findBench(in.Bench)
	if def == nil {
		httpErr(w, http.StatusBadRequest, "unknown benchmark")
		return
	}
	if _, ok := def.Info[in.Size]; !ok {
		httpErr(w, http.StatusBadRequest, "size must be quick, standard or thorough")
		return
	}
	name, args, env := def.build(p, in.Size)
	p.stream(w, r, def.Title+" ("+in.Size+")", name, args, env)
}
