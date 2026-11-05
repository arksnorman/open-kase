package statuses

import "testing"

func TestBuiltins(t *testing.T) {
	want := map[string]Category{CodeOpen: CategoryOpen, CodeWaitingCustomer: CategoryWaitingCustomer, CodeOnHold: CategoryOnHold, CodeResolved: CategoryResolved, CodeArchived: CategoryArchived}
	got := Builtins()
	if len(got) != len(want) {
		t.Fatalf("got %d builtins", len(got))
	}
	for _, d := range got {
		if want[d.Code] != d.Category || !d.Builtin {
			t.Errorf("builtin %+v", d)
		}
	}
}

func TestCategoryRules(t *testing.T) {
	if !CategoryArchived.Terminal() || CategoryResolved.Terminal() {
		t.Error("only archived is terminal")
	}
	if !CategoryResolved.Reopenable() || CategoryArchived.Reopenable() {
		t.Error("resolved reopenable, archived not")
	}
	for _, c := range []Category{CategoryResolved, CategoryArchived, CategoryOnHold} {
		if c.AllowedForCustom() {
			t.Errorf("custom status may not use %s", c)
		}
	}
	if !CategoryWaitingCustomer.AllowedForCustom() {
		t.Error("waitingCustomer must be allowed for custom statuses")
	}
}
