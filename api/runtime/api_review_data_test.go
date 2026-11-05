package runtime

import (
	"context"
	"testing"

	"go.mongodb.org/mongo-driver/v2/bson"

	"github.com/arksnorman/open-kase/api/internal/tickets"
)

// Regression tests of the data-exposure review (OC-DATA-…).

// cloneDocs inserts n copies of the stored document id of collection col,
// each changed by edit (bulk test data written directly, without the
// services).
func cloneDocs(t *testing.T, h *harness, col string, id bson.ObjectID, n int, edit func(i int, d bson.M)) {
	t.Helper()
	ctx := context.Background()
	var src bson.M
	if err := h.coll(col).FindOne(ctx, bson.M{"_id": id}).Decode(&src); err != nil {
		t.Fatal(err)
	}
	docs := make([]any, 0, n)
	for i := 0; i < n; i++ {
		d := bson.M{}
		for k, v := range src {
			d[k] = v
		}
		d["_id"] = bson.NewObjectID()
		edit(i, d)
		docs = append(docs, d)
	}
	if _, err := h.coll(col).InsertMany(ctx, docs); err != nil {
		t.Fatal(err)
	}
}

// OC-DATA-02: search bounded its candidates (1,000 matching subjects and
// 5,000 matching messages per audience) before applying the caller's
// ticket access. A group-restricted agent in a project with many matching
// tickets in other groups got incomplete results (here: none of their own
// tickets) together with totalCapped: true, which revealed that many
// tickets they cannot read match the words.
func TestSearchBoundAppliesAfterAuthorization(t *testing.T) {
	f := newTicketFixture(t)
	ctx := context.Background()
	// Dana works in Triage only.
	mine := f.createTicket(f.dana.Access, map[string]any{"subject": "Quasar relay down", "body": "The nebula gateway rejects mail."}).Ref()
	// Delivery holds more matches than the bounds, each ranking higher.
	other := f.createTicket(f.marco.Access, map[string]any{"subject": "Quasar quasar quasar", "groupId": f.delivery,
		"body": "nebula nebula nebula nebula"}).Str("id")
	otherID, _ := bson.ObjectIDFromHex(other)
	cloneDocs(t, f.harness, "tickets", otherID, tickets.MaxSearchCandidates, func(i int, d bson.M) {
		d["number"] = int64(100000 + i)
	})
	var msg bson.M
	if err := f.coll("messages").FindOne(ctx, bson.M{"ticketId": otherID}).Decode(&msg); err != nil {
		t.Fatal(err)
	}
	cloneDocs(t, f.harness, "messages", msg["_id"].(bson.ObjectID), 5*tickets.MaxSearchCandidates, func(i int, d bson.M) {
		d["seq"] = int64(1000 + i)
	})

	for _, q := range []string{"quasar", "nebula"} {
		p := decodeSearch(t, f.search(f.dana.Access, q).expect(200))
		if p.Total != 1 || p.TotalCapped || len(p.Items) != 1 || p.Items[0].Ticket.Ref() != mine {
			t.Errorf("%s: Dana (Triage) got total %d, capped %v, %d items; want only %s", q, p.Total, p.TotalCapped, len(p.Items), mine)
		}
	}
	// Marco reads both groups: his subject results are honestly capped.
	if p := decodeSearch(t, f.search(f.marco.Access, "quasar").expect(200)); !p.TotalCapped || p.Total < int64(tickets.MaxSearchCandidates) {
		t.Errorf("Marco: total %d, capped %v", p.Total, p.TotalCapped)
	}
}
