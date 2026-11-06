package validation

import "regexp"

// Workspace and users.
var (
	WorkspaceName = define(Rule{Field: "workspace.name", Kind: KindText, Required: true, MinLen: 1, MaxLen: 100})
	UserEmail     = define(Rule{Field: "user.email", Kind: KindEmail, Required: true, MaxLen: 254})
	// A user's name is stored in parts (owner request 2026-10-31); the
	// display name is derived from them. Values are trimmed before checking.
	UserGivenName  = define(Rule{Field: "user.givenName", Kind: KindText, Required: true, MinLen: 1, MaxLen: 50})
	UserMiddleName = define(Rule{Field: "user.middleName", Kind: KindText, MaxLen: 50})
	UserFamilyName = define(Rule{Field: "user.familyName", Kind: KindText, Required: true, MinLen: 1, MaxLen: 50})
	// UserUsername is checked after normalization (trimmed, lower case). It
	// is never an email address (no "@"), so a sign-in identifier with "@"
	// is always an email. Reserved-looking names such as "admin" are allowed.
	UserUsername = define(Rule{Field: "user.username", Kind: KindText, Required: true, MinLen: 3, MaxLen: 32,
		Pattern: regexp.MustCompile(`^[a-z][a-z0-9._-]{2,31}$`),
		Message: "Use 3 to 32 lower-case letters, digits, '.', '_' or '-', starting with a letter."})
	UserPassword = define(Rule{Field: "user.password", Kind: KindSecret, Required: true, MinLen: 12, MaxLen: 256,
		Message: "Use a password of 12 to 256 characters."})
)

// Projects, groups and workflow configuration.
var (
	ProjectKey = define(Rule{Field: "project.key", Kind: KindText, Required: true, MinLen: 2, MaxLen: 12,
		Pattern: regexp.MustCompile(`^[A-Z][A-Z0-9]{1,11}$`),
		Message: "Use 2 to 12 uppercase letters or digits, starting with a letter."})
	ProjectName     = define(Rule{Field: "project.name", Kind: KindText, Required: true, MinLen: 1, MaxLen: 100})
	GroupName       = define(Rule{Field: "group.name", Kind: KindText, Required: true, MinLen: 1, MaxLen: 80})
	StatusLabel     = define(Rule{Field: "status.label", Kind: KindText, Required: true, MinLen: 1, MaxLen: 60})
	StatusCategory  = define(Rule{Field: "status.category", Kind: KindEnum, Required: true, Choices: []string{"open", "waitingCustomer", "onHold", "resolved", "archived"}})
	RequestTypeName = define(Rule{Field: "requestType.name", Kind: KindText, Required: true, MinLen: 1, MaxLen: 60})
	RequestTypeCode = define(Rule{Field: "requestType.code", Kind: KindText, Required: true, MinLen: 2, MaxLen: 40, Pattern: regexp.MustCompile(`^[a-z][a-zA-Z0-9]{1,39}$`)})
)

// Tickets and messages.
var (
	TicketSubject = define(Rule{Field: "ticket.subject", Kind: KindText, Required: true, MinLen: 1, MaxLen: 200})
	// TicketCCEmail is one CC address of a ticket (checked after trimming
	// and lower-casing); TicketCCCount bounds the list.
	TicketCCEmail  = define(Rule{Field: "ticket.ccEmail", Kind: KindEmail, Required: true, MaxLen: 254})
	TicketCCCount  = define(Rule{Field: "ticket.ccEmails", Kind: KindInt, Min: 0, Max: 10, Message: "Use at most 10 CC addresses."})
	TicketPriority = define(Rule{Field: "ticket.priority", Kind: KindInt, Required: true, Min: 1, Max: 4,
		Message: "Choose Priority 1 to Priority 4."})
	MessageVisibility = define(Rule{Field: "message.visibility", Kind: KindEnum, Required: true, Choices: []string{"public", "agent"},
		Message: "Choose public or agent."})
	// MessageAs is who an integration token's public reply is authored by:
	// staff (the token as a bot) or requester (the ticket's requester).
	MessageAs = define(Rule{Field: "message.as", Kind: KindEnum, Choices: []string{"staff", "requester"},
		Message: "Choose staff or requester."})
	MessageBody = define(Rule{Field: "message.body", Kind: KindText, Required: true, MinLen: 1, MaxLen: 100_000, Multiline: true})
)

