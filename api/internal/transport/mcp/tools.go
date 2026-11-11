package mcp

import (
	"context"
	"strconv"
	"strings"

	"github.com/google/jsonschema-go/jsonschema"
	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/arksnorman/open-kase/api/internal/access"
	"github.com/arksnorman/open-kase/api/internal/customers"
	"github.com/arksnorman/open-kase/api/internal/holdreasons"
	"github.com/arksnorman/open-kase/api/internal/macros"
	"github.com/arksnorman/open-kase/api/internal/organisations"
	"github.com/arksnorman/open-kase/api/internal/projects"
	"github.com/arksnorman/open-kase/api/internal/statuses"
	"github.com/arksnorman/open-kase/api/internal/tickets"
	transporthttp "github.com/arksnorman/open-kase/api/internal/transport/http"
	"github.com/arksnorman/open-kase/api/package/fault"
	"github.com/arksnorman/open-kase/api/package/pagination"
	"github.com/arksnorman/open-kase/api/package/validation"
)

// Tool names are camelCase (<resource><Verb>, e.g. ticketsList), valid for every MCP
// client. docs/mcp.md lists each tool with its REST route and scope.
const (
	ToolTicketsList       = "ticketsList"
	ToolTicketsSearch     = "ticketsSearch"
	ToolTicketsGet        = "ticketsGet"
	ToolTicketsCreate     = "ticketsCreate"
	ToolTicketsFollowUp   = "ticketsFollowUp"
	ToolTicketsCopy       = "ticketsCopy"
	ToolTicketsClaim      = "ticketsClaim"
	ToolTicketsReply      = "ticketsReply"
	ToolTicketsAddNote    = "ticketsAddNote"
	ToolTicketsUpdate     = "ticketsUpdate"
	ToolTicketsTransition = "ticketsTransition"
	ToolTicketsAssign     = "ticketsAssign"
	ToolGroupsList        = "groupsList"
	ToolAgentsList        = "agentsList"
	ToolStatusesList      = "statusesList"
	ToolContactsList      = "contactsList"
	ToolContactsGet       = "contactsGet"
	ToolContactsUpsert    = "contactsUpsert"
	ToolCannedRepliesList = "cannedRepliesList"
	ToolTagsList          = "tagsList"
	ToolTagsCreate        = "tagsCreate"
	ToolTagsDelete        = "tagsDelete"

	ToolOrganisationsList   = "organisationsList"
	ToolOrganisationsGet    = "organisationsGet"
	ToolOrganisationsCreate = "organisationsCreate"
	ToolOrganisationsUpdate = "organisationsUpdate"
	ToolOrganisationsDelete = "organisationsDelete"
	ToolHoldReasonsList     = "holdReasonsList"
	ToolHoldReasonsCreate   = "holdReasonsCreate"
	ToolHoldReasonsUpdate   = "holdReasonsUpdate"
	ToolHoldReasonsDelete   = "holdReasonsDelete"
)

func boolp(b bool) *bool { return &b }

// readOnly / mutating annotations. Hints only: server policy governs effects.
func readOnly(title string) *sdk.ToolAnnotations {
	return &sdk.ToolAnnotations{Title: title, ReadOnlyHint: true, OpenWorldHint: boolp(false)}
}

func mutating(title string, idempotent bool) *sdk.ToolAnnotations {
	return &sdk.ToolAnnotations{Title: title, DestructiveHint: boolp(false), IdempotentHint: idempotent, OpenWorldHint: boolp(false)}
}

func destructive(title string) *sdk.ToolAnnotations {
	return &sdk.ToolAnnotations{Title: title, DestructiveHint: boolp(true), IdempotentHint: false, OpenWorldHint: boolp(false)}
}

// Schema helpers.

func prop(s *jsonschema.Schema, name string) *jsonschema.Schema {
	p := s.Properties[name]
	if p == nil {
		panic("mcp: schema has no property " + name)
	}
	return p
}

func enum(s *jsonschema.Schema, name string, values ...string) {
	p := prop(s, name)
	p.Enum = make([]any, 0, len(values))
	for _, v := range values {
		p.Enum = append(p.Enum, v)
	}
}

func intRange(s *jsonschema.Schema, name string, lo, hi float64) {
	p := prop(s, name)
	p.Minimum, p.Maximum = &lo, &hi
}

// Shared argument blocks.

type pageArgs struct {
	Cursor string `json:"cursor,omitempty" jsonschema:"Opaque nextCursor from a previous page."`
	Limit  int    `json:"limit,omitempty" jsonschema:"Page size, 1-100 (default 25)."`
}

func (a pageArgs) parse() (*pagination.Cursor, int, error) {
	raw := ""
	if a.Limit != 0 {
		raw = strconv.Itoa(a.Limit)
	}
	limit, err := pagination.ParseLimit(raw)
	if err != nil {
		return nil, 0, err
	}
	cur, err := pagination.Decode(a.Cursor)
	return cur, limit, err
}

func pageOf[T any](p pagination.Page[T], view func(T) any) map[string]any {
	items := make([]any, 0, len(p.Items))
	for _, it := range p.Items {
		items = append(items, view(it))
	}
	return map[string]any{"items": items, "nextCursor": p.NextCursor}
}

type ticketRef struct {
	Ticket string `json:"ticket" jsonschema:"Ticket number (e.g. 1042 or #1042) or ID."`
}

type mutationArgs struct {
	ExpectedVersion int64  `json:"expectedVersion,omitempty" jsonschema:"Ticket version you based this change on; a newer version fails with versionConflict. When omitted the current version is read first."`
	IdempotencyKey  string `json:"idempotencyKey,omitempty" jsonschema:"Optional key (1-200 printable ASCII); a retry with the same key and arguments returns the first result instead of repeating the change."`
}

// ---- Inputs ----

type ticketsListIn struct {
	View           string `json:"view,omitempty" jsonschema:"Queue/view. Users: myWork, unassigned, watching, awaitingCustomer, onHold, dueSoon, resolved, archived, all. API tokens: awaitingCustomer, onHold, resolved, archived, all."`
	Q              string `json:"q,omitempty" jsonschema:"Search text: a ticket number (1042 or #1042) or words from the subject."`
	GroupId        string `json:"groupId,omitempty" jsonschema:"Only tickets currently in this group."`
	StatusId       string `json:"statusId,omitempty" jsonschema:"Comma-separated status IDs (also for requester-side tokens)."`
	OrganisationId string `json:"organisationId,omitempty" jsonschema:"Comma-separated organisation IDs (also for requester-side tokens)."`
	RequesterId    string `json:"requesterId,omitempty" jsonschema:"A contact ID: only that contact's tickets (also for requester-side tokens)."`
	Assignee       string `json:"assignee,omitempty" jsonschema:"Users and staff tokens: me, unassigned or a user ID. Requester-side tokens: 403 userRequired."`
	Priority       string `json:"priority,omitempty" jsonschema:"Users and staff tokens: comma-separated priorities 1-4."`
	Tag            string `json:"tag,omitempty" jsonschema:"Users and staff tokens: comma-separated catalog tags."`
	RequestTypeId  string `json:"requestTypeId,omitempty" jsonschema:"Users and staff tokens: comma-separated request type IDs."`
	Due            string `json:"due,omitempty" jsonschema:"Users and staff tokens: SLA filter: dueSoon, breached, met, missed or running."`
	pageArgs
}

