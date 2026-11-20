// SPDX-License-Identifier: Apache-2.0

package accessgate

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
)

// 2^53 + 1: the first integer a float64 cannot hold.
const bigInt = "9007199254740993"

// Numbers must leave the guard the way they went in: a large integer is not
// rounded and a decimal keeps its digits (a balance of 1.10 is not 1.1).
func TestHandleKeepsNumbersExact(t *testing.T) {
	g := newGuard(t, testConfig())
	body := `{"id":"c1","email":"a@x.com","phone":"+1","balance":` + bigInt + `,"price":1.10,"rate":1e2}`
	rec := serveAs(g.Handle("customers", replyWith("application/json", body)), ViewPII)
	got := rec.Body.String()
	for _, want := range []string{`"balance":` + bigInt, `"price":1.10`, `"rate":1e2`} {
		if !strings.Contains(got, want) {
			t.Errorf("response %s lacks %s", got, want)
		}
	}
}

func TestCallAndProjectKeepNumbersExact(t *testing.T) {
	cfg := testConfig()
	cfg.Tools = append(cfg.Tools, ToolSpec{Name: "big", Resource: "customers", Handler: func(context.Context, json.RawMessage) (any, error) {
		return struct {
			ID      string `json:"id"`
			Balance int64  `json:"balance"`
		}{"c1", 9007199254740993}, nil
	}})
	g := newGuard(t, cfg)

	out, err := g.Call(as(ViewPII, "s"), "big", nil)
	if err != nil {
		t.Fatal(err)
	}
	if raw, _ := json.Marshal(out); !strings.Contains(string(raw), `"balance":`+bigInt) {
		t.Errorf("Call = %s, want balance %s", raw, bigInt)
	}

	out, err = g.Project(Principal{View: ViewPII}, "customers", map[string]any{"id": "c1", "balance": int64(9007199254740993)}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if raw, _ := json.Marshal(out); !strings.Contains(string(raw), `"balance":`+bigInt) {
		t.Errorf("Project = %s, want balance %s", raw, bigInt)
	}
}

// A MaskFunc sees a number as a json.Number (exact), and a mask that keeps
// only part of a number can still be written against it.
func TestMaskFuncGetsJSONNumber(t *testing.T) {
	var seen []string
	cfg := Config{Resources: []Resource{{
		Name: "accounts", Key: "id",
		Columns: []Column{
			{Name: "id"},
			{Name: "pin", Class: PII, MaskFunc: func(v any) any {
				seen = append(seen, fmt.Sprintf("%T", v))
				return "***"
			}},
		},
	}}}
	g := newGuard(t, cfg)
	out, err := g.Project(Principal{View: ViewPublic}, "accounts", map[string]any{"id": "a1", "pin": 1234}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(seen) != 1 || seen[0] != "json.Number" {
		t.Errorf("MaskFunc got %v, want [json.Number]", seen)
	}
	if raw, _ := json.Marshal(out); string(raw) != `{"id":"a1","pin":"***"}` {
		t.Errorf("projected = %s", raw)
	}
}

// KeepLast hides a number completely: it only keeps characters of strings.
func TestKeepLastMasksNumbersFully(t *testing.T) {
	if got := KeepLast(4)(json.Number("712345678")); got != "***" {
		t.Errorf("KeepLast on a number = %v, want ***", got)
	}
}

// Decoding is strict: anything after the first JSON value is an error, as with
// json.Unmarshal, so a handler cannot smuggle data past the projection.
func TestHandleRejectsDataAfterJSONValue(t *testing.T) {
	g := newGuard(t, testConfig())
	body := `{"id":"c1","balance":5} {"email":"a@x.com"}`
	rec := serveAs(g.Handle("customers", replyWith("application/json", body)), ViewPublic)
	if rec.Code != http.StatusInternalServerError || strings.Contains(rec.Body.String(), "a@x.com") {
		t.Errorf("trailing data = %d %q, want 500 and no handler data", rec.Code, rec.Body.String())
	}
}

// KeepLast keeps the last n characters only when something is hidden: a value
// of n characters or fewer is masked completely.
func TestKeepLastHidesShortValues(t *testing.T) {
	mask := KeepLast(4)
	for in, want := range map[string]string{
		"0712345678":  "******5678",
		"12345":       "*2345",
		"1234":        "****",
		"12":          "**",
		"":            "",
		"héllo wörld": "*******örld",
	} {
		if got := mask(in); got != want {
			t.Errorf("KeepLast(4)(%q) = %q, want %q", in, got, want)
		}
	}
}
