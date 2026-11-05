package runtime

import (
	"context"
	"slices"
	"strings"
	"testing"

	"go.mongodb.org/mongo-driver/v2/bson"
)

// Owner decision 2026-10-31: tags come from an admin-managed catalog.
// Agents and API tokens apply existing tags only; admins add a tag to the
// catalog before using it.

func TestTagCatalogAdministration(t *testing.T) {
	f := newTicketFixture(t)
	_, agentTok := f.token("Tag bot", "tickets:read", "tickets:write")

	// Only admins manage the catalog.
	f.do("POST", "/api/v1/tags", map[string]any{"name": "billing"}, bearer(f.dana.Access)).expect(403, "forbidden")
	f.do("POST", "/api/v1/tags", map[string]any{"name": "billing"}, bearer(agentTok)).expect(403)
	f.do("POST", "/api/v1/tags", map[string]any{"name": "billing"}, bearer(f.integration)).expect(403)
	created := f.do("POST", "/api/v1/tags", map[string]any{"name": "  Billing  Issue "}, bearer(f.admin.Access)).expect(201).JSON()
	if created["tag"] != "billing-issue" || created["count"] != float64(0) || created["createdAt"] == nil {
		t.Fatalf("created %v", created)
	}
	f.do("POST", "/api/v1/tags", map[string]any{"name": "billing-issue"}, bearer(f.lead.Access)).expect(409, "tagExists")
	if r := f.do("POST", "/api/v1/tags", map[string]any{"name": "#bad"}, bearer(f.admin.Access)).expect(422); r.Problem().Errors[0].Field != "name" {
		t.Fatalf("bad name error %s", r.Body)
	}
	f.do("POST", "/api/v1/tags", map[string]any{"name": "vip", "colour": "red"}, bearer(f.admin.Access)).expect(422)
	f.addTags(f.admin, "vip")

	// Every staff member lists the catalog, unused tags included; requester
	// tokens cannot.
	for _, tok := range []string{f.dana.Access, f.admin.Access, agentTok} {
		items := f.do("GET", "/api/v1/tags", nil, bearer(tok)).expect(200).Items()
		if len(items) != 2 || items[0]["tag"] != "billing-issue" || items[1]["tag"] != "vip" || items[0]["count"] != float64(0) {
			t.Fatalf("catalog %v", items)
		}
	}
	f.do("GET", "/api/v1/tags", nil, bearer(f.integration)).expect(403, "insufficientScope")

	// Deletion: admins only; it pulls the tag from every ticket in one go.
	a := f.createTicket(f.dana.Access, map[string]any{}).Ref()
	b := f.createTicket(f.marco.Access, map[string]any{"groupId": f.delivery}).Ref()
	f.do("PATCH", tpath(a), map[string]any{"tags": []string{"vip", "billing-issue"}}, bearer(f.dana.Access), ifMatchV(f.version(a))).expect(200)
	f.do("PATCH", tpath(b), map[string]any{"tags": []string{"vip"}}, bearer(f.marco.Access), ifMatchV(f.version(b))).expect(200)
	if items := f.do("GET", "/api/v1/tags", nil, bearer(f.admin.Access)).expect(200).Items(); items[1]["count"] != float64(2) {
		t.Fatalf("counts %v", items)
	}
	f.do("DELETE", "/api/v1/tags/vip", nil, bearer(f.dana.Access)).expect(403, "forbidden")
	f.do("DELETE", "/api/v1/tags/vip", nil, bearer(agentTok)).expect(403)
	va, vb := f.version(a), f.version(b)
	f.do("DELETE", "/api/v1/tags/vip", nil, bearer(f.admin.Access)).expect(204)
	if tk := f.get(f.admin.Access, a).expect(200).JSON(); !slices.Equal(anyStrings(tk["tags"]), []string{"billing-issue"}) || tk["version"] != float64(va+1) {
		t.Fatalf("ticket a after delete: tags %v version %v (was %d)", tk["tags"], tk["version"], va)
	}
	if tk := f.get(f.admin.Access, b).expect(200).JSON(); len(anyStrings(tk["tags"])) != 0 || tk["version"] != float64(vb+1) {
		t.Fatalf("ticket b after delete: %v", tk)
	}
	if items := f.do("GET", "/api/v1/tags", nil, bearer(f.admin.Access)).expect(200).Items(); len(items) != 1 || items[0]["tag"] != "billing-issue" {
		t.Fatalf("catalog after delete %v", items)
	}
	f.do("DELETE", "/api/v1/tags/vip", nil, bearer(f.admin.Access)).expect(404, "tagNotFound")
	f.do("PATCH", tpath(a), map[string]any{"tags": []string{"billing-issue", "vip"}}, bearer(f.admin.Access), ifMatchV(f.version(a))).expect(422)

	// Both catalog changes are audited as the admin.
	evs, err := f.c.Audit.RecentAudit(context.Background(), 500)
	if err != nil {
		t.Fatal(err)
	}
	var actions []string
	for _, ev := range evs {
		if strings.HasPrefix(ev.Action, "tag.") {
			if ev.ActorType != "user" || ev.Target == "" {
				t.Errorf("audit %+v", ev)
			}
			actions = append(actions, ev.Action+" "+ev.Target)
		}
	}
	for _, want := range []string{"tag.created tag/billing-issue", "tag.created tag/vip", "tag.deleted tag/vip"} {
		if !slices.Contains(actions, want) {
			t.Errorf("audit actions %v, want %s", actions, want)
		}
	}
}

