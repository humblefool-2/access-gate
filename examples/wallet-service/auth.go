// SPDX-License-Identifier: Apache-2.0

package main

import (
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"strings"

	accessgate "github.com/humblefool-2/access-gate/lib"
)

func errBody(msg string) map[string]string { return map[string]string{"error": msg} }

// This file is the service's own authentication, and only an example: static
// bearer tokens mapped to a role. the access-gate library never sees tokens. Swap this for
// JWT, a gateway header or anything else that yields an accessgate.Principal.

// defaultTokens are used when SESSION_TOKENS is unset.
const defaultTokens = "dev-read=read,dev-read-pii=read-pii,dev-write=write"

// tokenAuth builds an Authenticator from "token=read|read-pii|write,...".
// The session is the X-Session-ID header (one agent run); without it, the
// token itself identifies the session.
func tokenAuth(spec string) (accessgate.Authenticator, error) {
	type grant struct {
		view accessgate.View
		role string
	}
	grants := map[string]grant{}
	for _, pair := range strings.Split(spec, ",") {
		tok, lvl, ok := strings.Cut(strings.TrimSpace(pair), "=")
		if !ok || tok == "" {
			return nil, fmt.Errorf("bad token entry %q (want token=level)", pair)
		}
		// The service's own roles, each mapped to the view of the data it gets.
		// "write" is a permission the service checks itself; it sees all columns.
		switch lvl {
		case "read":
			grants[tok] = grant{accessgate.ViewPublic, "read"}
		case "read-pii":
			grants[tok] = grant{accessgate.ViewPII, "read-pii"}
		case "write":
			grants[tok] = grant{accessgate.ViewPII, "write"}
		default:
			return nil, fmt.Errorf("unknown role %q (want read, read-pii or write)", lvl)
		}
	}
	return func(r *http.Request) (accessgate.Principal, error) {
		tok, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		g, known := grants[tok]
		if !ok || !known {
			return accessgate.Principal{}, errors.New("missing or invalid token")
		}
		return accessgate.Principal{ID: tok, SessionID: r.Header.Get("X-Session-ID"), View: g.view, Attrs: map[string]string{"role": g.role}}, nil
	}, nil
}

func authFromEnv() (accessgate.Authenticator, error) {
	spec := os.Getenv("SESSION_TOKENS")
	if spec == "" {
		spec = defaultTokens
		slog.Warn("SESSION_TOKENS not set, using dev tokens")
	}
	return tokenAuth(spec)
}
