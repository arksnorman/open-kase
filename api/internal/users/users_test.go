package users

import (
	"strings"
	"testing"

	"github.com/arksnorman/open-kase/api/package/fault"
	"github.com/arksnorman/open-kase/api/package/validation"
)

func TestSplitNameAndFull(t *testing.T) {
	for in, want := range map[string]PersonName{
		"":                   {},
		"Cher":               {Given: "Cher"},
		" Dana  Okafor ":     {Given: "Dana", Family: "Okafor"},
		"Maria de la Cruz":   {Given: "Maria", Middle: "de la", Family: "Cruz"},
		"Ada B. C. Lovelace": {Given: "Ada", Middle: "B. C.", Family: "Lovelace"},
	} {
		if got := SplitName(in); got != want {
			t.Errorf("SplitName(%q) = %+v, want %+v", in, got, want)
		}
	}
	if got := (PersonName{Given: "Ada", Family: "Lovelace"}).Full(); got != "Ada Lovelace" {
		t.Errorf("Full without middle = %q", got)
	}
	if got := (PersonName{Given: " Mary  Ann ", Middle: "B.", Family: "Smith"}).Full(); got != "Mary Ann B. Smith" {
		t.Errorf("Full = %q", got)
	}
	var v validation.Collector
	PersonName{Given: "A", Family: ""}.Validate(&v, "admin")
	PersonName{Given: "", Middle: strings.Repeat("m", 51), Family: "B"}.Validate(&v, "")
	err := fault.As(v.Err())
	if err == nil || len(err.Fields) != 3 || err.Fields[0].Field != "adminFamilyName" || err.Fields[1].Field != "givenName" || err.Fields[2].Field != "middleName" {
		t.Fatalf("validation: %+v", err)
	}
}

func TestUsernameBaseSatisfiesRule(t *testing.T) {
	for email, want := range map[string]string{
		"admin@open-kase.local":                  "admin",
		"Dana@example.com":                       "dana",
		"marco.rossi@example.com":                "marco.rossi",
		"o'brien+tag@example.com":                "obrientag",
		"42jo@example.com":                       "user-jo",
		"9@example.com":                          "user",
		"x@example.com":                          "user-x",
		"ab@example.com":                         "user-ab",
		"__support__@example.com":                "support__",
		"ünïcode@example.com":                    "ncode",
		strings.Repeat("a", 40) + "@example.com": strings.Repeat("a", 32),
	} {
		got := UsernameBase(email)
		if got != want {
			t.Errorf("UsernameBase(%q) = %q, want %q", email, got, want)
		}
		if err := validation.UserUsername.CheckString("username", got); err != nil {
			t.Errorf("UsernameBase(%q) = %q fails the rule: %v", email, got, err.Message)
		}
	}
}

func TestUsernameCandidate(t *testing.T) {
	long := strings.Repeat("a", 32)
	for _, c := range []struct {
		base string
		n    int
		want string
	}{
		{"dana", 1, "dana"},
		{"dana", 2, "dana2"},
		{"dana", 10, "dana10"},
		{long, 2, strings.Repeat("a", 31) + "2"},
		{long, 100, strings.Repeat("a", 29) + "100"},
	} {
		got := UsernameCandidate(c.base, c.n)
		if got != c.want {
			t.Errorf("UsernameCandidate(%q, %d) = %q, want %q", c.base, c.n, got, c.want)
		}
		if err := validation.UserUsername.CheckString("username", got); err != nil {
			t.Errorf("%q fails the rule", got)
		}
	}
}

func TestUsernameRule(t *testing.T) {
	for _, ok := range []string{"admin", "dana", "a.b", "a_b-c", "x12", strings.Repeat("z", 32)} {
		if err := validation.UserUsername.CheckString("username", ok); err != nil {
			t.Errorf("%q rejected: %s", ok, err.Message)
		}
	}
	for _, bad := range []string{"ab", "1ab", ".ab", "Dana", "a b", "a@b.c", "dañ", strings.Repeat("z", 33)} {
		if err := validation.UserUsername.CheckString("username", bad); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
	if !IsEmailIdentifier("dana@example.com") || IsEmailIdentifier("dana") {
		t.Error("IsEmailIdentifier")
	}
}
