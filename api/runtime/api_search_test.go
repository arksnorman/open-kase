package runtime

import (
	"context"
	"encoding/json"
	"slices"
	"strconv"
	"strings"
	"testing"
	"unicode/utf16"

	"go.mongodb.org/mongo-driver/v2/bson"
)

// ---- Search ----

func (f *ticketFixture) search(tok, q string, extra ...string) result {
	f.t.Helper()
	path := "/api/v1/search?q=" + strings.ReplaceAll(q, " ", "+") + strings.Join(extra, "")
	return f.do("GET", path, nil, bearer(tok))
}

type searchHit struct {
	Ticket   searchTicket `json:"ticket"`
	Snippets []struct {
		Field      string  `json:"field"`
		MessageId  *string `json:"messageId"`
		Visibility *string `json:"visibility"`
		Text       string  `json:"text"`
		Highlights []struct {
			Start int `json:"start"`
			End   int `json:"end"`
		} `json:"highlights"`
	} `json:"snippets"`
}

type searchTicket struct {
	Number int64 `json:"number"`
}

// Ref is the ticket number as a string ("12").
func (t searchTicket) Ref() string { return strconv.FormatInt(t.Number, 10) }

type searchPage struct {
	Items       []searchHit `json:"items"`
	NextCursor  *string     `json:"nextCursor"`
	Total       int64       `json:"total"`
	TotalCapped bool        `json:"totalCapped"`
}

func decodeSearch(t *testing.T, r result) searchPage {
	t.Helper()
	var p searchPage
	if err := json.Unmarshal(r.Body, &p); err != nil {
		t.Fatalf("decode search: %v: %s", err, r.Body)
	}
	return p
}

func hitKeys(p searchPage) []string {
	out := []string{}
	for _, h := range p.Items {
		out = append(out, h.Ticket.Ref())
	}
	slices.Sort(out)
	return out
}

// highlighted returns the highlighted substrings (UTF-16 offsets).
func highlighted(text string, start, end int) string {
	u := utf16.Encode([]rune(text))
	if start < 0 || end > len(u) || start > end {
		return "<out of range>"
	}
	return string(utf16.Decode(u[start:end]))
}

func TestSearchSnippetsAndCountsNeverLeakAgentNotes(t *testing.T) {
	f := newTicketFixture(t)
	// #1: the word only appears in an agent note.
	f.createTicket(f.dana.Access, map[string]any{"subject": "Mailbox quota", "body": "My mailbox is full."})
	f.do("POST", tpath("1", "/messages"), map[string]any{"visibility": "agent", "body": "Internal: pineapple passphrase rotation for the relay."},
		bearer(f.dana.Access)).expect(201)
	// #2: subject match.
	f.createTicket(f.dana.Access, map[string]any{"subject": "Pineapple invoice question", "body": "Please resend the invoice."})
	// #3: public message match (posted by the requester through the integration), with a non-BMP character.
	f.createTicket(f.integration, map[string]any{"subject": "Shipment", "body": "🍍 Pineapple shipment is late again."})
	// #4: no match.
	f.createTicket(f.dana.Access, map[string]any{"subject": "Old", "body": "banana duplicate"})
	// #5: in Delivery, which Dana cannot see.
	f.createTicket(f.marco.Access, map[string]any{"subject": "Delivery", "body": "pineapple at the depot", "groupId": f.delivery})

	// Integration: public content only, counts after authorization.
	p := decodeSearch(t, f.search(f.integration, "pineapple").expect(200))
	if got := hitKeys(p); !slices.Equal(got, []string{"2", "3", "5"}) || p.Total != 3 || p.TotalCapped {
		t.Fatalf("integration hits %v total %d", got, p.Total)
	}
	raw := string(f.search(f.integration, "pineapple").Body)
	if strings.Contains(raw, "passphrase") || strings.Contains(raw, `"agent"`) || strings.Contains(raw, `"number":4,`) {
		t.Fatalf("integration search leaked private or non-matching content: %s", raw)
	}
	// A word that only occurs in an agent note: nothing, not even a count.
	if p := decodeSearch(t, f.search(f.integration, "passphrase").expect(200)); p.Total != 0 || len(p.Items) != 0 {
		t.Fatalf("agent note matched for integration: %+v", p)
	}
	// tickets:read alone reads public messages too: the same hits.
	if p := decodeSearch(t, f.search(f.readOnlyIntegraton, "pineapple").expect(200)); !slices.Equal(hitKeys(p), []string{"2", "3", "5"}) {
		t.Fatalf("read-only token search: %v", hitKeys(p))
	}

	// Dana (Triage only): sees her group's agent note, not Delivery.
	p = decodeSearch(t, f.search(f.dana.Access, "pineapple").expect(200))
	if got := hitKeys(p); !slices.Equal(got, []string{"1", "2", "3"}) || p.Total != 3 {
		t.Fatalf("dana hits %v total %d", got, p.Total)
	}
	for _, h := range p.Items {
		if len(h.Snippets) == 0 {
			t.Fatalf("%s has no snippet", h.Ticket.Ref())
		}
		for _, s := range h.Snippets {
			if len(s.Highlights) == 0 {
				t.Fatalf("%s snippet without highlight: %+v", h.Ticket.Ref(), s)
			}
			for _, r := range s.Highlights {
				if got := strings.ToLower(highlighted(s.Text, r.Start, r.End)); got != "pineapple" {
					t.Fatalf("%s highlight %q in %q", h.Ticket.Ref(), got, s.Text)
				}
			}
			if strings.ContainsAny(s.Text, "<>") {
				t.Fatalf("snippet contains markup: %q", s.Text)
			}
		}
		if h.Ticket.Ref() == "1" && (h.Snippets[len(h.Snippets)-1].Visibility == nil || *h.Snippets[len(h.Snippets)-1].Visibility != "agent") {
			t.Fatalf("#1 snippet should be the agent note: %+v", h.Snippets)
		}
		if h.Ticket.Ref() == "3" && h.Snippets[0].Highlights[0].Start != 3 {
			// "🍍" is two UTF-16 code units, then a space.
			t.Fatalf("UTF-16 offset: %+v", h.Snippets[0])
		}
	}
	// The lead sees every group; pagination is stable and complete.
	var seen []string
	cursor := ""
	for i := 0; i < 10; i++ {
		extra := "&limit=1"
		if cursor != "" {
			extra += "&cursor=" + cursor
		}
		p := decodeSearch(t, f.search(f.lead.Access, "pineapple", extra).expect(200))
		if p.Total != 4 {
			t.Fatalf("lead total %d", p.Total)
		}
		for _, h := range p.Items {
			seen = append(seen, h.Ticket.Ref())
		}
		if p.NextCursor == nil {
			break
		}
		cursor = *p.NextCursor
	}
	slices.Sort(seen)
	if !slices.Equal(seen, []string{"1", "2", "3", "5"}) {
		t.Fatalf("paged lead hits %v", seen)
	}

	// Validation, and the list q parameter keeps working.
	f.do("GET", "/api/v1/search?q=", nil, bearer(f.dana.Access)).expect(422, "validationFailed")
	f.do("GET", "/api/v1/search?q=-pineapple", nil, bearer(f.dana.Access)).expect(422, "validationFailed")
	if got := keys(f.do("GET", "/api/v1/tickets?q=Pineapple", nil, bearer(f.dana.Access)).expect(200).Items()); !slices.Equal(got, []string{"2"}) {
		t.Fatalf("list q: %v", got)
	}
}

