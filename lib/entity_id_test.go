// SPDX-License-Identifier: Apache-2.0

package accessgate

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
)

// scopedConfig has a resource whose routes name the entity {cid} and whose
// tool takes it as "cid"; the principal may only touch c1.
func scopedConfig(handler func(context.Context, json.RawMessage) (any, error)) Config {
	if handler == nil {
		handler = func(context.Context, json.RawMessage) (any, error) { return map[string]any{"id": "c1"}, nil }
	}
	return Config{Resources: []Resource{{
		Name: "customers", Key: "id", IDParam: "cid",
		Columns: []Column{{Name: "id"}, {Name: "balance"}},
		Scope:   func(p Principal, id string) bool { return id == "c1" },
	}}, Tools: []ToolSpec{{Name: "credit", Resource: "customers", Mutates: true, IDArg: "cid", Handler: handler}}}
}

func serveParam(h http.HandlerFunc, method, param, id, session string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, "/customers/"+id, nil).WithContext(asW(session))
	if param != "" {
		r.SetPathValue(param, id)
	}
	rec := httptest.NewRecorder()
	h(rec, r)
	return rec
}

// A route whose id is not called "id" must get the same checks as one that is:
// Scope before the handler, and the stale-read guard.
func TestHTTPRouteWithCustomIDParam(t *testing.T) {
	var ran atomic.Int32
	g := newGuard(t, scopedConfig(nil))
	write := g.Handle("customers", func(w http.ResponseWriter, r *http.Request) {
		ran.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"` + r.PathValue("cid") + `","balance":1}`))
	})

	if rec := serveParam(write, http.MethodPost, "cid", "c2", "a"); rec.Code != http.StatusNotFound || ran.Load() != 0 {
		t.Fatalf("out-of-scope write = %d, handler ran %d times; want 404 and the handler not run", rec.Code, ran.Load())
	}
	serveParam(write, http.MethodGet, "cid", "c1", "a")  // a reads c1
	serveParam(write, http.MethodPost, "cid", "c1", "b") // b writes c1
	if rec := serveParam(write, http.MethodPost, "cid", "c1", "a"); rec.Code != http.StatusConflict {
		t.Errorf("stale write on a {cid} route = %d, want 409", rec.Code)
	}
}

// A write on a scoped resource with no entity id never reaches the handler.
func TestScopedWriteWithoutIDIsRefusedBeforeHandler(t *testing.T) {
	var ran atomic.Int32
	g := newGuard(t, scopedConfig(func(context.Context, json.RawMessage) (any, error) {
		ran.Add(1)
		return map[string]any{"id": "c1", "balance": 1}, nil
	}))
	route := g.Handle("customers", func(w http.ResponseWriter, r *http.Request) {
		ran.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"c1","balance":1}`))
	})

	// the route has {id}, not {cid}: the guard cannot find the entity
	if rec := serveParam(route, http.MethodPost, "id", "c1", "a"); rec.Code != http.StatusNotFound {
		t.Errorf("route write without {cid} = %d, want 404", rec.Code)
	}
	for _, args := range []string{`{}`, `{"id":"c1"}`} { // missing, or under the wrong name
		if _, err := g.Call(asW("a"), "credit", json.RawMessage(args)); err == nil {
			t.Errorf("tool write with args %s succeeded, want refusal", args)
		}
	}
	if ran.Load() != 0 {
		t.Errorf("handler ran %d times, want 0", ran.Load())
	}
}

// The tool argument name is configurable too, and Scope sees its value.
func TestToolWithCustomIDArg(t *testing.T) {
	var ran atomic.Int32
	g := newGuard(t, scopedConfig(func(context.Context, json.RawMessage) (any, error) {
		ran.Add(1)
		return map[string]any{"id": "c1", "balance": 1}, nil
	}))
	if _, err := g.Call(asW("a"), "credit", json.RawMessage(`{"cid":"c2"}`)); err == nil || ran.Load() != 0 {
		t.Fatalf("out-of-scope tool write: err=%v, ran=%d; want refusal before the handler", err, ran.Load())
	}
	if _, err := g.Call(asW("a"), "credit", json.RawMessage(`{"cid":"c1"}`)); err != nil {
		t.Errorf("in-scope tool write: %v", err)
	}
}

// Reads without an id keep working on a scoped resource (lists are filtered).
func TestScopedReadWithoutIDStillWorks(t *testing.T) {
	g := newGuard(t, scopedConfig(nil))
	list := g.Handle("customers", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`[{"id":"c1","balance":1},{"id":"c2","balance":2}]`))
	})
	rec := serveParam(list, http.MethodGet, "", "", "a")
	if rec.Code != http.StatusOK || rec.Body.String() != `[{"balance":1,"id":"c1"}]`+"\n" {
		t.Errorf("list = %d %q, want only c1", rec.Code, rec.Body.String())
	}
}
