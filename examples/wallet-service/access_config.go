// SPDX-License-Identifier: Apache-2.0

package main

import (
	"encoding/json"
	"errors"
	"os"

	accessgate "github.com/humblefool-2/access-gate/lib"
)

// accessConfig is everything the lib needs to know about this service: which
// columns are PII, what each level sees by default, and which agent tools exist.
func accessConfig(a *app) accessgate.Config {
	return accessgate.Config{
		// STRICT_FIELDS=on: a ?fields= request for something the role may not see
		// (or that is not a column) is a 403 instead of being silently dropped.
		StrictFields: os.Getenv("STRICT_FIELDS") == "on",
		// The service's own permission rule for agent tools, the same one
		// requireWrite applies to the REST routes: only write-role principals
		// may use tools that change data.
		Authorize: func(p accessgate.Principal, t accessgate.ToolInfo) error {
			if t.Mutates && p.Attrs["role"] != "write" {
				return errors.New("tool " + t.Name + " needs the write role")
			}
			return nil
		},
		Resources: []accessgate.Resource{
			{
				Name: "customers",
				Key:  "customer_id",
				Columns: []accessgate.Column{
					{Name: "customer_id"},
					{Name: "email", Class: accessgate.PII, Mask: accessgate.Drop},
					{Name: "phone", Class: accessgate.PII, Mask: accessgate.Mask},
					{Name: "wallet"},
				},
				Fields: map[accessgate.View][]string{
					accessgate.ViewPublic: {"customer_id", "phone", "wallet"}, // phone shows as "***"
					accessgate.ViewPII:    {"customer_id", "email", "phone", "wallet"},
				},
			},
			{
				Name: "wallets",
				Key:  "wallet_id",
				Columns: []accessgate.Column{
					{Name: "wallet_id"}, {Name: "currency"}, {Name: "status"},
					{Name: "balance_minor"}, {Name: "blocked_minor"}, {Name: "available_minor"},
					{Name: "holds"},
				},
			},
		},
		Tools: []accessgate.ToolSpec{
			{Name: "get_customer", Description: "customer and wallet by {id}", Resource: "customers", Handler: tool(a.getCustomer), InputSchema: idSchema},
			{Name: "get_wallet", Description: "wallet and active holds by {id}", Resource: "wallets", Handler: tool(a.getWallet), InputSchema: idSchema},
			{Name: "credit", Description: "add {amount_minor} to wallet {id}", Resource: "wallets", Mutates: true, Handler: tool(a.credit), InputSchema: amountSchema},
			{Name: "debit", Description: "remove {amount_minor} from wallet {id}", Resource: "wallets", Mutates: true, Handler: tool(a.debit), InputSchema: amountSchema},
			{Name: "set_status", Description: "set wallet {id} {status}: active|frozen|closed", Resource: "wallets", Mutates: true, Handler: tool(a.setStatus), InputSchema: statusSchema},
		},
	}
}

// JSON Schemas for the tool arguments (shown to agents; the lib does not
// validate against them). "fields" narrows the response, never widens it.
var (
	idSchema = json.RawMessage(`{"type":"object","properties":{` +
		`"id":{"type":"string","description":"entity id"},` +
		`"fields":{"type":"array","items":{"type":"string"},"description":"only these columns"}},"required":["id"]}`)
	amountSchema = json.RawMessage(`{"type":"object","properties":{` +
		`"id":{"type":"string","description":"wallet id"},` +
		`"amount_minor":{"type":"integer","minimum":1,"description":"amount in minor units"}},"required":["id","amount_minor"]}`)
	statusSchema = json.RawMessage(`{"type":"object","properties":{` +
		`"id":{"type":"string","description":"wallet id"},` +
		`"status":{"type":"string","enum":["active","frozen","closed"]}},"required":["id","status"]}`)
)
