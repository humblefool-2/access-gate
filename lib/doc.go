// SPDX-License-Identifier: Apache-2.0

// Package accessgate lets a Go service declare once what each caller may see
// and do, and enforces it at the interface, for people, other services and
// agents. It sits behind the service's own authentication and gives every
// route and agent tool the same data rules, driven by one Config the service
// declares, so no handler repeats redaction or workflow checks.
//
// The library does no authentication and no authorization. The service
// verifies the caller however it likes (JWT, API key, mTLS), decides which
// roles may call which routes with its own middleware, and puts a [Principal]
// on the request context,
// either with an [Authenticator] wrapped by [Authenticate] or with
// [WithPrincipal]. From there a [Guard] does the rest:
//
//   - No principal is [ErrUnauthenticated]. For agent tools the service can
//     plug its permission rules into Config.Authorize; a refusal is
//     [ErrForbidden]. Nothing else in the lib is a permission check.
//   - Columns per view: handlers return full objects and the guard projects
//     them per principal. Columns are Public or PII, PII is dropped or masked
//     (a fixed "***" or a MaskFunc such as [KeepLast]), nested objects are
//     addressed by dot path, and columns that are not declared count as PII.
//     Callers can narrow a response with ?fields= but never widen it.
//   - Stale-read guard (a write is any method other than GET, HEAD, OPTIONS,
//     or a tool with Mutates set): if a session read an entity and any session wrote it
//     since, that session's next write is refused with [ErrStaleRead] until it
//     reads again. Writes to one entity are serialised; different entities run
//     in parallel.
//   - Row scoping: an optional Resource.Scope decides which entities a
//     principal may touch, before the handler runs. A refused entity is
//     [ErrNotInScope], answered as 404.
//   - Agent tools: [ToolSpec]s run under the same rules through
//     [Guard.ListTools] and [Guard.Call], so an MCP server or any agent runtime
//     is a thin wrapper.
//
// # Usage
//
//	cfg := accessgate.Config{
//		Resources: []accessgate.Resource{{
//			Name: "customers",
//			Key:  "customer_id",
//			Columns: []accessgate.Column{
//				{Name: "customer_id"},
//				{Name: "email", Class: accessgate.PII, Mask: accessgate.Drop},
//				{Name: "phone", Class: accessgate.PII, MaskFunc: accessgate.KeepLast(4)},
//			},
//		}},
//	}
//	guard, err := accessgate.New(cfg) // fails on config typos
//	...
//	mux.HandleFunc("GET /customers/{id}", guard.Handle("customers", getCustomer))
//	http.ListenAndServe(addr, accessgate.Authenticate(yourAuthn, mux))
//
// # Limits
//
// The version and read history live in memory and are unbounded; there is no
// idempotency, so a write retried after a lost response applies twice; per
// entity locks do not make a write that touches several entities atomic. See
// the README for the full list.
package accessgate