type ticketsSearchIn struct {
	Query string `json:"query" jsonschema:"Words to find in ticket subjects and in the messages the caller may read (max 100 characters). Quote phrases; exclude words with -word. A ticket number (1042 or #1042) is also matched exactly."`
	pageArgs
}

type ticketsGetIn struct {
	ticketRef
	IncludeMessages *bool  `json:"includeMessages,omitempty" jsonschema:"Include the conversation the caller may read (default true)."`
	MessagesCursor  string `json:"messagesCursor,omitempty" jsonschema:"nextCursor of the previous messages page."`
	MessagesLimit   int    `json:"messagesLimit,omitempty" jsonschema:"Messages per page, 1-100 (default 25)."`
}

type requesterIn struct {
	ContactId      string `json:"contactId,omitempty" jsonschema:"Existing contact ID. Otherwise give name and email."`
	Name           string `json:"name,omitempty" jsonschema:"Requester name."`
	Email          string `json:"email,omitempty" jsonschema:"Requester email."`
	ExternalId     string `json:"externalId,omitempty" jsonschema:"Requester ID in the calling system."`
	OrganisationId string `json:"organisationId,omitempty" jsonschema:"Optional with name and email: link a new or updated requester contact to this organisation (organisationsList). Explicit only."`
}

type ticketsCreateIn struct {
	Subject        string      `json:"subject" jsonschema:"Ticket subject."`
	Body           string      `json:"body" jsonschema:"First public message (Markdown; visible to the requester)."`
	Requester      requesterIn `json:"requester" jsonschema:"The requester contact."`
	Priority       int         `json:"priority,omitempty" jsonschema:"1 (critical) to 4 (low). From a requester-side token it is stored as the requested priority."`
	RequestTypeId  string      `json:"requestTypeId,omitempty" jsonschema:"Request type ID (default: issue)."`
	GroupId        string      `json:"groupId,omitempty" jsonschema:"Initial group ID (default: the project's triage group)."`
	OrganisationId string      `json:"organisationId,omitempty" jsonschema:"Optional: the organisation this ticket belongs to (organisationsCreate / organisationsList). Set explicitly; it is not taken from the requester."`
	CCEmails       []string    `json:"ccEmails,omitempty" jsonschema:"Optional: up to 10 email addresses copied on every email to the requester (not the requester's own address)."`
	IdempotencyKey string      `json:"idempotencyKey,omitempty" jsonschema:"Optional key (1-200 printable ASCII); a retry with the same key and arguments returns the first ticket."`
}

type ticketsFollowUpIn struct {
	ticketRef
	Subject        string   `json:"subject,omitempty" jsonschema:"Defaults to the original ticket's subject."`
	Body           string   `json:"body,omitempty" jsonschema:"First public message (Markdown); defaults to \"Follow-up to #<number>.\""`
	Priority       int      `json:"priority,omitempty" jsonschema:"1 (critical) to 4 (low)."`
	AddSignature   *bool    `json:"addSignature,omitempty" jsonschema:"Signed-in users only (default true): false skips the signatures on the first message."`
	GroupId        string   `json:"groupId,omitempty" jsonschema:"Defaults to the original's group."`
	RequestTypeId  string   `json:"requestTypeId,omitempty" jsonschema:"Defaults to the original's request type."`
	Tags           []string `json:"tags,omitempty" jsonschema:"Catalog tags for the follow-up (none by default)."`
	ContactId      string   `json:"contactId,omitempty" jsonschema:"Only when the first message is written as the requester (a token with neither tickets:comment nor tickets:all): open the follow-up as this contact of the original's organisation (422 contactNotInOrganisation otherwise): they become the requester of the new ticket and author its first message. Otherwise 422 notAllowed."`
	IdempotencyKey string   `json:"idempotencyKey,omitempty" jsonschema:"Optional key (1-200 printable ASCII); a retry with the same key and arguments returns the first ticket."`
}

type ticketsCopyIn struct {
	ticketRef
	Subject            string    `json:"subject,omitempty" jsonschema:"Defaults to the original's subject."`
	GroupId            string    `json:"groupId,omitempty" jsonschema:"Defaults to the original's group."`
	Priority           int       `json:"priority,omitempty" jsonschema:"1 (critical) to 4 (low); defaults to the original's."`
	RequestTypeId      string    `json:"requestTypeId,omitempty" jsonschema:"Defaults to the original's request type."`
	Tags               *[]string `json:"tags,omitempty" jsonschema:"Catalog tags; defaults to the original's tags that still exist."`
	Body               string    `json:"body,omitempty" jsonschema:"First public message (Markdown); defaults to the original's first public message."`
	IncludeAttachments *bool     `json:"includeAttachments,omitempty" jsonschema:"Copy the first message's attachments (default true)."`
	IdempotencyKey     string    `json:"idempotencyKey,omitempty" jsonschema:"Optional key (1-200 printable ASCII); a retry with the same key and arguments returns the first ticket."`
}

type ticketsClaimIn struct {
	ticketRef
	mutationArgs
}

type ticketsReplyIn struct {
	ticketRef
	Audience             string `json:"audience" jsonschema:"Must be \"public\": the reply is sent to the requester."`
	Body                 string `json:"body" jsonschema:"Reply text (Markdown)."`
	ExpectedLastPublicId string `json:"expectedLastPublicId,omitempty" jsonschema:"Newest public message ID you have read; fails with newerReplyExists when another public message arrived since."`
	ThenStatusId         string `json:"thenStatusId,omitempty" jsonschema:"Optional status to set in the same transaction (Resolved, or an open or awaiting-customer status ID from statusesList). Needs tickets:write (staff tokens) or a user; an invalid transition fails the call and nothing is posted."`
	AddSignature         *bool  `json:"addSignature,omitempty" jsonschema:"Signed-in users only (default true): false skips the user's signature and the workspace signature. Tokens never add one."`
	As                   string `json:"as,omitempty" jsonschema:"Tokens only: who authors the reply. staff: the token as a bot under its name (tickets:comment). requester: the ticket's requester (tickets:replyAsRequester; no thenStatusId). Omitted: staff when the token holds tickets:comment, otherwise requester."`
	ContactId            string `json:"contactId,omitempty" jsonschema:"Only with as requester: author the reply as this contact of the ticket's organisation instead of the requester (422 contactNotInOrganisation when the contact is not in the ticket's organisation)."`
	mutationArgs
}

type ticketsAddNoteIn struct {
	ticketRef
	Audience     string `json:"audience" jsonschema:"Must be \"agent\": the note is visible to support staff only, never to the requester or requester-side tokens."`
	Body         string `json:"body" jsonschema:"Note text (Markdown)."`
	ThenStatusId string `json:"thenStatusId,omitempty" jsonschema:"Optional status to set in the same transaction (as for ticketsReply)."`
	mutationArgs
}

