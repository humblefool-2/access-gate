// SPDX-License-Identifier: Apache-2.0

package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func call(t *testing.T, h http.Handler, method, path, key, body string) (int, map[string]any) {
	t.Helper()
	return callAs(t, h, method, path, key, "", body)
}

// callAs is call within a named agent session.
func callAs(t *testing.T, h http.Handler, method, path, key, session, body string) (int, map[string]any) {
	t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	if key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	if session != "" {
		req.Header.Set("X-Session-ID", session)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	var out map[string]any
	_ = json.NewDecoder(rec.Body).Decode(&out)
	return rec.Code, out
}

func wallet(t *testing.T, h http.Handler, customerID string) map[string]any {
	t.Helper()
	code, out := call(t, h, "GET", "/customers/"+customerID, "rk", "")
	if code != 200 {
		t.Fatalf("get customer = %d", code)
	}
	return out["wallet"].(map[string]any)
}

func TestAuthMatrix(t *testing.T) {
	h := newMux(testDB(t), testAuthn(t))
	body := `{"email":"a@x.com"}`
	for _, c := range []struct {
		method, path, key, body string
		want                    int
	}{
		{"GET", "/customers/nope", "", "", 401},
		{"GET", "/customers/nope", "bad", "", 401},
		{"GET", "/customers/nope", "rk", "", 404},
		{"GET", "/customers/nope", "wk", "", 404},
		{"POST", "/customers", "", body, 401},
		{"POST", "/customers", "rk", body, 403},
		{"POST", "/customers", "wk", body, 200},
	} {
		if code, _ := call(t, h, c.method, c.path, c.key, c.body); code != c.want {
			t.Errorf("%s %s key=%q = %d, want %d", c.method, c.path, c.key, code, c.want)
		}
	}
}

func TestWalletFlow(t *testing.T) {
	h := newMux(testDB(t), testAuthn(t))

	code, out := call(t, h, "POST", "/customers", "wk", `{"customer_id":"c1","email":"a@x.com","phone":"+123"}`)
	if code != 200 || out["email"] != "a@x.com" || out["phone"] != "+123" {
		t.Fatalf("create = %d %v", code, out)
	}
	wid := out["wallet"].(map[string]any)["wallet_id"].(string)

	if code, _ := call(t, h, "POST", "/customers", "wk", `{"customer_id":"c2","email":"a@x.com"}`); code != 409 {
		t.Errorf("duplicate email = %d, want 409", code)
	}

	call(t, h, "POST", "/wallets/"+wid+"/credit", "wk", `{"amount_minor":1000}`)
	_, out = call(t, h, "POST", "/wallets/"+wid+"/holds", "wk", `{"amount_minor":300,"reason":"bet"}`)
	holdID := out["holds"].([]any)[0].(map[string]any)["hold_id"].(string)

	w := wallet(t, h, "c1")
	if w["balance_minor"] != 1000.0 || w["blocked_minor"] != 300.0 || w["available_minor"] != 700.0 {
		t.Fatalf("wallet = %v", w)
	}

	if code, _ := call(t, h, "POST", "/wallets/"+wid+"/debit", "wk", `{"amount_minor":800}`); code != 409 {
		t.Errorf("debit over available = %d, want 409", code)
	}
	if code, _ := call(t, h, "POST", "/wallets/"+wid+"/debit", "wk", `{"amount_minor":700}`); code != 200 {
		t.Errorf("debit within available = %d, want 200", code)
	}

	call(t, h, "POST", "/holds/"+holdID+"/release", "wk", "")
	if w := wallet(t, h, "c1"); w["available_minor"] != 300.0 || w["blocked_minor"] != 0.0 {
		t.Errorf("after release = %v", w)
	}
	if code, _ := call(t, h, "POST", "/holds/"+holdID+"/release", "wk", ""); code != 409 {
		t.Errorf("double release = %d, want 409", code)
	}

	call(t, h, "POST", "/wallets/"+wid+"/status", "wk", `{"status":"frozen"}`)
	if code, _ := call(t, h, "POST", "/wallets/"+wid+"/debit", "wk", `{"amount_minor":1}`); code != 409 {
		t.Errorf("debit on frozen = %d, want 409", code)
	}
	if code, _ := call(t, h, "POST", "/wallets/"+wid+"/credit", "wk", `{"amount_minor":0}`); code != 400 {
		t.Errorf("zero amount = %d, want 400", code)
	}
}

func TestPIIProjection(t *testing.T) {
	h := newMux(testDB(t), testAuthn(t))
	call(t, h, "POST", "/customers", "wk", `{"customer_id":"c1","email":"a@x.com","phone":"+123"}`)

	for _, c := range []struct {
		key          string
		email, phone any
	}{
		{"rk", nil, "***"},
		{"pk", "a@x.com", "+123"},
		{"wk", "a@x.com", "+123"},
	} {
		code, out := call(t, h, "GET", "/customers/c1", c.key, "")
		if code != 200 || out["email"] != c.email || out["phone"] != c.phone {
			t.Errorf("key=%s: %d %v", c.key, code, out)
		}
	}

	_, out := call(t, h, "GET", "/customers/c1?fields=customer_id,email", "rk", "")
	if len(out) != 1 || out["customer_id"] != "c1" {
		t.Errorf("narrowed read session = %v", out)
	}
}

func TestToolsFollowSessionLevel(t *testing.T) {
	h := newMux(testDB(t), testAuthn(t))
	_, out := call(t, h, "POST", "/customers", "wk", `{"customer_id":"c1","email":"a@x.com"}`)
	wid := out["wallet"].(map[string]any)["wallet_id"].(string)

	_, out = call(t, h, "GET", "/tools", "rk", "")
	if n := len(out["tools"].([]any)); n != 2 {
		t.Errorf("read session sees %d tools, want 2", n)
	}
	_, out = call(t, h, "GET", "/tools", "wk", "")
	if n := len(out["tools"].([]any)); n != 5 {
		t.Errorf("write session sees %d tools, want 5", n)
	}

	if code, _ := call(t, h, "POST", "/tools/credit", "pk", `{"id":"`+wid+`","amount_minor":10}`); code != 403 {
		t.Errorf("read-pii credit tool = %d, want 403", code)
	}
	code, out := call(t, h, "POST", "/tools/credit", "wk", `{"id":"`+wid+`","amount_minor":10}`)
	if code != 200 || out["balance_minor"] != 10.0 {
		t.Errorf("write credit tool = %d %v", code, out)
	}
	if code, _ := call(t, h, "POST", "/tools/debit", "wk", `{"id":"`+wid+`","amount_minor":99}`); code != 409 {
		t.Errorf("overdraw via tool = %d, want 409", code)
	}
	_, out = call(t, h, "POST", "/tools/get_customer", "rk", `{"id":"c1"}`)
	if out["email"] != nil || out["customer_id"] != "c1" {
		t.Errorf("get_customer for read session = %v", out)
	}
}

func TestAgentWorkflow(t *testing.T) {
	h := newMux(testDB(t), testAuthn(t))
	_, out := call(t, h, "POST", "/customers", "wk", `{"customer_id":"c1","email":"a@x.com"}`)
	wid := out["wallet"].(map[string]any)["wallet_id"].(string)
	credit := `{"id":"` + wid + `","amount_minor":10}`

	// two write agents read the same wallet, then one writes: the other is blocked until it re-reads.
	callAs(t, h, "GET", "/wallets/"+wid, "wk", "agent-a", "")
	callAs(t, h, "GET", "/wallets/"+wid, "wk", "agent-b", "")
	if code, _ := callAs(t, h, "POST", "/tools/credit", "wk", "agent-a", credit); code != 200 {
		t.Fatalf("agent-a credit = %d", code)
	}
	code, out := callAs(t, h, "POST", "/tools/credit", "wk", "agent-b", credit)
	if code != 409 || !strings.Contains(out["error"].(string), "read it again") {
		t.Fatalf("agent-b stale credit = %d %v", code, out)
	}
	callAs(t, h, "GET", "/wallets/"+wid, "wk", "agent-b", "")
	if code, _ := callAs(t, h, "POST", "/tools/credit", "wk", "agent-b", credit); code != 200 {
		t.Errorf("agent-b credit after re-read = %d", code)
	}

	// a read-only agent trying to write fails.
	if code, _ := callAs(t, h, "POST", "/tools/credit", "rk", "reader", credit); code != 403 {
		t.Errorf("reader credit = %d, want 403", code)
	}
}
