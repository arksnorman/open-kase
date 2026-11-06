// Package access owns authorization decisions: the authenticated principal,
// named capabilities, the two user roles (admin and agent) and integration
// scopes.
//
// Every decision is made from trusted storage at request time. Access tokens
// carry identity only, so removing a group membership, demoting an admin or
// disabling a user takes effect on the next request.
package access

import (
	"slices"

	"github.com/arksnorman/open-kase/api/internal/audit"
	"github.com/arksnorman/open-kase/api/package/fault"
)

// Kind distinguishes human users from integration service principals.
type Kind string

const (
	KindUser        Kind = "user"
	KindIntegration Kind = "integration"
)

// Principal is the authenticated caller, resolved either from a user access
// token or from an integration credential.
type Principal struct {
	Kind        Kind
	Id          string // user ID or service principal ID
	WorkspaceId string
	Name        string
	Email       string // users only

	// Users.
	SessionId string
	// Admin is set for users with the admin role; every other user is an
	// agent.
	Admin bool
	// MFAEnrolmentRequired is set for an administrator who must enrol a
	// TOTP authenticator (auth.requireAdminMfa) before using the API;
	// transports admit only the enrolment routes until then.
	MFAEnrolmentRequired bool

	// Integrations: the project they are bound to (the deployment's single
	// project) and their scopes (webhook principals hold tickets:read only).
	ProjectId string
	Scopes    []string

	// Actor, when set, replaces the audit actor. Local CLI commands that
	// drive services on behalf of a user (seed-demo) are audited as the
	// operator who ran them, not as that user.
	Actor *audit.Actor
}

// IsUser reports whether the principal is a human user.
func (p Principal) IsUser() bool { return p.Kind == KindUser }

// IsStaff reports whether the principal acts as support staff: a user, or a
// token holding a staff scope (StaffScopes: tickets:comment,
// tickets:internal, tickets:write or tickets:all). Staff see the staff
// projection of tickets (assignee, tags, SLA, version) and may set the
// priority on create; a token speaks as a bot under its own name only with
// tickets:comment or tickets:all. Other tokens act for the requester side
// and see the requester projection.
func (p Principal) IsStaff() bool {
	return p.IsUser() || (p.Kind == KindIntegration && StaffScopes(p.Scopes))
}

// AuditActor returns the audit representation of the principal.
func (p Principal) AuditActor() audit.Actor {
	if p.Actor != nil {
		return *p.Actor
	}
	if p.Kind == KindIntegration {
		return audit.Actor{Type: audit.ActorService, Id: p.Id, Label: p.Name}
	}
	return audit.Actor{Type: audit.ActorUser, Id: p.Id}
}

// RequireUser rejects integration principals on human-only routes.
func (p Principal) RequireUser() error {
	if p.Kind != KindUser {
		return fault.Forbidden("userRequired", "This operation is available to signed-in users only.")
	}
	return nil
}

// RequireStaff rejects requester-side tokens (and webhook principals) on
// staff routes that staff tokens may use.
func (p Principal) RequireStaff() error {
	if !p.IsStaff() {
		return fault.Forbidden("userRequired", "This operation is available to signed-in users and to tokens with a staff scope (tickets:comment, tickets:internal, tickets:write or tickets:all) only.")
	}
	return nil
}

// RequireAdmin allows only users with the admin role.
func (p Principal) RequireAdmin() error {
	if p.Kind != KindUser || !p.Admin {
		return fault.Forbidden("forbidden", "Administrator access is required.")
	}
	return nil
}

// Capability is a named permission checked by application services.
type Capability string

