package tickets

import (
	"errors"
	"strings"
	"testing"

	"github.com/arksnorman/open-kase/api/internal/statuses"
)

func testSet(t *testing.T) (StatusSet, statuses.Status) {
	t.Helper()
	waiting := statuses.Status{Id: "w", Code: statuses.CodeWaitingCustomer, Name: "Awaiting customer", Category: statuses.CategoryWaitingCustomer}
	set, err := NewStatusSet([]statuses.Status{
		{Id: "o", Code: statuses.CodeOpen, Name: "Open", Category: statuses.CategoryOpen, Builtin: true},
		{Id: "h", Code: statuses.CodeOnHold, Name: "On hold", Category: statuses.CategoryOnHold, Builtin: true},
		{Id: "r", Code: statuses.CodeResolved, Name: "Resolved", Category: statuses.CategoryResolved, Builtin: true},
		{Id: "a", Code: statuses.CodeArchived, Name: "Archived", Category: statuses.CategoryArchived, Builtin: true},
		waiting,
	})
	if err != nil {
		t.Fatal(err)
	}
	return set, waiting
}

func ticketIn(st statuses.Status, assigned bool) Ticket {
	t := Ticket{Status: Ref{Id: st.Id, Name: st.Name}, StatusCategory: st.Category}
	if assigned {
		t.Assignee = &Ref{Id: "u1"}
	}
	return t
}

func TestNewStatusSetRequiresBuiltins(t *testing.T) {
	if _, err := NewStatusSet([]statuses.Status{{Id: "o", Code: "open", Builtin: true}}); err == nil {
		t.Fatal("missing built-ins accepted")
	}
	// A custom status cannot impersonate a built-in by code.
	if _, err := NewStatusSet([]statuses.Status{{Id: "x", Code: "open"}, {Id: "h", Code: "onHold", Builtin: true},
		{Id: "r", Code: "resolved", Builtin: true}, {Id: "a", Code: "archived", Builtin: true}}); err == nil {
		t.Fatal("non-built-in open accepted")
	}
}

func TestArchivedIsTerminal(t *testing.T) {
	set, waiting := testSet(t)
	tk := ticketIn(set.Archived, true)
	for _, action := range []string{"resolve", "reopen", "archive", "setStatus"} {
		if _, err := PlanTransition(tk, action, &waiting, set, true); !errors.Is(err, ErrArchived) {
			t.Errorf("%s on archived: %v", action, err)
		}
	}
	if _, err := PlanClaim(tk, set); !errors.Is(err, ErrArchived) {
		t.Error("claim on archived")
	}
	if _, err := PlanRelease(tk, set); !errors.Is(err, ErrArchived) {
		t.Error("release on archived")
	}
	if _, err := PlanAssignment(tk, set, "u2"); !errors.Is(err, ErrArchived) {
		t.Error("assign on archived")
	}
	if _, err := PlanRequesterReply(tk, set, true); !errors.Is(err, ErrArchived) {
		t.Error("reply on archived")
	}
}

func TestTransitionTable(t *testing.T) {
	set, waiting := testSet(t)
	cases := []struct {
		name     string
		from     statuses.Status
		assigned bool
		eligible bool
		action   string
		target   *statuses.Status
		want     string // resulting status ID, or "" for an error
		clear    bool
	}{
		{"resolve open", set.Open, false, false, "resolve", nil, "r", false},
		{"resolve waiting", waiting, true, true, "resolve", nil, "r", false},
		{"resolve resolved", set.Resolved, false, false, "resolve", nil, "", false},
		{"archive resolved", set.Resolved, true, true, "archive", nil, "a", false},
		{"archive open", set.Open, false, false, "archive", nil, "", false},
		{"reopen eligible", set.Resolved, true, true, "reopen", nil, "o", false},
		{"reopen ineligible", set.Resolved, true, false, "reopen", nil, "o", true},
		{"reopen unassigned", set.Resolved, false, false, "reopen", nil, "o", false},
		{"reopen open", set.Open, false, false, "reopen", nil, "", false},
		{"set waiting", set.Open, true, true, "setStatus", &waiting, "w", false},
		{"hold open", set.Open, true, true, "hold", nil, "h", false},
		{"hold waiting", waiting, false, false, "hold", nil, "h", false},
		{"hold on hold", set.OnHold, true, true, "hold", nil, "", false},
		{"hold resolved", set.Resolved, false, false, "hold", nil, "", false},
		{"set on hold via setStatus", set.Open, false, false, "setStatus", &set.OnHold, "", false},
		{"resume from hold", set.OnHold, true, true, "setStatus", &waiting, "w", false},
		{"resolve from hold", set.OnHold, true, true, "resolve", nil, "r", false},
		{"set resolved via setStatus", set.Open, false, false, "setStatus", &set.Resolved, "", false},
		{"set on resolved", set.Resolved, false, false, "setStatus", &waiting, "", false},
	}
	for _, c := range cases {
		p, err := PlanTransition(ticketIn(c.from, c.assigned), c.action, c.target, set, c.eligible)
		if c.want == "" {
			if err == nil {
				t.Errorf("%s: expected an error, got %+v", c.name, p)
			}
			continue
		}
		if err != nil || p.Status.Id != c.want || p.ClearAssignee != c.clear {
			t.Errorf("%s: got %+v %v", c.name, p, err)
		}
	}
}

