// SPDX-License-Identifier: Apache-2.0

package main

import (
	"database/sql"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	accessgate "github.com/humblefool-2/access-gate/lib"
)

func testAuthn(t *testing.T) accessgate.Authenticator {
	t.Helper()
	a, err := tokenAuth("rk=read,pk=read-pii,wk=write")
	if err != nil {
		t.Fatal(err)
	}
	return a
}

func testDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := openStore(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

func TestHealth(t *testing.T) {
	rec := httptest.NewRecorder()
	newMux(testDB(t), testAuthn(t)).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/health", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
}

func TestHealthDBDown(t *testing.T) {
	db := testDB(t)
	db.Close()

	rec := httptest.NewRecorder()
	newMux(db, testAuthn(t)).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/health", nil))

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", rec.Code)
	}
}

// The service's own auth (requireAuth) refuses a missing or bad token on both
// the guarded and the baseline mux; /health stays open. Role and field rules
// are the lib's job and are covered elsewhere.
func TestServiceAuthRefusesAnonymousOnBothMuxes(t *testing.T) {
	db := testDB(t)
	for name, h := range map[string]http.Handler{
		"guarded":  newMux(db, testAuthn(t)),
		"baseline": newBaselineMux(db, testAuthn(t)),
	} {
		for _, tok := range []string{"", "bad"} {
			req := httptest.NewRequest(http.MethodGet, "/customers/x", nil)
			if tok != "" {
				req.Header.Set("Authorization", "Bearer "+tok)
			}
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)
			if rec.Code != http.StatusUnauthorized {
				t.Errorf("%s token %q: status %d, want 401", name, tok, rec.Code)
			}
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/health", nil))
		if rec.Code != http.StatusOK {
			t.Errorf("%s /health: status %d, want 200", name, rec.Code)
		}
	}
}

// Who may call a write route is the service's decision (requireWrite), not the
// lib's, so a read principal is refused on the baseline mux (no guard) too.
func TestServiceRoleCheckRefusesReadersOnBothMuxes(t *testing.T) {
	db := testDB(t)
	for name, h := range map[string]http.Handler{
		"guarded":  newMux(db, testAuthn(t)),
		"baseline": newBaselineMux(db, testAuthn(t)),
	} {
		for _, path := range []string{"/wallets/wal_demo1/credit", "/wallets/wal_demo1/debit", "/customers"} {
			for _, tok := range []string{"rk", "pk"} {
				req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(`{"amount_minor":1}`))
				req.Header.Set("Authorization", "Bearer "+tok)
				rec := httptest.NewRecorder()
				h.ServeHTTP(rec, req)
				if rec.Code != http.StatusForbidden {
					t.Errorf("%s: POST %s as %s = %d, want 403", name, path, tok, rec.Code)
				}
			}
		}
	}
}