const (
	CapProjectRead        Capability = "project.read"
	CapProjectManage      Capability = "project.manage"      // settings, groups, SLA, canned replies, webhooks, reports
	CapIntegrationManage  Capability = "integration.manage"  // integration credentials
	CapTagManage          Capability = "tag.manage"          // the tag catalog (owner decision 2026-10-31)
	CapTagRead            Capability = "tag.read"            // list the tag catalog
	CapHoldReasonManage   Capability = "holdReason.manage"   // create, change and delete hold reasons
	CapHoldReasonRead     Capability = "holdReason.read"     // list hold reasons (needed to put a ticket on hold)
	CapOrganisationManage Capability = "organisation.manage" // create, change and delete organisations
	CapOrganisationRead   Capability = "organisation.read"   // list organisations
	CapGroupReadAll       Capability = "group.readAll"       // see every group, not only own
	CapMemberRead         Capability = "member.read"
	CapContactRead        Capability = "contact.read"
	CapContactWrite       Capability = "contact.write"
	CapTicketRead         Capability = "ticket.read"
	CapTicketCreate       Capability = "ticket.create"
	CapTicketReplyPublic  Capability = "ticket.replyPublic"
	// CapTicketReplyAsRequester posts a public reply as the ticket's
	// requester contact (integration tokens with tickets:replyAsRequester).
	CapTicketReplyAsRequester Capability = "ticket.replyAsRequester"
	CapTicketNoteAgent        Capability = "ticket.noteAgent"
	CapTicketClaim            Capability = "ticket.claim"
	CapTicketAssign           Capability = "ticket.assign"
	CapTicketTransition       Capability = "ticket.transition"
	CapMessageRedact          Capability = "message.redact" // admins: redact message bodies and remove attachments
	CapMessageReadPublic      Capability = "message.readPublic"
	CapMessageReadPrivate     Capability = "message.readAgent"
)

// User roles. Admins manage the deployment (users, groups, settings,
// integrations, webhooks, reports, redaction) and can do everything an agent can.
const (
	RoleAdmin = "admin"
	RoleAgent = "agent"
)

// Roles lists the user roles from most to least privileged.
func Roles() []string { return []string{RoleAdmin, RoleAgent} }

var agentCaps = []Capability{
	CapProjectRead, CapMemberRead, CapContactRead, CapContactWrite,
	CapTicketRead, CapTicketCreate, CapTicketReplyPublic, CapTicketNoteAgent, CapTicketClaim, CapTicketTransition,
	CapMessageReadPublic, CapMessageReadPrivate,
	// Owner decision 2026-10-31: agents transfer tickets to any group (with an
	// optional assignee from that group) and change the assignee within the
	// ticket's group; the service enforces group membership.
	CapTicketAssign,
	CapTagRead,
	CapHoldReasonRead,
	CapOrganisationRead,
}

var roleCaps = map[string][]Capability{
	RoleAgent: agentCaps,
	RoleAdmin: append(slices.Clone(agentCaps), CapGroupReadAll, CapProjectManage, CapIntegrationManage, CapTagManage, CapHoldReasonManage, CapOrganisationManage, CapMessageRedact),
}

// Integration scopes (owner decision 2026-10-31: one kind of integration
// token; its creator picks the scopes). No scope grants claiming or
// management (users, settings, groups, integrations, webhooks, reports), and
// no scope exceeds what a human agent may do.
const (
	ScopeTicketsRead             = "tickets:read"
	ScopeTicketsCreate           = "tickets:create"
	ScopeTicketsReplyAsRequester = "tickets:replyAsRequester"
	ScopeTicketsComment          = "tickets:comment"
	ScopeTicketsInternal         = "tickets:internal"
	ScopeTicketsWrite            = "tickets:write"
	ScopeTicketsAll              = "tickets:all"
	ScopeContactsRead            = "contacts:read"
	ScopeContactsWrite           = "contacts:write"
	// Tag catalog scopes (owner request 2026-10-31). They are not staff
	// scopes (they do not change how a token sees tickets) and tickets:all
	// does not include them.
	ScopeTagsRead  = "tags:read"
	ScopeTagsWrite = "tags:write"
	// Hold reason scopes: like the tag scopes they are not staff scopes and
	// tickets:all does not include them.
	ScopeHoldReasonsRead  = "holdReasons:read"
	ScopeHoldReasonsWrite = "holdReasons:write"
	// Organisation scopes (owner decision 2026-10-31): not staff scopes, not
	// in tickets:all.
	ScopeOrganisationsRead  = "organisations:read"
	ScopeOrganisationsWrite = "organisations:write"
)