// Persisted installation settings.
var (
	SettingsProductName        = define(Rule{Field: "settings.product.name", Kind: KindText, Required: true, MinLen: 1, MaxLen: 80})
	SettingsProductDescription = define(Rule{Field: "settings.product.description", Kind: KindText, MaxLen: 1000, Multiline: true})
	SettingsProductSignature   = define(Rule{Field: "settings.product.signature", Kind: KindText, MaxLen: 1000, Multiline: true})
	SettingsBaseURL            = define(Rule{Field: "settings.product.baseUrl", Kind: KindURL, MaxLen: 2048})
	SettingsAccessTokenTTL     = define(Rule{Field: "settings.auth.accessTokenTtlSeconds", Kind: KindInt, Min: 60, Max: 3600})
	SettingsRefreshTokenTTL    = define(Rule{Field: "settings.auth.refreshTokenTtlHours", Kind: KindInt, Min: 1, Max: 24 * 90})
	SettingsMaxAttachment      = define(Rule{Field: "settings.files.maxAttachmentBytes", Kind: KindInt, Min: 1024, Max: 1 << 30})
	SettingsMaxAttachments     = define(Rule{Field: "settings.files.maxAttachmentsPerMessage", Kind: KindInt, Min: 0, Max: 50})
	SettingsMaxMessageBytes    = define(Rule{Field: "settings.files.maxMessageTotalBytes", Kind: KindInt, Min: 1024, Max: 4 << 30})
	SettingsMaxMessageChars    = define(Rule{Field: "settings.files.maxMessageCodePoints", Kind: KindInt, Min: 1000, Max: 1_000_000})
	SettingsSMTPHost           = define(Rule{Field: "settings.smtp.host", Kind: KindHost, MaxLen: 253})
	SettingsSMTPPort           = define(Rule{Field: "settings.smtp.port", Kind: KindInt, Min: 1, Max: 65535})
	SettingsSMTPUsername       = define(Rule{Field: "settings.smtp.username", Kind: KindText, MaxLen: 256})
	SettingsSMTPFromAddress    = define(Rule{Field: "settings.smtp.fromAddress", Kind: KindEmail, MaxLen: 254})
	SettingsSMTPFromName       = define(Rule{Field: "settings.smtp.fromName", Kind: KindText, MaxLen: 100})
	SettingsSMTPCACertificate  = define(Rule{Field: "settings.smtp.caCertificate", Kind: KindText, MaxLen: 65536, Multiline: true})
	SettingsSMTPTLSMode        = define(Rule{Field: "settings.smtp.tlsMode", Kind: KindEnum, Required: true, Choices: []string{"starttls", "tls", "none"}})
	// Base of the link in requester emails (<base>/<ticket number>); optional.
	SettingsRequesterLinkBaseURL = define(Rule{Field: "settings.notifications.requesterLinkBaseUrl", Kind: KindURL, MaxLen: 2048})
)

// Notifications and webhooks (Milestone 5).
var (
	WebhookURL       = define(Rule{Field: "webhook.url", Kind: KindURL, Required: true, MaxLen: 2048})
	WebhookEventType = define(Rule{Field: "webhook.eventType", Kind: KindEnum, Required: true,
		Choices: []string{"ticket.created", "ticket.updated", "ticket.message.public", "ticket.message.agentNote",
			"ticket.assigned", "ticket.statusChanged",
			"ticket.slaWarning", "ticket.slaBreached", "ticket.followupReminder"}})
	NotificationTestTo = define(Rule{Field: "notification.testTo", Kind: KindEmail, Required: true, MaxLen: 254})
	DeliveryStatus     = define(Rule{Field: "delivery.status", Kind: KindEnum, Required: true,
		Choices: []string{"pending", "retrying", "sent", "failed", "unknownOutcome", "skipped", "skippedUnconfigured"}})
	DeliveryChannel = define(Rule{Field: "delivery.channel", Kind: KindEnum, Required: true, Choices: []string{"email", "webhook"}})
	JobState        = define(Rule{Field: "job.state", Kind: KindEnum, Required: true, Choices: []string{"queued", "running", "succeeded", "dead"}})
)

