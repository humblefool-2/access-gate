// SPDX-License-Identifier: Apache-2.0

package accessgate

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
)

var walletOwner = map[string]string{"w1": "alice", "w2": "bob", "w3": "alice"}

func scopeConfig(calls *int) Config {
	get := func(_ context.Context, args json.RawMessage) (any, error) {
		*calls++
		var a struct{ ID string }
		_ = json.Unmarshal(args, &a)
		return map[string]any{"id": a.ID, "balance": 1}, nil
	}
	return Config{
		Authorize: writersOnly,
		Resources: []Resource{{
			Name: "wallets", Key: "id", MaxRows: 2,
			Columns: []Column{{Name: "id"}, {Name: "balance"}},
			Scope:   func(p Principal, id string) bool { return walletOwner[id] == p.Attrs["user"] },
		}},
		Tools: []ToolSpec{
			{Name: "get", Resource: "wallets", Handler: get},
			{Name: "credit", Resource: "wallets", Mutates: true, Handler: get},
			{Name: "list", Resource: "wallets", Handler: func(context.Context, json.RawMessage) (any, error) {
				*calls++
				return []any{
					map[string]any{"id": "w1", "balance": 1}, map[string]any{"id": "w2", "balance": 2},
					map[string]any{"id": "w3", "balance": 3}, map[string]any{"id": "w1", "balance": 4},
				}, nil
			}},
			// a lookup whose answer names a different entity than the one asked for
			{Name: "lookup", Resource: "wallets", Handler: func(context.Context, json.RawMessage) (any, error) {
				*calls++
				return map[string]any{"id": "w2", "balance": 2}, nil
			}},
		},
	}
}

func userW(session, name string) context.Context {
	return WithPrincipal(context.Background(), Principal{ID: name, SessionID: session, View: ViewPII, Attrs: map[string]string{"user": name, "role": "writer"}})
}

func user(level View, session, name string) context.Context {
	return WithPrincipal(context.Background(), Principal{ID: name, SessionID: session, View: level, Attrs: map[string]string{"user": name}})
}

func TestScopeRefusesBeforeHandler(t *testing.T) {
	calls := 0
	g := newGuard(t, scopeConfig(&calls))

	if _, err := g.Call(user(ViewPublic, "s", "alice"), "get", json.RawMessage(`{"id":"w1"}`)); err != nil {
		t.Fatalf("own wallet = %v", err)
	}
	calls = 0
	_, err := g.Call(user(ViewPublic, "s", "alice"), "get", json.RawMessage(`{"id":"w2"}`))
	if !errors.Is(err, ErrNotInScope) {
		t.Fatalf("other's wallet = %v", err)
	}
	if code, msg := statusOf(err); code != http.StatusNotFound || msg != "not found" {
		t.Errorf("status = %d %q, want 404 not found (and no reason)", code, msg)
	}
	if calls != 0 {
		t.Errorf("handler ran for an out-of-scope read")
	}

	// a write on someone else's entity must never run, or it would save and then 404
	if _, err := g.Call(userW("w", "alice"), "credit", json.RawMessage(`{"id":"w2"}`)); !errors.Is(err, ErrNotInScope) {
		t.Fatalf("out-of-scope write = %v", err)
	}
	if calls != 0 {
		t.Errorf("handler ran for an out-of-scope write")
	}
	// the refused write left no trace in the workflow state: bob reads, nothing is stale
	g.Call(user(ViewPublic, "b", "bob"), "get", json.RawMessage(`{"id":"w2"}`))
	if _, err := g.Call(userW("b", "bob"), "credit", json.RawMessage(`{"id":"w2"}`)); err != nil {
		t.Errorf("bob's write after alice's refused one = %v", err)
	}
	// no attrs at all sees nothing
	if _, err := g.Call(as(ViewPublic, "n"), "get", json.RawMessage(`{"id":"w1"}`)); !errors.Is(err, ErrNotInScope) {
		t.Errorf("no attrs = %v", err)
	}
}

func TestScopeFiltersListsBeforeMaxRows(t *testing.T) {
	calls := 0
	g := newGuard(t, scopeConfig(&calls))
	got, err := g.Call(user(ViewPublic, "s", "alice"), "list", nil)
	want := []any{map[string]any{"id": "w1", "balance": 1.0}, map[string]any{"id": "w3", "balance": 3.0}} // MaxRows=2 counts visible rows
	if err != nil || !equal(got, want) {
		t.Errorf("alice list = %v, %v\nwant %v", got, err, want)
	}
	got, _ = g.Call(user(ViewPublic, "s", "bob"), "list", nil)
	if !equal(got, []any{map[string]any{"id": "w2", "balance": 2.0}}) {
		t.Errorf("bob list = %v", got)
	}
	got, _ = g.Call(user(ViewPublic, "s", "carol"), "list", nil)
	if !equal(got, []any{}) {
		t.Errorf("carol list = %v, want empty", got)
	}
}

func TestScopeChecksResponseKeyToo(t *testing.T) {
	calls := 0
	g := newGuard(t, scopeConfig(&calls))
	// alice asks for her own w1 but the handler answers with bob's w2
	_, err := g.Call(user(ViewPublic, "s", "alice"), "lookup", json.RawMessage(`{"id":"w1"}`))
	if !errors.Is(err, ErrNotInScope) {
		t.Errorf("response for another entity = %v", err)
	}
}

func TestScopeOverHTTP(t *testing.T) {
	calls := 0
	g := newGuard(t, scopeConfig(&calls))
	mux := http.NewServeMux()
	mux.HandleFunc("GET /wallets/{id}", g.Handle("wallets", func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"id": r.PathValue("id"), "balance": 1})
	}))
	do := func(name, id string) (int, string) {
		req := httptest.NewRequest("GET", "/wallets/"+id, nil).WithContext(user(ViewPublic, "s", name))
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)
		return rec.Code, rec.Body.String()
	}
	if code, _ := do("alice", "w1"); code != 200 {
		t.Errorf("own = %d", code)
	}
	calls = 0
	if code, body := do("alice", "w2"); code != 404 || body != "{\"error\":\"not found\"}\n" || calls != 0 {
		t.Errorf("other = %d %q calls=%d", code, body, calls)
	}
	if code, body := do("alice", "nope"); code != 404 || body != "{\"error\":\"not found\"}\n" {
		t.Errorf("unknown id looks the same = %d %q", code, body)
	}
}

func TestScopeNeedsKey(t *testing.T) {
	cfg := scopeConfig(new(int))
	cfg.Resources[0].Key = ""
	if _, err := New(cfg); err == nil {
		t.Error("Scope without Key should fail validation")
	}
}
