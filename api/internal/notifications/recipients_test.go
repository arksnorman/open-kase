package notifications

import (
	"testing"

	"github.com/arksnorman/open-kase/api/internal/audit"
	"github.com/arksnorman/open-kase/api/internal/outbox"
	"github.com/arksnorman/open-kase/api/templates"
)

func ev(typ, vis string, actor string, snap outbox.Snapshot) outbox.Record {
	return outbox.Record{Id: "e1", Type: typ, Visibility: vis, Actor: audit.Actor{Type: audit.ActorUser, Id: actor}, Snapshot: snap}
}

func targets(ts []EmailTarget) map[string]EmailTarget {
	out := map[string]EmailTarget{}
	for _, t := range ts {
		out[t.Kind+":"+t.Id] = t
	}
	return out
}

func TestSLAEventsGoToAssigneeAndGroupOnly(t *testing.T) {
	for _, typ := range []string{outbox.TicketSLAWarning, outbox.TicketSLABreached} {
		e := outbox.Record{Id: "e1", Type: typ, Visibility: "agent", Actor: audit.System(audit.SystemSLA, "sla timer"),
			Snapshot: outbox.Snapshot{RequesterId: "contact1", AssigneeId: "marco", GroupId: "g"}}
		got := targets(PlanEmail(Facts{Event: e, RequesterAck: true, Watchers: []string{"lea"}, GroupMembers: []string{"dana", "marco"}}))
		if _, ok := got["contact:contact1"]; ok || len(got) != 2 {
			t.Fatalf("%s recipients %+v", typ, got)
		}
		if got["user:marco"].Reason != ReasonAssignee || got["user:dana"].Reason != ReasonGroupQueue || got["user:dana"].Template != templates.KindSLAAlert {
			t.Fatalf("%s recipients %+v", typ, got)
		}
	}
}

func TestAgentNoteNeverReachesContacts(t *testing.T) {
	e := ev(outbox.MessageCreated, "agent", "dana", outbox.Snapshot{RequesterId: "contact1", AssigneeId: "marco"})
	e.MessageAuthorKind, e.MessageAuthorId = RecipientUser, "dana"
	got := targets(PlanEmail(Facts{Event: e, RequesterAck: true, Watchers: []string{"lea", "dana"}, GroupMembers: []string{"g1"}}))
	if _, ok := got["contact:contact1"]; ok {
		t.Fatal("agent note planned for the requester contact")
	}
	for k, tg := range got {
		if tg.Template != templates.KindAgentNoteNotice {
			t.Errorf("%s got template %s, want link-only agent note notice", k, tg.Template)
		}
	}
	if got["user:marco"].Reason != ReasonAssignee || got["user:lea"].Reason != ReasonWatcher {
		t.Errorf("unexpected recipients %+v", got)
	}
	if _, ok := got["user:dana"]; ok {
		t.Error("author notified about their own note")
	}
	if _, ok := got["user:g1"]; ok {
		t.Error("group queue notified about an agent note")
	}
}

func TestPublicReplyRecipients(t *testing.T) {
	snap := outbox.Snapshot{RequesterId: "c1", AssigneeId: "marco"}
	staff := ev(outbox.MessageCreated, "public", "dana", snap)
	staff.MessageAuthorKind, staff.MessageAuthorId = RecipientUser, "dana"
	got := targets(PlanEmail(Facts{Event: staff, Watchers: []string{"marco", "lea"}}))
	if got["contact:c1"].Template != templates.KindPublicReply || got["user:marco"].Reason != ReasonAssignee || got["user:lea"].Reason != ReasonWatcher {
		t.Fatalf("staff reply: %+v", got)
	}
	if len(got) != 3 {
		t.Fatalf("deduplication failed: %+v", got)
	}

	// The requester's own reply (posted by an integration): no self notice;
	// unassigned → group queue.
	req := outbox.Record{Id: "e2", Type: outbox.MessageCreated, Visibility: "public", Actor: audit.Actor{Type: audit.ActorService, Id: "int1"},
		MessageAuthorKind: RecipientContact, MessageAuthorId: "c1", Snapshot: outbox.Snapshot{RequesterId: "c1"}}
	got = targets(PlanEmail(Facts{Event: req, GroupMembers: []string{"dana", "marco"}}))
	if _, ok := got["contact:c1"]; ok {
		t.Fatal("requester notified about their own reply")
	}
	if got["user:dana"].Reason != ReasonGroupQueue || got["user:marco"].Reason != ReasonGroupQueue {
		t.Fatalf("unassigned requester reply should reach the group queue: %+v", got)
	}
}

