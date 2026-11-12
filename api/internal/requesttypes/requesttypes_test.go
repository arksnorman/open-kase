package requesttypes

import "testing"

func TestDeriveCode(t *testing.T) {
	for name, want := range map[string]string{
		"Billing": "billing", "Service request": "serviceRequest", "API access": "apiAccess", "  spaced   out  ": "spacedOut",
		"snake_case_name": "snakeCaseName", "Tabs\tand\nlines": "tabsAndLines", "UPPER": "upper", "9 lives": "lives",
		"Ünï": "n", "日本語": "", "Q": "q", "x-1": "x1",
	} {
		if got := DeriveCode(name); got != want {
			t.Errorf("%q: %q, want %q", name, got, want)
		}
	}
}
