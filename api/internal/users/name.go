package users

import (
	"strings"

	"github.com/arksnorman/open-kase/api/package/validation"
)

// PersonName is a staff user's name in parts (owner request 2026-10-31):
// given name, optional middle name and family name. The display name shown
// everywhere is derived from it (Full).
type PersonName struct {
	Given, Middle, Family string
}

// Trimmed returns the name with surrounding white space removed from each
// part.
func (n PersonName) Trimmed() PersonName {
	return PersonName{Given: strings.TrimSpace(n.Given), Middle: strings.TrimSpace(n.Middle), Family: strings.TrimSpace(n.Family)}
}

// Full joins the non-empty parts with single spaces.
func (n PersonName) Full() string {
	parts := make([]string, 0, 3)
	for _, p := range []string{n.Given, n.Middle, n.Family} {
		if p = strings.Join(strings.Fields(p), " "); p != "" {
			parts = append(parts, p)
		}
	}
	return strings.Join(parts, " ")
}

// Validate records field errors for the parts under the request fields
// "givenName", "middleName" and "familyName", each capitalised after a
// non-empty camelCase prefix (adminGivenName).
// Given and family names are required. Call it on a trimmed name.
func (n PersonName) Validate(v *validation.Collector, prefix string) {
	v.String(validation.UserGivenName, nameField(prefix, "givenName"), n.Given)
	v.String(validation.UserMiddleName, nameField(prefix, "middleName"), n.Middle)
	v.String(validation.UserFamilyName, nameField(prefix, "familyName"), n.Family)
}

func nameField(prefix, name string) string {
	if prefix == "" {
		return name
	}
	return prefix + strings.ToUpper(name[:1]) + name[1:]
}

// SplitName splits a single-string name on white space: the first word is
// the given name, the last the family name and any words between the middle
// name. A single word is a given name only.
func SplitName(s string) PersonName {
	words := strings.Fields(s)
	switch len(words) {
	case 0:
		return PersonName{}
	case 1:
		return PersonName{Given: words[0]}
	}
	return PersonName{Given: words[0], Middle: strings.Join(words[1:len(words)-1], " "), Family: words[len(words)-1]}
}

// NamePatch changes name parts; nil parts are unchanged and an empty middle
// name clears it.
type NamePatch struct {
	Given, Middle, Family *string
}

// Empty reports whether the patch changes nothing.
func (p NamePatch) Empty() bool { return p.Given == nil && p.Middle == nil && p.Family == nil }

// Apply returns cur with the patch applied and trimmed.
func (p NamePatch) Apply(cur PersonName) PersonName {
	if p.Given != nil {
		cur.Given = *p.Given
	}
	if p.Middle != nil {
		cur.Middle = *p.Middle
	}
	if p.Family != nil {
		cur.Family = *p.Family
	}
	return cur.Trimmed()
}
