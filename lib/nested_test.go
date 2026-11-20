// SPDX-License-Identifier: Apache-2.0

package accessgate

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

func peopleConfig() Config {
	return Config{Resources: []Resource{{
		Name: "people",
		Key:  "meta.id",
		Columns: []Column{
			{Name: "meta.id"},
			{Name: "email", Class: PII, Mask: Drop},
			{Name: "address.city"},
			{Name: "address.street", Class: PII, Mask: Drop},
			{Name: "address.zip", Class: PII, MaskFunc: KeepLast(2)},
			{Name: "phone", Class: PII, MaskFunc: KeepLast(4)},
			{Name: "tags"},
		},
	}}}
}

var person = map[string]any{
	"meta":    map[string]any{"id": "p1", "secret": "s"},
	"email":   "a@x.com",
	"phone":   "+40700000000",
	"tags":    []any{"a", map[string]any{"b": 1}},
	"address": map[string]any{"city": "Cluj", "street": "Main 1", "zip": "12345", "extra": "e"},
	"extra":   "top",
}

func project(t *testing.T, cfg Config, level View, requested ...string) (any, error) {
	t.Helper()
	g := newGuard(t, cfg)
	if len(requested) == 0 {
		requested = nil
	}
	raw, _ := json.Marshal(person)
	var v any
	_ = json.Unmarshal(raw, &v)
	return g.Project(Principal{ID: "u", View: level}, "people", v, requested)
}

func TestNestedProjectionPerLevel(t *testing.T) {
	pub := map[string]any{
		"meta":    map[string]any{"id": "p1"}, // meta.secret is undeclared: PII
		"phone":   "TTTTTT",                   // replaced below
		"tags":    []any{"a", map[string]any{"b": 1.0}},
		"address": map[string]any{"city": "Cluj", "zip": "***45"},
	}
	pub["phone"] = "********0000"
	got, err := project(t, peopleConfig(), ViewPublic)
	if err != nil || !equal(got, pub) {
		t.Errorf("public = %v, %v\nwant %v", got, err, pub)
	}
	got, err = project(t, peopleConfig(), ViewPII)
	if err != nil || !equal(got, person) {
		t.Errorf("pii = %v, %v\nwant %v", got, err, person)
	}
}

func TestNestedRequestedNarrowing(t *testing.T) {
	cases := []struct {
		level     View
		requested []string
		want      any
	}{
		{ViewPublic, []string{"address.city"}, map[string]any{"address": map[string]any{"city": "Cluj"}}},
		{ViewPublic, []string{"address"}, map[string]any{"address": map[string]any{"city": "Cluj", "zip": "***45"}}},
		{ViewPublic, []string{"address.street"}, map[string]any{}}, // PII, dropped, no empty "address"
		{ViewPII, []string{"address.street", "meta.id"}, map[string]any{"address": map[string]any{"street": "Main 1"}, "meta": map[string]any{"id": "p1"}}},
		{ViewPII, []string{"meta"}, map[string]any{"meta": map[string]any{"id": "p1", "secret": "s"}}},
	}
	for _, c := range cases {
		got, err := project(t, peopleConfig(), c.level, c.requested...)
		if err != nil || !equal(got, c.want) {
			t.Errorf("%v %v = %v, %v\nwant %v", c.level, c.requested, got, err, c.want)
		}
	}
}

func TestNestedDefaultFieldsPerLevel(t *testing.T) {
	cfg := peopleConfig()
	cfg.Resources[0].Fields = map[View][]string{ViewPublic: {"meta.id", "address.city", "tags"}}
	got, err := project(t, cfg, ViewPublic)
	want := map[string]any{"meta": map[string]any{"id": "p1"}, "address": map[string]any{"city": "Cluj"}, "tags": person["tags"]}
	if err != nil || !equal(got, want) {
		t.Errorf("got %v, %v\nwant %v", got, err, want)
	}
	// narrowing can never widen past the default projection
	got, _ = project(t, cfg, ViewPublic, "address.zip")
	if !equal(got, map[string]any{}) {
		t.Errorf("widened: %v", got)
	}
}