// Identity, sessions and preferences.
var (
	UserSignature = define(Rule{Field: "user.signature", Kind: KindText, MaxLen: 1000, Multiline: true})
	UserTheme     = define(Rule{Field: "user.theme", Kind: KindEnum, Required: true, Choices: []string{"system", "light", "dark"}, Message: "Choose system, light or dark."})
	UserStatus    = define(Rule{Field: "user.status", Kind: KindEnum, Required: true, Choices: []string{"active", "disabled"}, Message: "Choose active or disabled."})
	// AuthLogin (an email address or a username) and AuthPassword only
	// bound login input; they never reveal whether an account exists or
	// which rules applied when it was set.
	AuthLogin    = define(Rule{Field: "auth.login", Kind: KindText, Required: true, MinLen: 1, MaxLen: 254})
	AuthPassword = define(Rule{Field: "auth.password", Kind: KindSecret, Required: true, MinLen: 1, MaxLen: 256})
	// ObjectID is the opaque identifier format used at the API boundary.
	ObjectID = define(Rule{Field: "common.objectId", Kind: KindText, Required: true, Pattern: regexp.MustCompile(`^[0-9a-f]{24}$`), Message: "Use a valid identifier."})
)

// Project structure: descriptions, groups and integrations.
var (
	ProjectDescription = define(Rule{Field: "project.description", Kind: KindText, MaxLen: 1000, Multiline: true})
	GroupDescription   = define(Rule{Field: "group.description", Kind: KindText, MaxLen: 500, Multiline: true})
	IntegrationName    = define(Rule{Field: "integration.name", Kind: KindText, Required: true, MinLen: 1, MaxLen: 80})
	IntegrationScope   = define(Rule{Field: "integration.scope", Kind: KindEnum, Required: true,
		Choices: []string{"tickets:read", "tickets:create", "tickets:replyAsRequester", "tickets:comment", "tickets:internal", "tickets:write", "tickets:all", "contacts:read", "contacts:write", "tags:read", "tags:write", "holdReasons:read", "holdReasons:write", "organisations:read", "organisations:write"}})
)

// Contacts (requesters).
var (
	ContactName       = define(Rule{Field: "contact.name", Kind: KindText, Required: true, MinLen: 1, MaxLen: 200})
	ContactEmail      = define(Rule{Field: "contact.email", Kind: KindEmail, Required: true, MaxLen: 254})
	ContactExternalID = define(Rule{Field: "contact.externalId", Kind: KindText, MaxLen: 128, Pattern: externalIDPattern,
		Message: "Use up to 128 letters, digits or . _ : @ / + - characters."})
	// Organisations (customers of an integrating system; owner decision
	// 2026-10-31). ExternalRef is the integrator's own ID for the organisation.
	OrganisationName        = define(Rule{Field: "organisation.name", Kind: KindText, Required: true, MinLen: 1, MaxLen: 200})
	OrganisationExternalRef = define(Rule{Field: "organisation.externalRef", Kind: KindText, MaxLen: 128})
	SearchQuery             = define(Rule{Field: "common.searchQuery", Kind: KindText, MaxLen: 100})
)

var externalIDPattern = regexp.MustCompile(`^[A-Za-z0-9._:@/+-]{1,128}$`)

// Tickets, messages, drafts and attachments (Milestone 4).
var (
	TicketView = define(Rule{Field: "ticket.view", Kind: KindEnum, Required: true,
		Choices: []string{"myWork", "unassigned", "watching", "awaitingCustomer", "onHold", "dueSoon", "resolved", "archived", "all"}})
	TicketTransitionAction = define(Rule{Field: "ticket.transition.action", Kind: KindEnum, Required: true,
		Choices: []string{"resolve", "reopen", "archive", "hold", "setStatus"}, Message: "Choose resolve, reopen, archive, hold or setStatus."})
	TicketTransitionReason = define(Rule{Field: "ticket.transition.reason", Kind: KindText, MaxLen: 2000, Multiline: true})
	TicketReleaseReason    = define(Rule{Field: "ticket.releaseReason", Kind: KindText, Required: true, MinLen: 3, MaxLen: 500, Multiline: true})
	DraftBody              = define(Rule{Field: "draft.body", Kind: KindText, MaxLen: 100_000, Multiline: true})
	AttachmentFilename     = define(Rule{Field: "attachment.filename", Kind: KindText, Required: true, MinLen: 1, MaxLen: 255})
)

