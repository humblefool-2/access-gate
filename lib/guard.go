// SPDX-License-Identifier: Apache-2.0

package accessgate

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"strings"
	"sync"
)

// Guard sits behind the service's own authentication. For each call it checks
// the principal's role, applies workflow rules per session (stale-read guard),
// and projects the response to the role's columns. HTTP
// routes and agent tools both go through it.
type Guard struct {
	cfg   Config
	res   map[string]*Resource
	tools map[string]ToolSpec
	hist  *history
	// locks serialise writes per entity so check-then-write is atomic for one
	// entity while writes to different entities run in parallel. An entity
	// maps to a stripe by hash; two entities sharing a stripe only wait on each
	// other, never deadlock, because a write holds exactly one stripe.
	locks [lockStripes]stripe
}

const lockStripes = 256

// stripe pads each mutex to its own cache line so neighbouring stripes do not
// bounce between cores.
type stripe struct {
	sync.Mutex
	_ [56]byte
}

// lockFor picks the stripe for resource/entity (FNV-1a, no allocation). A
// write with no entity id (entity == "") lands on one stripe per resource, so
// such routes stay serialised among themselves as before.
func (g *Guard) lockFor(resource, entity string) *sync.Mutex {
	h := uint32(2166136261)
	for i := 0; i < len(resource); i++ {
		h = (h ^ uint32(resource[i])) * 16777619
	}
	h = (h ^ '/') * 16777619
	for i := 0; i < len(entity); i++ {
		h = (h ^ uint32(entity[i])) * 16777619
	}
	return &g.locks[h%lockStripes].Mutex
}

func New(cfg Config) (*Guard, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	g := &Guard{cfg: cfg, res: map[string]*Resource{}, tools: map[string]ToolSpec{}, hist: newHistory()}
	for _, r := range cfg.Resources {
		r := r // own copy: prepare must not write into the caller's slice
		r.prepare()
		g.res[r.Name] = &r
	}
	for _, t := range cfg.Tools {
		g.tools[t.Name] = t
	}
	return g, nil
}

// isWrite reports whether an HTTP method changes data.
func isWrite(method string) bool {
	switch method {
	case http.MethodGet, http.MethodHead, http.MethodOptions:
		return false
	}
	return true
}

// exec runs fn (which returns the full, unprojected value) under the
// workflow rules and returns the projected result. authz, if not nil, is the
// service's permission decision for this call (see Config.Authorize).
func (g *Guard) exec(ctx context.Context, resource string, write bool, authz func(Principal) error, entity string, requested []string, fn func() (any, error)) (any, error) {
	p, ok := PrincipalFrom(ctx)
	if !ok {
		return nil, ErrUnauthenticated
	}
	sid := p.session()
	if authz != nil {
		if err := authz(p); err != nil {
			return nil, fmt.Errorf("%w: %v", ErrForbidden, err)
		}
	}

	res := g.res[resource]
	if write && res.Scope != nil && entity == "" {
		// a write cannot be checked against Scope without an id, and it must not run first
		return nil, fmt.Errorf("%w: a write on %s needs an entity id", ErrNotInScope, resource)
	}
	if res.Scope != nil && entity != "" && !res.Scope(p, entity) {
		return nil, fmt.Errorf("%w: %s/%s is outside the scope of %s", ErrNotInScope, resource, entity, p.ID)
	}
	if g.cfg.StrictFields {
		if err := res.checkRequested(p, requested); err != nil {
			return nil, fmt.Errorf("%w: %s", err, strings.Join(requested, ","))
		}
	}

	if write {
		mu := g.lockFor(resource, entity)
		mu.Lock()
		defer mu.Unlock()
		if entity != "" && g.hist.stale(sid, resource, entity) {
			return nil, fmt.Errorf("%w: %s/%s changed since this session last read it; read it again, then retry", ErrStaleRead, resource, entity)
		}
	}
	var preVer uint64
	if entity != "" {
		preVer = g.hist.version(resource, entity) // taken before fn so a concurrent write is never masked
	}

	v, err := fn()
	if err == nil {
		v, err = decoded(v)
	}
	var pt passthrough
	if errors.As(err, &pt) {
		// The handler's own response goes out unchanged. A write that
		// succeeded (2xx) still changed the entity, so it bumps the version.
		if write && entity != "" && pt.buf.code/100 == 2 {
			g.hist.bump(sid, resource, entity)
		}
		return nil, err
	}
	if err != nil {
		return nil, err
	}

	key := g.keyOf(resource, v)
	if key == "" {
		key = entity
	}
	if res.Scope != nil && key != "" && key != entity && !res.Scope(p, key) {
		return nil, fmt.Errorf("%w: %s/%s is outside the scope of %s", ErrNotInScope, resource, key, p.ID)
	}
	if key != "" {
		switch {
		case write:
			g.hist.bump(sid, resource, key)
		case key == entity:
			g.hist.markSeen(sid, resource, key, preVer)
		default:
			g.hist.markSeen(sid, resource, key, g.hist.version(resource, key))
		}
	}

	out, err := g.projectDecoded(p, res, v, requested)
	if err != nil {
		return nil, err
	}
	return out, nil
}

