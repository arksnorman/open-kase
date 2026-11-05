package access

import (
	"context"
	"slices"
	"testing"

	"github.com/arksnorman/open-kase/api/package/fault"
)

type fakeDir struct {
	project ProjectRecord
	groups  map[string][]string // user -> group IDs
}

func (f fakeDir) TheProject(_ context.Context, ws string) (ProjectRecord, error) {
	if f.project.WorkspaceId != ws {
		return ProjectRecord{}, ErrNotFound
	}
	return f.project, nil
}

func (f fakeDir) UserGroups(_ context.Context, _, userID string) ([]string, error) {
	return f.groups[userID], nil
}

func newFake() *Service {
	return NewService(fakeDir{
		project: ProjectRecord{Id: "p1", WorkspaceId: "ws"},
		groups:  map[string][]string{"agent": {"g1"}},
	})
}

func user(id string, admin bool) Principal {
	return Principal{Kind: KindUser, Id: id, WorkspaceId: "ws", Admin: admin}
}

func code(err error) string {
	if err == nil {
		return ""
	}
	return fault.As(err).Code
}

// TestCapabilityMatrix checks each role against each capability class.
func TestCapabilityMatrix(t *testing.T) {
	s := newFake()
	ctx := context.Background()
	type row struct {
		p                                       Principal
		read, manage, integ, groupsAll, contact bool
	}
	rows := map[string]row{
		"admin": {user("root", true), true, true, true, true, true},
		"agent": {user("agent", false), true, false, false, false, true},
	}
	for name, r := range rows {
		sc, err := s.Scope(ctx, r.p)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		got := []bool{sc.Can(CapProjectRead), sc.Can(CapProjectManage), sc.Can(CapIntegrationManage), sc.Can(CapGroupReadAll), sc.Can(CapContactWrite)}
		want := []bool{r.read, r.manage, r.integ, r.groupsAll, r.contact}
		for i := range got {
			if got[i] != want[i] {
				t.Errorf("%s: capability %d = %v, want %v", name, i, got[i], want[i])
			}
		}
		if err := sc.Require(CapProjectManage); r.manage != (err == nil) || (err != nil && code(err) != "forbidden") {
			t.Errorf("%s: Require(manage) = %v", name, err)
		}
	}
}

func TestUnscopablePrincipals(t *testing.T) {
	s := newFake()
	ctx := context.Background()
	for name, p := range map[string]Principal{
		"other workspace": {Kind: KindUser, Id: "agent", WorkspaceId: "ws2"},
		"integration bound to another project": {Kind: KindIntegration, Id: "i1", WorkspaceId: "ws", ProjectId: "p2",
			Scopes: []string{ScopeTicketsRead}},
		"empty principal": {},
	} {
		if _, err := s.Scope(ctx, p); code(err) != "projectNotFound" || fault.As(err).Kind != fault.KindNotFound {
			t.Errorf("%s: %v", name, err)
		}
	}
}

func TestIntegrationScopes(t *testing.T) {
	s := newFake()
	p := Principal{Kind: KindIntegration, Id: "i1", WorkspaceId: "ws", ProjectId: "p1",
		Scopes: []string{ScopeContactsRead, ScopeTicketsCreate, ScopeTicketsReplyAsRequester, ScopeTicketsRead}}
	sc, err := s.Scope(context.Background(), p)
	if err != nil {
		t.Fatal(err)
	}
	if !sc.Can(CapContactRead) || !sc.Can(CapTicketCreate) || !sc.Can(CapProjectRead) || !sc.Can(CapTicketReplyAsRequester) {
		t.Fatal("granted scopes missing")
	}
	for _, c := range []Capability{CapContactWrite, CapProjectManage, CapIntegrationManage, CapMessageReadPrivate, CapTicketNoteAgent,
		CapGroupReadAll, CapMemberRead, CapTicketReplyPublic} {
		if sc.Can(c) {
			t.Errorf("integration has %s", c)
		}
	}
	if code(sc.Require(CapContactWrite)) != "insufficientScope" {
		t.Fatal("expected insufficientScope")
	}
	if p.RequireUser() == nil || p.RequireAdmin() == nil || p.RequireStaff() == nil || p.IsStaff() {
		t.Fatal("requester-side token passed a user/staff check")
	}
	// Private content needs tickets:internal, nothing else grants it; the
	// staff scopes are exactly comment, internal, write and all.
	for _, scope := range Scopes() {
		caps := ScopeCapabilities([]string{scope})
		_, private := caps[CapMessageReadPrivate]
		if private != (scope == ScopeTicketsInternal || scope == ScopeTicketsAll) {
			t.Errorf("scope %s: private read %v", scope, private)
		}
		staff := scope == ScopeTicketsComment || scope == ScopeTicketsInternal || scope == ScopeTicketsWrite || scope == ScopeTicketsAll
		if StaffScopes([]string{scope}) != staff {
			t.Errorf("scope %s: staff %v", scope, !staff)
		}
	}
	// tickets:all never manages the tag catalog; tags:write does.
	if _, ok := ScopeCapabilities([]string{ScopeTicketsAll})[CapTagManage]; ok {
		t.Error("tickets:all grants tag.manage")
	}
	if got := ScopeCapabilities([]string{ScopeTagsWrite})[CapTagManage]; got != ScopeTagsWrite {
		t.Errorf("tags:write maps tag.manage to %q", got)
	}
	if StaffScopes([]string{ScopeTagsRead, ScopeTagsWrite}) {
		t.Error("tag scopes make a token staff")
	}
	// tickets:all never manages hold reasons; holdReasons:write does, and neither
	// hold reason scope makes a token staff.
	if _, ok := ScopeCapabilities([]string{ScopeTicketsAll})[CapHoldReasonManage]; ok {
		t.Error("tickets:all grants holdReason.manage")
	}
	if got := ScopeCapabilities([]string{ScopeHoldReasonsWrite})[CapHoldReasonManage]; got != ScopeHoldReasonsWrite {
		t.Errorf("holdReasons:write maps holdReason.manage to %q", got)
	}
	if _, ok := ScopeCapabilities([]string{ScopeHoldReasonsRead})[CapHoldReasonManage]; ok {
		t.Error("holdReasons:read grants holdReason.manage")
	}
	if StaffScopes([]string{ScopeHoldReasonsRead, ScopeHoldReasonsWrite}) {
		t.Error("hold reason scopes make a token staff")
	}
	// tickets:all never lets a token reply as the requester.
	if _, ok := ScopeCapabilities([]string{ScopeTicketsAll})[CapTicketReplyAsRequester]; ok {
		t.Error("tickets:all grants replyAsRequester")
	}
}

