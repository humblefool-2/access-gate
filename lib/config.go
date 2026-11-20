// SPDX-License-Identifier: Apache-2.0

package accessgate

import (
	"context"
	"encoding/json"
	"fmt"
)

// Class says whether a column holds personal data.
type Class int

const (
	Public Class = iota
	PII
)

// MaskMode is what a session below ViewPII sees for a PII column.
type MaskMode int

const (
	Drop MaskMode = iota // key removed
	Mask                 // value replaced with "***"
)

// Column declares one field of a resource. Name is a top-level key or a dot
// path into a nested object ("address.city"). Dot paths address objects, not
// arrays: an array is one column.
type Column struct {
	Name  string
	Class Class
	Mask  MaskMode
	// MaskFunc, if set, builds what a session below ViewPII sees instead of the
	// fixed "***" (it implies Mask). It gets the decoded JSON value, so it must
	// handle any type: a string, a bool, nil, a map or slice, or a json.Number
	// for any number (kept exact, not a float64). Only valid on PII columns.
	MaskFunc func(v any) any
}

// Resource describes one kind of response object (e.g. a customer row).
// Handlers return full objects; the lib projects them per session.
type Resource struct {
	Name    string
	Columns []Column
	// Key is the column holding the entity id (e.g. "wallet_id"). The lib reads
	// it from full responses to track versions for the stale-read guard.
	Key string
	// IDParam is the path value that names the entity on this resource's HTTP
	// routes (a route /wallets/{wallet_id} sets "wallet_id"). Default "id". The
	// stale-read check and Scope use it before the handler runs.
	IDParam string
	// Fields is the default response projection per view. A view with no
	// entry gets every column it is allowed to see. Callers can narrow it with
	// ?fields=a,b but never widen it.
	Fields  map[View][]string
	MaxRows int // caps array responses; 0 = no cap
	// ListPath names the top-level key that holds the rows when a handler
	// answers with an envelope such as {"data": [...], "next": "..."}. The rows
	// are projected, scoped and capped by MaxRows like a plain list. The other
	// envelope keys are dropped unless listed in ListKeep, which are copied
	// unchanged (so they must not hold PII). Empty = the response is the row or
	// list itself.
	ListPath string
	ListKeep []string
	// Scope limits which entities a principal may touch, decided by entity id
	// (the Key column value). It runs before the handler for any call that
	// names an entity (the IDParam path value or the IDArg tool argument), and a write
	// with no entity id is refused (ErrNotInScope), so an
	// out-of-scope write never happens, and again on every row of a list and on
	// a single object whose key differs from the requested id. A refused entity
	// looks like it does not exist (ErrNotInScope, 404). Requires Key. It
	// cannot see row contents, only the id; look anything else up yourself.
	// The id it gets from a route is the route's {id}, so on a scoped resource
	// that must be the entity's key (a route like /holds/{id}/release, whose id
	// is another kind of entity, belongs on an unscoped resource).
	Scope func(p Principal, id string) bool
	// AllowNonJSON lets Guard.Handle pass a handler's 2xx response through
	// unchanged when its body is not JSON (files, HTML). Default false: such a
	// response is refused with ErrNotJSON (500), because the guard cannot
	// project it and would otherwise leak whatever it holds. JSON bodies are
	// projected either way, and a 2xx with an empty body (204) always passes.
	AllowNonJSON bool

	// lookup tables built once by New (see prepare)
	cols    map[string]Column
	colAnc  map[string]bool // objects that hold declared columns
	allowed map[View]*pathSet
}

// ToolHandler runs a tool. Write tools should do their own DB work; the
// returned value is projected through the tool's Resource.
type ToolHandler func(ctx context.Context, args json.RawMessage) (any, error)

type ToolSpec struct {
	Name        string
	Description string
	Resource    string
	// Mutates marks a tool that changes data. The stale-read guard applies to
	// it (the session must have read the entity since anyone last wrote it) and
	// its writes to one entity are serialised.
	Mutates bool
	// IDArg is the tool argument that names the entity (default "id").
	IDArg   string
	Handler ToolHandler
	// InputSchema is an optional JSON Schema (an object) describing the tool's
	// arguments. The lib only passes it on, in ListTools, so an agent runtime or
	// MCP server can show it to the model; it does not validate arguments.
	InputSchema json.RawMessage
}

