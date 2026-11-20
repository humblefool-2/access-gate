// SPDX-License-Identifier: Apache-2.0

package accessgate

import "fmt"

// View is what a session sees of the data: which columns come back. It is not
// a role and not a permission. Roles belong to the service: it decides who may
// call a write route, then maps each role to the View that caller gets, and
// several roles can share one View. Each View includes the ones below it.
type View int

const (
	NoView     View = iota
	ViewPublic      // non-PII columns only
	ViewPII         // every column
)

// Allows reports whether this View includes the View `need`.
func (v View) Allows(need View) bool { return v >= need }

func (v View) String() string {
	switch v {
	case ViewPublic:
		return "public"
	case ViewPII:
		return "pii"
	}
	return "none"
}

// ParseView accepts "public" and "pii".
func ParseView(s string) (View, error) {
	for _, v := range []View{ViewPublic, ViewPII} {
		if v.String() == s {
			return v, nil
		}
	}
	return NoView, fmt.Errorf("unknown view %q (want public or pii)", s)
}