// Scopes lists every integration scope.
func Scopes() []string {
	return []string{ScopeTicketsRead, ScopeTicketsCreate, ScopeTicketsReplyAsRequester, ScopeTicketsComment,
		ScopeTicketsInternal, ScopeTicketsWrite, ScopeTicketsAll, ScopeContactsRead, ScopeContactsWrite, ScopeTagsRead, ScopeTagsWrite,
		ScopeHoldReasonsRead, ScopeHoldReasonsWrite, ScopeOrganisationsRead, ScopeOrganisationsWrite}
}

// staffScopes make a token act as support staff (see Principal.IsStaff).
var staffScopes = []string{ScopeTicketsComment, ScopeTicketsInternal, ScopeTicketsWrite, ScopeTicketsAll}

// StaffScopes reports whether a token holding scopes acts as support staff:
// it holds tickets:comment, tickets:internal, tickets:write or tickets:all.
// Any other token acts for the requester side and sees public content only.
func StaffScopes(scopes []string) bool {
	return slices.ContainsFunc(scopes, func(s string) bool { return slices.Contains(staffScopes, s) })
}

// scopeCaps maps scopes to capabilities. tickets:all is every ticket scope
// except tickets:replyAsRequester (it never lets a token speak for the
// requester).
var scopeCaps = map[string][]Capability{
	ScopeTicketsRead:             {CapTicketRead, CapMessageReadPublic},
	ScopeTicketsCreate:           {CapTicketCreate},
	ScopeTicketsReplyAsRequester: {CapTicketReplyAsRequester},
	ScopeTicketsComment:          {CapTicketReplyPublic},
	ScopeTicketsInternal:         {CapTicketNoteAgent, CapMessageReadPrivate},
	ScopeTicketsWrite:            {CapTicketTransition, CapTicketAssign},
	ScopeContactsRead:            {CapContactRead},
	ScopeContactsWrite:           {CapContactWrite},
	ScopeTagsRead:                {CapTagRead},
	ScopeTagsWrite:               {CapTagRead, CapTagManage},
	ScopeHoldReasonsRead:         {CapHoldReasonRead},
	ScopeHoldReasonsWrite:        {CapHoldReasonRead, CapHoldReasonManage},
	ScopeOrganisationsRead:       {CapOrganisationRead},
	ScopeOrganisationsWrite:      {CapOrganisationRead, CapOrganisationManage},
}

// staffReadCaps are added to tickets:read for staff tokens: the group and
// member directory and the tag catalog (canned replies need staff and
// tickets:read as well).
var staffReadCaps = []Capability{CapMemberRead, CapGroupReadAll, CapTagRead, CapHoldReasonRead, CapOrganisationRead}

func init() {
	var all []Capability
	for _, s := range []string{ScopeTicketsRead, ScopeTicketsCreate, ScopeTicketsComment, ScopeTicketsInternal, ScopeTicketsWrite} {
		all = append(all, scopeCaps[s]...)
	}
	scopeCaps[ScopeTicketsAll] = all
}

// RoleCapabilities returns the capabilities granted by a user role.
func RoleCapabilities(role string) []Capability { return slices.Clone(roleCaps[role]) }

// ScopeCapabilities returns the capabilities granted by scopes, each mapped
// to the scope that grants it (a specific scope wins over tickets:all). Every
// integration may read the project's identity (granted by no particular
// scope).
func ScopeCapabilities(scopes []string) map[Capability]string {
	out := map[Capability]string{CapProjectRead: ""}
	add := func(c Capability, s string) {
		if prev, ok := out[c]; !ok || prev == ScopeTicketsAll {
			out[c] = s
		}
	}
	staff := StaffScopes(scopes)
	for _, s := range scopes {
		for _, c := range scopeCaps[s] {
			add(c, s)
		}
		if staff && (s == ScopeTicketsRead || s == ScopeTicketsAll) {
			for _, c := range staffReadCaps {
				add(c, s)
			}
		}
	}
	return out
}
