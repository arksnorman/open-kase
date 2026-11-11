package notifications

import (
	"testing"

	"github.com/arksnorman/open-kase/api/internal/audit"
	"github.com/arksnorman/open-kase/api/internal/outbox"
	"github.com/arksnorman/open-kase/api/templates"
)

func TestRequesterSideResolveRecipients(t *testing.T) {
	snap := outbox.Snapshot{RequesterId: "c1", AssigneeId: "dana"}
	e := outbox.Record{Id: "e1", Type: outbox.TicketStatusChanged, Visibility: "public", Transition: outbox.TransitionResolve, Snapshot: snap,
		Actor: audit.Actor{Type: audit.ActorService, Id: "tok", Scopes: []string{"tickets:replyAsRequester"}}}
	cc := []string{"cc@example.com", "Tess@example.com"}
	// The requester resolved: the CC addresses are told, the requester is not.
	got := targets(PlanEmail(Facts{Event: e, Watchers: []string{"lea"}, CC: cc, ActorEmail: "req@example.com"}))
	if _, ok := got["contact:c1"]; ok {
		t.Error("requester emailed about their own resolve")
	}
	if got["address:cc@example.com"].Template != templates.KindStatusChanged || got["address:Tess@example.com"].Reason != ReasonCC {
		t.Errorf("CC addresses: %+v", got)
	}
	if got["user:dana"].Reason != ReasonAssignee || got["user:lea"].Reason != ReasonWatcher || len(got) != 4 {
		t.Fatalf("recipients: %+v", got)
	}
	// A teammate resolved: the requester and the CC addresses except the teammate's.
	e.Changes = []string{"contact:c2"}
	got = targets(PlanEmail(Facts{Event: e, CC: cc, ActorEmail: "tess@example.com"}))
	if got["contact:c1"].Template != templates.KindStatusChanged || got["address:cc@example.com"].Reason != ReasonCC {
		t.Fatalf("teammate resolve: %+v", got)
	}
	if _, ok := got["address:Tess@example.com"]; ok || len(got) != 3 {
		t.Fatalf("teammate emailed about their own resolve: %+v", got)
	}
	// A requester reply that reopened the ticket sends no status email.
	e.Transition, e.Changes = outbox.TransitionRequesterReply, nil
	if got = targets(PlanEmail(Facts{Event: e, CC: cc})); len(got) != 0 {
		t.Fatalf("requesterReply: %+v", got)
	}
	// A staff token notifies the whole customer side.
	e.Transition, e.Actor.Scopes = outbox.TransitionResolve, []string{"tickets:write"}
	got = targets(PlanEmail(Facts{Event: e, CC: cc}))
	if got["contact:c1"].Template != templates.KindStatusChanged || got["address:cc@example.com"].Reason != ReasonCC || len(got) != 4 {
		t.Fatalf("staff resolve: %+v", got)
	}
}
