// SPDX-License-Identifier: Apache-2.0

package accessgate

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// writersOnly is the kind of rule a service plugs into Config.Authorize: the
// lib has no role logic, so tests that need a refusal supply one.
func writersOnly(p Principal, t ToolInfo) error {
	if t.Mutates && p.Attrs["role"] != "writer" {
		return errors.New("write role needed")
	}
	return nil
}

func testConfig() Config {
	return Config{
		Authorize: writersOnly,
		Resources: []Resource{{
			Name: "customers",
			Key:  "id",
			Columns: []Column{
				{Name: "id"},
				{Name: "email", Class: PII, Mask: Drop},
				{Name: "phone", Class: PII, Mask: Mask},
				{Name: "balance"},
			},
			Fields: map[View][]string{ViewPublic: {"id", "phone", "balance"}},
		}},
		Tools: []ToolSpec{
			{Name: "get", Resource: "customers", Handler: func(context.Context, json.RawMessage) (any, error) {
				return map[string]any{"id": "c1", "email": "a@x.com", "phone": "+1", "balance": 5, "extra": "new"}, nil
			}},
			{Name: "credit", Resource: "customers", Mutates: true, Handler: func(context.Context, json.RawMessage) (any, error) {
				return map[string]any{"id": "c1", "balance": 6}, nil
			}},
		},
	}
}

func newGuard(t *testing.T, cfg Config) *Guard {
	t.Helper()
	g, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	return g
}

// as returns a context for a principal; the lib never sees how it was authenticated.
func asW(session string) context.Context {
	return WithPrincipal(context.Background(), Principal{ID: "agent-" + session, SessionID: session, View: ViewPII, Attrs: map[string]string{"role": "writer"}})
}

func as(level View, session string) context.Context {
	return WithPrincipal(context.Background(), Principal{ID: "agent-" + session, SessionID: session, View: level})
}

func TestLevelImplication(t *testing.T) {
	for _, c := range []struct {
		have, need View
		want       bool
	}{
		{ViewPII, ViewPII, true}, {ViewPII, ViewPublic, true},
		{ViewPublic, ViewPII, false}, {NoView, ViewPublic, false},
	} {
		if got := c.have.Allows(c.need); got != c.want {
			t.Errorf("%v.Allows(%v) = %v", c.have, c.need, got)
		}
	}
}

func TestNoPrincipalIsUnauthenticated(t *testing.T) {
	g := newGuard(t, testConfig())
	if _, err := g.Call(context.Background(), "get", nil); !errors.Is(err, ErrUnauthenticated) {
		t.Errorf("call = %v", err)
	}
	if _, err := g.ListTools(context.Background()); !errors.Is(err, ErrUnauthenticated) {
		t.Errorf("list = %v", err)
	}
}

func TestProjectPerLevel(t *testing.T) {
	g := newGuard(t, testConfig())
	for _, c := range []struct {
		level View
		want  map[string]any
	}{
		{ViewPublic, map[string]any{"id": "c1", "phone": "***", "balance": 5.0}},
		// unknown "extra" column is PII, so only PII roles see it
		{ViewPII, map[string]any{"id": "c1", "email": "a@x.com", "phone": "+1", "balance": 5.0, "extra": "new"}},
	} {
		got, err := g.Call(as(c.level, "s"), "get", nil)
		if err != nil {
			t.Fatal(err)
		}
		if !equal(got, c.want) {
			t.Errorf("%v: got %v, want %v", c.level, got, c.want)
		}
	}
}

func TestRequestedFieldsNarrowOnly(t *testing.T) {
	g := newGuard(t, testConfig())
	got, _ := g.Call(as(ViewPublic, "s"), "get", json.RawMessage(`{"fields":["id","email"]}`))
	if !equal(got, map[string]any{"id": "c1"}) {
		t.Errorf("public narrowed = %v", got)
	}
	got, _ = g.Call(as(ViewPII, "s"), "get", json.RawMessage(`{"fields":["email"]}`))
	if !equal(got, map[string]any{"email": "a@x.com"}) {
		t.Errorf("pii narrowed = %v", got)
	}

	cfg := testConfig()
	cfg.StrictFields = true
	g = newGuard(t, cfg)
	if _, err := g.Call(as(ViewPublic, "s"), "get", json.RawMessage(`{"fields":["email"]}`)); !errors.Is(err, ErrFieldNotAllowed) {
		t.Errorf("strict = %v", err)
	}
}

