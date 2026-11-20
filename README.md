# access-gate

Declare once what each caller may see and do, and enforce it at the interface of your service, for people, other services and agents alike. Standard library only.

[![Go Reference](https://pkg.go.dev/badge/github.com/humblefool-2/access-gate.svg)](https://pkg.go.dev/github.com/humblefool-2/access-gate)
[![License](https://img.shields.io/badge/license-Apache--2.0-blue.svg)](LICENSE)

## The problem

A service that exposes data gets called by more than people now: other services, and increasingly agents that read, think for a few seconds and write. Two things go wrong, and every service (and every MCP server) fixes them again by hand:

- **PII over-exposure.** The handler returns the full row and each endpoint is supposed to redact it. One forgotten field, one new column, one new route, and an email or phone number reaches a caller that should not see it. Agents make this worse: whatever they read goes into a prompt, a log, or another system.
- **Lost updates from stale reads.** An agent reads a wallet, spends seconds deciding, then writes. Meanwhile something else changed it. Without a check, the write goes through on a view that is no longer true and silently overwrites the other change.

Each team builds its own version of these rules, in each service, and they tend to drift.

## The idea

Fix it once, at the interface. Handlers keep returning full objects and know nothing about roles. You declare per resource which columns are public and which are PII, and wrap each route (or agent tool) with one `Guard`. The guard projects every response for the caller, and refuses a write made on a stale read.

The same customer response, as a `ViewPublic` caller sees it:

What the handler returns:

```json
{
  "customer_id": "c-42",
  "email": "ana@example.com",
  "phone": "0712345678"
}
```

What the caller gets through the guard (email dropped, phone masked; the example service's playground masks the phone fully, as `***`):

```json
{
  "customer_id": "c-42",
  "phone": "******5678"
}
```

A caller that is allowed to see PII gets the full object. Callers can narrow a response with `?fields=a,b`, never widen it. The rules are the same for HTTP routes and for agent tools, so an MCP server or any agent runtime is a thin wrapper.

The library does **no authentication and no authorization**. Your service verifies callers and decides who may call what; the lib decides what each caller sees and makes writes safe.

## Why a library and not an MCP server?

MCP and access-gate are different layers. MCP is a protocol that tells an agent how to discover and call tools. access-gate enforces what a call may see and do (columns, rows, stale writes). You can have both: `examples/wallet-service/mcp.go` is a ~150-line MCP server on top of `ListTools`/`Call` with no access logic of its own.

The fair alternative is a generic MCP gateway in front of your APIs, with the same per-resource config. The difference is where the rules run:

| | access-gate (library) | MCP gateway |
| --- | --- | --- |
| Runs | Inside the service | In front of the service |
| Can be bypassed | Not if every route is wrapped | Yes, if anything reaches the backend directly |
| Safe concurrent writes (stale-read check) | Yes, per-entity lock around the handler | Needs its own state; can't lock around the backend write |
| Extra hop to run | No | Yes |
| Languages | Go only | Any |
| Scope | One service | Many services, one config |
| Instances | One (state is per process) | Depends on the gateway |

Use the library for Go services where you need enforcement inside the process, with no path around it for wrapped routes. Use a gateway for many languages when you can force all traffic through it. They combine: the library in each service, a gateway on top for discovery.

## Quick start

```bash
go get github.com/humblefool-2/access-gate
```

```go
package main

import (
	"encoding/json"
	"net/http"

	accessgate "github.com/humblefool-2/access-gate/lib"
)

func main() {
	guard, err := accessgate.New(accessgate.Config{ // fails on config typos
		Resources: []accessgate.Resource{{
			Name: "customers",
			Key:  "customer_id",
			Columns: []accessgate.Column{
				{Name: "customer_id"},
				{Name: "email", Class: accessgate.PII, Mask: accessgate.Drop},
				{Name: "phone", Class: accessgate.PII, MaskFunc: accessgate.KeepLast(4)},
			},
		}},
	})
	if err != nil {
		panic(err)
	}

	getCustomer := func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"customer_id": r.PathValue("id"),
			"email":       "ana@example.com",
			"phone":       "0712345678",
		})
	}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /customers/{id}", guard.Handle("customers", getCustomer))

	// yourAuthn is your own accessgate.Authenticator (JWT, API key, mTLS...).
	http.ListenAndServe(":8080", accessgate.Authenticate(yourAuthn, mux))
}
```

Requires Go 1.22 or newer (`r.PathValue`). `lib/example_test.go` has a runnable version with no auth setup.

Build a `Config`, create the guard with `accessgate.New(cfg)`, wrap each route with `guard.Handle(resource, handler)` (and put your own role middleware around write routes), and wrap the whole mux with `accessgate.Authenticate(yourAuthn, mux)`. `Authenticate` never rejects by itself, so unguarded routes (health) keep working.

## Features

- **Views**: `ViewPublic` (non-PII columns) or `ViewPII` (all columns). Your service maps its roles to a view and decides who may write.
- **Column projection**: handlers return full objects; the lib drops or masks PII per view. Undeclared columns count as PII. Callers narrow with `?fields=`, never widen (`StrictFields` makes a disallowed field a 403).
- **Stale-read guard**: a write is refused (409) if anyone wrote the entity since this session last read it.
- **Agent tools**: `ToolSpec`s follow the same rules, and `ListTools` shows only what the caller may call. The example has a stdlib MCP server on top.
- **Row scoping** (optional): `Resource.Scope` limits which entities a caller may touch; others answer 404.

Details of each option are in `go doc ./lib`.

### Plugging in any authentication

Your service authenticates callers however it likes (JWT, API key, gateway headers, mTLS) and gives the lib an `accessgate.Principal{ID, SessionID, View}`. An `Authenticator` is just `func(*http.Request) (accessgate.Principal, error)`. Anything that can produce a principal works:

```go
// static tokens: see examples/wallet-service/auth.go (the one the example and benchmarks use)
// JWT sketch: verify with your JWT library, then map claims to a View
authn := func(r *http.Request) (accessgate.Principal, error) {
	claims, err := verifyJWT(r) // your code
	if err != nil {
		return accessgate.Principal{}, err
	}
	return accessgate.Principal{ID: claims.Sub, SessionID: claims.SessionID, View: viewFromScope(claims.Scope)}, nil
}
handler := accessgate.Authenticate(authn, mux)
```

Swapping the authenticator changes nothing else: columns per view and the stale-read guard stay as configured.

**Authorization stays in the service.** The lib has no permission logic. Which roles may call `POST /wallets/{id}/credit` is the service's own middleware (the example has `requireWrite` in `app.go`), exactly as it would be without the lib. The principal's `View` is only used to pick the view of the data. For agent tools, which the lib serves itself, the service can plug its rule into `Config.Authorize(principal, tool)`; a tool it refuses is hidden from `ListTools` and refused by `Call` with `ErrForbidden`. The lib treats a request as a write (stale-read guard, per-entity lock) when the HTTP method is not GET, HEAD or OPTIONS, or when a tool has `Mutates: true`.

## Example service and playground

`examples/wallet-service` is a complete service built on the library: customers and wallets in SQLite, static-token auth, REST routes, agent tools and an MCP server. It is also the target of the benchmarks. See [its README](examples/wallet-service/README.md) for endpoints and setup.

```bash
make run          # example service on :8080
make playground   # local web UI on http://localhost:9090
make test         # library + example
```

The playground lets you try the library without curl, and it is the quickest way to see the rules work:

- **Try requests**: pick a role (Read, Read-PII, Write, Not logged in) and send a request. It runs with the lib and without it, side by side, so you see the masked PII and the 403/409 errors.
- **Stale-read demo**: one button. Session A reads, session B writes, session A writes again: with the lib the last write is refused (409); without it the write goes through and overwrites what A saw.
- **Run benchmarks**: pick a benchmark and a size and press Run. See [Benchmarks](#benchmarks).

Step-by-step instructions are in the [example's README](examples/wallet-service/README.md).

Repo layout:

```
lib/                     the library (package accessgate; import github.com/humblefool-2/access-gate/lib)
examples/wallet-service/ example service, playground UI and tests (own module)
benchmarks/              benchmark method, results and the agent-flow load driver (own module)
go.work                  links the three modules locally
```

## Benchmarks

Each benchmark compares the guarded service with a baseline that serves the same handlers with no guard (`GUARD=off`); both keep the service's own authentication, so the difference is the lib alone. Measured on an Apple M4 Pro laptop (Go 1.26), so compare deltas, not absolute numbers.

| What | Cost of the library |
| --- | --- |
| Per request (`GET` / `POST`, no network) | +5 to +7 µs, about 2x the allocations |
| 1 agent, no think time | +0.005 to +0.012 ms at p50 (ranges include zero) |
| 32 agents on 32 wallets | +0.19 ms (reads) and +0.33 to +0.50 ms (writes) at p50; throughput 9.1k to 10.0k req/s against 10.3k to 11.0k |
| Slow store, writes to one entity | capped at about 1 / store latency (9.9 ops/s at 100 ms), by design |
| Slow store, writes to 64 entities | 342 ops/s at 100 ms (about a third of baseline); reads are unaffected |

Tables, projection cost and memory growth are in [benchmarks/RESULTS.md](benchmarks/RESULTS.md); the method and flags are in [benchmarks/README.md](benchmarks/README.md).

**Run them**: `make playground` and use Run benchmarks (Request overhead, Datastore latency or Compare Performance, in Quick, Standard or Thorough), or from the command line:

```bash
go test -run '^$' -bench BenchmarkRequest -benchmem -count 10 ./examples/wallet-service   # then: benchstat -col /mode
REPS=5 make bench-compare ARGS="-agents 1 -think 0"   # guarded vs baseline, median latency delta
make bench-memory                                     # service RSS under sustained load
make bench-all                                        # everything, about 35 min
```

## Known limits

Each is a choice made to keep the library small. Numbers behind the memory and lock points are in [benchmarks/RESULTS.md](benchmarks/RESULTS.md).

**State**
- **Versions and read history are in memory, unbounded, and lost on restart.** About 0.25 KB of RSS per request (106 MB after 10 minutes at about 590 req/s).
- **State is per process.** Two instances each have their own view of who read what.
- **Only calls through the guard are tracked.** A write by a batch job or another service makes nobody stale.

**Writes**
- **No idempotency.** A write retried after a lost response applies twice, because the guard marks the writer's own write as seen.
- **Same-entity writes are serialised while the handler runs**, so with a slow store they run at about 1 / store latency (9.9 ops/s at 100 ms). Different entities run in parallel (256 stripe locks, so two entities can share one).
- **A write touching several entities is protected only for the entity in its route.**
- **Set `Resource.IDParam` (or `ToolSpec.IDArg`) when the path parameter is not the entity id.** A route with no entity id in the path (for example `/holds/{id}/release`) skips the pre-write stale check and `Scope`, and its version bump can race with a concurrent write. Keep such routes on a resource without `Scope`.
- **Read-before-write is not enforced.** A session that never read an entity can still write it.

**Projection**
- **A 2xx response that is not JSON is refused** (`ErrNotJSON`, 500) unless the resource sets `AllowNonJSON`. A handler's own non-2xx response passes through, so keep PII out of error bodies.
- **Paginated envelopes** need `Resource.ListPath`; other envelope keys are dropped unless in `ListKeep`.
- **Request bodies are capped at 1 MiB** for `ToolsHandler` (`Config.MaxBodyBytes`). Cap bodies in your own routes too.
- **The whole response is decoded first**, so `MaxRows` saves only about 17%.
- **Dot paths address objects, not arrays.** An array is one column.
- **A `MaskFunc` gets the decoded JSON value** (`string`, `bool`, `nil`, map, slice or `json.Number`). Its result is shown as is, so a mask that returns its input leaks it.

**Scope and tools**
- **`Scope` decides by entity id only**, not row contents, because a write must be refused before it runs. It needs `Key`, and the route's id must be that key.
- **`ToolSpec.InputSchema` is passed on but never enforced.** Validate arguments in the handler.

**Caching**
- **The guard sets no cache headers, yet responses differ by caller.** Set `Cache-Control: private` (or `Vary` on your auth header) in the service, as the example does.

**Out of scope**
- No token issuing, expiry, JWT parsing or role-to-route permissions: that is the service's job.
- A session is whatever id the service gives it (`Principal.SessionID`, falling back to `Principal.ID`).

## Single instance, and why not `ETag` / `If-Match`

State (versions, and who read what) lives in memory in one process. Run one instance, or accept that each replica has its own view. A persistent version store is planned for a later version.

`If-Match` plus a version column in your database is the right tool when one client updates one row and you control the schema. This library tracks the *session*: it refuses a write when anything wrote that entity since this session last read it, for REST routes and agent tools alike, with no schema change and no client cooperation. If you already have optimistic locking in your database, use it as well; they do not conflict.

## Threat model

**It protects against** a caller seeing columns its role may not see, a caller narrowing but never widening a response, a write based on a read that another session has since made stale, a caller touching entities outside its scope.

**It does not protect against** a caller that was never authenticated correctly (that is your authentication), a service that returns PII on a route the guard does not wrap, a handler that leaks PII in an error body, writers that bypass the guard, or several replicas: state is per process.

## Stability

v0. The API may change before v1. The module needs Go 1.22 or newer.

## API

| Piece | Where |
| --- | --- |
| `View`, `ParseView` (`public`, `pii`) | `lib/view.go` |
| `Principal`, `WithPrincipal`, `PrincipalFrom`, `Authenticator`, `Authenticate(authn, next)` | `lib/principal.go` |
| `Config`, `Resource`, `Column`, `ToolSpec`, `Validate` | `lib/config.go` |
| per-column projection | `lib/schema.go` |
| `New(cfg)`, `Guard.Handle(resource, handler)`, `Guard.Project` | `lib/guard.go` |
| per-session versions and seen-map | `lib/workflow.go` |
| `ListTools`, `Call`, `ToolsHandler` | `lib/tool.go` |

Full reference: `go doc ./lib` or pkg.go.dev (badge above).

## Tests

`go test -race ./lib` (`access_test.go` covers roles, projection, narrowing, strict fields, stale reads, per-entity write locking, middleware, config validation; `nested_test.go` dot paths and `MaskFunc`; `scope_test.go` row scoping; `tool_schema_test.go` `InputSchema`). `make test` also runs the example service's tests.

## License

Apache-2.0. See [LICENSE](LICENSE) and [NOTICE](NOTICE). Redistributions must keep both files.
