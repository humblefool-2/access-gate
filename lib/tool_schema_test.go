// SPDX-License-Identifier: Apache-2.0

package accessgate

import (
	"encoding/json"
	"testing"
)

func TestToolInputSchema(t *testing.T) {
	cfg := scopeConfig(new(int))
	cfg.Tools[0].InputSchema = json.RawMessage(`{"type":"object","properties":{"id":{"type":"string"}}}`)
	g := newGuard(t, cfg)
	list, err := g.ListTools(user(ViewPublic, "s", "alice"))
	if err != nil {
		t.Fatal(err)
	}
	var withSchema, without int
	for _, ti := range list {
		if len(ti.InputSchema) > 0 {
			withSchema++
			b, _ := json.Marshal(ti)
			if !json.Valid(b) {
				t.Errorf("ToolInfo JSON invalid: %s", b)
			}
		} else {
			without++
		}
	}
	if withSchema != 1 || without == 0 {
		t.Errorf("schema on %d tools, none on %d", withSchema, without)
	}
	for _, bad := range []string{`[]`, `"x"`, `{`, `null`} {
		cfg := scopeConfig(new(int))
		cfg.Tools[0].InputSchema = json.RawMessage(bad)
		if _, err := New(cfg); err == nil {
			t.Errorf("InputSchema %s should fail validation", bad)
		}
	}
}
