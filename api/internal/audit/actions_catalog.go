package audit

// Action codes: every audit action the system writes. Writers use these
// constants; TestEveryActionIsCatalogued fails when code uses an action
// string that is not in the catalog below.
const (
	// Tickets
	TicketCreated              Action = "ticket.created"
	TicketUpdated              Action = "ticket.updated"
	TicketClaimed              Action = "ticket.claimed"
	TicketReleased             Action = "ticket.released"
	TicketAssigned             Action = "ticket.assigned"
	TicketTransferred          Action = "ticket.transferred"
	TicketStatusChanged        Action = "ticket.statusChanged"
	TicketResolved             Action = "ticket.resolved"
	TicketReopened             Action = "ticket.reopened"
	TicketArchived             Action = "ticket.archived"
	TicketHeld                 Action = "ticket.held"
	TicketHoldEnded            Action = "ticket.holdEnded"
	TicketWatcherAdded         Action = "ticket.watcherAdded"
	TicketWatcherRemoved       Action = "ticket.watcherRemoved"
	TicketLinkAdded            Action = "ticket.linkAdded"
	TicketCopied               Action = "ticket.copied"
	TicketLinkRemoved          Action = "ticket.linkRemoved"
	TicketFollowupReminderSent Action = "ticket.followupReminderSent"

	// Messages
	MessageCreated              Action = "message.created"
	MessageRedacted             Action = "message.redacted"
	AttachmentUploaded          Action = "attachment.uploaded"
	ScheduledMessageCreated     Action = "scheduledMessage.created"
	ScheduledMessageUpdated     Action = "scheduledMessage.updated"
	ScheduledMessageNeedsReview Action = "scheduledMessage.needsReview"
	ScheduledMessageCancelled   Action = "scheduledMessage.cancelled"

	// Contacts
	ContactCreated Action = "contact.created"
	ContactUpdated Action = "contact.updated"

	// Users & access
	UserCreated            Action = "user.created"
	UserUpdated            Action = "user.updated"
	UserDisabled           Action = "user.disabled"
	UserProfileUpdated     Action = "user.profileUpdated"
	UserPasswordSet        Action = "user.passwordSet"
	UserPasswordChanged    Action = "user.passwordChanged"
	UserInvitationSent     Action = "user.invitationSent"
	UserInvitationRevoked  Action = "user.invitationRevoked"
	UserInvitationAccepted Action = "user.invitationAccepted"

	// Groups
	GroupCreated           Action = "group.created"
	GroupUpdated           Action = "group.updated"
	GroupActivated         Action = "group.activated"
	GroupDeactivated       Action = "group.deactivated"
	GroupDeleted           Action = "group.deleted"
	GroupMembershipAdded   Action = "groupMembership.added"
	GroupMembershipRemoved Action = "groupMembership.removed"

	// Tags
	TagCreated Action = "tag.created"
	TagDeleted Action = "tag.deleted"

	// Integrations
	IntegrationCreated Action = "integration.created"
	IntegrationUpdated Action = "integration.updated"
	IntegrationRevoked Action = "integration.revoked"
	MCPToolCalled      Action = "mcp.toolCalled"

	// Webhooks
	WebhookCreated Action = "webhook.created"
	WebhookUpdated Action = "webhook.updated"
	WebhookDeleted Action = "webhook.deleted"

	// Configuration
	SettingsUpdated                Action = "settings.updated"
	ProjectCreated                 Action = "project.created"
	ProjectUpdated                 Action = "project.updated"
	AutomationUpdated              Action = "automation.updated"
	MacroCreated                   Action = "macro.created"
	MacroUpdated                   Action = "macro.updated"
	MacroDeleted                   Action = "macro.deleted"
	HoldReasonCreated              Action = "holdReason.created"
	HoldReasonUpdated              Action = "holdReason.updated"
	HoldReasonDeleted              Action = "holdReason.deleted"
	OrganisationCreated            Action = "organisation.created"
	OrganisationUpdated            Action = "organisation.updated"
	OrganisationDeleted            Action = "organisation.deleted"
	NotificationTestEmailRequested Action = "notification.testEmailRequested"

	// SLA
	SLAUpdated       Action = "sla.updated"
	SLABackfilled    Action = "sla.backfilled"
	SLACyclesStopped Action = "sla.cyclesStopped"

	// Sign-in & security
	AuthLogin                       Action = "auth.login"
	AuthLogout                      Action = "auth.logout"
	AuthMFAChallenged               Action = "auth.mfaChallenged"
	AuthMFA                         Action = "auth.mfa"
	AuthReauthenticate              Action = "auth.reauthenticate"
	AuthPasswordResetRequested      Action = "auth.passwordResetRequested"
	AuthPasswordResetCompleted      Action = "auth.passwordResetCompleted"
	AuthRefreshReuseDetected        Action = "auth.refreshReuseDetected"
	UserMFAEnrolmentStarted         Action = "user.mfaEnrolmentStarted"
	UserMFAEnrolmentConfirm         Action = "user.mfaEnrolmentConfirm"
	UserMFAEnabled                  Action = "user.mfaEnabled"
	UserMFADisabled                 Action = "user.mfaDisabled"
	UserMFAReset                    Action = "user.mfaReset"
	UserMFARecoveryCodeUsed         Action = "user.mfaRecoveryCodeUsed"
	UserMFARecoveryCodesRegenerated Action = "user.mfaRecoveryCodesRegenerated"
	SigningKeyCreated               Action = "signingKey.created"
	SigningKeyRotated               Action = "signingKey.rotated"

	// System
	InstallationInitialized     Action = "installation.initialized"
	NotificationDeliveryRetried Action = "notification.deliveryRetried"
)