// fresh marks a value the guard itself just decoded from JSON (a map or
// slice nobody else holds), so exec can use it as is instead of encoding and
// decoding it again.
type fresh struct{ v any }

// decoded returns the JSON-shaped (map[string]any / []any / scalar) form of a
// handler's result with one encode+decode, or none when it is already fresh.
// A tool's own return value may be a struct or a map it shares with other
// goroutines, so it is always copied this way.
func decoded(v any) (any, error) {
	if f, ok := v.(fresh); ok {
		return f.v, nil
	}
	raw, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	return decodeJSON(raw)
}

// decodeJSON decodes one JSON value into map[string]any / []any / scalars with
// numbers kept exact as json.Number, so a large integer or a decimal such as
// 1.10 comes out the way it went in instead of rounded through float64.
func decodeJSON(b []byte) (any, error) {
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return nil, err
	}
	if _, err := dec.Token(); err != io.EOF {
		return nil, errors.New("invalid JSON: data after the first value")
	}
	return v, nil
}

// keyOf reads the resource's key column from a decoded object response.
func (g *Guard) keyOf(resource string, v any) string {
	r := g.res[resource]
	if r.Key == "" {
		return ""
	}
	obj, _ := v.(map[string]any)
	return idString(lookup(obj, r.Key))
}

// idString is an entity id as text: a string as is, a number as written in the
// JSON (1 -> "1"), anything else (missing, null, object) none. This is what lets
// a resource keyed by a numeric id be tracked and scoped like a string one.
func idString(v any) string {
	switch x := v.(type) {
	case string:
		return x
	case json.Number:
		return x.String()
	}
	return ""
}

// Project turns any JSON-shaped value (object or array of objects) into the
// view this principal may see for the named resource.
func (g *Guard) Project(p Principal, resource string, v any, requested []string) (any, error) {
	r, ok := g.res[resource]
	if !ok {
		return nil, fmt.Errorf("unknown resource %q", resource)
	}
	generic, err := decoded(v)
	if err != nil {
		return nil, err
	}
	return g.projectDecoded(p, r, generic, requested)
}

// projectDecoded projects a value already in decoded JSON form. MaxRows cuts
// the array before any per-row work.
func (g *Guard) projectDecoded(p Principal, r *Resource, generic any, requested []string) (any, error) {
	if g.cfg.StrictFields {
		if err := r.checkRequested(p, requested); err != nil {
			return nil, err
		}
	}
	v := r.view(p, requested)
	if r.ListPath != "" {
		return r.projectEnvelope(p, v, generic)
	}
	switch x := generic.(type) {
	case map[string]any:
		return r.project(v, x), nil
	case []any:
		return r.projectRows(p, v, x)
	}
	return generic, nil
}

