// SPDX-License-Identifier: Apache-2.0

package accessgate

import (
	"errors"
	"sync"
)

var ErrStaleRead = errors.New("stale read")

// ErrNotJSON means a handler wrapped by Guard.Handle answered 2xx with a body
// that is not JSON, on a resource that did not set AllowNonJSON. The guard
// cannot project such a body, so it refuses it (500) instead of letting it
// reach the caller unprojected.
var ErrNotJSON = errors.New("handler response is not JSON")

// ErrNotInScope means the principal may not see this entity (Resource.Scope).
// It is answered as 404 so it does not reveal that the entity exists.
var ErrNotInScope = errors.New("not found")

// history tracks entity versions (bumped on every write through the guard)
// and what each session last saw.
type history struct {
	mu       sync.Mutex
	versions map[string]uint64            // resource/entity -> version
	seen     map[string]map[string]uint64 // session -> resource/entity -> version when last read
}

func newHistory() *history {
	return &history{versions: map[string]uint64{}, seen: map[string]map[string]uint64{}}
}

func ek(resource, entity string) string { return resource + "/" + entity }

func (h *history) version(resource, entity string) uint64 {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.versions[ek(resource, entity)]
}

func (h *history) markSeen(session, resource, entity string, ver uint64) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.seen[session] == nil {
		h.seen[session] = map[string]uint64{}
	}
	h.seen[session][ek(resource, entity)] = ver
}

// stale reports whether session read the entity earlier and it has changed since.
func (h *history) stale(session, resource, entity string) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	ver, ok := h.seen[session][ek(resource, entity)]
	return ok && ver < h.versions[ek(resource, entity)]
}

// bump records a write and marks it as seen by the writer, whose response
// already reflects the new state.
func (h *history) bump(session, resource, entity string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	k := ek(resource, entity)
	h.versions[k]++
	if h.seen[session] == nil {
		h.seen[session] = map[string]uint64{}
	}
	h.seen[session][k] = h.versions[k]
}