// TestAgentTokenScopes: an agent token never exceeds the agent role except
// for group transfer and assignment (tickets:write) and tag catalog
// management (tags:write, owner request 2026-10-31) and hold reason management (holdReasons:write), never claims, and
// records the scope that allowed each check for the audit actor.
func TestAgentTokenScopes(t *testing.T) {
	agentRole := map[Capability]bool{CapTicketAssign: true, CapGroupReadAll: true, CapTagManage: true, CapHoldReasonManage: true, CapOrganisationManage: true}
	for _, c := range RoleCapabilities(RoleAgent) {
		agentRole[c] = true
	}
	all := ScopeCapabilities(Scopes())
	delete(all, CapTicketReplyAsRequester) // speaks for the requester, not as an agent
	for c := range all {
		if !agentRole[c] {
			t.Errorf("agent token can %s", c)
		}
	}
	for _, c := range []Capability{CapTicketClaim, CapProjectManage, CapIntegrationManage, CapMessageRedact} {
		if _, ok := all[c]; ok {
			t.Errorf("agent token has %s", c)
		}
	}
	// Organisation scopes are not staff scopes and tickets:all excludes them;
	// only organisations:write manages.
	if StaffScopes([]string{ScopeOrganisationsRead, ScopeOrganisationsWrite}) {
		t.Error("organisation scopes are staff scopes")
	}
	if _, ok := ScopeCapabilities([]string{ScopeTicketsAll})[CapOrganisationManage]; ok {
		t.Error("tickets:all grants organisation.manage")
	}
	if _, ok := ScopeCapabilities([]string{ScopeOrganisationsRead})[CapOrganisationManage]; ok {
		t.Error("organisations:read grants organisation.manage")
	}
	if got := ScopeCapabilities([]string{ScopeOrganisationsWrite})[CapOrganisationManage]; got != ScopeOrganisationsWrite {
		t.Errorf("organisations:write maps organisation.manage to %q", got)
	}
	// tickets:all equals the five tickets:* scopes.
	five := ScopeCapabilities([]string{ScopeTicketsRead, ScopeTicketsCreate, ScopeTicketsComment, ScopeTicketsInternal, ScopeTicketsWrite})
	viaAll := ScopeCapabilities([]string{ScopeTicketsAll})
	if len(five) != len(viaAll) {
		t.Fatalf("tickets:all %v != %v", viaAll, five)
	}

	p := Principal{Kind: KindIntegration, Id: "i2", Name: "Support bot", WorkspaceId: "ws", ProjectId: "p1",
		Scopes: []string{ScopeTicketsAll, ScopeTicketsInternal}}
	if !p.IsStaff() || p.RequireStaff() != nil || p.RequireUser() == nil {
		t.Fatal("agent token staff checks")
	}
	sc, err := newFake().Scope(context.Background(), p)
	if err != nil {
		t.Fatal(err)
	}
	if a := sc.AuditActor(); len(a.Scopes) != 0 {
		t.Fatalf("no check yet, scopes %v", a.Scopes)
	}
	if sc.Require(CapTicketRead) != nil {
		t.Fatal("agent token denied")
	}
	if a := sc.AuditActor(); len(a.Scopes) != 1 || a.Scopes[0] != ScopeTicketsAll {
		t.Fatalf("a read names its read scope: %v", a.Scopes)
	}
	if sc.Require(CapTicketNoteAgent) != nil || sc.Require(CapTicketAssign) != nil {
		t.Fatal("agent token denied")
	}
	if code(sc.Require(CapTicketClaim)) != "insufficientScope" {
		t.Fatal("agent token may claim")
	}
	a := sc.AuditActor()
	// The specific scope wins over tickets:all.
	if a.Type != "service" || a.Id != "i2" || a.Label != "Support bot" || len(a.Scopes) != 2 ||
		!slices.Contains(a.Scopes, ScopeTicketsInternal) || !slices.Contains(a.Scopes, ScopeTicketsAll) {
		t.Fatalf("audit actor %+v", a)
	}
}

func TestGroupMembership(t *testing.T) {
	sc, err := newFake().Scope(context.Background(), user("agent", false))
	if err != nil {
		t.Fatal(err)
	}
	if !sc.InGroup("g1") || sc.InGroup("g2") || sc.Role != RoleAgent {
		t.Fatal("group membership")
	}
}