func TestTicketTagsMustBeInCatalog(t *testing.T) {
	f := newMCPFixture(t)
	_, agentTok := f.token("Tag bot", "tickets:read", "tickets:write")
	f.addTags(f.admin, "vip", "dns")
	key := f.createTicket(f.dana.Access, map[string]any{}).Ref()
	patch := func(tok string, tags ...string) result {
		if tags == nil {
			tags = []string{}
		}
		return f.do("PATCH", tpath(key), map[string]any{"tags": tags}, bearer(tok), ifMatchV(f.version(key)))
	}

	// An agent applies catalog tags.
	if tk := patch(f.dana.Access, "VIP").expect(200).JSON(); !slices.Equal(anyStrings(tk["tags"]), []string{"vip"}) {
		t.Fatalf("tags %v", tk["tags"])
	}
	// Nobody adds a tag that is not in the catalog: agents, agent tokens
	// and admins alike.
	for name, tok := range map[string]string{"agent": f.dana.Access, "agent token": agentTok, "admin": f.admin.Access} {
		v := f.version(key)
		r := patch(tok, "vip", "dns", "Brand New", "brand-new").expect(422, "validationFailed")
		errs := r.Problem().Errors
		if len(errs) != 1 || errs[0].Field != "tags[2]" || errs[0].Code != "unknownTag" || !strings.Contains(errs[0].Message, "brand-new") {
			t.Fatalf("%s: errors %+v", name, errs)
		}
		if f.version(key) != v {
			t.Fatalf("%s: rejected update changed the ticket", name)
		}
	}
	// Tickets are created without tags (the create body has no tags field).
	f.do("POST", "/api/v1/tickets", map[string]any{"subject": "S", "body": "B", "tags": []string{"brand-new"},
		"requester": map[string]any{"name": "R", "email": "r@x.example"}}, bearer(f.dana.Access)).expect(422)

	// A tag the ticket already carries but the catalog lacks (written
	// before the catalog existed) can be kept or removed.
	if _, err := f.coll("tickets").UpdateOne(context.Background(), bson.D{{Key: "number", Value: 1}},
		bson.D{{Key: "$push", Value: bson.D{{Key: "tags", Value: "ghost"}}}}); err != nil {
		t.Fatal(err)
	}
	patch(f.dana.Access, "ghost", "vip", "dns").expect(200)
	if tk := patch(f.dana.Access).expect(200).JSON(); len(anyStrings(tk["tags"])) != 0 {
		t.Fatalf("tags not removed: %v", tk["tags"])
	}
	patch(f.dana.Access, "ghost").expect(422, "validationFailed")

	// MCP: the same rule through ticketsUpdate, and tagsList returns the
	// REST catalog.
	for name, tok := range map[string]string{"agent token": agentTok, "agent": f.dana.Access} {
		s := f.connect(tok)
		r := mustFail(t, s, "ticketsUpdate", map[string]any{"ticket": key, "tags": []string{"brand-new"}}, "validationFailed")
		if len(r.Problem.Errors) != 1 || r.Problem.Errors[0].Code != "unknownTag" {
			t.Fatalf("%s: MCP errors %+v", name, r.Problem.Errors)
		}
		mustCall(t, s, "ticketsUpdate", map[string]any{"ticket": key, "tags": []string{"dns"}})
		got := mustCall(t, s, "tagsList", nil)
		items, _ := got["items"].([]any)
		if len(items) != 2 || items[0].(map[string]any)["tag"] != "dns" || items[0].(map[string]any)["count"] != float64(1) {
			t.Fatalf("%s: tagsList %v", name, got)
		}
	}
	if tk := f.get(f.admin.Access, key).expect(200).JSON(); !slices.Equal(anyStrings(tk["tags"]), []string{"dns"}) {
		t.Fatalf("tags after MCP %v", tk["tags"])
	}
}