type Config struct {
	Resources []Resource
	Tools     []ToolSpec
	// StrictFields makes a request for a field the session may not see a 403
	// instead of silently omitting it.
	StrictFields bool
	// MaxBodyBytes caps the request body ToolsHandler reads (default 1 MiB).
	// A larger body is refused with ErrBodyTooLarge (413).
	MaxBodyBytes int64
	// Authorize, if set, decides whether a principal may list or call a tool.
	// The lib has no role logic of its own: the service plugs its own rules in
	// here (a tool that returns an error is hidden from ListTools and refused by
	// Call with ErrForbidden). Leave it nil when
	// the service already protects the tools routes with its own middleware.
	// It must be fast and safe to call concurrently.
	Authorize func(p Principal, t ToolInfo) error
}

// validPath rejects names that cannot be a key or a dot path.
func validPath(s string) error {
	if s == "" {
		return fmt.Errorf("empty name")
	}
	start := 0
	for i := 0; i <= len(s); i++ {
		if i == len(s) || s[i] == '.' {
			if i == start {
				return fmt.Errorf("empty segment in dot path")
			}
			start = i + 1
		}
	}
	return nil
}

// Validate checks the config so a service refuses to start on a typo.
func (c Config) Validate() error {
	res := map[string]Resource{}
	for _, r := range c.Resources {
		if r.Name == "" {
			return fmt.Errorf("resource with empty name")
		}
		if _, dup := res[r.Name]; dup {
			return fmt.Errorf("duplicate resource %q", r.Name)
		}
		cols := map[string]bool{}
		for _, col := range r.Columns {
			if err := validPath(col.Name); err != nil {
				return fmt.Errorf("resource %q: column %q: %w", r.Name, col.Name, err)
			}
			if cols[col.Name] {
				return fmt.Errorf("resource %q: duplicate column %q", r.Name, col.Name)
			}
			if col.MaskFunc != nil && col.Class != PII {
				return fmt.Errorf("resource %q: column %q has a MaskFunc but is not PII", r.Name, col.Name)
			}
			cols[col.Name] = true
		}
		// objects holding declared columns; a column may not also be one
		holders := map[string]bool{}
		for name := range cols {
			for i := 0; i < len(name); i++ {
				if name[i] == '.' {
					holders[name[:i]] = true
				}
			}
		}
		for name := range holders {
			if cols[name] {
				return fmt.Errorf("resource %q: column %q overlaps its sub-columns: declare the object or its fields, not both", r.Name, name)
			}
		}
		if r.Key != "" && !cols[r.Key] {
			return fmt.Errorf("resource %q: key %q is not a column", r.Name, r.Key)
		}
		if r.ListPath == "" && len(r.ListKeep) > 0 {
			return fmt.Errorf("resource %q: ListKeep needs a ListPath", r.Name)
		}
		if r.Scope != nil && r.Key == "" {
			return fmt.Errorf("resource %q: Scope needs a Key (rows are scoped by their id)", r.Name)
		}
		for lvl, fields := range r.Fields {
			for _, f := range fields {
				if !cols[f] && !holders[f] {
					return fmt.Errorf("resource %q: %s fields lists unknown column %q", r.Name, lvl, f)
				}
			}
		}
		res[r.Name] = r
	}
	seen := map[string]bool{}
	for _, t := range c.Tools {
		switch {
		case t.Name == "" || t.Handler == nil:
			return fmt.Errorf("tool %q needs a name and handler", t.Name)
		case seen[t.Name]:
			return fmt.Errorf("duplicate tool %q", t.Name)
		}
		if _, ok := res[t.Resource]; !ok {
			return fmt.Errorf("tool %q: unknown resource %q", t.Name, t.Resource)
		}
		if len(t.InputSchema) > 0 {
			var obj map[string]any
			if err := json.Unmarshal(t.InputSchema, &obj); err != nil || obj == nil {
				return fmt.Errorf("tool %q: InputSchema must be a JSON object", t.Name)
			}
		}
		seen[t.Name] = true
	}
	return nil
}

// idParam is the path value naming the entity on this resource's routes.
func (r *Resource) idParam() string {
	if r.IDParam == "" {
		return "id"
	}
	return r.IDParam
}

// idArg is the argument naming the entity for this tool.
func (t ToolSpec) idArg() string {
	if t.IDArg == "" {
		return "id"
	}
	return t.IDArg
}
