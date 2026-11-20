// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	accessgate "github.com/humblefool-2/access-gate/lib"
)

// mcpSession sends one JSON-RPC message per line and returns the responses by id.
func mcpSession(t *testing.T, role string, lines ...string) map[string]map[string]any {
	t.Helper()
	db := testDB(t)
	if err := seedDemo(db); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	view := accessgate.ViewPII
	if role == "read" {
		view = accessgate.ViewPublic
	}
	p := accessgate.Principal{ID: "agent", SessionID: "mcp-test", View: view, Attrs: map[string]string{"role": role}}
	if err := serveMCP(newGuard(&app{db: db}), p, strings.NewReader(strings.Join(lines, "\n")+"\n"), &out); err != nil {
		t.Fatal(err)
	}
	got := map[string]map[string]any{}
	for _, l := range strings.Split(strings.TrimSpace(out.String()), "\n") {
		if l == "" {
			continue
		}
		var m map[string]any
		if err := json.Unmarshal([]byte(l), &m); err != nil {
			t.Fatalf("response is not JSON: %q", l)
		}
		if m["jsonrpc"] != "2.0" {
			t.Errorf("jsonrpc = %v", m["jsonrpc"])
		}
		id, _ := json.Marshal(m["id"])
		got[string(id)] = m
	}
	return got
}

func resultOf(t *testing.T, m map[string]any) map[string]any {
	t.Helper()
	r, ok := m["result"].(map[string]any)
	if !ok {
		t.Fatalf("no result in %v", m)
	}
	return r
}

func toolNames(t *testing.T, m map[string]any) []string {
	var names []string
	for _, x := range resultOf(t, m)["tools"].([]any) {
		tool := x.(map[string]any)
		if _, ok := tool["inputSchema"].(map[string]any); !ok {
			t.Errorf("tool %v has no inputSchema object", tool["name"])
		}
		names = append(names, tool["name"].(string))
	}
	return names
}

func TestMCPInitializeAndList(t *testing.T) {
	got := mcpSession(t, "read",
		`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-03-26"}}`,
		`{"jsonrpc":"2.0","method":"notifications/initialized"}`,
		`{"jsonrpc":"2.0","id":2,"method":"tools/list"}`,
		`{"jsonrpc":"2.0","id":3,"method":"ping"}`,
		`not json`,
		`{"jsonrpc":"2.0","id":4,"method":"nope"}`,
	)
	init := resultOf(t, got["1"])
	if init["protocolVersion"] != "2025-03-26" || init["capabilities"].(map[string]any)["tools"] == nil {
		t.Errorf("initialize = %v", init)
	}
	if len(got) != 5 { // the notification got no response; the parse error did (id null)
		t.Errorf("%d responses, want 5: %v", len(got), got)
	}
	if names := toolNames(t, got["2"]); strings.Join(names, ",") != "get_customer,get_wallet" {
		t.Errorf("read tools = %v (a read-only principal must not see write tools)", names)
	}
	if got["null"]["error"].(map[string]any)["code"] != -32700.0 {
		t.Errorf("parse error = %v", got["null"])
	}
	if got["4"]["error"].(map[string]any)["code"] != -32601.0 {
		t.Errorf("unknown method = %v", got["4"])
	}
	// write principal sees all five
	got = mcpSession(t, "write", `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`)
	if n := len(toolNames(t, got["1"])); n != 5 {
		t.Errorf("write tools = %d", n)
	}
}

func textOf(t *testing.T, m map[string]any) (string, bool) {
	r := resultOf(t, m)
	c := r["content"].([]any)[0].(map[string]any)
	return c["text"].(string), r["isError"].(bool)
}

func TestMCPCallsGoThroughTheGuard(t *testing.T) {
	got := mcpSession(t, "read",
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"get_customer","arguments":{"id":"cus_demo1"}}}`,
		`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"credit","arguments":{"id":"wal_demo1","amount_minor":5}}}`,
		`{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"nope","arguments":{}}}`,
		`{"jsonrpc":"2.0","id":4,"method":"tools/call","params":{"name":"get_wallet","arguments":{"id":"wal_missing"}}}`,
		`{"jsonrpc":"2.0","id":5,"method":"tools/call","params":{}}`,
	)
	text, isErr := textOf(t, got["1"])
	if isErr || strings.Contains(text, "ana@example.com") || !strings.Contains(text, "cus_demo1") || !strings.Contains(text, "***") {
		t.Errorf("get_customer as read = %v %s (email must be dropped, phone masked)", isErr, text)
	}
	if text, isErr := textOf(t, got["2"]); !isErr || !strings.Contains(text, "not authorized") {
		t.Errorf("credit as read = %v %q", isErr, text)
	}
	if got["3"]["error"].(map[string]any)["code"] != -32602.0 {
		t.Errorf("unknown tool = %v", got["3"])
	}
	if text, isErr := textOf(t, got["4"]); !isErr || strings.Contains(text, "sql") {
		t.Errorf("missing wallet = %v %q", isErr, text)
	}
	if got["5"]["error"].(map[string]any)["code"] != -32602.0 {
		t.Errorf("no name = %v", got["5"])
	}
}

// One MCP connection is one session, so the stale-read guard
// works for MCP agents exactly as for HTTP ones.
func TestMCPWriteSession(t *testing.T) {
	db := testDB(t)
	_ = seedDemo(db)
	g := newGuard(&app{db: db})
	p := accessgate.Principal{ID: "agent", SessionID: "mcp-s1", View: accessgate.ViewPII, Attrs: map[string]string{"role": "write"}}
	var out bytes.Buffer
	in := `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"credit","arguments":{"id":"wal_demo2","amount_minor":100}}}` + "\n"
	if err := serveMCP(g, p, strings.NewReader(in), &out); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), `\"balance_minor\":4600`) {
		t.Errorf("credit result = %s", out.String())
	}
}