func requestedFields(r *http.Request) []string {
	q := r.URL.Query().Get("fields")
	if q == "" {
		return nil
	}
	return strings.Split(q, ",")
}

// statusOf maps errors to HTTP statuses. Errors may carry their own via
// HTTPStatus() int.
func statusOf(err error) (int, string) {
	var hs interface{ HTTPStatus() int }
	switch {
	case errors.Is(err, ErrUnauthenticated):
		return http.StatusUnauthorized, err.Error()
	case errors.Is(err, ErrNotInScope):
		return http.StatusNotFound, ErrNotInScope.Error() // never say why
	case errors.Is(err, ErrForbidden), errors.Is(err, ErrFieldNotAllowed):
		return http.StatusForbidden, err.Error()
	case errors.Is(err, ErrStaleRead):
		return http.StatusConflict, err.Error()
	case errors.Is(err, ErrUnknownTool):
		return http.StatusNotFound, err.Error()
	case errors.Is(err, ErrBadArgs):
		return http.StatusBadRequest, err.Error()
	case errors.Is(err, ErrBodyTooLarge):
		return http.StatusRequestEntityTooLarge, ErrBodyTooLarge.Error()
	case errors.Is(err, ErrNotJSON):
		return http.StatusInternalServerError, ErrNotJSON.Error()
	case errors.As(err, &hs):
		return hs.HTTPStatus(), err.Error()
	}
	return http.StatusInternalServerError, "internal error"
}

func writeJSON(w http.ResponseWriter, code int, body any) {
	writeJSONAs(w, code, "application/json", body)
}

func writeJSONAs(w http.ResponseWriter, code int, contentType string, body any) {
	w.Header().Set("Content-Type", contentType)
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(body)
}

func writeErr(w http.ResponseWriter, err error) {
	code, msg := statusOf(err)
	writeJSON(w, code, map[string]string{"error": msg})
}

type buffered struct {
	header http.Header
	code   int
	body   bytes.Buffer
}

func (b *buffered) Header() http.Header         { return b.header }
func (b *buffered) Write(p []byte) (int, error) { return b.body.Write(p) }
func (b *buffered) WriteHeader(code int)        { b.code = code }

// isJSONType reports whether a Content-Type header names JSON:
// application/json or any application/*+json (problem+json, vnd.api+json),
// with or without parameters such as charset.
func isJSONType(ct string) bool {
	mt, _, err := mime.ParseMediaType(ct)
	if err != nil {
		return false
	}
	return mt == "application/json" || (strings.HasPrefix(mt, "application/") && strings.HasSuffix(mt, "+json"))
}

// passthrough carries a handler's own response (a non-2xx, an empty 2xx, or a
// non-JSON 2xx on a resource with AllowNonJSON) back out unchanged.
type passthrough struct{ buf *buffered }

func (p passthrough) Error() string { return fmt.Sprintf("handler returned %d", p.buf.code) }

