// SPDX-License-Identifier: Apache-2.0

package accessgate

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func postTool(g *Guard, name, body string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(http.MethodPost, "/tools/"+name, strings.NewReader(body)).WithContext(asW("s"))
	rec := httptest.NewRecorder()
	g.ToolsHandler().ServeHTTP(rec, r)
	return rec
}

func TestToolsHandlerRefusesBadArguments(t *testing.T) {
	g := newGuard(t, testConfig())
	for _, body := range []string{`{"id":`, `[1,2]`, `"id"`, `{"fields":"email"}`, `{"fields":[1]}`} {
		if rec := postTool(g, "get", body); rec.Code != http.StatusBadRequest {
			t.Errorf("body %s = %d, want 400", body, rec.Code)
		}
	}
	for _, body := range []string{``, `{}`, `null`, `{"id":"c1","fields":["id"]}`} {
		if rec := postTool(g, "get", body); rec.Code != http.StatusOK {
			t.Errorf("body %q = %d, want 200", body, rec.Code)
		}
	}
}

func TestToolsHandlerLimitsBodySize(t *testing.T) {
	cfg := testConfig()
	cfg.MaxBodyBytes = 64
	g := newGuard(t, cfg)
	big := `{"id":"c1","pad":"` + strings.Repeat("x", 200) + `"}`
	if rec := postTool(g, "get", big); rec.Code != http.StatusRequestEntityTooLarge {
		t.Errorf("oversized body = %d, want 413", rec.Code)
	}
	if rec := postTool(g, "get", `{"id":"c1"}`); rec.Code != http.StatusOK {
		t.Errorf("small body = %d, want 200", rec.Code)
	}

	// default limit is 1 MiB
	g = newGuard(t, testConfig())
	huge := `{"id":"c1","pad":"` + strings.Repeat("x", 2<<20) + `"}`
	if rec := postTool(g, "get", huge); rec.Code != http.StatusRequestEntityTooLarge {
		t.Errorf("2 MiB body with the default limit = %d, want 413", rec.Code)
	}
}

// Call (used by an MCP server directly) rejects bad arguments too.
func TestCallRejectsBadArguments(t *testing.T) {
	g := newGuard(t, testConfig())
	if _, err := g.Call(asW("s"), "get", []byte(`[1]`)); err == nil {
		t.Error("Call with array arguments succeeded, want ErrBadArgs")
	}
}