func TestCreatedStatusAndAssignmentRecipients(t *testing.T) {
	snap := outbox.Snapshot{RequesterId: "c1", GroupId: "g"}
	got := targets(PlanEmail(Facts{Event: ev(outbox.TicketCreated, "public", "dana", snap), RequesterAck: true, GroupMembers: []string{"dana", "marco"}}))
	if got["contact:c1"].Template != templates.KindTicketReceived || got["user:marco"].Template != templates.KindTicketCreated {
		t.Fatalf("created: %+v", got)
	}
	if _, ok := got["user:dana"]; ok {
		t.Error("creator notified about their own ticket")
	}
	got = targets(PlanEmail(Facts{Event: ev(outbox.TicketCreated, "public", "dana", snap), RequesterAck: false}))
	if _, ok := got["contact:c1"]; ok {
		t.Error("acknowledgement sent although the project disabled it")
	}

	st := ev(outbox.TicketStatusChanged, "public", "dana", outbox.Snapshot{RequesterId: "c1", AssigneeId: "dana"})
	st.Transition = outbox.TransitionResolve
	got = targets(PlanEmail(Facts{Event: st, Watchers: []string{"lea"}}))
	if got["contact:c1"].Template != templates.KindStatusChanged || got["user:lea"].Reason != ReasonWatcher {
		t.Fatalf("resolve: %+v", got)
	}
	st.Transition = outbox.TransitionSetStatus
	if n := len(PlanEmail(Facts{Event: st, Watchers: []string{"lea"}})); n != 0 {
		t.Errorf("setStatus should not email, planned %d", n)
	}
	st.Transition = outbox.TransitionReopen
	if _, ok := targets(PlanEmail(Facts{Event: st}))["contact:c1"]; ok {
		t.Error("reopen should not email the requester")
	}

	claim := ev(outbox.TicketClaimed, "agent", "marco", outbox.Snapshot{RequesterId: "c1", AssigneeId: "marco"})
	if n := len(PlanEmail(Facts{Event: claim, GroupMembers: []string{"dana"}})); n != 0 {
		t.Errorf("claiming yourself should notify nobody, planned %d", n)
	}
	assign := ev(outbox.TicketAssigned, "agent", "lea", outbox.Snapshot{RequesterId: "c1", AssigneeId: "marco"})
	got = targets(PlanEmail(Facts{Event: assign}))
	if got["user:marco"].Template != templates.KindAssignment || len(got) != 1 {
		t.Fatalf("assignment: %+v", got)
	}
	release := ev(outbox.TicketReleased, "agent", "marco", outbox.Snapshot{RequesterId: "c1"})
	got = targets(PlanEmail(Facts{Event: release, GroupMembers: []string{"marco", "dana"}}))
	if got["user:dana"].Reason != ReasonGroupQueue || len(got) != 1 {
		t.Fatalf("release: %+v", got)
	}
	for _, tg := range PlanEmail(Facts{Event: release, GroupMembers: []string{"dana"}}) {
		if tg.Kind == RecipientContact {
			t.Fatal("contacts never receive assignment notices")
		}
	}
}

func TestCCAddressesCopyRequesterEmailsOnly(t *testing.T) {
	cc := []string{"ops@acme.example", "lead@acme.example"}
	reply := ev(outbox.MessageCreated, "public", "dana", outbox.Snapshot{RequesterId: "c1", AssigneeId: "marco"})
	reply.MessageAuthorKind, reply.MessageAuthorId = RecipientUser, "dana"
	got := targets(PlanEmail(Facts{Event: reply, CC: cc, Watchers: []string{"lea"}}))
	for _, a := range cc {
		tg, ok := got[RecipientAddress+":"+a]
		if !ok || tg.Reason != ReasonCC || tg.Template != templates.KindPublicReply {
			t.Fatalf("cc %s: %+v", a, got)
		}
	}
	// No requester email, no CC: agent notes and SLA alerts.
	note := ev(outbox.MessageCreated, "agent", "dana", outbox.Snapshot{RequesterId: "c1", AssigneeId: "marco"})
	sla := ev(outbox.TicketSLABreached, "agent", "", outbox.Snapshot{RequesterId: "c1", AssigneeId: "marco"})
	for _, e := range []outbox.Record{note, sla} {
		for _, tg := range PlanEmail(Facts{Event: e, CC: cc}) {
			if tg.Kind == RecipientAddress {
				t.Fatalf("%s/%s planned cc: %+v", e.Type, e.Visibility, tg)
			}
		}
	}
}

func TestCustomerSideReplyRecipients(t *testing.T) {
	snap := outbox.Snapshot{RequesterId: "c1", AssigneeId: "marco"}
	cc := []string{"ops@acme.example", "Tess@Acme.example"}
	reply := func(author string) outbox.Record {
		return outbox.Record{Id: "e1", Type: outbox.MessageCreated, Visibility: "public", Actor: audit.Actor{Type: audit.ActorService, Id: "int1"},
			MessageAuthorKind: RecipientContact, MessageAuthorId: author, Snapshot: snap}
	}
	// The requester replied: CC addresses only (not the requester).
	got := targets(PlanEmail(Facts{Event: reply("c1"), CC: cc, ActorEmail: "req@acme.example"}))
	if _, ok := got["contact:c1"]; ok {
		t.Error("requester emailed about their own reply")
	}
	for _, a := range cc {
		if got[RecipientAddress+":"+a].Template != templates.KindPublicReply {
			t.Errorf("cc %s not told: %+v", a, got)
		}
	}
	// A CC address equal to the requester's own email is not emailed either.
	got = targets(PlanEmail(Facts{Event: reply("c1"), CC: []string{"REQ@acme.example", "ops@acme.example"}, ActorEmail: "req@acme.example"}))
	if _, ok := got["address:REQ@acme.example"]; ok || len(got) != 2 {
		t.Errorf("requester's own address in CC: %+v", got)
	}
	// A teammate replied: the requester and the CC addresses except the teammate's address.
	got = targets(PlanEmail(Facts{Event: reply("c2"), CC: cc, ActorEmail: "tess@acme.example"}))
	if got["contact:c1"].Template != templates.KindPublicReply || got["address:ops@acme.example"].Reason != ReasonCC {
		t.Fatalf("teammate reply: %+v", got)
	}
	if _, ok := got["address:Tess@Acme.example"]; ok || len(got) != 3 {
		t.Fatalf("teammate emailed about their own reply: %+v", got)
	}
	// Staff recipients are unchanged.
	if got["user:marco"].Reason != ReasonAssignee {
		t.Fatalf("assignee: %+v", got)
	}
	// An agent note never reaches the customer side, whoever wrote it.
	note := ev(outbox.MessageCreated, "agent", "dana", snap)
	for _, tg := range PlanEmail(Facts{Event: note, CC: cc}) {
		if tg.Kind != RecipientUser {
			t.Fatalf("agent note planned for %+v", tg)
		}
	}
}