// Queues, SLA and support productivity (Milestone 6).
var (
	// TicketTag is checked after normalization (trimmed, lower case,
	// inner spaces replaced by "-").
	TicketTag = define(Rule{Field: "ticket.tag", Kind: KindText, Required: true, MinLen: 1, MaxLen: 40,
		Pattern: regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,39}$`),
		Message: "Use 1 to 40 lower-case letters, digits, '-' or '_', starting with a letter or digit."})
	// TagName is a tag catalog entry, checked after the same normalization.
	TagName = define(Rule{Field: "tag.name", Kind: KindText, Required: true, MinLen: 1, MaxLen: 40,
		Pattern: regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,39}$`),
		Message: "Use 1 to 40 lower-case letters, digits, '-' or '_', starting with a letter or digit."})
	TicketTagCount = define(Rule{Field: "ticket.tags", Kind: KindInt, Min: 0, Max: 20, Message: "Use at most 20 tags."})

	ViewAssignee = define(Rule{Field: "ticketFilter.assignee", Kind: KindText, MaxLen: 24,
		Pattern: regexp.MustCompile(`^(me|unassigned|[0-9a-f]{24})$`), Message: "Use me, unassigned or a user ID."})
	// OrganisationActiveFilter is the list parameter `active` of GET /organisations.
	OrganisationActiveFilter = define(Rule{Field: "organisation.activeFilter", Kind: KindEnum, Choices: []string{"true", "false", "all"}})
	ViewDue                  = define(Rule{Field: "ticketFilter.due", Kind: KindEnum, Required: true, Choices: []string{"dueSoon", "breached", "met", "missed", "running"}})

	HoldReasonName    = define(Rule{Field: "holdReason.name", Kind: KindText, Required: true, MinLen: 1, MaxLen: 80})
	HoldReasonMaxDays = define(Rule{Field: "holdReason.maxDays", Kind: KindInt, Required: true, Min: 1, Max: 365, Message: "Use 1 to 365 days."})

	MacroName = define(Rule{Field: "macro.name", Kind: KindText, Required: true, MinLen: 1, MaxLen: 80})
	MacroBody = define(Rule{Field: "macro.body", Kind: KindText, MaxLen: 20_000, Multiline: true})

	SLATargetMinutes  = define(Rule{Field: "sla.targetMinutes", Kind: KindInt, Min: 1, Max: 365 * 24 * 60})
	SLAWarningPercent = define(Rule{Field: "sla.warningPercent", Kind: KindInt, Min: 50, Max: 95})

	ReportDate = define(Rule{Field: "report.date", Kind: KindText, Required: true,
		Pattern: regexp.MustCompile(`^[0-9]{4}-[0-9]{2}-[0-9]{2}$`), Message: "Use a YYYY-MM-DD date."})
)

// Audit log listing filters (admin Logs area).
var (
	AuditActorType = define(Rule{Field: "audit.actorType", Kind: KindEnum, Required: true, Choices: []string{"user", "token", "system"}})
	AuditAction    = define(Rule{Field: "audit.action", Kind: KindText, Required: true, MinLen: 1, MaxLen: 100,
		Pattern: regexp.MustCompile(`^[a-zA-Z0-9.]+$`), Message: "Use an action code such as user.created, or a prefix ending in a dot such as ticket."})
	AuditActorQuery = define(Rule{Field: "audit.actorQuery", Kind: KindText, Required: true, MinLen: 1, MaxLen: 100})
	AuditActorLimit = define(Rule{Field: "audit.actorLimit", Kind: KindInt, Required: true, Min: 1, Max: 200})
	AuditTicket     = define(Rule{Field: "audit.ticket", Kind: KindText, Required: true,
		Pattern: regexp.MustCompile(`^([0-9a-f]{24}|#?[0-9]{1,12}|[A-Za-z][A-Za-z0-9]{0,19}-[0-9]{1,12})$`), Message: "Use a ticket number or ID."})
	AuditDate = define(Rule{Field: "audit.date", Kind: KindText, Required: true,
		Pattern: regexp.MustCompile(`^[0-9]{4}-[0-9]{2}-[0-9]{2}$`), Message: "Use a YYYY-MM-DD date."})
)
