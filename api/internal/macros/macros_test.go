package macros

import (
	"testing"

	"github.com/arksnorman/open-kase/api/internal/tickets"
)

func TestRenderEscapesValuesAndKeepsUnknownText(t *testing.T) {
	tk := tickets.Ticket{Number: 1042, Requester: tickets.ContactRef{Name: "Priya [click](https://evil.example) *now*\n# Heading"}}
	got := Render("Hi {{requester.name}}, about {{ ticket.number }} / {{ticket.number}} — {{agent.name}}. {{unknown}} {{ticket.key}}", tk, "Dana <b>Okafor</b>")
	want := `Hi Priya \[click\]\(https://evil.example\) \*now\* \# Heading, about #1042 / #1042 — Dana \<b\>Okafor\</b\>. {{unknown}} {{ticket.key}}`
	if got != want {
		t.Fatalf("render:\n got %q\nwant %q", got, want)
	}
}
