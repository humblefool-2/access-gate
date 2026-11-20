// SPDX-License-Identifier: Apache-2.0

package accessgate

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

const customerBody = `{"id":"c1","email":"a@x.com","phone":"+1","balance":5}`

// replyWith returns a handler that answers 200 with the given content type
// (none when ct is empty) and body, the way a service's own handler would.
func replyWith(ct, body string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if ct != "" {
			w.Header().Set("Content-Type", ct)
		}
		_, _ = w.Write([]byte(body))
	}
}

func serveAs(h http.HandlerFunc, level View) *httptest.ResponseRecorder {
	r := httptest.NewRequest(http.MethodGet, "/customers/c1", nil).WithContext(as(level, "s"))
	r.SetPathValue("id", "c1")
	rec := httptest.NewRecorder()
	h(rec, r)
	return rec
}

// A ViewPublic caller must never see PII, whatever JSON content type the
// handler used, or none at all.
func TestHandleProjectsEveryJSONContentType(t *testing.T) {
	g := newGuard(t, testConfig())
	for _, ct := range []string{
		"application/json",
		"application/json; charset=utf-8",
		"application/problem+json",
		"application/vnd.api+json",
		"APPLICATION/JSON",
		"", // the handler forgot the header
	} {
		rec := serveAs(g.Handle("customers", replyWith(ct, customerBody)), ViewPublic)
		if rec.Code != http.StatusOK {
			t.Errorf("content type %q: status %d, want 200", ct, rec.Code)
		}
		if body := rec.Body.String(); strings.Contains(body, "a@x.com") || strings.Contains(body, `"+1"`) {
			t.Errorf("content type %q leaked PII: %s", ct, body)
		}
	}
}

// A 2xx body the guard cannot project is refused, not passed to the caller.
func TestHandleRefusesNonJSONSuccess(t *testing.T) {
	for _, c := range []struct{ name, ct, body string }{
		{"html", "text/html", "<p>a@x.com</p>"},
		{"octet-stream", "application/octet-stream", "a@x.com"},
		{"text", "text/plain", "a@x.com"},
		{"no type, not JSON", "", "contact a@x.com"},
		{"json body with a text type", "text/plain", customerBody},
	} {
		g := newGuard(t, testConfig())
		rec := serveAs(g.Handle("customers", replyWith(c.ct, c.body)), ViewPublic)
		if rec.Code != http.StatusInternalServerError {
			t.Errorf("%s: status %d, want 500", c.name, rec.Code)
		}
		if body := rec.Body.String(); strings.Contains(body, "a@x.com") || !strings.Contains(body, ErrNotJSON.Error()) {
			t.Errorf("%s: body %q should say the response is not JSON and hold no handler data", c.name, body)
		}
	}
}

// A resource that really serves files or HTML opts out; JSON is still projected.
func TestHandleAllowNonJSONPassesThrough(t *testing.T) {
	cfg := testConfig()
	cfg.Resources[0].AllowNonJSON = true
	g := newGuard(t, cfg)

	rec := serveAs(g.Handle("customers", replyWith("text/html", "<p>report</p>")), ViewPublic)
	if rec.Code != http.StatusOK || rec.Body.String() != "<p>report</p>" {
		t.Errorf("non-JSON with AllowNonJSON = %d %q, want the handler's response unchanged", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("Content-Type"); got != "text/html" {
		t.Errorf("Content-Type = %q, want text/html", got)
	}

	rec = serveAs(g.Handle("customers", replyWith("application/json", customerBody)), ViewPublic)
	if strings.Contains(rec.Body.String(), "a@x.com") {
		t.Errorf("AllowNonJSON must not stop JSON being projected: %s", rec.Body.String())
	}
}

// Nothing to project: an empty 2xx and the handler's own errors pass through.
func TestHandleEmptyAndErrorResponsesPassThrough(t *testing.T) {
	g := newGuard(t, testConfig())

	noContent := func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusNoContent) }
	if rec := serveAs(g.Handle("customers", noContent), ViewPublic); rec.Code != http.StatusNoContent || rec.Body.Len() != 0 {
		t.Errorf("204 = %d %q, want 204 and no body", rec.Code, rec.Body.String())
	}

	notFound := func(w http.ResponseWriter, r *http.Request) { http.Error(w, "no such customer", http.StatusNotFound) }
	if rec := serveAs(g.Handle("customers", notFound), ViewPublic); rec.Code != http.StatusNotFound {
		t.Errorf("handler 404 = %d, want it passed through", rec.Code)
	}
}