func TestNestedStrictFields(t *testing.T) {
	cfg := peopleConfig()
	cfg.StrictFields = true
	for _, f := range []string{"address.street", "email", "address.nope", "nope", "meta.secret"} {
		if _, err := project(t, cfg, ViewPublic, f); !errors.Is(err, ErrFieldNotAllowed) {
			t.Errorf("public asking %q = %v, want ErrFieldNotAllowed", f, err)
		}
	}
	// masked and public fields, and whole objects, are fine
	for _, f := range []string{"address.zip", "address.city", "address", "phone"} {
		if _, err := project(t, cfg, ViewPublic, f); err != nil {
			t.Errorf("public asking %q = %v", f, err)
		}
	}
	if _, err := project(t, cfg, ViewPII, "address.street"); err != nil {
		t.Errorf("pii asking address.street = %v", err)
	}
}

func TestNestedKeyAndArrayRows(t *testing.T) {
	g := newGuard(t, peopleConfig())
	rows := []any{
		map[string]any{"meta": map[string]any{"id": "p1"}, "address": map[string]any{"city": "A", "street": "x"}},
		map[string]any{"meta": map[string]any{"id": "p2"}, "address": map[string]any{"city": "B", "street": "y"}},
	}
	got, err := g.Project(Principal{View: ViewPublic}, "people", rows, nil)
	want := []any{
		map[string]any{"meta": map[string]any{"id": "p1"}, "address": map[string]any{"city": "A"}},
		map[string]any{"meta": map[string]any{"id": "p2"}, "address": map[string]any{"city": "B"}},
	}
	if err != nil || !equal(got, want) {
		t.Errorf("rows = %v, %v", got, err)
	}
	if k := g.keyOf("people", rows[0]); k != "p1" {
		t.Errorf("dotted key = %q", k)
	}
}

func TestMaskFunc(t *testing.T) {
	g := newGuard(t, peopleConfig())
	got, _ := g.Project(Principal{View: ViewPublic}, "people", map[string]any{"phone": 12345.0, "address": map[string]any{"zip": "98"}}, nil)
	// non-strings fall back to "***"; a value shorter than n stays as is
	if !equal(got, map[string]any{"phone": "***", "address": map[string]any{"zip": "**"}}) {
		t.Errorf("maskfunc = %v", got)
	}
	if v := KeepLast(2)("héllo"); v != "***lo" {
		t.Errorf("KeepLast runes = %v", v)
	}
}

func TestValidateColumnsAndPaths(t *testing.T) {
	bad := map[string]func(*Config){
		"overlapping object and field": func(c *Config) { c.Resources[0].Columns = append(c.Resources[0].Columns, Column{Name: "address"}) },
		"leading dot":                  func(c *Config) { c.Resources[0].Columns[0].Name = ".a" },
		"trailing dot":                 func(c *Config) { c.Resources[0].Columns[0].Name = "a." },
		"empty segment":                func(c *Config) { c.Resources[0].Columns[0].Name = "a..b" },
		"empty name":                   func(c *Config) { c.Resources[0].Columns[0].Name = "" },
		"duplicate column":             func(c *Config) { c.Resources[0].Columns = append(c.Resources[0].Columns, Column{Name: "email"}) },
		"maskfunc on public column": func(c *Config) {
			c.Resources[0].Columns = append(c.Resources[0].Columns, Column{Name: "pub", MaskFunc: KeepLast(1)})
		},
		"unknown field":    func(c *Config) { c.Resources[0].Fields = map[View][]string{ViewPublic: {"address.nope"}} },
		"key not a column": func(c *Config) { c.Resources[0].Key = "meta" },
	}
	for name, mutate := range bad {
		cfg := peopleConfig()
		mutate(&cfg)
		if _, err := New(cfg); err == nil {
			t.Errorf("%s: want a config error", name)
		}
	}
	cfg := peopleConfig()
	cfg.Resources[0].Fields = map[View][]string{ViewPublic: {"address", "meta.id"}} // an object is a valid field entry
	if _, err := New(cfg); err != nil {
		t.Errorf("object as field entry = %v", err)
	}
	if _, err := New(peopleConfig()); err != nil || strings.Contains("", "x") {
		t.Errorf("valid config = %v", err)
	}
}