type ticketsUpdateIn struct {
	ticketRef
	Priority       int       `json:"priority,omitempty" jsonschema:"1 (critical), 2 (high), 3 (normal) or 4 (low)."`
	Tags           *[]string `json:"tags,omitempty" jsonschema:"Replaces the ticket's tag set (an empty list removes every tag). Added tags must exist in the catalog (tagsList); unknown ones are rejected with unknownTag."`
	RequestTypeId  string    `json:"requestTypeId,omitempty" jsonschema:"Request type ID."`
	Subject        string    `json:"subject,omitempty" jsonschema:"New subject: only a requester-side token, on a ticket it created; staff receive subjectLocked."`
	OrganisationId *string   `json:"organisationId,omitempty" jsonschema:"Staff only: the ticket's organisation ID (an empty string clears it). Allowed for tokens on any ticket; signed-in users cannot change it on API-created tickets."`
	CCEmails       *[]string `json:"ccEmails,omitempty" jsonschema:"Staff only: replaces the CC addresses copied on requester emails (up to 10; an empty list clears them)."`
	mutationArgs
}

type ticketsAssignIn struct {
	ticketRef
	GroupId    string `json:"groupId" jsonschema:"Target group ID (groupsList); the current group to change only the assignee."`
	AssigneeId string `json:"assigneeId,omitempty" jsonschema:"User ID to assign: an active member of the target group (agentsList). Omitted: an assignee who is a member of the target group is kept, otherwise cleared."`
	Unassign   bool   `json:"unassign,omitempty" jsonschema:"Clear the assignee; the ticket returns to the group queue."`
	mutationArgs
}

type noArgs struct{}

type contactsListIn struct {
	Q          string `json:"q,omitempty" jsonschema:"Name or email prefix."`
	ExternalId string `json:"externalId,omitempty" jsonschema:"Exact match on the contact's external ID (at most one contact)."`
	pageArgs
}

type contactsGetIn struct {
	ContactId string `json:"contactId" jsonschema:"Contact ID."`
}

type tagNameIn struct {
	Name string `json:"name" jsonschema:"Tag name: 1-40 lower-case letters, digits, '-' or '_' (input is trimmed, lower-cased and spaces become '-')."`
}

type holdReasonCreateIn struct {
	Name    string `json:"name" jsonschema:"Reason name: 1-80 characters, unique in the project."`
	MaxDays int    `json:"maxDays" jsonschema:"Days a ticket stays on hold for this reason (1-365)."`
}

type holdReasonUpdateIn struct {
	HoldReasonId    string `json:"holdReasonId" jsonschema:"Hold reason ID (holdReasonsList)."`
	ExpectedVersion int64  `json:"expectedVersion,omitempty" jsonschema:"Version you based this change on (from holdReasonsList); a newer version fails with versionConflict. When omitted the current version is read first."`
	Name            string `json:"name,omitempty" jsonschema:"New name."`
	MaxDays         int    `json:"maxDays,omitempty" jsonschema:"New number of days (1-365); affects only holds started afterwards."`
}

type holdReasonDeleteIn struct {
	HoldReasonId    string `json:"holdReasonId" jsonschema:"Hold reason ID (holdReasonsList)."`
	ExpectedVersion int64  `json:"expectedVersion,omitempty" jsonschema:"Version you based this change on; when omitted the current version is read first."`
}

type contactsUpsertIn struct {
	Name           string  `json:"name" jsonschema:"Contact name."`
	Email          string  `json:"email" jsonschema:"Email address; an existing contact with this email is updated."`
	ExternalId     string  `json:"externalId,omitempty" jsonschema:"The contact's ID in the calling system."`
	OrganisationId *string `json:"organisationId,omitempty" jsonschema:"Link the contact to an organisation (organisationsList). Omitted: unchanged; an empty string clears the link."`
}

type organisationsListIn struct {
	Q           string `json:"q,omitempty" jsonschema:"Search the organisation name or external reference (substring, case-insensitive)."`
	Active      string `json:"active,omitempty" jsonschema:"true (default), false or all."`
	ExternalRef string `json:"externalRef,omitempty" jsonschema:"Exact match on the external reference (at most one organisation; deactivated ones need active all)."`
	pageArgs
}

type organisationsGetIn struct {
	OrganisationId string `json:"organisationId" jsonschema:"Organisation ID."`
}

type organisationsCreateIn struct {
	Name        string `json:"name" jsonschema:"Organisation name: 1-200 characters, unique in the project (case-insensitive)."`
	ExternalRef string `json:"externalRef,omitempty" jsonschema:"Optional: your own ID for the organisation (up to 128 characters, unique in the project)."`
}

type organisationsUpdateIn struct {
	OrganisationId  string  `json:"organisationId" jsonschema:"Organisation ID."`
	ExpectedVersion int64   `json:"expectedVersion,omitempty" jsonschema:"Version you based this change on; a newer version fails with versionConflict. When omitted the current version is read first."`
	Name            string  `json:"name,omitempty" jsonschema:"New name."`
	ExternalRef     *string `json:"externalRef,omitempty" jsonschema:"New external reference (an empty string clears it)."`
	Active          *bool   `json:"active,omitempty" jsonschema:"false deactivates the organisation (hidden from lists by default, cannot be linked to new tickets or contacts); true reactivates it."`
}

type organisationsDeleteIn struct {
	OrganisationId  string `json:"organisationId" jsonschema:"Organisation ID."`
	ExpectedVersion int64  `json:"expectedVersion,omitempty" jsonschema:"Version you based this change on; when omitted the current version is read first."`
}

type ticketsTransitionIn struct {
	ticketRef
	Action       string `json:"action" jsonschema:"resolve, reopen (resolved tickets), archive (resolved tickets; final), hold (Open or Awaiting customer tickets; needs holdReasonId) or setStatus."`
	StatusId     string `json:"statusId,omitempty" jsonschema:"Target status ID; only with setStatus (an open or awaiting-customer status)."`
	Reason       string `json:"reason,omitempty" jsonschema:"Optional reason recorded in the audit history."`
	HoldReasonId string `json:"holdReasonId,omitempty" jsonschema:"Required with hold: a hold reason ID from holdReasonsList. The ticket returns to Open after the reason's maxDays."`
	Note         string `json:"note,omitempty" jsonschema:"Only with hold: an optional internal note saved with the hold (needs tickets:internal for tokens)."`
	ContactId    string `json:"contactId,omitempty" jsonschema:"Only with resolve by a requester-side token: the contact of the ticket's organisation who resolved it (422 contactNotInOrganisation otherwise). Any other caller: 422 notAllowed."`
	mutationArgs
}