// Handle wraps an HTTP handler. The principal comes from the request context
// (set by the service's authentication, see Authenticate). The entity id for
// the stale-read guard and Scope is the path value named by Resource.IDParam
// (default "id"). The handler returns full
// objects and knows nothing about roles or PII.
//
// A 2xx response is projected when it is JSON: Content-Type application/json
// or application/*+json, or no Content-Type and a body that parses as JSON. Any
// other 2xx body is refused with ErrNotJSON (500) unless the resource sets
// AllowNonJSON. An empty body (204) and the handler's own non-2xx responses
// pass through unchanged. The handler's status and headers (Location,
// Set-Cookie, Cache-Control, ...) reach the caller with the projected body,
// except the headers that describe the full body, which no longer fits
// (Content-Length, ETag, Content-MD5, Digest, Content-Range, Content-Encoding,
// Transfer-Encoding). Caches are the service's concern: the guard adds no cache
// headers, so set Cache-Control: private (or Vary) on responses that differ by
// caller.
func (g *Guard) Handle(resource string, h http.HandlerFunc) http.HandlerFunc {
	res, ok := g.res[resource]
	if !ok {
		panic(fmt.Sprintf("access-gate: unknown resource %q", resource))
	}
	return func(w http.ResponseWriter, r *http.Request) {
		code := http.StatusOK
		var hdr http.Header // the handler's own headers, copied to the caller with the projected body
		out, err := g.exec(r.Context(), resource, isWrite(r.Method), nil, r.PathValue(res.idParam()), requestedFields(r), func() (any, error) {
			buf := &buffered{header: http.Header{}, code: http.StatusOK}
			h(buf, r)
			code, hdr = buf.code, buf.header
			body := buf.body.Bytes()
			if buf.code/100 != 2 || len(bytes.TrimSpace(body)) == 0 {
				return nil, passthrough{buf} // the handler's own error, or nothing to project
			}
			ct := buf.header.Get("Content-Type")
			switch {
			case isJSONType(ct), ct == "" && json.Valid(body): // no header: JSON if it parses
				return decodeJSON(body)
			case res.AllowNonJSON:
				return nil, passthrough{buf}
			default:
				return nil, ErrNotJSON
			}
		})
		var pt passthrough
		if errors.As(err, &pt) {
			for k, v := range pt.buf.header {
				w.Header()[k] = v
			}
			w.WriteHeader(pt.buf.code)
			_, _ = w.Write(pt.buf.body.Bytes())
			return
		}
		if err != nil {
			writeErr(w, err)
			return
		}
		ct := hdr.Get("Content-Type")
		if !isJSONType(ct) {
			ct = "application/json"
		}
		for k, v := range hdr {
			if !bodyHeaders[http.CanonicalHeaderKey(k)] {
				w.Header()[k] = append([]string(nil), v...)
			}
		}
		writeJSONAs(w, code, ct, out)
	}
}

// bodyHeaders describe the handler's full body, which the caller does not get
// (the guard projects it), so Handle does not copy them. Every other header the
// handler set (Location, Set-Cookie, Cache-Control, ...) reaches the caller.
var bodyHeaders = map[string]bool{
	"Content-Length":    true,
	"Content-Encoding":  true,
	"Content-Range":     true,
	"Content-Md5":       true,
	"Digest":            true,
	"Etag":              true,
	"Transfer-Encoding": true,
}

// projectRows projects an array of objects: rows the principal may not see
// (Scope) are left out and do not count toward MaxRows.
func (r *Resource) projectRows(p Principal, v view, rows []any) ([]any, error) {
	out := make([]any, 0, len(rows))
	for _, e := range rows {
		if r.MaxRows > 0 && len(out) >= r.MaxRows {
			break
		}
		obj, ok := e.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("resource %q: array element is not an object", r.Name)
		}
		if r.Scope != nil {
			id := idString(lookup(obj, r.Key))
			if id == "" || !r.Scope(p, id) {
				continue
			}
		}
		out = append(out, r.project(v, obj))
	}
	return out, nil
}

// projectEnvelope handles a paginated response such as
// {"data": [...], "next": "..."}: the rows under ListPath are projected like a
// list, the envelope keys named in ListKeep are copied as they are, and every
// other envelope key is dropped.
func (r *Resource) projectEnvelope(p Principal, v view, generic any) (any, error) {
	env, ok := generic.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("resource %q: expected an object with a %q list", r.Name, r.ListPath)
	}
	rows, ok := env[r.ListPath].([]any)
	if !ok {
		return nil, fmt.Errorf("resource %q: %q is not a list", r.Name, r.ListPath)
	}
	projected, err := r.projectRows(p, v, rows)
	if err != nil {
		return nil, err
	}
	out := map[string]any{r.ListPath: projected}
	for _, k := range r.ListKeep {
		if val, ok := env[k]; ok {
			out[k] = val
		}
	}
	return out, nil
}