// actions is the catalog in display order (grouped by category).
var actions = []ActionInfo{
	{Code: TicketCreated, Label: "Created a ticket", Category: CategoryTickets},
	{Code: TicketUpdated, Label: "Updated a ticket", Category: CategoryTickets},
	{Code: TicketClaimed, Label: "Took a ticket", Category: CategoryTickets},
	{Code: TicketReleased, Label: "Released a ticket", Category: CategoryTickets},
	{Code: TicketAssigned, Label: "Assigned a ticket", Category: CategoryTickets},
	{Code: TicketTransferred, Label: "Moved a ticket to another group", Category: CategoryTickets},
	{Code: TicketStatusChanged, Label: "Changed a ticket’s status", Category: CategoryTickets},
	{Code: TicketResolved, Label: "Resolved a ticket", Category: CategoryTickets},
	{Code: TicketReopened, Label: "Reopened a ticket", Category: CategoryTickets},
	{Code: TicketArchived, Label: "Archived a ticket", Category: CategoryTickets},
	{Code: TicketHeld, Label: "Put a ticket on hold", Category: CategoryTickets},
	{Code: TicketHoldEnded, Label: "Took a ticket off hold", Category: CategoryTickets},
	{Code: TicketWatcherAdded, Label: "Added a watcher", Category: CategoryTickets},
	{Code: TicketWatcherRemoved, Label: "Removed a watcher", Category: CategoryTickets},
	{Code: TicketLinkAdded, Label: "Linked tickets", Category: CategoryTickets},
	{Code: TicketCopied, Label: "Copied a ticket", Category: CategoryTickets},
	{Code: TicketLinkRemoved, Label: "Removed a ticket link", Category: CategoryTickets},
	{Code: TicketFollowupReminderSent, Label: "Sent a follow-up reminder", Category: CategoryTickets},
	{Code: MessageCreated, Label: "Added a message", Category: CategoryMessages},
	{Code: MessageRedacted, Label: "Redacted a message", Category: CategoryMessages},
	{Code: AttachmentUploaded, Label: "Uploaded an attachment", Category: CategoryMessages},
	{Code: ScheduledMessageCreated, Label: "Scheduled a message", Category: CategoryMessages},
	{Code: ScheduledMessageUpdated, Label: "Changed a scheduled message", Category: CategoryMessages},
	{Code: ScheduledMessageNeedsReview, Label: "Held a scheduled message for review", Category: CategoryMessages},
	{Code: ScheduledMessageCancelled, Label: "Cancelled a scheduled message", Category: CategoryMessages},
	{Code: ContactCreated, Label: "Created a contact", Category: CategoryContacts},
	{Code: ContactUpdated, Label: "Updated a contact", Category: CategoryContacts},
	{Code: UserCreated, Label: "Created a user", Category: CategoryUsers},
	{Code: UserUpdated, Label: "Updated a user", Category: CategoryUsers},
	{Code: UserDisabled, Label: "Deactivated a user", Category: CategoryUsers},
	{Code: UserProfileUpdated, Label: "Updated their profile", Category: CategoryUsers},
	{Code: UserPasswordSet, Label: "Set a user’s password", Category: CategoryUsers},
	{Code: UserPasswordChanged, Label: "Changed their password", Category: CategoryUsers},
	{Code: UserInvitationSent, Label: "Sent an invitation", Category: CategoryUsers},
	{Code: UserInvitationRevoked, Label: "Revoked an invitation", Category: CategoryUsers},
	{Code: UserInvitationAccepted, Label: "Accepted an invitation", Category: CategoryUsers},
	{Code: GroupCreated, Label: "Created a group", Category: CategoryGroups},
	{Code: GroupUpdated, Label: "Updated a group", Category: CategoryGroups},
	{Code: GroupActivated, Label: "Activated a group", Category: CategoryGroups},
	{Code: GroupDeactivated, Label: "Deactivated a group", Category: CategoryGroups},
	{Code: GroupDeleted, Label: "Deleted a group", Category: CategoryGroups},
	{Code: GroupMembershipAdded, Label: "Added a group member", Category: CategoryGroups},
	{Code: GroupMembershipRemoved, Label: "Removed a group member", Category: CategoryGroups},
	{Code: TagCreated, Label: "Created a tag", Category: CategoryTags},
	{Code: TagDeleted, Label: "Deleted a tag", Category: CategoryTags},
	{Code: IntegrationCreated, Label: "Created an API token", Category: CategoryIntegrations},
	{Code: IntegrationUpdated, Label: "Updated an API token", Category: CategoryIntegrations},
	{Code: IntegrationRevoked, Label: "Revoked an API token", Category: CategoryIntegrations},
	{Code: MCPToolCalled, Label: "Called an MCP tool", Category: CategoryIntegrations},
	{Code: WebhookCreated, Label: "Created a webhook", Category: CategoryWebhooks},
	{Code: WebhookUpdated, Label: "Updated a webhook", Category: CategoryWebhooks},
	{Code: WebhookDeleted, Label: "Deleted a webhook", Category: CategoryWebhooks},
	{Code: SettingsUpdated, Label: "Changed settings", Category: CategoryConfiguration},
	{Code: ProjectCreated, Label: "Created the project", Category: CategoryConfiguration},
	{Code: ProjectUpdated, Label: "Updated the project", Category: CategoryConfiguration},
	{Code: AutomationUpdated, Label: "Changed lifecycle automation", Category: CategoryConfiguration},
	{Code: MacroCreated, Label: "Created a canned reply", Category: CategoryConfiguration},
	{Code: MacroUpdated, Label: "Updated a canned reply", Category: CategoryConfiguration},
	{Code: MacroDeleted, Label: "Deleted a canned reply", Category: CategoryConfiguration},
	{Code: HoldReasonCreated, Label: "Created a hold reason", Category: CategoryConfiguration},
	{Code: HoldReasonUpdated, Label: "Updated a hold reason", Category: CategoryConfiguration},
	{Code: HoldReasonDeleted, Label: "Deleted a hold reason", Category: CategoryConfiguration},
	{Code: OrganisationCreated, Label: "Created an organisation", Category: CategoryContacts},
	{Code: OrganisationUpdated, Label: "Updated an organisation", Category: CategoryContacts},
	{Code: OrganisationDeleted, Label: "Deleted an organisation", Category: CategoryContacts},
	{Code: NotificationTestEmailRequested, Label: "Sent a test email", Category: CategoryConfiguration},
	{Code: SLAUpdated, Label: "Changed the SLA", Category: CategorySLA},
	{Code: SLABackfilled, Label: "Started SLA clocks for existing tickets", Category: CategorySLA},
	{Code: SLACyclesStopped, Label: "Stopped SLA clocks", Category: CategorySLA},
	{Code: AuthLogin, Label: "Signed in", Category: CategorySecurity},
	{Code: AuthLogout, Label: "Signed out", Category: CategorySecurity},
	{Code: AuthMFAChallenged, Label: "Asked for a second factor", Category: CategorySecurity},
	{Code: AuthMFA, Label: "Entered a second factor", Category: CategorySecurity},
	{Code: AuthReauthenticate, Label: "Confirmed their password", Category: CategorySecurity},
	{Code: AuthPasswordResetRequested, Label: "Requested a password reset", Category: CategorySecurity},
	{Code: AuthPasswordResetCompleted, Label: "Reset their password", Category: CategorySecurity},
	{Code: AuthRefreshReuseDetected, Label: "Refresh token reuse detected", Category: CategorySecurity},
	{Code: UserMFAEnrolmentStarted, Label: "Started two-step setup", Category: CategorySecurity},
	{Code: UserMFAEnrolmentConfirm, Label: "Confirmed two-step setup", Category: CategorySecurity},
	{Code: UserMFAEnabled, Label: "Turned on two-step verification", Category: CategorySecurity},
	{Code: UserMFADisabled, Label: "Turned off two-step verification", Category: CategorySecurity},
	{Code: UserMFAReset, Label: "Reset a user’s two-step verification", Category: CategorySecurity},
	{Code: UserMFARecoveryCodeUsed, Label: "Used a recovery code", Category: CategorySecurity},
	{Code: UserMFARecoveryCodesRegenerated, Label: "Regenerated recovery codes", Category: CategorySecurity},
	{Code: SigningKeyCreated, Label: "Created a token signing key", Category: CategorySecurity},
	{Code: SigningKeyRotated, Label: "Rotated the token signing key", Category: CategorySecurity},
	{Code: InstallationInitialized, Label: "Initialized the installation", Category: CategorySystem},
	{Code: NotificationDeliveryRetried, Label: "Retried a delivery", Category: CategorySystem},
}
