// SPDX-License-Identifier: Apache-2.0

package accessgate

import "errors"

var ErrFieldNotAllowed = errors.New("requested field not allowed for this session")

const maskValue = "***"

// pathSet is a set of dot paths ("address.city") with its strict ancestors
// ("address") indexed, so a walk down a nested object can ask "is this path
// selected" and "is something below it selected" with map lookups.
type pathSet struct {
	exact map[string]bool
	anc   map[string]bool
}

func newPathSet(paths []string) *pathSet {
	s := &pathSet{exact: make(map[string]bool, len(paths)), anc: map[string]bool{}}
	for _, p := range paths {
		s.exact[p] = true
		for i := 0; i < len(p); i++ {
			if p[i] == '.' {
				s.anc[p[:i]] = true
			}
		}
	}
	return s
}

// covers reports whether p or one of its ancestors is an entry.
func (s *pathSet) covers(p string) bool {
	if s.exact[p] {
		return true
	}
	for i := len(p) - 1; i >= 0; i-- {
		if p[i] == '.' && s.exact[p[:i]] {
			return true
		}
	}
	return false
}

// masks reports whether a session below ViewPII sees a masked value (rather
// than nothing) for this column.
func (c Column) masks() bool { return c.Mask == Mask || c.MaskFunc != nil }

// prepare builds the lookup tables project and checkRequested use, so they do
// no per-call (and per-row) map building. New calls it once; the tables are
// read-only afterwards, so concurrent use needs no lock.
func (r *Resource) prepare() {
	r.cols = make(map[string]Column, len(r.Columns))
	r.colAnc = map[string]bool{}
	for _, c := range r.Columns {
		r.cols[c.Name] = c
		for i := 0; i < len(c.Name); i++ {
			if c.Name[i] == '.' {
				r.colAnc[c.Name[:i]] = true
			}
		}
	}
	r.allowed = make(map[View]*pathSet, len(r.Fields))
	for l, fields := range r.Fields {
		r.allowed[l] = newPathSet(fields)
	}
}

// baseFields is the view's default projection; ok is false when the view
// has no declared projection (it then gets every column it may see).
func (r *Resource) baseFields(l View) (*pathSet, bool) {
	set, ok := r.allowed[l]
	return set, ok
}

// checkRequested is the strict-fields check that needs no response object, so
// exec can run it before the handler (a write must not save and then 403).
// In strict mode a requested path must be a declared column or an object
// holding declared columns, which also catches typos on resources with no
// per-level Fields. It then refuses a path outside the level's declared
// projection, and a PII column the level may not see that is not masked.
func (r *Resource) checkRequested(p Principal, requested []string) error {
	if requested == nil {
		return nil
	}
	canPII := p.View.Allows(ViewPII)
	base, hasBase := r.baseFields(p.View)
	for _, f := range requested {
		col, declared := r.cols[f]
		if !declared && !r.colAnc[f] {
			return ErrFieldNotAllowed
		}
		if hasBase && !base.covers(f) && (declared || !base.anc[f]) {
			return ErrFieldNotAllowed
		}
		if declared && col.Class == PII && !canPII && !col.masks() {
			return ErrFieldNotAllowed
		}
	}
	return nil
}

// view is what one principal (and one ?fields= request) may see of a
// resource. Built once per response, shared by every row.
type view struct {
	canPII bool
	// allowed is the level's default projection; aFull means no restriction.
	allowed *pathSet
	aFull   bool
	// req is the caller's narrowing; rFull means none was asked for.
	req   *pathSet
	rFull bool
}

func (r *Resource) view(p Principal, requested []string) view {
	v := view{canPII: p.View.Allows(ViewPII), aFull: true, rFull: true}
	if set, ok := r.baseFields(p.View); ok {
		v.allowed, v.aFull = set, false
	}
	if requested != nil {
		v.req, v.rFull = newPathSet(requested), false
	}
	return v
}

// project applies the level's default fields, narrows to the requested ones,
// then drops, masks or keeps PII columns for sessions below ViewPII. Nested
// objects are walked by dot path. Columns the resource does not declare count
// as PII, so a new field fails closed.
func (r *Resource) project(v view, obj map[string]any) map[string]any {
	return r.walk(v, obj, "", v.aFull, v.rFull)
}

// walk projects obj, found at prefix. aFull/rFull say an ancestor path was
// selected whole (default projection / request), so everything below is too.
func (r *Resource) walk(v view, obj map[string]any, prefix string, aFull, rFull bool) map[string]any {
	out := make(map[string]any, len(obj))
	for k, val := range obj {
		path := k
		if prefix != "" {
			path = prefix + "." + k
		}
		af := aFull || (v.allowed != nil && v.allowed.exact[path])
		if !af && !(v.allowed != nil && v.allowed.anc[path]) {
			continue
		}
		rf := rFull || (v.req != nil && v.req.exact[path])
		if !rf && !(v.req != nil && v.req.anc[path]) {
			continue
		}

		if col, declared := r.cols[path]; declared {
			if !af || !rf {
				continue
			}
			if col.Class == PII && !v.canPII {
				switch {
				case col.MaskFunc != nil:
					out[k] = col.MaskFunc(val)
				case col.Mask == Mask:
					out[k] = maskValue
				}
				continue
			}
			out[k] = val
			continue
		}
		if sub, ok := val.(map[string]any); ok && r.colAnc[path] {
			if res := r.walk(v, sub, path, af, rf); len(res) > 0 {
				out[k] = res
			}
			continue
		}
		// undeclared: treated as PII
		if af && rf && v.canPII {
			out[k] = val
		}
	}
	return out
}

// lookup reads a dot path from a decoded object.
func lookup(obj map[string]any, path string) any {
	for {
		i := 0
		for i < len(path) && path[i] != '.' {
			i++
		}
		if i == len(path) {
			return obj[path]
		}
		next, ok := obj[path[:i]].(map[string]any)
		if !ok {
			return nil
		}
		obj, path = next, path[i+1:]
	}
}

// KeepLast returns a MaskFunc that hides all but the last n characters of a
// string with '*'. A string of n characters or fewer is hidden completely, so
// the mask never shows a whole value. Anything that is not a string becomes "***".
func KeepLast(n int) func(any) any {
	return func(v any) any {
		s, ok := v.(string)
		if !ok {
			return maskValue
		}
		r := []rune(s)
		hide := len(r) - n
		if hide <= 0 { // n or fewer characters: keeping the last n would show all of it
			hide = len(r)
		}
		for i := 0; i < hide; i++ {
			r[i] = '*'
		}
		return string(r)
	}
}
