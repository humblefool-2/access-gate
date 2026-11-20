// SPDX-License-Identifier: Apache-2.0

package accessgate

import (
	"context"
	"errors"
	"net/http"
)

var (
	ErrUnauthenticated = errors.New("no authenticated principal")
	ErrForbidden       = errors.New("not authorized for this operation")
)

// Principal is the caller as the service authenticated them. The lib never
// sees credentials: the service verifies a JWT, API key, mTLS cert or
// anything else, then hands over who it is and what role it holds.
type Principal struct {
	ID        string // who (user or agent)
	SessionID string // one agent run / conversation; workflow rules are per session
	View      View   // the view of the data this caller gets (columns); not a permission
	// Attrs carries whatever else the service knows about the caller (tenant,
	// customer id, region ...) for Resource.Scope to use. The lib never reads it.
	Attrs map[string]string
}

func (p Principal) session() string {
	if p.SessionID != "" {
		return p.SessionID
	}
	return p.ID
}

type ctxKey struct{}

func WithPrincipal(ctx context.Context, p Principal) context.Context {
	return context.WithValue(ctx, ctxKey{}, p)
}

func PrincipalFrom(ctx context.Context) (Principal, bool) {
	p, ok := ctx.Value(ctxKey{}).(Principal)
	return p, ok
}

// Authenticator is supplied by the service. Returning an error means "no
// valid principal"; the guard then answers 401 on guarded routes.
type Authenticator func(*http.Request) (Principal, error)

// Authenticate puts the principal from authn on the request context. It never
// rejects by itself, so unguarded routes (health checks) keep working.
func Authenticate(authn Authenticator, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if p, err := authn(r); err == nil {
			r = r.WithContext(WithPrincipal(r.Context(), p))
		}
		next.ServeHTTP(w, r)
	})
}
