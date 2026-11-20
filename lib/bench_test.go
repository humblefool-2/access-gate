// SPDX-License-Identifier: Apache-2.0

package accessgate

import (
	"fmt"
	"testing"
)

// benchRows builds n customer-shaped objects (3 public columns, 2 PII, 1 undeclared).
func benchRows(n int) []map[string]any {
	rows := make([]map[string]any, n)
	for i := range rows {
		rows[i] = map[string]any{
			"id": fmt.Sprintf("c%d", i), "email": "a@x.com", "phone": "+15550000",
			"balance": 5, "name": "Alice Example", "extra": "new",
		}
	}
	return rows
}

func benchGuard(b *testing.B, maxRows int) *Guard {
	b.Helper()
	cfg := Config{Resources: []Resource{{
		Name: "customers",
		Key:  "id",
		Columns: []Column{
			{Name: "id"},
			{Name: "email", Class: PII, Mask: Drop},
			{Name: "phone", Class: PII, Mask: Mask},
			{Name: "balance"},
			{Name: "name", Class: PII, Mask: Drop},
		},
		Fields:  map[View][]string{ViewPublic: {"id", "phone", "balance"}},
		MaxRows: maxRows,
	}}}
	g, err := New(cfg)
	if err != nil {
		b.Fatal(err)
	}
	return g
}

// BenchmarkProject measures Guard.Project by role and payload size
// (a JSON round-trip plus per-column projection; no HTTP, no DB).
func BenchmarkProject(b *testing.B) {
	for _, lvl := range []View{ViewPublic, ViewPII} {
		for _, n := range []int{1, 10, 100, 1000} {
			b.Run(fmt.Sprintf("%s/rows=%d", lvl, n), func(b *testing.B) {
				g := benchGuard(b, 0)
				p := Principal{ID: "a", SessionID: "s", View: lvl}
				var v any = benchRows(n)
				if n == 1 {
					v = benchRows(1)[0]
				}
				b.ReportAllocs()
				b.ResetTimer()
				for i := 0; i < b.N; i++ {
					if _, err := g.Project(p, "customers", v, nil); err != nil {
						b.Fatal(err)
					}
				}
			})
		}
	}
}

// BenchmarkProjectNarrow is the same with ?fields= narrowing, and with MaxRows capping a big array.
func BenchmarkProjectNarrow(b *testing.B) {
	p := Principal{ID: "a", SessionID: "s", View: ViewPII}
	b.Run("fields=2/rows=100", func(b *testing.B) {
		g := benchGuard(b, 0)
		v := benchRows(100)
		b.ReportAllocs()
		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			if _, err := g.Project(p, "customers", v, []string{"id", "balance"}); err != nil {
				b.Fatal(err)
			}
		}
	})
	b.Run("maxrows=100/rows=1000", func(b *testing.B) {
		g := benchGuard(b, 100)
		v := benchRows(1000)
		b.ReportAllocs()
		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			if _, err := g.Project(p, "customers", v, nil); err != nil {
				b.Fatal(err)
			}
		}
	})
}
