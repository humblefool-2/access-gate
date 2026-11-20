// SPDX-License-Identifier: Apache-2.0

package main

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	accessgate "github.com/humblefool-2/access-gate/lib"
)

// Benchmark 1: per-request overhead of the guard.
//
// Same handlers, same SQLite, with and without the guard, driven through
// httptest (no network). Pair the runs with benchstat:
//
//	go test -run '^$' -bench BenchmarkRequest -benchmem -count 10 ./examples/wallet-service | tee out.txt
//	benchstat -col /mode out.txt
//
// Note: for POST credit, the read and read-pii principals are refused (403) by
// the service's own role check on both muxes (before the guard runs), so those
// two rows compare two refusals, not real writes. Do not quote their geomean.
func BenchmarkRequest(b *testing.B) {
	authn, err := tokenAuth("rk=read,pk=read-pii,wk=write")
	if err != nil {
		b.Fatal(err)
	}

	routes := []struct {
		name, method, path, body string
		// guardedWant is the status both muxes must return per token.
		guardedWant map[string]int
	}{
		{"customer", http.MethodGet, "/customers/cus_demo1", "", map[string]int{"rk": 200, "pk": 200, "wk": 200}},
		{"wallet", http.MethodGet, "/wallets/wal_demo1", "", map[string]int{"rk": 200, "pk": 200, "wk": 200}},
		{"credit", http.MethodPost, "/wallets/wal_demo1/credit", `{"amount_minor":1}`, map[string]int{"rk": 403, "pk": 403, "wk": 200}},
	}
	principals := []struct{ name, token string }{
		{"read", "rk"}, {"read-pii", "pk"}, {"write", "wk"},
	}
	modes := []string{"baseline", "guarded"}

	for _, rt := range routes {
		for _, p := range principals {
			for _, mode := range modes {
				b.Run("route="+rt.name+"/principal="+p.name+"/mode="+mode, func(b *testing.B) {
					db, err := openStore(":memory:")
					if err != nil {
						b.Fatal(err)
					}
					defer db.Close()
					if err := seedDemo(db); err != nil {
						b.Fatal(err)
					}

					var h http.Handler
					// Both muxes share the service's own role check, so the status is the same.
					want := rt.guardedWant[p.token]
					if mode == "guarded" {
						h = newMux(db, authn)
					} else {
						h = newBaselineMux(db, authn)
					}

					do := func() int {
						req := httptest.NewRequest(rt.method, rt.path, strings.NewReader(rt.body))
						req.Header.Set("Authorization", "Bearer "+p.token)
						req.Header.Set("X-Session-ID", "bench")
						rec := httptest.NewRecorder()
						h.ServeHTTP(rec, req)
						return rec.Code
					}

					// Warm up and verify the case does what its name says.
					if got := do(); got != want {
						b.Fatalf("status = %d, want %d", got, want)
					}

					b.ReportAllocs()
					b.ResetTimer()
					for i := 0; i < b.N; i++ {
						if got := do(); got != want {
							b.Fatalf("status = %d, want %d", got, want)
						}
					}
				})
			}
		}
	}
}

// Benchmark 7: what a slow backend does to throughput.
//
// The handler is a stub that sleeps for the given delay and returns a fixed
// wallet, standing in for any store (SQLite, Postgres, Kafka, OpenSearch). It
// runs behind the real guard and behind no guard. The guard holds the
// entity's write lock while the handler runs, so guarded writes to one entity
// cap at about 1 / delay while writes to different entities run in parallel,
// and baseline writes scale with entities. Reads are not locked.
//
//	go test -run '^$' -bench BenchmarkStoreLatency -benchmem .
//
// All workers share one session: with per-worker sessions a second writer
// would make the first one's next write a 409 stale read, and the benchmark
// would measure refusals. Compare ops/s across rows; ns/op is wall time per
// request across all workers.
func BenchmarkStoreLatency(b *testing.B) {
	authn, err := tokenAuth("wk=write")
	if err != nil {
		b.Fatal(err)
	}
	guard, err := accessgate.New(accessConfig(&app{}))
	if err != nil {
		b.Fatal(err)
	}

	for _, delay := range []time.Duration{0, time.Millisecond, 5 * time.Millisecond, 20 * time.Millisecond, 100 * time.Millisecond} {
		stub := func(w http.ResponseWriter, r *http.Request) {
			time.Sleep(delay)
			writeJSON(w, http.StatusOK, map[string]any{
				"wallet_id": r.PathValue("id"), "currency": "EUR", "status": "active",
				"balance_minor": 1000, "blocked_minor": 0, "available_minor": 1000, "holds": []any{},
			})
		}
		wrap := map[string]wrapFunc{
			"baseline": func(_ string, h http.HandlerFunc) http.HandlerFunc { return h },
			"guarded":  guard.Handle,
		}
		for _, mode := range []string{"baseline", "guarded"} {
			mux := http.NewServeMux()
			mux.HandleFunc("GET /wallets/{id}", wrap[mode]("wallets", stub))
			mux.HandleFunc("POST /wallets/{id}/credit", wrap[mode]("wallets", stub))
			h := accessgate.Authenticate(authn, mux)

			for _, op := range []string{"read", "write"} {
				for _, entities := range []int{1, 64} {
					name := fmt.Sprintf("op=%s/entities=%d/delay=%s/mode=%s", op, entities, delay, mode)
					b.Run(name, func(b *testing.B) {
						method, suffix := http.MethodGet, ""
						if op == "write" {
							method, suffix = http.MethodPost, "/credit"
						}
						var n atomic.Uint64
						b.SetParallelism(8) // 8 x GOMAXPROCS workers, so the lock is contended
						b.ReportAllocs()
						b.ResetTimer()
						b.RunParallel(func(pb *testing.PB) {
							for pb.Next() {
								id := n.Add(1) % uint64(entities)
								req := httptest.NewRequest(method, fmt.Sprintf("/wallets/wal_%d%s", id, suffix), strings.NewReader(`{"amount_minor":1}`))
								req.Header.Set("Authorization", "Bearer wk")
								req.Header.Set("X-Session-ID", "bench")
								rec := httptest.NewRecorder()
								h.ServeHTTP(rec, req)
								if rec.Code != http.StatusOK {
									b.Errorf("status = %d: %s", rec.Code, rec.Body.String())
									return
								}
							}
						})
						b.ReportMetric(float64(b.N)/b.Elapsed().Seconds(), "ops/s")
					})
				}
			}
		}
	}
}