// A write asking for a disallowed field (e.g. a typo) must be refused before
// the handler runs, not saved and then 403'd.
func TestStrictFieldsRefusesWriteBeforeHandler(t *testing.T) {
	cfg := testConfig()
	cfg.StrictFields = true
	cfg.Resources[0].Fields[ViewPII] = []string{"id", "email", "phone", "balance"}
	calls := 0
	cfg.Tools[1].Handler = func(context.Context, json.RawMessage) (any, error) {
		calls++
		return map[string]any{"id": "c1", "balance": 6}, nil
	}
	g := newGuard(t, cfg)

	_, err := g.Call(asW("A"), "credit", json.RawMessage(`{"id":"c1","fields":["balanc"]}`))
	if !errors.Is(err, ErrFieldNotAllowed) {
		t.Fatalf("typo field = %v, want ErrFieldNotAllowed", err)
	}
	if code, _ := statusOf(err); code != http.StatusForbidden {
		t.Errorf("status = %d, want 403", code)
	}
	if calls != 0 {
		t.Fatalf("handler ran %d times for a refused request", calls)
	}

	if _, err := g.Call(asW("A"), "credit", json.RawMessage(`{"id":"c1","fields":["balance"]}`)); err != nil {
		t.Fatalf("valid field = %v", err)
	}
	if calls != 1 {
		t.Errorf("handler calls = %d, want 1", calls)
	}
}

// slowWriteGuard has a write tool that reports how many calls are inside the
// handler at once and holds each for hold.
func slowWriteGuard(t *testing.T, hold func(inside int32)) *Guard {
	t.Helper()
	cfg := testConfig()
	var inside atomic.Int32
	cfg.Tools[1].Handler = func(_ context.Context, args json.RawMessage) (any, error) {
		var a struct{ ID string }
		_ = json.Unmarshal(args, &a)
		hold(inside.Add(1))
		defer inside.Add(-1)
		return map[string]any{"id": a.ID, "balance": 6}, nil
	}
	return newGuard(t, cfg)
}

// Writes to different entities must not wait on each other: with a global
// lock the two handlers could never be inside at the same time.
func TestWritesToDifferentEntitiesRunInParallel(t *testing.T) {
	var g *Guard
	both := make(chan struct{})
	g = slowWriteGuard(t, func(inside int32) {
		if inside == 2 {
			close(both)
		}
		select {
		case <-both:
		case <-time.After(2 * time.Second):
		}
	})
	if g.lockFor("customers", "c1") == g.lockFor("customers", "c2") {
		t.Fatal("c1 and c2 share a stripe; pick other ids")
	}
	var wg sync.WaitGroup
	errs := make(chan error, 2)
	for _, id := range []string{"c1", "c2"} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := g.Call(asW("s-"+id), "credit", json.RawMessage(`{"id":"`+id+`"}`))
			errs <- err
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	select {
	case <-both:
	default:
		t.Fatal("writes to different entities never overlapped: still serialised")
	}
}

// Writes to the same entity stay serialised, so check-then-write is atomic.
func TestWritesToSameEntityAreSerialised(t *testing.T) {
	var maxInside atomic.Int32
	g := slowWriteGuard(t, func(inside int32) {
		for {
			m := maxInside.Load()
			if inside <= m || maxInside.CompareAndSwap(m, inside) {
				break
			}
		}
		time.Sleep(time.Millisecond)
	})
	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := g.Call(asW("s"), "credit", json.RawMessage(`{"id":"c1"}`)); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	if m := maxInside.Load(); m != 1 {
		t.Errorf("%d writes ran at once on one entity, want 1", m)
	}
}