// ---- Idempotency ----

func TestIdempotencyKeyReuseWithDifferentBodyIsRejected(t *testing.T) {
	f := newTicketFixture(t)
	key := header("Idempotency-Key", "same-key-123")

	type call struct {
		name     string
		path     string
		tok      string
		body     map[string]any
		other    map[string]any
		status   int
		countCol string
	}
	ticket := map[string]any{"subject": "Idempotent", "body": "First body", "requester": map[string]any{"name": "Priya Raman", "email": "priya@acme.example"}}
	f.createTicket(f.dana.Access, map[string]any{})
	calls := []call{
		{"create ticket", "/api/v1/tickets", f.dana.Access, ticket,
			map[string]any{"subject": "Idempotent", "body": "Changed body", "requester": map[string]any{"name": "Priya Raman", "email": "priya@acme.example"}}, 201, "tickets"},
		{"post message", tpath("1", "/messages"), f.integration, map[string]any{"visibility": "public", "body": "Hello"},
			map[string]any{"visibility": "public", "body": "Hello!"}, 201, "messages"},
	}
	for _, c := range calls {
		ctx := context.Background()
		first := f.do("POST", c.path, c.body, bearer(c.tok), key).expect(c.status)
		n, _ := f.coll(c.countCol).CountDocuments(ctx, bson.D{})
		replay := f.do("POST", c.path, c.body, bearer(c.tok), key).expect(c.status)
		if replay.Header.Get("Idempotent-Replayed") != "true" || replay.Str("id") != first.Str("id") {
			t.Fatalf("%s: replay %s vs %s", c.name, replay.Body, first.Body)
		}
		// Deterministic: every reuse with a different body is the same 409.
		for i := 0; i < 3; i++ {
			f.do("POST", c.path, c.other, bearer(c.tok), key).expect(409, "idempotencyKeyReused")
		}
		if m, _ := f.coll(c.countCol).CountDocuments(ctx, bson.D{}); m != n {
			t.Fatalf("%s: documents %d → %d", c.name, n, m)
		}
		// The original request still replays after the conflicts.
		f.do("POST", c.path, c.body, bearer(c.tok), key).expect(c.status)
	}
}
