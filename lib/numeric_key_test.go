// SPDX-License-Identifier: Apache-2.0

package accessgate

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
)

func numericKeyConfig() Config {
	return Config{Resources: []Resource{{
		Name: "orders", Key: "id",
		Columns: []Column{{Name: "id"}, {Name: "total"}},
		Scope:   func(p Principal, id string) bool { return id == "1" || id == "3" },
	}}, Tools: []ToolSpec{
		{Name: "get", Resource: "orders", Handler: func(context.Context, json.RawMessage) (any, error) {
			return map[string]any{"id": 1, "total": 10}, nil
		}},
		{Name: "pay", Resource: "orders", Mutates: true, Handler: func(context.Context, json.RawMessage) (any, error) {
			return map[string]any{"id": 1, "total": 0}, nil
		}},
	}}
}

// A numeric key is an entity like any other: the stale-read guard tracks it.
func TestNumericKeyIsTrackedByStaleReadGuard(t *testing.T) {
	g := newGuard(t, numericKeyConfig())
	args := json.RawMessage(`{"id":1}`) // the id as a number, as an agent would send it
	if _, err := g.Call(asW("a"), "get", args); err != nil {
		t.Fatal(err)
	}
	if _, err := g.Call(asW("b"), "pay", args); err != nil {
		t.Fatal(err)
	}
	if _, err := g.Call(asW("a"), "pay", args); err == nil {
		t.Error("a's write after b's succeeded; the numeric key was not tracked")
	}
}

// Scope and list filtering work on numeric keys.
func TestNumericKeyScopeFiltersListRows(t *testing.T) {
	g := newGuard(t, numericKeyConfig())
	list := g.Handle("orders", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`[{"id":1,"total":10},{"id":2,"total":20},{"id":3,"total":30}]`))
	})
	rec := serveParam(list, http.MethodGet, "", "", "a")
	if want := `[{"id":1,"total":10},{"id":3,"total":30}]` + "\n"; rec.Body.String() != want {
		t.Errorf("list = %q, want %q", rec.Body.String(), want)
	}
	if _, err := g.Call(asW("a"), "pay", json.RawMessage(`{"id":2}`)); err == nil {
		t.Error("write to an out-of-scope numeric id succeeded")
	}
}
