package validation

// Ticket workflow: links, follow-ups, redaction and lifecycle automation.
var (
	TicketLinkType = define(Rule{Field: "ticketLink.type", Kind: KindEnum, Required: true,
		Choices: []string{"relatedTo", "duplicateOf", "followUpTo"}, Message: "Choose relatedTo, duplicateOf or followUpTo."})
	MessageRedactReason = define(Rule{Field: "message.redact.reason", Kind: KindText, Required: true, MinLen: 3, MaxLen: 1000, Multiline: true})

	// AutomationArchiveDays is archiveAfterResolvedDays (0 = disabled).
	AutomationArchiveDays = define(Rule{Field: "automation.archiveAfterResolvedDays", Kind: KindInt, Min: 0, Max: 3650,
		Message: "Use 0 (disabled) to 3650 days."})
	// AutomationFollowupHours is customerFollowupAfterHours (0 = disabled).
	AutomationFollowupHours = define(Rule{Field: "automation.customerFollowupAfterHours", Kind: KindInt, Min: 0, Max: 24 * 90,
		Message: "Use 0 (disabled) to 2160 hours."})
	AutomationMaxReminders   = define(Rule{Field: "automation.maxReminders", Kind: KindInt, Min: 1, Max: 5})
	AutomationCooldownHours  = define(Rule{Field: "automation.reminderCooldownHours", Kind: KindInt, Min: 1, Max: 24 * 90})
	AutomationAutoResolveHrs = define(Rule{Field: "automation.autoResolveAfterHours", Kind: KindInt, Min: 0, Max: 24 * 90,
		Message: "Use 0 (disabled) to 2160 hours."})
)
