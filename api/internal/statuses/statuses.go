// Package statuses defines the protected built-in ticket statuses and the
// stable lifecycle categories that custom statuses map onto.
package statuses

// Category is an immutable lifecycle category.
type Category string

const (
	CategoryOpen            Category = "open"
	CategoryWaitingCustomer Category = "waitingCustomer"
	CategoryOnHold          Category = "onHold"
	CategoryResolved        Category = "resolved"
	CategoryArchived        Category = "archived"
)

// Terminal reports whether no ordinary mutation is allowed (archive).
func (c Category) Terminal() bool { return c == CategoryArchived }

// Reopenable reports whether a ticket in this category may be reopened.
func (c Category) Reopenable() bool { return c == CategoryResolved }

// AllowedForCustom reports whether a project-defined status may use c.
// V1 creates no custom resolved, terminal or on-hold statuses (a hold needs
// a managed hold reason).
func (c Category) AllowedForCustom() bool {
	switch c {
	case CategoryOpen, CategoryWaitingCustomer:
		return true
	}
	return false
}

// Built-in status codes. These identities cannot be deleted or repurposed.
const (
	CodeOpen            = "open"
	CodeWaitingCustomer = "waitingCustomer"
	CodeOnHold          = "onHold"
	CodeResolved        = "resolved"
	CodeArchived        = "archived"
)

// Definition describes a status to seed or display.
type Definition struct {
	Code     string
	Label    string
	Category Category
	Order    int
	Builtin  bool
}

// Builtins returns the protected installation-wide statuses in display order
// (for open-kase the "customer" is the requester contact).
func Builtins() []Definition {
	return []Definition{
		{Code: CodeOpen, Label: "Open", Category: CategoryOpen, Order: 10, Builtin: true},
		{Code: CodeWaitingCustomer, Label: "Awaiting customer", Category: CategoryWaitingCustomer, Order: 20, Builtin: true},
		{Code: CodeOnHold, Label: "On hold", Category: CategoryOnHold, Order: 30, Builtin: true},
		{Code: CodeResolved, Label: "Resolved", Category: CategoryResolved, Order: 40, Builtin: true},
		{Code: CodeArchived, Label: "Archived", Category: CategoryArchived, Order: 50, Builtin: true},
	}
}

// Workable reports whether ordinary work (claim, setStatus, resolve) may
// start from c: every category except resolved and archived.
func (c Category) Workable() bool {
	switch c {
	case CategoryOpen, CategoryWaitingCustomer, CategoryOnHold:
		return true
	}
	return false
}
