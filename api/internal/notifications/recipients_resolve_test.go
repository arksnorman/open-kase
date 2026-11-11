package notifications

import (
	"testing"

	"github.com/arksnorman/open-kase/api/internal/audit"
	"github.com/arksnorman/open-kase/api/internal/outbox"
	"github.com/arksnorman/open-kase/api/templates"
)

func TestRequesterSideResolveDoesNotEmailRequesterOrCC(t *testing.T) {
	snap := outbox.Snapshot{RequesterId: "c1", AssigneeId: "dana"}
	e := outbox.Record{Id: "e1", Type: outbox.TicketStatusChanged, Visibility: "public", Transition: outbox.TransitionResolve, Snapshot: snap,
		Actor: audit.Actor{Type: audit.ActorService, Id: "tok", Scopes: []string{"tickets:replyAsRequester"}}}
	got := targets(PlanEmail(Facts{Event: e, Watchers: []string{"lea"}, CC: []string{"cc@example.com"}}))
	if _, ok := got["contact:c1"]; ok {
		t.Error("requester emailed about their own resolve")
	}
	if _, ok := got["address:cc@example.com"]; ok {
		t.Error("CC address emailed about a requester-side resolve")
	}
	if got["user:dana"].Reason != ReasonAssignee || got["user:lea"].Reason != ReasonWatcher || len(got) != 2 {
		t.Fatalf("staff recipients: %+v", got)
	}
	// A staff token still notifies the requester and the CC addresses.
	e.Actor.Scopes = []string{"tickets:write"}
	got = targets(PlanEmail(Facts{Event: e, CC: []string{"cc@example.com"}}))
	if got["contact:c1"].Template != templates.KindStatusChanged || got["address:cc@example.com"].Reason != ReasonCC {
		t.Fatalf("staff resolve: %+v", got)
	}
}
