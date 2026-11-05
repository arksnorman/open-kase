package runtime

import (
	"slices"
	"testing"
)

// Tickets are identified by number: responses carry number and no key,
// /tickets/{t} accepts the number, "#number" and the ID, and list/search find
// "1042" and "#1042".
func TestTicketNumbersReplaceKeys(t *testing.T) {
	f := newTicketFixture(t)
	tk := f.createTicket(f.dana.Access, map[string]any{"subject": "Relay rejects mail"})
	if _, ok := tk.JSON()["key"]; ok || tk.JSON()["number"] != float64(1) {
		t.Fatalf("created: %s", tk.Body)
	}
	if loc := tk.Header.Get("Location"); loc != "/api/v1/tickets/1" {
		t.Fatalf("Location %q", loc)
	}
	for _, ref := range []string{"1", "%231", tk.Str("id")} {
		if got := f.get(f.dana.Access, ref).expect(200); got.Str("id") != tk.Str("id") {
			t.Fatalf("%s: %s", ref, got.Body)
		}
	}
	for _, ref := range []string{"0", "1x", "-1", "%23", "MAIL-1", "sup-1"} {
		f.get(f.dana.Access, ref).expect(404, "ticketNotFound")
	}
	f.createTicket(f.dana.Access, map[string]any{"subject": "Second"})
	for _, q := range []string{"1", "%231"} {
		if got := keys(f.do("GET", "/api/v1/tickets?q="+q, nil, bearer(f.dana.Access)).expect(200).Items()); !slices.Equal(got, []string{"1"}) {
			t.Fatalf("list q=%s: %v", q, got)
		}
	}
	// Full-text search puts the numbered ticket first.
	p := decodeSearch(t, f.do("GET", "/api/v1/search?q=%232", nil, bearer(f.dana.Access)).expect(200))
	if len(p.Items) == 0 || p.Items[0].Ticket.Ref() != "2" {
		t.Fatalf("search #2: %+v", p)
	}
	// A ticket the caller cannot read is not revealed by its number.
	hidden := f.createTicket(f.marco.Access, map[string]any{"subject": "Delivery only", "groupId": f.delivery}).Ref()
	p = decodeSearch(t, f.do("GET", "/api/v1/search?q=%23"+hidden, nil, bearer(f.dana.Access)).expect(200))
	for _, h := range p.Items {
		if h.Ticket.Ref() == hidden {
			t.Fatal("search revealed a ticket outside Dana's groups")
		}
	}
}
