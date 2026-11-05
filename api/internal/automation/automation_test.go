package automation

import (
	"testing"
	"time"

	"github.com/arksnorman/open-kase/api/internal/statuses"
	"github.com/arksnorman/open-kase/api/internal/tickets"
)

func TestDue(t *testing.T) {
	t0 := time.Date(2026, 10, 5, 9, 0, 0, 0, time.UTC)
	p := Policy{ArchiveAfterResolvedDays: 2, FollowupAfterHours: 24, MaxReminders: 2, ReminderCooldownHours: 12, AutoResolveAfterHours: 48}
	// The last reminder of a complete series carries the notice.
	waiting := func(reminders int, last *time.Time) tickets.Ticket {
		return tickets.Ticket{StatusCategory: statuses.CategoryWaitingCustomer,
			Automation: tickets.AutomationState{WaitingSince: &t0, Reminders: reminders, LastReminderAt: last, NoticeGiven: reminders >= p.MaxReminders}}
	}
	// Reminders exhausted before auto-resolution existed: no notice yet.
	unnoticed := func(reminders int, last *time.Time) tickets.Ticket {
		x := waiting(reminders, last)
		x.Automation.NoticeGiven = false
		return x
	}
	at := func(h int) time.Time { return t0.Add(time.Duration(h) * time.Hour) }
	r1 := at(24)
	cases := []struct {
		name string
		t    tickets.Ticket
		now  time.Time
		want string
	}{
		{"before follow-up", waiting(0, nil), at(23), ""},
		{"first reminder", waiting(0, nil), at(24), ActionRemind},
		{"cooldown", waiting(1, &r1), at(35), ""},
		{"second reminder", waiting(1, &r1), at(36), ActionRemind},
		{"notice period", waiting(2, &r1), at(71), ""},
		{"auto-resolve", waiting(2, &r1), at(72), ActionResolve},
		// OC-ASYNC-06: never resolve without notice; a final reminder
		// carries it after the cooldown, then the auto-resolve delay runs.
		{"no notice, cooldown", unnoticed(2, &r1), at(35), ""},
		{"no notice, final reminder", unnoticed(2, &r1), at(36), ActionRemind},
		{"no notice, never resolved", unnoticed(2, &r1), at(1000), ActionRemind},
		{"lowered maximum, final reminder", unnoticed(3, &r1), at(36), ActionRemind},
		{"no clock", tickets.Ticket{StatusCategory: statuses.CategoryWaitingCustomer}, at(500), ""},
		{"archived", tickets.Ticket{StatusCategory: statuses.CategoryArchived, ResolvedAt: &t0}, at(500), ""},
		{"resolved early", tickets.Ticket{StatusCategory: statuses.CategoryResolved, ResolvedAt: &t0}, at(47), ""},
		{"archive", tickets.Ticket{StatusCategory: statuses.CategoryResolved, ResolvedAt: &t0}, at(48), ActionArchive},
		{"other status", tickets.Ticket{StatusCategory: statuses.CategoryOnHold, Automation: tickets.AutomationState{WaitingSince: &t0}}, at(500), ""},
	}
	for _, c := range cases {
		got, _ := Due(p, c.t, c.now)
		if got != c.want {
			t.Errorf("%s: %q, want %q", c.name, got, c.want)
		}
	}
	// Without auto-resolution reminders stop at the maximum.
	p.AutoResolveAfterHours = 0
	if got, _ := Due(p, waiting(2, &r1), at(1000)); got != "" {
		t.Errorf("no auto-resolve: %q", got)
	}
	if (Policy{}).Enabled() || !(Policy{ArchiveAfterResolvedDays: 1}).Enabled() {
		t.Error("Enabled")
	}
}
