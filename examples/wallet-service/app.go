// SPDX-License-Identifier: Apache-2.0

package main

import (
	"database/sql"
	"encoding/json"
	"net/http"

	accessgate "github.com/humblefool-2/access-gate/lib"
)

type app struct {
	db *sql.DB
}

// newMux wires the routes. authn is the service's own authentication; the
// service decides who may call which route (requireWrite); the access guard
// adds per-session workflow rules and column projection.
// Handlers return full objects.
func newMux(db *sql.DB, authn accessgate.Authenticator) http.Handler {
	a := &app{db: db}
	guard := newGuard(a)
	return routes(a, authn, guard.Handle, guard)
}

// newGuard builds the access guard for the service (HTTP routes and the MCP
// adapter share it).
func newGuard(a *app) *accessgate.Guard {
	guard, err := accessgate.New(accessConfig(a))
	if err != nil {
		panic("invalid access config: " + err.Error())
	}
	return guard
}

// newBaselineMux serves the same handlers with no guard: no role checks, no
// projection, no stale-read rule, no /tools. Only for
// benchmarks (GUARD=off), to measure what the guard adds. It keeps the
// service's own authentication and role checks, so the comparison is fair.
func newBaselineMux(db *sql.DB, authn accessgate.Authenticator) http.Handler {
	plain := func(_ string, h http.HandlerFunc) http.HandlerFunc { return h }
	return routes(&app{db: db}, authn, plain, nil)
}

type wrapFunc func(resource string, h http.HandlerFunc) http.HandlerFunc

func routes(a *app, authn accessgate.Authenticator, guard wrapFunc, g *accessgate.Guard) http.Handler {
	db := a.db
	mux := http.NewServeMux()

	mux.HandleFunc("GET /health", func(w http.ResponseWriter, r *http.Request) {
		if err := db.PingContext(r.Context()); err != nil {
			writeJSON(w, http.StatusServiceUnavailable, map[string]string{"status": "db unavailable"})
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	})

	// Read API (any role).
	mux.HandleFunc("GET /customers/{id}", guard("customers", a.getCustomer))
	mux.HandleFunc("GET /wallets/{id}", guard("wallets", a.getWallet))

	// Write API (write role only, checked by the service, not the lib).
	mux.HandleFunc("POST /customers", requireWrite(guard("customers", a.createCustomer)))
	mux.HandleFunc("POST /wallets/{id}/credit", requireWrite(guard("wallets", a.credit)))
	mux.HandleFunc("POST /wallets/{id}/debit", requireWrite(guard("wallets", a.debit)))
	mux.HandleFunc("POST /wallets/{id}/holds", requireWrite(guard("wallets", a.createHold)))
	mux.HandleFunc("POST /wallets/{id}/status", requireWrite(guard("wallets", a.setStatus)))
	mux.HandleFunc("POST /holds/{id}/release", requireWrite(guard("wallets", a.releaseHold)))

	// Agent tools (same guard, so same rules).
	if g != nil {
		mux.Handle("/tools", g.ToolsHandler())
		mux.Handle("/tools/", g.ToolsHandler())
	}

	return accessgate.Authenticate(authn, requireAuth(mux))
}

// requireWrite is the service's own role check: only a write-role principal
// reaches the wrapped handler. In a real service this is whatever authorization
// middleware it already has (JWT claims, RBAC). The access-gate lib has no
// role logic of its own.
func requireWrite(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if p, ok := accessgate.PrincipalFrom(r.Context()); !ok || p.Attrs["role"] != "write" {
			writeJSON(w, http.StatusForbidden, errBody("role too low for this operation"))
			return
		}
		next(w, r)
	}
}

// requireAuth stands in for the JWT middleware every real service already has:
// a request with no valid principal is refused with 401 before any handler
// (or the guard) runs. /health stays open for probes. It sits in front of both
// the guarded and the baseline service, so the baseline is "service auth only".
func requireAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, ok := accessgate.PrincipalFrom(r.Context()); !ok && r.URL.Path != "/health" {
			writeJSON(w, http.StatusUnauthorized, errBody("missing or invalid token"))
			return
		}
		next.ServeHTTP(w, r)
	})
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	// The response differs by caller, so a shared cache must not reuse it.
	w.Header().Set("Cache-Control", "private, no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}