func TestClaimReleaseAssignmentStatus(t *testing.T) {
	set, waiting := testSet(t)
	// No automatic Open to In progress flip: claiming and assigning keep the
	// status; releasing keeps it too.
	if p, _ := PlanClaim(ticketIn(set.Open, false), set); p.Status.Id != "o" || p.Changed(ticketIn(set.Open, false)) {
		t.Errorf("claim open → %s", p.Status.Id)
	}
	if p, _ := PlanClaim(ticketIn(waiting, false), set); p.Status.Id != "w" {
		t.Errorf("claim waiting → %s", p.Status.Id)
	}
	if _, err := PlanClaim(ticketIn(set.Resolved, false), set); err == nil {
		t.Error("claim resolved allowed")
	}
	if p, _ := PlanRelease(ticketIn(set.Open, true), set); p.Status.Id != "o" || !p.ClearAssignee {
		t.Errorf("release open → %+v", p)
	}
	if p, _ := PlanRelease(ticketIn(waiting, true), set); p.Status.Id != "w" {
		t.Errorf("release waiting → %+v", p)
	}
	if p, _ := PlanAssignment(ticketIn(set.Open, true), set, ""); p.Status.Id != "o" || !p.ClearAssignee {
		t.Errorf("unassign → %+v", p)
	}
	if p, _ := PlanAssignment(ticketIn(set.Open, false), set, "u2"); p.Status.Id != "o" {
		t.Errorf("assign open → %+v", p)
	}
}

func TestRequesterReply(t *testing.T) {
	set, waiting := testSet(t)
	if p, _ := PlanRequesterReply(ticketIn(waiting, true), set, true); p.Status.Id != "o" || p.ClearAssignee {
		t.Errorf("waiting+eligible → %+v", p)
	}
	if p, _ := PlanRequesterReply(ticketIn(set.OnHold, true), set, true); p.Status.Id != "o" || p.ClearAssignee {
		t.Errorf("on hold+eligible → %+v", p)
	}
	if p, _ := PlanRequesterReply(ticketIn(set.Resolved, true), set, false); p.Status.Id != "o" || !p.ClearAssignee || !p.ClearResolved {
		t.Errorf("resolved+ineligible → %+v", p)
	}
	if p, _ := PlanRequesterReply(ticketIn(set.Open, true), set, true); p.Status.Id != "o" || p.Changed(ticketIn(set.Open, true)) {
		t.Errorf("open → %+v", p)
	}
}

func TestSanitizeFilename(t *testing.T) {
	cases := map[string]string{
		`..\..\bounce-report.eml`: "bounce-report.eml",
		"/etc/passwd":             "passwd",
		"":                        "attachment",
		"..":                      "attachment",
		"a\x00b\r\n.txt":          "ab.txt",
		"report\u202egnp.exe":     "reportgnp.exe",
		`quo"te.pdf`:              "quote.pdf",
		"  spaced name.docx  ":    "spaced name.docx",
		"ünïcødé файл.txt":        "ünïcødé файл.txt",
	}
	for in, want := range cases {
		if got := SanitizeFilename(in); got != want {
			t.Errorf("SanitizeFilename(%q) = %q, want %q", in, got, want)
		}
	}
	long := SanitizeFilename(strings.Repeat("x", 300) + ".pdf")
	if len([]rune(long)) != 255 || long[len(long)-4:] != ".pdf" {
		t.Errorf("long name: %d %q", len([]rune(long)), long[len(long)-8:])
	}
}
