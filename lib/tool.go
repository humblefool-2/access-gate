// SPDX-License-Identifier: Apache-2.0

package accessgate

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sort"
)

var ErrUnknownTool = errors.New("unknown tool")

// ErrBadArgs means a tool's arguments are not a JSON object of the expected shape (400).
var ErrBadArgs = errors.New("bad tool arguments")

// ErrBodyTooLarge means a request body is over Config.MaxBodyBytes (413).
var ErrBodyTooLarge = errors.New("request body too large")

const defaultMaxBodyBytes = 1 << 20

// ToolInfo is what an agent sees when it lists tools.
type ToolInfo struct {
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
	Mutates     bool   `json:"mutates"`
	// InputSchema is the tool's JSON Schema for its arguments, if it has one.
	InputSchema json.RawMessage `json:"input_schema,omitempty"`
}

func infoOf(t ToolSpec) ToolInfo {
	return ToolInfo{Name: t.Name, Description: t.Description, Mutates: t.Mutates, InputSchema: t.InputSchema}
}

// ListTools returns the tools the context's principal may call (every tool
// when Config.Authorize is nil), sorted by name.
func (g *Guard) ListTools(ctx context.Context) ([]ToolInfo, error) {
	p, ok := PrincipalFrom(ctx)
	if !ok {
		return nil, ErrUnauthenticated
	}
	out := []ToolInfo{}
	for _, t := range g.tools {
		if g.cfg.Authorize == nil || g.cfg.Authorize(p, infoOf(t)) == nil {
			out = append(out, infoOf(t))
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// Call runs a tool for the context's principal under the same rules as HTTP
// routes. args may carry the entity id under ToolSpec.IDArg, default "id" (the entity for the stale-read guard and Scope)
// and {"fields": [...]} to narrow the response.
func (g *Guard) Call(ctx context.Context, name string, args json.RawMessage) (any, error) {
	if _, ok := PrincipalFrom(ctx); !ok {
		return nil, ErrUnauthenticated
	}
	t, ok := g.tools[name]
	if !ok {
		return nil, ErrUnknownTool
	}
	if len(args) == 0 {
		args = json.RawMessage(`{}`)
	}
	var meta struct {
		ID     string
		Fields []string
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(args, &raw); err != nil {
		return nil, fmt.Errorf("%w: arguments must be a JSON object", ErrBadArgs)
	}
	meta.ID = idText(raw[t.idArg()])
	if f, ok := raw["fields"]; ok {
		if err := json.Unmarshal(f, &meta.Fields); err != nil {
			return nil, fmt.Errorf(`%w: "fields" must be a list of strings`, ErrBadArgs)
		}
	}

	var authz func(Principal) error
	if g.cfg.Authorize != nil {
		authz = func(p Principal) error { return g.cfg.Authorize(p, infoOf(t)) }
	}
	return g.exec(ctx, t.Resource, t.Mutates, authz, meta.ID, meta.Fields, func() (any, error) {
		return t.Handler(ctx, args)
	})
}

// ToolsHandler serves GET /tools and POST /tools/{name} for humans and
// agents; mount it at both "/tools" and "/tools/". An MCP server can call
// ListTools/Call directly instead.
func (g *Guard) ToolsHandler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /tools", func(w http.ResponseWriter, r *http.Request) {
		list, err := g.ListTools(r.Context())
		if err != nil {
			writeErr(w, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"tools": list})
	})
	mux.HandleFunc("POST /tools/{name}", func(w http.ResponseWriter, r *http.Request) {
		limit := g.cfg.MaxBodyBytes
		if limit <= 0 {
			limit = defaultMaxBodyBytes
		}
		body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, limit))
		if err != nil {
			var tooBig *http.MaxBytesError
			if errors.As(err, &tooBig) {
				writeErr(w, ErrBodyTooLarge)
			} else {
				writeErr(w, fmt.Errorf("%w: could not read the body", ErrBadArgs))
			}
			return
		}
		out, err := g.Call(r.Context(), r.PathValue("name"), body)
		if err != nil {
			writeErr(w, err)
			return
		}
		writeJSON(w, http.StatusOK, out)
	})
	return mux
}

// idText reads an entity id from a tool argument: a JSON string, or a number
// (1 -> "1"). Anything else is no id.
func idText(raw json.RawMessage) string {
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	var n json.Number
	if json.Unmarshal(raw, &n) == nil {
		return n.String()
	}
	return ""
}
