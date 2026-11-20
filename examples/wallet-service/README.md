# wallet-service example

An example service (own module) over SQLite, built on the [access-gate library](../../README.md) at the repo root. It shows the library in a working service, and it is the target of the [benchmarks](../../benchmarks/README.md), run with static-token auth. Run it from the repo root: `make run` (`:8080`, `PORT` to change).

The same routes serve three kinds of caller, and the library applies the same rules to each:

- **A person**: `curl` or the [playground UI](#playground-ui), with a bearer token for their role (see [Examples](#examples)).
- **Another service**: REST calls with a bearer token and an optional `X-Session-ID` (see [Auth](#auth-example) and [Endpoints](#endpoints)).
- **An agent**: the tools API (`GET /tools`, `POST /tools/{name}`) or the MCP stdio server (see [MCP](#mcp-stdio)).

## Files

| File | Purpose |
| --- | --- |
| `main.go` | env config, opens the store, seeds demo data, starts the server |
| `app.go` | `newMux(db, authn)`: routes, each wrapped by the access guard, plus `/health`, `/tools` |
| `access_config.go` | the one place that declares roles' columns, PII classes, default fields per level, and tools |
| `auth.go` | the service's own authentication: static bearer tokens (an example only; swap for JWT etc.) |
| `wallets.go` | handlers; they return full objects and know nothing about roles or PII |
| `tools.go` | exposes the same handlers as agent tools |
| `store.go`, `schema.sql` | SQLite open plus schema, applied on every start |
| `seed.go` | 5 demo customers and wallets when the DB is empty (`cus_demo1..5`, `wal_demo1..5`, one frozen, no holds) |

## Auth (example)

`SESSION_TOKENS="tok=read|read-pii|write,..."`, default `dev-read`, `dev-read-pii`, `dev-write` (logs a warning). Send `Authorization: Bearer <tok>`. The optional `X-Session-ID` header names one agent run; the stale-read guard is per session (falls back to the token).

## Endpoints

Public: `GET /health`.

Read (any role; `?fields=a,b` narrows):
- `GET /customers/{id}`: `customer_id`, `email`, `phone`, `wallet`. `read` sees `phone` as `***` and no `email`; `read-pii` and `write` see all.
- `GET /wallets/{id}`: wallet plus active `holds`

Write (`write` role, enforced by the service's `requireWrite`, not by the lib):
- `POST /customers` `{customer_id?, email, phone, currency?}`
- `POST /wallets/{id}/credit` and `/debit` `{amount_minor}`
- `POST /wallets/{id}/holds` `{amount_minor, reason}`
- `POST /holds/{id}/release`
- `POST /wallets/{id}/status` `{status: active|frozen|closed}`

Tools (same data rules; the service's `Config.Authorize` in `access_config.go` lets only the write role use tools that change data): `GET /tools` lists what the role may call; `POST /tools/{name}` with JSON args. Tools: `get_customer {id}`, `get_wallet {id}`, `credit`/`debit {id, amount_minor}`, `set_status {id, status}`.

Errors: 401 no/invalid token, 403 role too low (service role check, or a tool refused by `Authorize`), 409 stale read (re-read, then retry) or business conflict, 404 not found.

## Data

SQLite at `data/poc.db` (`DB_PATH` to change). Tables `customers`, `wallets`, `wallet_holds`. While prototyping, edit `schema.sql` and delete the DB file. Money is integer minor units.

## Examples

```bash
curl localhost:8080/customers/cus_demo1 -H 'Authorization: Bearer dev-read'
curl localhost:8080/customers/cus_demo1 -H 'Authorization: Bearer dev-read-pii'
curl -XPOST localhost:8080/tools/credit -H 'Authorization: Bearer dev-write' -H 'X-Session-ID: run-1' -d '{"id":"wal_demo1","amount_minor":500}'
```

## MCP (stdio)

`go run ./examples/wallet-service mcp` serves the agent tools as an MCP server over stdin/stdout (newline-delimited JSON-RPC 2.0, standard library only; `mcp.go`). One process is one principal and one session: `MCP_TOKEN` picks the token (default `dev-read`; `dev-write` for `credit`, `debit`, `set_status`), `MCP_SESSION` names the session. `tools/list` shows only the tools that principal may call, with their `InputSchema`; a refused call (role, stale read, refused field) comes back as a tool result with `isError: true` and a message the model can act on. Logs go to stderr.

```json
{"mcpServers":{"wallet":{"command":"go","args":["run","./examples/wallet-service","mcp"],"env":{"MCP_TOKEN":"dev-read"}}}}
```

Tests: `make test` (`app_test.go` health, `wallets_test.go` wallet flow, PII projection, tools, and stale-read).

Agent-flow load: `make agentload`; guarded vs unguarded: `make bench-compare` (see `benchmarks/README.md`). `GUARD=off` serves the handlers with no access guard, for benchmark baselines only. `STRICT_FIELDS=on` makes a `?fields=` request for something the role may not see (or that is not a column) a 403 instead of silently dropping it; the `rewrite` agentload flow needs it.

## Playground UI

A local web page to try the library without curl or terminal commands:

```bash
make playground    # from the repo root, then open http://localhost:9090  (PLAYGROUND_PORT to change)
```

Try it in two minutes:

1. Under **1. Who am I?** pick **Read**, then under **2. Try something** press **Get customer**. The with-lib pane shows no `email` and a masked `phone`; the without-lib pane shows everything.
2. Pick **Read-PII** and press **Get customer** again: both panes now show every column.
3. Pick **Write** and press **Credit +500**, then **Get wallet**: the balance moved.
4. Press **Stale-read demo**: session A reads, session B writes, session A writes again. With the lib the last write is refused (409); without it the write goes through and overwrites what A saw.
5. Under **3. Benchmarks** pick a benchmark and a size and press **Run**.

Details:

- **Try requests:** pick a role (Read, Read-PII, Write, Not logged in), then press Get customer, Get wallet, Credit or Debit. Each request runs with the lib and without it, side by side, with status, latency and response (masked PII, 403/409 errors). "Not logged in" gets a 401 from the service on both sides (the example service has the auth check a real service has, standing in for JWT middleware: `requireAuth` in `app.go`; `/health` stays open). The service's own role check (`requireWrite`) refuses Read and Read-PII on credit and debit on both sides, so both panes show 403; the lib's part is what each role sees.
- **Stale-read demo:** one button, as in step 4 above. **Reset data** restarts both services on fresh seeded data.
- **Run benchmarks:** pick a benchmark (Request overhead, Datastore latency, Compare Performance) and a size (Quick, Standard or Thorough), then press Run. Nothing to configure: each choice maps to fixed settings, shown under the dropdowns. Output streams live, a status line shows running, done or stopped with elapsed time, and Stop is active only while something runs. `go test` benchmarks end with a `benchstat` comparison when `benchstat` is installed (`go install golang.org/x/perf/cmd/benchstat@latest`). What each benchmark measures: [benchmarks/README.md](../../benchmarks/README.md).

The playground builds and starts its own guarded (`:18182`) and baseline (`:18183`) services on separate databases under `data/playground/`, so it does not touch `make run` on `:8080`. Local use only. Source: `playground/`.