// The handler's own status and headers must reach the caller with the
// projected body; the headers that describe the full body must not.
func TestHandleCopiesHandlerHeaders(t *testing.T) {
	g := newGuard(t, testConfig())
	created := func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("Content-Type", "application/problem+json")
		h.Set("Location", "/customers/c1")
		h.Set("Cache-Control", "private, max-age=60")
		h.Add("Set-Cookie", "a=1")
		h.Add("Set-Cookie", "b=2")
		h.Set("X-Request-Id", "req-7")
		// describe the full body, which the caller does not get
		h.Set("Content-Length", "999")
		h.Set("ETag", `"full-object"`)
		h.Set("Content-MD5", "abc")
		h.Set("Content-Encoding", "identity")
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(customerBody))
	}
	rec := serveAs(g.Handle("customers", created), ViewPublic)

	if rec.Code != http.StatusCreated {
		t.Errorf("status = %d, want 201", rec.Code)
	}
	for k, want := range map[string]string{
		"Location":      "/customers/c1",
		"Cache-Control": "private, max-age=60",
		"X-Request-Id":  "req-7",
		"Content-Type":  "application/problem+json",
	} {
		if got := rec.Header().Get(k); got != want {
			t.Errorf("%s = %q, want %q", k, got, want)
		}
	}
	if got := rec.Header().Values("Set-Cookie"); len(got) != 2 || got[0] != "a=1" || got[1] != "b=2" {
		t.Errorf("Set-Cookie = %v, want both cookies", got)
	}
	for _, k := range []string{"Content-Length", "ETag", "Content-MD5", "Content-Encoding"} {
		if got := rec.Header().Get(k); got != "" {
			t.Errorf("%s = %q, must not be copied", k, got)
		}
	}
	if strings.Contains(rec.Body.String(), "a@x.com") {
		t.Errorf("PII leaked: %s", rec.Body.String())
	}
}

// Without a JSON content type from the handler the caller still gets one.
func TestHandleDefaultsContentTypeToJSON(t *testing.T) {
	g := newGuard(t, testConfig())
	rec := serveAs(g.Handle("customers", replyWith("", customerBody)), ViewPublic)
	if got := rec.Header().Get("Content-Type"); got != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", got)
	}
}

func doAs(h http.HandlerFunc, method, session string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, "/customers/c1", nil).WithContext(as(ViewPII, session))
	r.SetPathValue("id", "c1")
	rec := httptest.NewRecorder()
	h(rec, r)
	return rec
}

// A 2xx write that passes through (a 204) still makes other sessions' reads
// of that entity stale; a handler's own 404 reaches the caller unchanged.
func TestPassthroughBumpsVersion(t *testing.T) {
	g := newGuard(t, testConfig())
	read := g.Handle("customers", replyWith("application/json", customerBody))
	noContent := g.Handle("customers", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusNoContent) })
	notFound := g.Handle("customers", func(w http.ResponseWriter, r *http.Request) { http.Error(w, "gone", http.StatusNotFound) })

	doAs(read, http.MethodGet, "a")
	if rec := doAs(noContent, http.MethodPost, "b"); rec.Code != http.StatusNoContent {
		t.Fatalf("204 write = %d", rec.Code)
	}
	if rec := doAs(noContent, http.MethodPost, "a"); rec.Code == http.StatusNoContent {
		t.Errorf("session a wrote after b's 204 write without re-reading; want a stale-read refusal")
	}
	if rec := doAs(notFound, http.MethodGet, "c"); rec.Code != http.StatusNotFound {
		t.Errorf("handler 404 = %d", rec.Code)
	}
}
