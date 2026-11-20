// SPDX-License-Identifier: Apache-2.0

package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func stub(t *testing.T, tag string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		w.WriteHeader(http.StatusConflict)
		_, _ = w.Write([]byte(tag + "|" + r.Method + "|" + r.URL.Path + "|" + r.Header.Get("Authorization") + "|" + r.Header.Get("X-Session-ID") + "|" + string(b)))
	}))
}

func post(t *testing.T, h http.Handler, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, path, strings.NewReader(body)))
	return rec
}

func TestSendForwardsToBothTargets(t *testing.T) {
	g, b := stub(t, "g"), stub(t, "b")
	defer g.Close()
	defer b.Close()
	h := newPG(map[string]string{"guarded": g.URL, "baseline": b.URL}).mux()

	rec := post(t, h, "/api/send", `{"target":"both","method":"POST","path":"/wallets/w1/credit","token":"dev-write","session":"s1","body":"{\"amount_minor\":5}"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body)
	}
	var out []sendRes
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if len(out) != 2 || out[0].Target != "guarded" || out[1].Target != "baseline" {
		t.Fatalf("unexpected results: %+v", out)
	}
	for i, tag := range []string{"g", "b"} {
		want := tag + `|POST|/wallets/w1/credit|Bearer dev-write|s1|{"amount_minor":5}`
		if out[i].Status != http.StatusConflict || out[i].Body != want {
			t.Errorf("%s: status %d body %q, want 409 %q", tag, out[i].Status, out[i].Body, want)
		}
	}
}

func TestSendRejectsBadInput(t *testing.T) {
	h := newPG(map[string]string{}).mux()
	for _, body := range []string{
		`{"target":"both","method":"DELETE","path":"/x"}`,
		`{"target":"both","method":"GET","path":"http://evil/x"}`,
		`{"target":"nope","method":"GET","path":"/x"}`,
		`not json`,
	} {
		if rec := post(t, h, "/api/send", body); rec.Code != http.StatusBadRequest {
			t.Errorf("%s: got %d, want 400", body, rec.Code)
		}
	}
}

func TestBenchValidation(t *testing.T) {
	h := newPG(map[string]string{}).mux()
	for _, c := range []struct{ path, body string }{
		{"/api/bench/agentload", `{"target":"guarded","agents":0,"duration":"5s","think":"0s","api":"rest"}`},
		{"/api/bench/agentload", `{"target":"guarded","agents":5,"duration":"; rm","think":"0s","api":"rest"}`},
		{"/api/bench/agentload", `{"target":"x","agents":5,"duration":"5s","think":"0s","api":"rest"}`},
		{"/api/bench/compare", `{"reps":0,"agents":1,"duration":"3s","think":"0s"}`},
	} {
		if rec := post(t, h, c.path, c.body); rec.Code != http.StatusBadRequest {
			t.Errorf("%s %s: got %d, want 400", c.path, c.body, rec.Code)
		}
	}
}

func TestRepeatSummarizes(t *testing.T) {
	g, b := stub(t, "g"), stub(t, "b")
	defer g.Close()
	defer b.Close()
	h := newPG(map[string]string{"guarded": g.URL, "baseline": b.URL}).mux()
	rec := post(t, h, "/api/repeat", `{"path":"/wallets/w1","token":"dev-read","n":10}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body)
	}
	var out struct {
		N       int
		Guarded pctl
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if out.N != 10 || out.Guarded.Bad != 10 {
		t.Errorf("unexpected summary: %+v", out)
	}
}

func TestBenchRunValidation(t *testing.T) {
	h := newPG(map[string]string{}).mux()
	for _, body := range []string{
		`not json`,
		`{"bench":"rm -rf","size":"quick"}`,
		`{"bench":"nope","size":"huge"}`,
		`{"bench":"nope","size":"quick"}`,
	} {
		if rec := post(t, h, "/api/bench/run", body); rec.Code != http.StatusBadRequest {
			t.Errorf("%s: got %d, want 400", body, rec.Code)
		}
	}
}

func TestBenchCatalogComplete(t *testing.T) {
	p := newPG(map[string]string{"guarded": "http://g", "baseline": "http://b"})
	p.root, p.bin = "/repo", "/bin"
	for _, b := range catalog {
		for _, s := range sizes {
			if b.Info[s] == "" {
				t.Errorf("%s: no info for size %s", b.ID, s)
			}
			name, _, _ := b.build(p, s)
			if name == "" {
				t.Errorf("%s/%s: empty command", b.ID, s)
			}
		}
	}
	rec := httptest.NewRecorder()
	p.mux().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/bench/options", nil))
	var out struct{ Benchmarks []struct{ ID string } }
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil || len(out.Benchmarks) != len(catalog) {
		t.Fatalf("options: %v, %s", err, rec.Body)
	}
}
