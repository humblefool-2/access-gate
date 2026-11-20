// SPDX-License-Identifier: Apache-2.0

package accessgate_test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"

	accessgate "github.com/humblefool-2/access-gate/lib"
)

// demoGuard declares what each caller may see, once. The handlers below return
// full objects and know nothing about roles.
func demoGuard() *accessgate.Guard {
	guard, err := accessgate.New(accessgate.Config{
		Resources: []accessgate.Resource{
			{
				Name: "customers",
				Key:  "customer_id",
				Columns: []accessgate.Column{
					{Name: "customer_id"},
					{Name: "email", Class: accessgate.PII, Mask: accessgate.Drop},
					{Name: "phone", Class: accessgate.PII, MaskFunc: accessgate.KeepLast(4)},
				},
			},
			{
				Name: "wallets",
				Key:  "wallet_id",
				Columns: []accessgate.Column{
					{Name: "wallet_id"},
					{Name: "balance_minor"},
				},
			},
		},
	})
	if err != nil {
		panic(err)
	}
	return guard
}

func getCustomer(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"customer_id": r.PathValue("id"),
		"email":       "ana@example.com",
		"phone":       "0712345678",
	})
}

func getWallet(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"wallet_id": r.PathValue("id"), "balance_minor": 1000})
}

func creditWallet(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"wallet_id": r.PathValue("id"), "balance_minor": 1500})
}

// serve sends one request for entity id as principal p. In a real service the
// principal comes from your authentication (see Authenticate); WithPrincipal
// puts one on the request context directly.
func serve(h http.HandlerFunc, method, id string, p accessgate.Principal) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, "/"+id, nil)
	r.SetPathValue("id", id)
	r = r.WithContext(accessgate.WithPrincipal(r.Context(), p))
	rec := httptest.NewRecorder()
	h(rec, r)
	return rec
}

// A caller with the public view never sees the email, and the phone number is
// masked, although the handler returned both in full.
func ExampleGuard_Handle() {
	guard := demoGuard()
	h := guard.Handle("customers", getCustomer)

	caller := accessgate.Principal{ID: "agent-1", SessionID: "run-1", View: accessgate.ViewPublic}
	fmt.Print(serve(h, http.MethodGet, "c-42", caller).Body.String())
	// Output:
	// {"customer_id":"c-42","phone":"******5678"}
}

// The same route and handler give a caller with the PII view every column.
func ExampleGuard_Handle_readPII() {
	guard := demoGuard()
	h := guard.Handle("customers", getCustomer)

	caller := accessgate.Principal{ID: "support-1", SessionID: "ticket-9", View: accessgate.ViewPII}
	fmt.Print(serve(h, http.MethodGet, "c-42", caller).Body.String())
	// Output:
	// {"customer_id":"c-42","email":"ana@example.com","phone":"0712345678"}
}

// A session that read a wallet cannot write it after another session changed
// it, until it reads the wallet again. The guard answers 409 instead of letting
// the write overwrite what the first session never saw.
func ExampleGuard_Handle_staleRead() {
	guard := demoGuard()
	read := guard.Handle("wallets", getWallet)
	credit := guard.Handle("wallets", creditWallet)

	a := accessgate.Principal{ID: "agent-a", SessionID: "a", View: accessgate.ViewPII}
	b := accessgate.Principal{ID: "agent-b", SessionID: "b", View: accessgate.ViewPII}

	// A reads the wallet, then B changes it.
	serve(read, http.MethodGet, "w-1", a)
	serve(credit, http.MethodPost, "w-1", b)

	// A's write is refused as stale.
	fmt.Println(serve(credit, http.MethodPost, "w-1", a).Code)

	// After A reads again, the write goes through.
	serve(read, http.MethodGet, "w-1", a)
	fmt.Println(serve(credit, http.MethodPost, "w-1", a).Code)
	// Output:
	// 409
	// 200
}
