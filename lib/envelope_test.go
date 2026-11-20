// SPDX-License-Identifier: Apache-2.0

package accessgate

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

const page = `{"data":[{"id":"c1","email":"a@x.com","phone":"+1"},{"id":"c2","email":"b@x.com","phone":"+2"},{"id":"c3","email":"c@x.com","phone":"+3"}],` +
	`"next":"cur-9","total":3,"owner_email":"boss@x.com"}`

func envelopeConfig() Config {
	cfg := testConfig()
	r := &cfg.Resources[0]
	r.ListPath, r.ListKeep = "data", []string{"next", "total"}
	return cfg
}

func TestEnvelopeRowsAreProjectedAndExtrasAreDropped(t *testing.T) {
	g := newGuard(t, envelopeConfig())
	rec := serveAs(g.Handle("customers", replyWith("application/json", page)), ViewPublic)
	want := `{"data":[{"id":"c1","phone":"***"},{"id":"c2","phone":"***"},{"id":"c3","phone":"***"}],"next":"cur-9","total":3}` + "\n"
	if rec.Code != http.StatusOK || rec.Body.String() != want {
		t.Errorf("envelope = %d %q\nwant %q", rec.Code, rec.Body.String(), want)
	}
	for _, leak := range []string{"@x.com", "owner_email"} {
		if strings.Contains(rec.Body.String(), leak) {
			t.Errorf("response leaks %q", leak)
		}
	}
}

func TestEnvelopeAppliesScopeAndMaxRows(t *testing.T) {
	cfg := envelopeConfig()
	r := &cfg.Resources[0]
	r.Scope = func(p Principal, id string) bool { return id != "c1" }
	r.MaxRows = 1
	g := newGuard(t, cfg)
	h := g.Handle("customers", replyWith("application/json", page))
	rec := httptest.NewRecorder()
	h(rec, httptest.NewRequest(http.MethodGet, "/customers", nil).WithContext(as(ViewPublic, "s"))) // a list route: no id
	want := `{"data":[{"id":"c2","phone":"***"}],"next":"cur-9","total":3}` + "\n"
	if rec.Body.String() != want {
		t.Errorf("scoped, capped envelope = %q, want %q", rec.Body.String(), want)
	}
}

// A response that does not have the declared shape is refused, not passed on.
func TestEnvelopeWithWrongShapeIsRefused(t *testing.T) {
	g := newGuard(t, envelopeConfig())
	for _, body := range []string{`[{"id":"c1","email":"a@x.com"}]`, `{"items":[{"id":"c1","email":"a@x.com"}]}`, `{"data":"a@x.com"}`} {
		rec := serveAs(g.Handle("customers", replyWith("application/json", body)), ViewPublic)
		if rec.Code == http.StatusOK || strings.Contains(rec.Body.String(), "a@x.com") {
			t.Errorf("body %s = %d %q, want a refusal with no data", body, rec.Code, rec.Body.String())
		}
	}
}

func TestListKeepNeedsListPath(t *testing.T) {
	cfg := testConfig()
	cfg.Resources[0].ListKeep = []string{"next"}
	if _, err := New(cfg); err == nil {
		t.Error("ListKeep without ListPath was accepted")
	}
}