// Same guarantee for a resource that declares no per-level Fields (like the
// example's wallets): a typo'd field cannot be checked against a projection,
// but it is not a declared column either, so strict mode refuses it up front.
func TestStrictFieldsUndeclaredFieldRefusedBeforeWrite(t *testing.T) {
	calls := 0
	cfg := Config{
		StrictFields: true,
		Resources: []Resource{{
			Name: "wallets", Key: "id",
			Columns: []Column{{Name: "id"}, {Name: "balance"}},
		}},
		Tools: []ToolSpec{{Name: "credit", Resource: "wallets", Mutates: true, Handler: func(context.Context, json.RawMessage) (any, error) {
			calls++
			return map[string]any{"id": "w1", "balance": 6}, nil
		}}},
	}
	g := newGuard(t, cfg)
	for range 1 {
		_, err := g.Call(asW("A"), "credit", json.RawMessage(`{"id":"w1","fields":["balanc"]}`))
		if !errors.Is(err, ErrFieldNotAllowed) {
			t.Fatalf("typo field = %v, want ErrFieldNotAllowed", err)
		}
	}
	if calls != 0 {
		t.Fatalf("handler ran %d times: the write was saved before the 403", calls)
	}
	if _, err := g.Call(asW("A"), "credit", json.RawMessage(`{"id":"w1","fields":["balance"]}`)); err != nil || calls != 1 {
		t.Fatalf("declared field: err=%v calls=%d", err, calls)
	}
}

func TestToolListAndRoleChecks(t *testing.T) {
	g := newGuard(t, testConfig())
	list, _ := g.ListTools(as(ViewPublic, "s"))
	if len(list) != 1 || list[0].Name != "get" {
		t.Errorf("read list = %v", list)
	}
	list, _ = g.ListTools(asW("s"))
	if len(list) != 2 {
		t.Errorf("write list = %v", list)
	}
	if _, err := g.Call(as(ViewPII, "s"), "credit", nil); !errors.Is(err, ErrForbidden) {
		t.Errorf("read-pii credit = %v", err)
	}
	if _, err := g.Call(asW("s"), "nope", nil); !errors.Is(err, ErrUnknownTool) {
		t.Errorf("unknown tool = %v", err)
	}
}

func TestStaleReadGuard(t *testing.T) {
	g := newGuard(t, testConfig())
	args := json.RawMessage(`{"id":"c1"}`)

	// agent A reads, agent B writes, then A's write must be blocked until A re-reads.
	if _, err := g.Call(asW("A"), "get", args); err != nil {
		t.Fatal(err)
	}
	if _, err := g.Call(asW("B"), "credit", args); err != nil {
		t.Fatal(err)
	}
	_, err := g.Call(asW("A"), "credit", args)
	if !errors.Is(err, ErrStaleRead) {
		t.Fatalf("A's write after B's = %v, want stale read", err)
	}
	if code, _ := statusOf(err); code != http.StatusConflict {
		t.Errorf("stale status = %d, want 409", code)
	}
	if _, err := g.Call(asW("A"), "get", args); err != nil {
		t.Fatal(err)
	}
	if _, err := g.Call(asW("A"), "credit", args); err != nil {
		t.Errorf("A's write after re-read = %v", err)
	}
	// A's own write does not make A stale.
	if _, err := g.Call(asW("A"), "credit", args); err != nil {
		t.Errorf("A's second write = %v", err)
	}
	// A session that never read the entity is not blocked (read-before-write is not enforced).
	if _, err := g.Call(asW("C"), "credit", args); err != nil {
		t.Errorf("C's blind write = %v", err)
	}
}