func optional(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

// registerTools adds the tool set: every capability of an API token (and
// the claim action of users). There are deliberately no delete,
// restore, redaction or settings tools.
func (h *Handler) registerTools() {
	addTool(h, &sdk.Tool{Name: ToolTicketsList, Description: "List tickets in a queue/view, newest activity first, with cursor pagination. Only tickets the caller may see are returned.",
		Annotations: readOnly("List tickets")}, func(s *jsonschema.Schema) {
		enum(s, "view", validation.TicketView.Choices...)
		intRange(s, "limit", 1, 100)
	}, h.ticketsList)

	addTool(h, &sdk.Tool{Name: ToolTicketsSearch, Description: "Full-text search over the caller's visible tickets: subjects and the messages the caller may read (agent notes only for users and staff tokens with tickets:internal). Returns hits with plain-text snippets, the authorized total and, for a ticket number, the exact match.",
		Annotations: readOnly("Search tickets")}, func(s *jsonschema.Schema) { intRange(s, "limit", 1, 100) }, h.ticketsSearch)

	addTool(h, &sdk.Tool{Name: ToolTicketsGet, Description: "Get one ticket and the messages the caller may read: public messages with tickets:read, agent notes as well for users and staff tokens with tickets:internal.",
		Annotations: readOnly("Get ticket")}, func(s *jsonschema.Schema) { intRange(s, "messagesLimit", 1, 100) }, h.ticketsGet)

	addTool(h, &sdk.Tool{Name: ToolTicketsCreate, Description: "Create a ticket with its first public message (tickets:create). It enters a group queue unassigned. organisationId optionally links the ticket to an organisation (never taken from the requester); ccEmails optionally copies addresses on every requester email. A token holding tickets:comment (or tickets:all) writes the message as the bot; any other token writes it as the requester (staff tokens still set the priority).",
		Annotations: mutating("Create ticket", false)}, func(s *jsonschema.Schema) { intRange(s, "priority", 1, 4) }, h.ticketsCreate)

	addTool(h, &sdk.Tool{Name: ToolTicketsFollowUp, Description: "Create a new ticket linked as a follow-up to an existing one, e.g. an archived ticket that cannot change any more (tickets:create and tickets:read). Copies the subject, requester, organisation, CC addresses, request type and group only. With contactId (requester-side tokens) that teammate is the requester of the new ticket and their own address is dropped from the copied CC list.",
		Annotations: mutating("Create follow-up ticket", false)}, func(s *jsonschema.Schema) { intRange(s, "priority", 1, 4) }, h.ticketsFollowUp)

	addTool(h, &sdk.Tool{Name: ToolTicketsCopy, Description: "Copy a ticket, also an archived one (tickets:create and tickets:read; agent tokens and users). The new Open ticket is linked copiedFrom the original and copies the requester, CC addresses, organisation, priority, request type, group, tags, first public message and its attachments (unless includeAttachments is false); arguments override the copied values. Assignee, watchers, notes, later messages and history are not copied.",
		Annotations: mutating("Copy ticket", false)}, func(s *jsonschema.Schema) { intRange(s, "priority", 1, 4) }, h.ticketsCopy)

	addTool(h, &sdk.Tool{Name: ToolTicketsClaim, Description: "Claim an unassigned ticket in one of your groups (users only). Exactly one concurrent claim wins; the group is kept.",
		Annotations: mutating("Claim ticket", false)}, nil, h.ticketsClaim)

	addTool(h, &sdk.Tool{Name: ToolTicketsReply, Description: "Post a PUBLIC reply that the requester will see. Requires audience \"public\". as \"staff\" (tickets:comment) replies as the bot under the token's name (a staff reply); as \"requester\" (tickets:replyAsRequester) replies as the ticket's requester. Omitted: staff when the token holds tickets:comment, otherwise requester. For staff-only notes use ticketsAddNote.",
		Annotations: mutating("Reply publicly", false)}, func(s *jsonschema.Schema) {
		enum(s, "audience", tickets.VisibilityPublic)
		enum(s, "as", validation.MessageAs.Choices...)
	}, h.ticketsReply)

	addTool(h, &sdk.Tool{Name: ToolTicketsAddNote, Description: "Add a staff-only agent note (users, and staff tokens with tickets:internal). Requires audience \"agent\"; never visible to the requester or requester-side tokens.",
		Annotations: mutating("Add agent note", false)}, func(s *jsonschema.Schema) { enum(s, "audience", tickets.VisibilityAgent) }, h.ticketsAddNote)

	addTool(h, &sdk.Tool{Name: ToolTicketsUpdate, Description: "Change priority, tags, request type, the requester organisation or the CC addresses (users, and staff tokens with tickets:write). Only given fields change.",
		Annotations: mutating("Update ticket", true)}, func(s *jsonschema.Schema) { intRange(s, "priority", 1, 4) }, h.ticketsUpdate)

	addTool(h, &sdk.Tool{Name: ToolTicketsTransition, Description: "Resolve, reopen, archive (final), put on hold (needs a holdReasonsList ID; the ticket returns to Open after the reason's maxDays) or set an open or awaiting-customer status (users, and staff tokens with tickets:write). A requester-side token with tickets:replyAsRequester may resolve only (no expectedVersion needed); everything else is staff only.",
		Annotations: mutating("Change status", false)}, func(s *jsonschema.Schema) {
		enum(s, "action", validation.TicketTransitionAction.Choices...)
	}, h.ticketsTransition)

	addTool(h, &sdk.Tool{Name: ToolTicketsAssign, Description: "Move the ticket to a group and/or assign it to an agent who is an active member of that group (admins, and staff tokens with tickets:write). A staff token is never an assignee.",
		Annotations: mutating("Assign ticket", true)}, nil, h.ticketsAssign)

	addTool(h, &sdk.Tool{Name: ToolGroupsList, Description: "List the groups (team queues) the caller can see (users; staff tokens with tickets:read see all groups).",
		Annotations: readOnly("List groups")}, func(s *jsonschema.Schema) { intRange(s, "limit", 1, 100) }, h.groupsList)

	addTool(h, &sdk.Tool{Name: ToolAgentsList, Description: "List the support staff directory with each person's role, groups and active state, for choosing an assignee (users; staff tokens with tickets:read).",
		Annotations: readOnly("List agents")}, func(s *jsonschema.Schema) { intRange(s, "limit", 1, 100) }, h.agentsList)

	addTool(h, &sdk.Tool{Name: ToolStatusesList, Description: "List the ticket statuses (IDs for ticketsTransition setStatus and thenStatusId).",
		Annotations: readOnly("List statuses")}, nil, h.statusesList)

	addTool(h, &sdk.Tool{Name: ToolContactsList, Description: "List contacts (requesters), optionally by name or email prefix (contacts:read).",
		Annotations: readOnly("List contacts")}, func(s *jsonschema.Schema) { intRange(s, "limit", 1, 100) }, h.contactsList)

	addTool(h, &sdk.Tool{Name: ToolContactsGet, Description: "Get one contact (contacts:read).",
		Annotations: readOnly("Get contact")}, nil, h.contactsGet)

	addTool(h, &sdk.Tool{Name: ToolContactsUpsert, Description: "Create a contact, or update the contact with this email (contacts:write). Returns the contact and whether it was created.",
		Annotations: mutating("Create or update contact", true)}, nil, h.contactsUpsert)

	addTool(h, &sdk.Tool{Name: ToolCannedRepliesList, Description: "List the canned replies (name, audience, Markdown body) staff use to answer common questions (users; staff tokens with tickets:read).",
		Annotations: readOnly("List canned replies")}, nil, h.cannedRepliesList)

	addTool(h, &sdk.Tool{Name: ToolTagsList, Description: "List the tag catalog: the only tags that can be added to a ticket, each with the number of tickets you can read as staff that carry it (users; tokens with tags:read or tags:write; staff tokens with tickets:read).",
		Annotations: readOnly("List tags")}, nil, h.tagsList)
	addTool(h, &sdk.Tool{Name: ToolTagsCreate, Description: "Add a tag to the catalog (admins; tokens with tags:write). Fails with tagExists when it already exists.",
		Annotations: mutating("Create tag", false)}, nil, h.tagsCreate)
	addTool(h, &sdk.Tool{Name: ToolTagsDelete, Description: "Delete a catalog tag and remove it from every ticket that carries it (admins; tokens with tags:write). Returns how many tickets carried it.",
		Annotations: destructive("Delete tag")}, nil, h.tagsDelete)

	addTool(h, &sdk.Tool{Name: ToolOrganisationsList, Description: "List the customer organisations of the project by name, optionally searching name or external reference (users; tokens with organisations:read or organisations:write). Contact and ticket counts are reported to callers who manage organisations.",
		Annotations: readOnly("List organisations")}, func(s *jsonschema.Schema) { intRange(s, "limit", 1, 100) }, h.organisationsList)
	addTool(h, &sdk.Tool{Name: ToolOrganisationsGet, Description: "Get one organisation (organisations:read).",
		Annotations: readOnly("Get organisation")}, nil, h.organisationsGet)
	addTool(h, &sdk.Tool{Name: ToolOrganisationsCreate, Description: "Create an organisation and get its ID, then link the ID to contacts (contactsUpsert organisationId) and tickets (ticketsCreate organisationId). Admins; tokens with organisations:write. Fails with organisationExists or organisationRefTaken.",
		Annotations: mutating("Create organisation", false)}, nil, h.organisationsCreate)
	addTool(h, &sdk.Tool{Name: ToolOrganisationsUpdate, Description: "Rename an organisation or change its external reference (admins; tokens with organisations:write).",
		Annotations: mutating("Update organisation", false)}, nil, h.organisationsUpdate)
	addTool(h, &sdk.Tool{Name: ToolOrganisationsDelete, Description: "Delete an organisation (admins; tokens with organisations:write). Refused with organisationInUse while any contact or ticket references it: deactivate it instead (organisationsUpdate active false).",
		Annotations: destructive("Delete organisation")}, nil, h.organisationsDelete)

	addTool(h, &sdk.Tool{Name: ToolHoldReasonsList, Description: "List the hold reasons (ID, name, maxDays) a ticket can be put on hold for with ticketsTransition hold (users; tokens with holdReasons:read or holdReasons:write; staff tokens with tickets:read). inUse is reported to callers who manage hold reasons.",
		Annotations: readOnly("List hold reasons")}, nil, h.holdReasonsList)
	addTool(h, &sdk.Tool{Name: ToolHoldReasonsCreate, Description: "Create a hold reason (admins; tokens with holdReasons:write). Fails with holdReasonExists when the name is taken.",
		Annotations: mutating("Create hold reason", false)}, func(s *jsonschema.Schema) { intRange(s, "maxDays", 1, 365) }, h.holdReasonsCreate)
	addTool(h, &sdk.Tool{Name: ToolHoldReasonsUpdate, Description: "Rename a hold reason or change its maxDays (admins; tokens with holdReasons:write). A new maxDays affects only holds started afterwards.",
		Annotations: mutating("Update hold reason", false)}, func(s *jsonschema.Schema) { intRange(s, "maxDays", 1, 365) }, h.holdReasonsUpdate)
	addTool(h, &sdk.Tool{Name: ToolHoldReasonsDelete, Description: "Delete a hold reason (admins; tokens with holdReasons:write). Refused with holdReasonInUse while a ticket is on hold with it.",
		Annotations: destructive("Delete hold reason")}, nil, h.holdReasonsDelete)
}

// ---- Read tools ----

func (h *Handler) list(ctx context.Context, c *call, in tickets.ListInput, pg pageArgs) (any, error) {
	s, err := h.scope(ctx, c)
	if err != nil {
		return nil, err
	}
	if in.After, in.Limit, err = pg.parse(); err != nil {
		return nil, err
	}
	p, err := h.d.Tickets.List(ctx, s, in)
	if err != nil {
		return nil, err
	}
	return pageOf(p, func(t tickets.Ticket) any { return transporthttp.TicketView(t) }), nil
}

func (h *Handler) ticketsList(ctx context.Context, c *call, in ticketsListIn) (any, error) {
	params := map[string]string{"statusId": in.StatusId, "organisationId": in.OrganisationId, "requesterId": in.RequesterId,
		"assignee": in.Assignee, "priority": in.Priority, "tag": in.Tag, "requestTypeId": in.RequestTypeId, "due": in.Due}
	li := tickets.ListInput{View: in.View, Q: in.Q, GroupId: in.GroupId, Filter: transporthttp.ListFilter(func(name string) string { return params[name] })}
	return h.list(ctx, c, li, in.pageArgs)
}

// ticketsSearch uses the audience-scoped full-text engine (the REST
// /search route): matches, counts and snippets only ever come from content
// the caller may read. A query that is a ticket number ("1042", "#1042")
// also returns that ticket as exactMatch when the caller can read it.
func (h *Handler) ticketsSearch(ctx context.Context, c *call, in ticketsSearchIn) (any, error) {
	if strings.TrimSpace(in.Query) == "" {
		return nil, fault.Field("query", validation.CodeRequired, "Enter words to search for or a ticket number.")
	}
	s, err := h.scope(ctx, c)
	if err != nil {
		return nil, err
	}
	after, limit, err := in.pageArgs.parse()
	if err != nil {
		return nil, err
	}
	var exact any
	if _, n, ok := tickets.ParseRef(in.Query); after == nil && ok && n > 0 {
		if t, err := h.d.Tickets.Get(ctx, s, in.Query); err == nil {
			exact = transporthttp.TicketView(t)
		}
	}
	res, err := h.d.Tickets.Search(ctx, s, tickets.SearchInput{Q: in.Query, After: after, Limit: limit})
	if err != nil {
		if exact != nil && fault.As(err).Kind == fault.KindValidation {
			// A bare reference has no searchable words: return the exact match.
			return map[string]any{"items": []any{}, "nextCursor": nil, "total": 0, "totalCapped": false, "exactMatch": exact}, nil
		}
		return nil, err
	}
	out := transporthttp.SearchView(res)
	out["exactMatch"] = exact
	return out, nil
}

func (h *Handler) ticketsGet(ctx context.Context, c *call, in ticketsGetIn) (any, error) {
	s, err := h.scope(ctx, c)
	if err != nil {
		return nil, err
	}
	t, err := h.d.Tickets.Get(ctx, s, in.Ticket)
	if err != nil {
		return nil, err
	}
	c.ticketID = t.Id
	out := map[string]any{"ticket": transporthttp.TicketView(t)}
	// Messages need a message-read capability (tokens: tickets:read); the
	// service filters audiences (agent notes only with tickets:internal).
	if (in.IncludeMessages == nil || *in.IncludeMessages) && s.Can(access.CapMessageReadPublic) {
		after, limit, err := pageArgs{Cursor: in.MessagesCursor, Limit: in.MessagesLimit}.parse()
		if err != nil {
			return nil, err
		}
		msgs, err := h.d.Tickets.ListMessages(ctx, s, t.Id, "", after, limit)
		if err != nil {
			return nil, err
		}
		out["messages"] = pageOf(msgs, func(m tickets.Message) any { return transporthttp.MessageView(m) })
	}
	return out, nil
}

// ---- Mutations ----

// ticketScope resolves the caller's scope and an accessible ticket.
func (h *Handler) ticketScope(ctx context.Context, c *call, ref string) (*access.ProjectScope, tickets.Ticket, error) {
	s, err := h.scope(ctx, c)
	if err != nil {
		return nil, tickets.Ticket{}, err
	}
	t, err := h.d.Tickets.Get(ctx, s, ref)
	if err != nil {
		return nil, tickets.Ticket{}, err
	}
	c.ticketID = t.Id
	return s, t, nil
}

// expected returns the caller's expected version, or the version just read.
func expected(t tickets.Ticket, v int64) int64 {
	if v > 0 {
		return v
	}
	return t.Version
}

func (h *Handler) recheckTicket(c *call, ref string) func(context.Context) error {
	return func(ctx context.Context) error {
		_, _, err := h.ticketScope(ctx, c, ref)
		return err
	}
}

func (h *Handler) ticketsCreate(ctx context.Context, c *call, in ticketsCreateIn) (any, error) {
	s, err := h.scope(ctx, c)
	if err != nil {
		return nil, err
	}
	recheck := func(ctx context.Context) error { _, err := h.scope(ctx, c); return err }
	return h.idempotent(ctx, c, ToolTicketsCreate, in.IdempotencyKey, in, recheck, func(ctx context.Context) (any, error) {
		var prio *int
		if in.Priority != 0 {
			prio = &in.Priority
		}
		t, err := h.d.Tickets.Create(ctx, s, tickets.CreateInput{
			Subject: in.Subject, Body: in.Body, Priority: prio, RequestTypeId: optional(in.RequestTypeId), GroupId: optional(in.GroupId),
			Requester: tickets.RequesterInput{ContactId: optional(in.Requester.ContactId), Name: optional(in.Requester.Name),
				Email: optional(in.Requester.Email), ExternalId: optional(in.Requester.ExternalId),
				OrganisationId: optional(in.Requester.OrganisationId)},
			OrganisationId: optional(in.OrganisationId), CCEmails: in.CCEmails,
		})
		if err != nil {
			return nil, err
		}
		c.ticketID = t.Id
		return transporthttp.TicketView(t), nil
	})
}

func (h *Handler) ticketsFollowUp(ctx context.Context, c *call, in ticketsFollowUpIn) (any, error) {
	s, t, err := h.ticketScope(ctx, c, in.Ticket)
	if err != nil {
		return nil, err
	}
	return h.idempotent(ctx, c, ToolTicketsFollowUp, in.IdempotencyKey, in, h.recheckTicket(c, in.Ticket), func(ctx context.Context) (any, error) {
		var prio *int
		if in.Priority != 0 {
			prio = &in.Priority
		}
		out, err := h.d.Tickets.FollowUp(ctx, s, t.Id, tickets.FollowUpInput{Subject: optional(in.Subject), Body: optional(in.Body), Priority: prio,
			GroupId: optional(in.GroupId), RequestTypeId: optional(in.RequestTypeId), Tags: in.Tags,
			NoSignature: in.AddSignature != nil && !*in.AddSignature, ContactId: in.ContactId})
		if err != nil {
			return nil, err
		}
		c.ticketID = out.Id
		return transporthttp.TicketView(out), nil
	})
}

func (h *Handler) ticketsCopy(ctx context.Context, c *call, in ticketsCopyIn) (any, error) {
	s, t, err := h.ticketScope(ctx, c, in.Ticket)
	if err != nil {
		return nil, err
	}
	return h.idempotent(ctx, c, ToolTicketsCopy, in.IdempotencyKey, in, h.recheckTicket(c, in.Ticket), func(ctx context.Context) (any, error) {
		var prio *int
		if in.Priority != 0 {
			prio = &in.Priority
		}
		out, err := h.d.Tickets.Copy(ctx, s, t.Id, tickets.CopyInput{Subject: optional(in.Subject), GroupId: optional(in.GroupId), Priority: prio,
			RequestTypeId: optional(in.RequestTypeId), Tags: in.Tags, Body: optional(in.Body), IncludeAttachments: in.IncludeAttachments})
		if err != nil {
			return nil, err
		}
		c.ticketID = out.Id
		return transporthttp.TicketView(out), nil
	})
}

func (h *Handler) ticketsClaim(ctx context.Context, c *call, in ticketsClaimIn) (any, error) {
	s, t, err := h.ticketScope(ctx, c, in.Ticket)
	if err != nil {
		return nil, err
	}
	return h.idempotent(ctx, c, ToolTicketsClaim, in.IdempotencyKey, in, h.recheckTicket(c, in.Ticket), func(ctx context.Context) (any, error) {
		out, err := h.d.Tickets.Claim(ctx, s, t.Id, expected(t, in.ExpectedVersion))
		if err != nil {
			return nil, err
		}
		return transporthttp.TicketView(out), nil
	})
}

// post adds a message with an explicit audience. expectedVersion, when
// given, is checked against the ticket just read; expectedLastPublicId
// is the service's atomic stale-reply guard.
func (h *Handler) post(ctx context.Context, c *call, tool, ref, audience, as, body, lastPublic, thenStatus string, noSignature bool, contactId string, m mutationArgs, args any) (any, error) {
	s, t, err := h.ticketScope(ctx, c, ref)
	if err != nil {
		return nil, err
	}
	if m.ExpectedVersion > 0 && m.ExpectedVersion != t.Version {
		return nil, fault.Conflict("versionConflict", "This ticket was changed by someone else. Read it again and retry.")
	}
	return h.idempotent(ctx, c, tool, m.IdempotencyKey, args, h.recheckTicket(c, ref), func(ctx context.Context) (any, error) {
		res, err := h.d.Tickets.PostMessage(ctx, s, t.Id, tickets.PostInput{Visibility: audience, Body: body, ExpectedLastPublicId: optional(lastPublic),
			ThenStatusId: optional(thenStatus), As: as, ContactId: contactId, NoSignature: noSignature})
		if err != nil {
			return nil, err
		}
		return map[string]any{"message": transporthttp.MessageView(res.Message), "ticket": transporthttp.TicketView(res.Ticket)}, nil
	})
}

func (h *Handler) ticketsReply(ctx context.Context, c *call, in ticketsReplyIn) (any, error) {
	if in.Audience != tickets.VisibilityPublic {
		return nil, fault.Field("audience", "audienceRequired", "Set audience to \"public\" to reply to the requester.")
	}
	return h.post(ctx, c, ToolTicketsReply, in.Ticket, tickets.VisibilityPublic, in.As, in.Body, in.ExpectedLastPublicId, in.ThenStatusId, in.AddSignature != nil && !*in.AddSignature, in.ContactId, in.mutationArgs, in)
}

func (h *Handler) ticketsAddNote(ctx context.Context, c *call, in ticketsAddNoteIn) (any, error) {
	// Never default a note's audience: a missing or different value is an
	// error, not a public reply.
	if in.Audience != tickets.VisibilityAgent {
		return nil, fault.Field("audience", "audienceRequired", "Set audience to \"agent\" for a staff-only note.")
	}
	return h.post(ctx, c, ToolTicketsAddNote, in.Ticket, tickets.VisibilityAgent, "", in.Body, "", in.ThenStatusId, false, "", in.mutationArgs, in)
}

func (h *Handler) ticketsUpdate(ctx context.Context, c *call, in ticketsUpdateIn) (any, error) {
	s, t, err := h.ticketScope(ctx, c, in.Ticket)
	if err != nil {
		return nil, err
	}
	return h.idempotent(ctx, c, ToolTicketsUpdate, in.IdempotencyKey, in, h.recheckTicket(c, in.Ticket), func(ctx context.Context) (any, error) {
		up := tickets.UpdateInput{Subject: optional(in.Subject), RequestTypeId: optional(in.RequestTypeId), Tags: in.Tags,
			OrganisationId: in.OrganisationId, CCEmails: in.CCEmails}
		if in.Priority != 0 {
			prio := in.Priority
			up.Priority = &prio
		}
		out, err := h.d.Tickets.Update(ctx, s, t.Id, expected(t, in.ExpectedVersion), up)
		if err != nil {
			return nil, err
		}
		return transporthttp.TicketView(out), nil
	})
}

func (h *Handler) ticketsAssign(ctx context.Context, c *call, in ticketsAssignIn) (any, error) {
	if in.Unassign && in.AssigneeId != "" {
		return nil, fault.Field("unassign", "invalidCombination", "Send either assigneeId or unassign, not both.")
	}
	s, t, err := h.ticketScope(ctx, c, in.Ticket)
	if err != nil {
		return nil, err
	}
	return h.idempotent(ctx, c, ToolTicketsAssign, in.IdempotencyKey, in, h.recheckTicket(c, in.Ticket), func(ctx context.Context) (any, error) {
		a := tickets.AssignInput{GroupId: in.GroupId}
		switch {
		case in.Unassign:
			a.AssigneeId = tickets.OptionalID{Set: true}
		case in.AssigneeId != "":
			a.AssigneeId = tickets.OptionalID{Set: true, Value: &in.AssigneeId}
		}
		out, err := h.d.Tickets.Assign(ctx, s, t.Id, expected(t, in.ExpectedVersion), a)
		if err != nil {
			return nil, err
		}
		return transporthttp.TicketView(out), nil
	})
}

// ---- Directory, contacts and canned replies ----

func (h *Handler) groupsList(ctx context.Context, c *call, in pageArgs) (any, error) {
	s, err := h.scope(ctx, c)
	if err != nil {
		return nil, err
	}
	after, limit, err := in.parse()
	if err != nil {
		return nil, err
	}
	p, err := h.d.Projects.ListGroups(ctx, s, after, limit)
	if err != nil {
		return nil, err
	}
	return pageOf(p, func(g projects.Group) any { return transporthttp.GroupView(g) }), nil
}

func (h *Handler) agentsList(ctx context.Context, c *call, in pageArgs) (any, error) {
	s, err := h.scope(ctx, c)
	if err != nil {
		return nil, err
	}
	after, limit, err := in.parse()
	if err != nil {
		return nil, err
	}
	p, err := h.d.Projects.ListMembers(ctx, s, projects.MemberFilter{}, after, limit)
	if err != nil {
		return nil, err
	}
	return pageOf(p, func(m projects.Member) any { return transporthttp.MemberView(m) }), nil
}

func (h *Handler) statusesList(ctx context.Context, c *call, _ noArgs) (any, error) {
	s, err := h.scope(ctx, c)
	if err != nil {
		return nil, err
	}
	rows, err := h.d.Workflow.Statuses(ctx, s)
	if err != nil {
		return nil, err
	}
	return itemsOf(rows, func(st statuses.Status) any { return transporthttp.StatusView(st) }), nil
}

func (h *Handler) contactsList(ctx context.Context, c *call, in contactsListIn) (any, error) {
	s, err := h.scope(ctx, c)
	if err != nil {
		return nil, err
	}
	after, limit, err := in.pageArgs.parse()
	if err != nil {
		return nil, err
	}
	p, err := h.d.Customers.ListContacts(ctx, s, in.Q, in.ExternalId, after, limit)
	if err != nil {
		return nil, err
	}
	return pageOf(p, func(ct customers.Contact) any { return transporthttp.ContactView(ct) }), nil
}

func (h *Handler) contactsGet(ctx context.Context, c *call, in contactsGetIn) (any, error) {
	s, err := h.scope(ctx, c)
	if err != nil {
		return nil, err
	}
	ct, err := h.d.Customers.GetContact(ctx, s, in.ContactId)
	if err != nil {
		return nil, err
	}
	return transporthttp.ContactView(ct), nil
}

func (h *Handler) contactsUpsert(ctx context.Context, c *call, in contactsUpsertIn) (any, error) {
	s, err := h.scope(ctx, c)
	if err != nil {
		return nil, err
	}
	res, err := h.d.Customers.UpsertContact(ctx, s, customers.ContactInput{Name: in.Name, Email: in.Email, ExternalId: optional(in.ExternalId), OrganisationId: in.OrganisationId})
	if err != nil {
		return nil, err
	}
	return map[string]any{"contact": transporthttp.ContactView(res.Contact), "created": res.Created}, nil
}

func (h *Handler) cannedRepliesList(ctx context.Context, c *call, _ noArgs) (any, error) {
	s, err := h.scope(ctx, c)
	if err != nil {
		return nil, err
	}
	rows, err := h.d.Macros.List(ctx, s)
	if err != nil {
		return nil, err
	}
	return itemsOf(rows, func(m macros.Macro) any { return transporthttp.MacroView(m) }), nil
}

func (h *Handler) tagsList(ctx context.Context, c *call, _ noArgs) (any, error) {
	s, err := h.scope(ctx, c)
	if err != nil {
		return nil, err
	}
	rows, err := h.d.Tickets.Tags(ctx, s)
	if err != nil {
		return nil, err
	}
	return itemsOf(rows, func(t tickets.TagCount) any { return transporthttp.TagView(t) }), nil
}

func (h *Handler) tagsCreate(ctx context.Context, c *call, in tagNameIn) (any, error) {
	s, err := h.scope(ctx, c)
	if err != nil {
		return nil, err
	}
	t, err := h.d.Tickets.CreateTag(ctx, s, in.Name)
	if err != nil {
		return nil, err
	}
	return transporthttp.TagView(t), nil
}

func (h *Handler) tagsDelete(ctx context.Context, c *call, in tagNameIn) (any, error) {
	s, err := h.scope(ctx, c)
	if err != nil {
		return nil, err
	}
	n, err := h.d.Tickets.DeleteTag(ctx, s, in.Name)
	if err != nil {
		return nil, err
	}
	return map[string]any{"deleted": tickets.NormalizeTag(in.Name), "tickets": n}, nil
}

func (h *Handler) holdReasonsList(ctx context.Context, c *call, _ noArgs) (any, error) {
	s, err := h.scope(ctx, c)
	if err != nil {
		return nil, err
	}
	rows, err := h.d.HoldReasons.List(ctx, s)
	if err != nil {
		return nil, err
	}
	return itemsOf(rows, func(r holdreasons.Reason) any { return transporthttp.HoldReasonView(r) }), nil
}

func (h *Handler) holdReasonsCreate(ctx context.Context, c *call, in holdReasonCreateIn) (any, error) {
	s, err := h.scope(ctx, c)
	if err != nil {
		return nil, err
	}
	r, err := h.d.HoldReasons.Create(ctx, s, holdreasons.Input{Name: &in.Name, MaxDays: &in.MaxDays})
	if err != nil {
		return nil, err
	}
	return transporthttp.HoldReasonView(r), nil
}

func (h *Handler) holdReasonsUpdate(ctx context.Context, c *call, in holdReasonUpdateIn) (any, error) {
	s, err := h.scope(ctx, c)
	if err != nil {
		return nil, err
	}
	cur, err := h.d.HoldReasons.Get(ctx, s, in.HoldReasonId)
	if err != nil {
		return nil, err
	}
	version := in.ExpectedVersion
	if version == 0 {
		version = cur.Version
	}
	var maxDays *int
	if in.MaxDays != 0 {
		maxDays = &in.MaxDays
	}
	r, err := h.d.HoldReasons.Update(ctx, s, cur.Id, version, holdreasons.Input{Name: optional(in.Name), MaxDays: maxDays})
	if err != nil {
		return nil, err
	}
	return transporthttp.HoldReasonView(r), nil
}

func (h *Handler) holdReasonsDelete(ctx context.Context, c *call, in holdReasonDeleteIn) (any, error) {
	s, err := h.scope(ctx, c)
	if err != nil {
		return nil, err
	}
	cur, err := h.d.HoldReasons.Get(ctx, s, in.HoldReasonId)
	if err != nil {
		return nil, err
	}
	version := in.ExpectedVersion
	if version == 0 {
		version = cur.Version
	}
	if err := h.d.HoldReasons.Delete(ctx, s, cur.Id, version); err != nil {
		return nil, err
	}
	return map[string]any{"deleted": cur.Id}, nil
}

func (h *Handler) organisationsList(ctx context.Context, c *call, in organisationsListIn) (any, error) {
	s, err := h.scope(ctx, c)
	if err != nil {
		return nil, err
	}
	after, limit, err := in.pageArgs.parse()
	if err != nil {
		return nil, err
	}
	p, err := h.d.Organisations.List(ctx, s, in.Q, in.Active, in.ExternalRef, after, limit)
	if err != nil {
		return nil, err
	}
	return pageOf(p, func(o organisations.Organisation) any { return transporthttp.OrganisationView(o) }), nil
}

func (h *Handler) organisationsGet(ctx context.Context, c *call, in organisationsGetIn) (any, error) {
	s, err := h.scope(ctx, c)
	if err != nil {
		return nil, err
	}
	o, err := h.d.Organisations.Get(ctx, s, in.OrganisationId)
	if err != nil {
		return nil, err
	}
	return transporthttp.OrganisationView(o), nil
}

func (h *Handler) organisationsCreate(ctx context.Context, c *call, in organisationsCreateIn) (any, error) {
	s, err := h.scope(ctx, c)
	if err != nil {
		return nil, err
	}
	o, err := h.d.Organisations.Create(ctx, s, organisations.Input{Name: &in.Name, ExternalRef: optional(in.ExternalRef)})
	if err != nil {
		return nil, err
	}
	return transporthttp.OrganisationView(o), nil
}

func (h *Handler) organisationsUpdate(ctx context.Context, c *call, in organisationsUpdateIn) (any, error) {
	s, err := h.scope(ctx, c)
	if err != nil {
		return nil, err
	}
	cur, err := h.d.Organisations.Get(ctx, s, in.OrganisationId)
	if err != nil {
		return nil, err
	}
	version := in.ExpectedVersion
	if version == 0 {
		version = cur.Version
	}
	o, err := h.d.Organisations.Update(ctx, s, cur.Id, version, organisations.Input{Name: optional(in.Name), ExternalRef: in.ExternalRef, Active: in.Active})
	if err != nil {
		return nil, err
	}
	return transporthttp.OrganisationView(o), nil
}

func (h *Handler) organisationsDelete(ctx context.Context, c *call, in organisationsDeleteIn) (any, error) {
	s, err := h.scope(ctx, c)
	if err != nil {
		return nil, err
	}
	cur, err := h.d.Organisations.Get(ctx, s, in.OrganisationId)
	if err != nil {
		return nil, err
	}
	version := in.ExpectedVersion
	if version == 0 {
		version = cur.Version
	}
	if err := h.d.Organisations.Delete(ctx, s, cur.Id, version); err != nil {
		return nil, err
	}
	return map[string]any{"deleted": cur.Id}, nil
}

func itemsOf[T any](rows []T, view func(T) any) map[string]any {
	items := make([]any, 0, len(rows))
	for _, r := range rows {
		items = append(items, view(r))
	}
	return map[string]any{"items": items}
}

func (h *Handler) ticketsTransition(ctx context.Context, c *call, in ticketsTransitionIn) (any, error) {
	s, t, err := h.ticketScope(ctx, c, in.Ticket)
	if err != nil {
		return nil, err
	}
	return h.idempotent(ctx, c, ToolTicketsTransition, in.IdempotencyKey, in, h.recheckTicket(c, in.Ticket), func(ctx context.Context) (any, error) {
		out, err := h.d.Tickets.Transition(ctx, s, t.Id, expected(t, in.ExpectedVersion),
			tickets.TransitionInput{Action: in.Action, StatusId: optional(in.StatusId), Reason: in.Reason, HoldReasonId: in.HoldReasonId, Note: in.Note, ContactId: in.ContactId})
		if err != nil {
			return nil, err
		}
		return transporthttp.TicketView(out), nil
	})
}
