package runtime

import (
	"bytes"
	"context"
	"slices"
	"strings"
	"testing"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
)

func TestSeedDemoIsIdempotent(t *testing.T) {
	h := newHarness(t)
	admin := h.admin()

	ctx := context.Background()
	var out bytes.Buffer
	if err := seedDemo(ctx, h.c, seedOptions{AgentPassword: "demo agent password"}, &out); err != nil {
		t.Fatalf("seed: %v\n%s", err, out.String())
	}
	tickets := h.do("GET", "/api/v1/tickets?limit=100", nil, bearer(admin.Access)).expect(200).Items()
	if got := keys(tickets); len(got) != 10 || got[0] != "1042" {
		t.Fatalf("tickets: %v", got)
	}

	// The sample ticket matches the brief.
	tk := h.do("GET", "/api/v1/tickets/1042", nil, bearer(admin.Access)).expect(200).JSON()
	if tk["priority"] != float64(1) || tk["requestedPriority"] != float64(1) || tk["assignee"] != nil ||
		obj(tk, "group")["name"] != "Triage" || tk["watcherCount"] != float64(2) {
		t.Fatalf("#1042: %v", tk)
	}
	msgs := h.do("GET", "/api/v1/tickets/1042/messages", nil, bearer(admin.Access)).expect(200).Items()
	if len(msgs) != 4 || msgs[1]["visibility"] != "agent" || obj(msgs[0], "author")["name"] != "Priya Raman" ||
		len(msgs[0]["attachments"].([]any)) != 1 || obj(msgs[2], "author")["name"] != "Marco Silva" {
		t.Fatalf("#1042 messages: %v", msgs)
	}
	updated, _ := time.Parse(time.RFC3339, tk["updatedAt"].(string))
	if ago := time.Since(updated); ago < 10*time.Minute || ago > 15*time.Minute {
		t.Fatalf("#1042 updated %v ago", ago)
	}
	waiting := h.do("GET", "/api/v1/tickets/1036", nil, bearer(admin.Access)).expect(200).JSON()
	if obj(waiting, "status")["name"] != "Awaiting customer" || obj(waiting, "assignee")["name"] != "Dana Okafor" {
		t.Fatalf("#1036: %v", waiting)
	}

	// Dana can sign in and sees her queues.
	dana := h.login(danaEmail, "demo agent password")
	counts := h.do("GET", "/api/v1/queue-counts", nil, bearer(dana.Access)).expect(200).JSON()
	if counts["myWork"] != float64(2) || counts["watching"] != float64(1) || counts["awaitingCustomer"] != float64(1) || counts["onHold"] != float64(1) {
		t.Fatalf("dana counts: %v", counts)
	}
	// The group directory lists every group (Triage, Delivery, Billing) to agents.
	if gs := h.do("GET", "/api/v1/groups", nil, bearer(dana.Access)).expect(200).Items(); len(gs) != 3 {
		t.Fatalf("dana groups: %v", gs)
	}

	// No notification will ever be sent for demo data; the integration
	// used for requester messages is revoked.
	n, _ := h.coll("outbox").CountDocuments(ctx, bson.D{{Key: "state", Value: bson.D{{Key: "$ne", Value: "suppressed"}}}})
	if n != 0 {
		t.Fatalf("%d unsuppressed outbox events", n)
	}
	igs := h.do("GET", "/api/v1/integrations", nil, bearer(admin.Access)).expect(200).Items()
	if len(igs) != 1 || igs[0]["status"] != "revoked" {
		t.Fatalf("demo integration: %v", igs)
	}

	// Second run changes nothing.
	before, _ := h.coll("tickets").CountDocuments(ctx, bson.D{})
	beforeMsgs, _ := h.coll("messages").CountDocuments(ctx, bson.D{})
	out.Reset()
	if err := seedDemo(ctx, h.c, seedOptions{}, &out); err != nil {
		t.Fatalf("second seed: %v\n%s", err, out.String())
	}
	after, _ := h.coll("tickets").CountDocuments(ctx, bson.D{})
	afterMsgs, _ := h.coll("messages").CountDocuments(ctx, bson.D{})
	if before != after || beforeMsgs != afterMsgs || strings.Contains(out.String(), ": created") {
		t.Fatalf("second run created data (%d→%d tickets, %d→%d messages):\n%s", before, after, beforeMsgs, afterMsgs, out.String())
	}
	h.login(danaEmail, "demo agent password") // password kept

	// Demo data: the example SLA, tags and the canned replies.
	pol := h.do("GET", "/api/v1/sla", nil, bearer(admin.Access)).expect(200).JSON()
	if pol["active"] != true || obj(pol, "targets")["1"].(map[string]any)["firstResponseMinutes"] != float64(30) ||
		obj(pol, "targets")["4"].(map[string]any)["firstResponseMinutes"] != float64(24*60) {
		t.Fatalf("demo SLA %v", pol)
	}
	tk = h.do("GET", "/api/v1/tickets/1042", nil, bearer(admin.Access)).expect(200).JSON()
	s := slaOf(tk)
	// Marco answered in public 41 minutes ago: the first response is met (SLA
	// is first response only; Priya's later reply owes nothing).
	if s == nil || s["kind"] != "firstResponse" || s["state"] != "met" || len(s["targets"].([]any)) != 1 ||
		len(tk["tags"].([]any)) != 2 {
		t.Fatalf("#1042 extras %v", tk)
	}
	// The tags the demo tickets use are in the admin-managed catalog.
	catalog := func() []string {
		var names []string
		for _, tg := range h.do("GET", "/api/v1/tags", nil, bearer(admin.Access)).expect(200).Items() {
			names = append(names, tg["tag"].(string))
		}
		return names
	}
	wantTags := []string{"billing", "deliverability", "dns", "gmail", "outage", "quota", "spf"}
	if got := catalog(); !slices.Equal(got, wantTags) {
		t.Fatalf("tag catalog %v, want %v", got, wantTags)
	}
	mac := h.do("GET", "/api/v1/macros", nil, bearer(admin.Access)).expect(200).Items()
	if len(mac) != 2 || mac[0]["name"] != "Escalated to Delivery" || mac[1]["name"] != "Request more info" {
		t.Fatalf("canned replies %v", mac)
	}

	// An installation seeded before Milestone 6 (no policy, cycles, tags):
	// the re-run adds them and backfills a cycle for every open ticket.
	for _, col := range []string{"slaPolicies", "slaInstances", "macros", "tags"} {
		if _, err := h.coll(col).DeleteMany(ctx, bson.D{}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := h.coll("tickets").UpdateMany(ctx, bson.D{}, bson.D{{Key: "$unset", Value: bson.D{{Key: "tags", Value: ""},
		{Key: "slaDueAt", Value: ""}, {Key: "slaWarnAt", Value: ""}}}}); err != nil {
		t.Fatal(err)
	}
	out.Reset()
	if err := seedDemo(ctx, h.c, seedOptions{}, &out); err != nil {
		t.Fatalf("upgrade seed: %v\n%s", err, out.String())
	}
	if !strings.Contains(out.String(), "SLA cycles backfilled for 8 existing ticket(s)") ||
		!strings.Contains(out.String(), "#1042: demo tags added") {
		t.Fatalf("upgrade output:\n%s", out.String())
	}
	tk = h.do("GET", "/api/v1/tickets/1042", nil, bearer(admin.Access)).expect(200).JSON()
	if s := slaOf(tk); s == nil || s["kind"] != "firstResponse" || s["state"] != "met" || len(tk["tags"].([]any)) != 2 {
		t.Fatalf("backfilled #1042 %v", tk)
	}
	if got := catalog(); !slices.Equal(got, wantTags) {
		t.Fatalf("tag catalog after upgrade %v", got)
	}
	// Never answered in public: the first response is still owed.
	if tk := h.do("GET", "/api/v1/tickets/1044", nil, bearer(admin.Access)).expect(200).JSON(); slaOf(tk)["kind"] != "firstResponse" ||
		slaOf(tk)["state"] == "met" {
		t.Fatalf("unanswered ticket must owe its first response: %v", tk["sla"])
	}
	if n, _ := h.coll("slaInstances").CountDocuments(ctx, bson.D{{Key: "quiet", Value: false}}); n != 0 {
		t.Fatalf("%d demo SLA cycles would notify", n)
	}
}

func TestSeedDemoRefusesProduction(t *testing.T) {
	t.Setenv("APP_ENV", "production")
	var out, errOut bytes.Buffer
	if code := Execute(context.Background(), []string{"seed-demo"}, IO{Out: &out, Err: &errOut}); code != ExitUsage ||
		!strings.Contains(errOut.String(), "refuses to run with APP_ENV=production") {
		t.Fatalf("code %d: %s", code, errOut.String())
	}
}
