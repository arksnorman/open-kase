package audit

// The standard action and actor catalogs: every action code the system writes is defined once in actions_catalog.go with a
// human label and a category, and every kind of actor is defined here, so the
// audit log, its filters, MCP and other clients all use the same names.

// Action is an audit action code such as "ticket.created".
type Action string

// Action categories, in display order.
const (
	CategoryTickets       = "Tickets"
	CategoryMessages      = "Messages"
	CategoryContacts      = "Contacts"
	CategoryUsers         = "Users & access"
	CategoryGroups        = "Groups"
	CategoryTags          = "Tags"
	CategoryIntegrations  = "Integrations"
	CategoryWebhooks      = "Webhooks"
	CategoryConfiguration = "Configuration"
	CategorySLA           = "SLA"
	CategorySecurity      = "Sign-in & security"
	CategorySystem        = "System"
)

// ActionInfo describes one catalogued action.
type ActionInfo struct {
	Code     Action
	Label    string // e.g. "Created a ticket"
	Category string
}

var actionIndex = func() map[Action]ActionInfo {
	m := make(map[Action]ActionInfo, len(actions))
	for _, a := range actions {
		m[a.Code] = a
	}
	return m
}()

// Actions returns the catalog in display order (grouped by category).
func Actions() []ActionInfo { return append([]ActionInfo(nil), actions...) }

// LookupAction returns the catalog entry of a code.
func LookupAction(code string) (ActionInfo, bool) {
	a, ok := actionIndex[Action(code)]
	return a, ok
}

// ActionLabel returns the human label of a code, or the code itself when it
// is not catalogued.
func ActionLabel(code string) string {
	if a, ok := actionIndex[Action(code)]; ok {
		return a.Label
	}
	return code
}

// Actor types as listed to readers (audit log, actor catalog). Stored
// "service" actors are integration tokens; local CLI operators ("operator")
// are listed as system actors.
const (
	ListedUser   = "user"
	ListedToken  = "token"
	ListedSystem = "system"
)

// ListedType maps a stored actor type to the listed one.
func ListedType(stored string) string {
	switch stored {
	case ActorUser:
		return ListedUser
	case ActorService:
		return ListedToken
	default:
		return ListedSystem
	}
}

// StoredTypes maps a listed actor type to the stored types it covers.
func StoredTypes(listed string) []string {
	switch listed {
	case ListedUser:
		return []string{ActorUser}
	case ListedToken:
		return []string{ActorService}
	case ListedSystem:
		return []string{ActorSystem, ActorOperator}
	}
	return nil
}

// System actor kinds. A system or operator actor stores its kind as the
// actor ID, so the audit log can be filtered by it; the free-form label
// keeps the detail (job ID, OS user, …).
const (
	SystemAutomation   = "automation"   // auto-resolve, auto-archive, follow-up reminders
	SystemScheduler    = "scheduler"    // scheduled sends
	SystemSLA          = "sla"          // SLA timers and turning SLA off
	SystemJobs         = "jobs"         // background jobs such as releasing assignments
	SystemSigningKeys  = "signingKeys"  // token signing key creation and rotation
	SystemInstallation = "installation" // first-run installation (init)
	SystemCLI          = "cli"          // other command-line operations
)

// SystemActorInfo describes one system actor kind.
type SystemActorInfo struct {
	Id     string
	Name   string // e.g. "System · Automation"
	Detail string
}

var systemActors = []SystemActorInfo{
	{SystemAutomation, "System · Automation", "Auto-resolve, auto-archive and follow-up reminders"},
	{SystemScheduler, "System · Scheduled send", "Sends, holds or cancels scheduled messages"},
	{SystemSLA, "System · SLA", "SLA timers and stopping clocks when SLA is turned off"},
	{SystemJobs, "System · Background jobs", "Releases assignments of users who lost access"},
	{SystemSigningKeys, "System · Signing keys", "Creates and rotates token signing keys"},
	{SystemInstallation, "System · Installation", "First-run installation (init)"},
	{SystemCLI, "System · Command line", "Operator commands run on the server"},
}

// SystemActors returns the system actor kinds in display order.
func SystemActors() []SystemActorInfo { return append([]SystemActorInfo(nil), systemActors...) }

// SystemActorName returns the display name of a system actor kind
// ("System" for an unknown or empty kind).
func SystemActorName(kind string) string {
	for _, s := range systemActors {
		if s.Id == kind {
			return s.Name
		}
	}
	return "System"
}

// KnownSystemActor reports whether kind is a catalogued system actor kind.
func KnownSystemActor(kind string) bool {
	for _, s := range systemActors {
		if s.Id == kind {
			return true
		}
	}
	return false
}