// Owner request 2026-10-31: tags:read lists the catalog and tags:write
// manages it, for any token; tickets:all does not include them, and neither
// makes a token staff.
func TestTagScopes(t *testing.T) {
	f := newMCPFixture(t)
	f.addTags(f.admin, "vip")
	a := f.createTicket(f.dana.Access, map[string]any{}).Ref()
	f.do("PATCH", tpath(a), map[string]any{"tags": []string{"vip"}}, bearer(f.dana.Access), ifMatchV(f.version(a))).expect(200)

	readerID, reader := f.token("Tag reader", "tags:read")
	_, writer := f.token("Tag writer", "tags:write", "tickets:create")
	_, all := f.token("All tickets", "tickets:all")
	for _, s := range f.do("GET", "/api/v1/integration-scopes", nil, bearer(f.admin.Access)).expect(200).Items() {
		if (s["name"] == "tags:read" || s["name"] == "tags:write") && (s["staff"] != false || s["description"] == "") {
			t.Fatalf("scope %v", s)
		}
	}
	// tags:read lists the catalog; counts are staff data (zero here).
	items := f.do("GET", "/api/v1/tags", nil, bearer(reader)).expect(200).Items()
	if len(items) != 1 || items[0]["tag"] != "vip" || items[0]["count"] != float64(0) {
		t.Fatalf("reader catalog %v", items)
	}
	f.do("POST", "/api/v1/tags", map[string]any{"name": "billing"}, bearer(reader)).expect(403, "insufficientScope")
	f.do("POST", "/api/v1/tags", map[string]any{"name": "billing"}, bearer(all)).expect(403, "insufficientScope")
	f.do("DELETE", "/api/v1/tags/vip", nil, bearer(all)).expect(403, "insufficientScope")
	// A staff token still lists through tickets:read (tickets:all), with counts.
	if items := f.do("GET", "/api/v1/tags", nil, bearer(all)).expect(200).Items(); len(items) != 1 || items[0]["count"] != float64(1) {
		t.Fatalf("staff token catalog %v", items)
	}
	// tags:write creates and deletes; the token stays requester-side.
	if r := f.do("POST", "/api/v1/tags", map[string]any{"name": "Billing"}, bearer(writer)).expect(201).JSON(); r["tag"] != "billing" {
		t.Fatalf("created %v", r)
	}
	f.do("DELETE", "/api/v1/tags/vip", nil, bearer(writer)).expect(204)
	if tk := f.get(f.admin.Access, a).expect(200).JSON(); len(anyStrings(tk["tags"])) != 0 {
		t.Fatalf("tag not pulled: %v", tk["tags"])
	}
	tk := f.createTicket(writer, map[string]any{}).JSON()
	if tk["version"] != float64(0) {
		t.Fatalf("tags:write made the token staff: %v", tk)
	}
	_ = readerID

	// MCP parity.
	s := f.connect(writer)
	if got := mustCall(t, s, "tagsCreate", map[string]any{"name": "dns"}); got["tag"] != "dns" {
		t.Fatalf("tagsCreate %v", got)
	}
	if got := mustCall(t, s, "tagsDelete", map[string]any{"name": "dns"}); got["tickets"] != float64(0) {
		t.Fatalf("tagsDelete %v", got)
	}
	mustFail(t, f.connect(reader), "tagsCreate", map[string]any{"name": "x"}, "insufficientScope")
	if got := mustCall(t, f.connect(reader), "tagsList", nil); len(got["items"].([]any)) != 1 {
		t.Fatalf("tagsList %v", got)
	}
}

func anyStrings(v any) []string {
	out := []string{}
	for _, x := range v.([]any) {
		out = append(out, x.(string))
	}
	return out
}