func TestHandleHTTP(t *testing.T) {
	g := newGuard(t, testConfig())
	read := g.Handle("customers", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"id": "c1", "email": "a@x.com", "balance": 1})
	})
	write := g.Handle("customers", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusConflict)
		_ = json.NewEncoder(w).Encode(map[string]any{"error": "nope", "email": "leak"})
	})
	do := func(h http.Handler, ctx context.Context, method string) (int, map[string]any) {
		req := httptest.NewRequest(method, "/", nil).WithContext(ctx)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		var out map[string]any
		_ = json.NewDecoder(rec.Body).Decode(&out)
		return rec.Code, out
	}
	if code, _ := do(read, context.Background(), "GET"); code != 401 {
		t.Errorf("no principal = %d", code)
	}
	code, out := do(read, as(ViewPublic, "s"), "GET")
	if code != 200 || out["email"] != nil || out["id"] != "c1" {
		t.Errorf("public = %d %v", code, out)
	}
	if code, out = do(read, as(ViewPII, "s"), "GET"); code != 200 || out["email"] != "a@x.com" {
		t.Errorf("pii = %d %v", code, out)
	}
	// Who may call a write route is the service's decision, not the lib's: a
	// read-pii principal reaches the handler here (its own 409 comes back).
	if code, _ := do(write, as(ViewPII, "s"), "POST"); code != 409 {
		t.Errorf("lib must not gate write routes by level, got %d", code)
	}
	if code, out := do(write, asW("s"), "POST"); code != 409 || out["email"] != "leak" {
		t.Errorf("handler's own error passes through = %d %v", code, out)
	}
}

func TestAuthenticateMiddleware(t *testing.T) {
	var got Principal
	var ok bool
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { got, ok = PrincipalFrom(r.Context()) })
	authn := func(r *http.Request) (Principal, error) {
		if r.Header.Get("X-Who") == "" {
			return Principal{}, errors.New("no creds")
		}
		return Principal{ID: r.Header.Get("X-Who"), View: ViewPII}, nil
	}
	h := Authenticate(authn, next)

	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", "/", nil))
	if ok {
		t.Error("principal set without credentials")
	}
	req := httptest.NewRequest("GET", "/", nil)
	req.Header.Set("X-Who", "bob")
	h.ServeHTTP(httptest.NewRecorder(), req)
	if !ok || got.ID != "bob" || got.View != ViewPII {
		t.Errorf("principal = %+v %v", got, ok)
	}
}

func TestValidate(t *testing.T) {
	cfg := testConfig()
	cfg.Resources[0].Fields[ViewPublic] = []string{"nope"}
	if _, err := New(cfg); err == nil {
		t.Error("unknown field column should fail")
	}
	cfg = testConfig()
	cfg.Tools[0].Resource = "ghost"
	if _, err := New(cfg); err == nil {
		t.Error("unknown tool resource should fail")
	}
	cfg = testConfig()
	cfg.Resources[0].Key = "nope"
	if _, err := New(cfg); err == nil {
		t.Error("unknown key column should fail")
	}
}

func equal(a, b any) bool {
	x, _ := json.Marshal(a)
	y, _ := json.Marshal(b)
	return string(x) == string(y)
}

// The lib has no role logic: with no Authorize hook every tool is listed and
// callable by any authenticated principal; the hook is the service's rule.
func TestAuthorizeHookIsTheOnlyToolGate(t *testing.T) {
	cfg := testConfig()
	cfg.Authorize = nil
	g := newGuard(t, cfg)
	if list, _ := g.ListTools(as(ViewPublic, "s")); len(list) != 2 {
		t.Errorf("no hook: read principal sees %d tools, want 2", len(list))
	}
	if _, err := g.Call(as(ViewPublic, "s"), "credit", json.RawMessage(`{"id":"c1"}`)); err != nil {
		t.Errorf("no hook: credit = %v", err)
	}

	cfg.Authorize = writersOnly
	g = newGuard(t, cfg)
	list, _ := g.ListTools(as(ViewPublic, "s"))
	if len(list) != 1 || list[0].Name != "get" || list[0].Mutates {
		t.Errorf("hook: read list = %+v", list)
	}
	_, err := g.Call(as(ViewPublic, "s"), "credit", json.RawMessage(`{"id":"c1"}`))
	if !errors.Is(err, ErrForbidden) {
		t.Errorf("hook: credit = %v, want ErrForbidden", err)
	}
}
