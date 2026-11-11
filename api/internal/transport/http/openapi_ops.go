package http

import (
	"github.com/arksnorman/open-kase/api/package/validation"
)

// Permission texts.
const (
	permPublic = "None (public)."
	permUser   = "Any signed-in user."
	permAdmin  = "Administrators."
	permMember = "Admins and agents."
	// Staff-token permission suffixes (requester-side tokens are refused unless
	// a route says otherwise).
	permAgentRead = " Staff tokens: `tickets:read`; requester-side tokens: `403 userRequired`."
)

func enumOf(vals ...string) map[string]any {
	out := make([]any, 0, len(vals))
	for _, v := range vals {
		out = append(out, v)
	}
	return map[string]any{"type": "string", "enum": out}
}

func q(name, desc string) param { return param{Name: name, Description: desc} }

func qe(name, desc string, r validation.Rule) param {
	return param{Name: name, Description: desc, Schema: enumOf(r.Choices...)}
}

var (
	ticketListParams = []param{
		qe("view", "Queue view (default `all`). `myWork`, `unassigned`, `watching` and `dueSoon` are for signed-in users. Staff may combine any view with the list filters.", validation.TicketView),
		q("q", "Ticket number (`42` or `#42`), or a case-insensitive subject substring (≤ 100 characters). Full-text search is `GET /search`."),
		q("groupId", "Only tickets in this group (intersected with access)."),
		q("assignee", "`me`, `unassigned` or a user ID (signed-in users)."),
		q("priority", "Comma-separated priorities 1–4."),
		q("statusId", "Comma-separated status IDs (also for requester-side tokens)."),
		q("tag", "Comma-separated tags; tickets carrying every tag."),
		q("requestTypeId", "Comma-separated request type IDs."),
		q("organisationId", "Comma-separated organisation IDs (also for requester-side tokens)."),
		q("requesterId", "A contact ID: only that contact's tickets (also for requester-side tokens). `assignee`, `priority`, `tag`, `requestTypeId` and `due` are staff only (`403 userRequired` for requester-side tokens)."),
		qe("due", "SLA filter: `dueSoon` (warning time passed, not yet past due), `breached` (running past its deadline), `met` (first response in time), `missed` (first response after the deadline) or `running` (within time).", validation.ViewDue),
	}
	deliveryParams = []param{
		qe("channel", "Delivery channel.", validation.DeliveryChannel),
		qe("status", "Delivery status.", validation.DeliveryStatus),
		q("eventId", "Only deliveries of this outbox event."),
	}
	auditParams = []param{
		qe("actorType", "`user`, `token` (integration token) or `system` (automatic actions and CLI operators).", validation.AuditActorType),
		q("actorId", "Only events of this actor: a user or integration token ID, or with `actorType=system` a system actor kind (GET /audit-actors)."),
		q("action", "Exact action code from GET /audit-actions (e.g. `user.created`), or a prefix ending in a dot (e.g. `ticket.`)."),
		q("ticket", "Ticket ID or number (`12` or `#12`); an unknown number gives an empty page."),
		q("from", "First day (YYYY-MM-DD, UTC), inclusive."),
		q("to", "Last day (YYYY-MM-DD, UTC), inclusive."),
	}
)

var (
	errTicket    = errSpec{404, []string{"ticketNotFound"}}
	errArchived  = errSpec{409, []string{"ticketArchived"}}
	errForbidden = errSpec{403, []string{"forbidden", "insufficientScope"}}
)

// operations is the REST surface. Every route registered by NewRouter must
// appear here exactly once (enforced by TestOpenAPICoversEveryRoute).
func operations() []op {
	return []op{
		// ---- Meta ----
		{Method: "GET", Path: "/api/v1/meta", Id: "getMeta", Tag: "Meta", Summary: "API identity", Auth: authNone, Permission: permPublic, Response: metaDTO{}},
		{Method: "GET", Path: "/api/v1/openapi.json", Id: "getOpenAPI", Tag: "Meta", Summary: "This OpenAPI document", Auth: authNone,
			Permission: permPublic, Media: "application/json"},

		// ---- Auth ----
		{Method: "POST", Path: "/api/v1/auth/login", Id: "login", Tag: "Auth", Summary: "Sign in", Auth: authNone, Permission: permPublic,
			Description: "Send `login` (an email address or a username; it is an email address exactly when it contains `@`). " +
				"Returns an access token and sets the `kase_refresh` (HttpOnly) and `kase_csrf` cookies. " +
				"Failed attempts are throttled per 15 minutes (`429 loginThrottled` with `Retry-After`, the same whether or not the account exists): " +
				"5 per account (shared by its email and username) or identifier from one client lock only that client out (a client is the IPv4 address or the IPv6 /64); " +
				"50 per account or identifier from all clients together lock it for every client; 50 per client IP. " +
				"For a user with TOTP MFA the response instead has `mfaRequired: true` and a single-use `mfaToken` (5 minutes); no session or cookie is created until `POST /auth/mfa` succeeds. " +
				"`mfaEnrolmentRequired: true` means an administrator must enrol MFA (`auth.requireAdminMfa`) before any route other than `/me` and `/me/mfa*`.",
			Request: loginRequest{}, Response: loginDTO{}, Errors: []errSpec{{401, []string{"invalidCredentials"}}, {403, []string{"originRejected"}}, {429, []string{"loginThrottled"}}},
			Example: map[string]any{"login": "dana", "password": "correct horse battery"}},
		{Method: "POST", Path: "/api/v1/auth/refresh", Id: "refreshSession", Tag: "Auth", Summary: "Rotate the refresh token", Auth: authCookie,
			Permission: "A valid `kase_refresh` cookie, matching `X-CSRF-Token` and an allowed `Origin`.",
			Description: "One-use rotation: the response sets the next `kase_refresh` cookie. Presenting a used refresh token revokes the whole session (`401 refreshTokenReused`), " +
				"with one exception for browser tabs that share the cookie: for 10 seconds after a rotation the token that was just rotated is still accepted while its successor is unused. " +
				"That request gets a new access token for the same session and no `kase_refresh` cookie (the successor set by the rotating response stays current).",
			Response: authDTO{}, Errors: []errSpec{{401, []string{"refreshTokenMissing", "invalidRefreshToken", "refreshTokenReused"}}, {403, []string{"csrfFailed", "originRejected"}}}},
		{Method: "POST", Path: "/api/v1/auth/logout", Id: "logout", Tag: "Auth", Summary: "Sign out", Auth: authCookie,
			Permission: "A valid `kase_refresh` cookie, matching `X-CSRF-Token` and an allowed `Origin`.",
			Errors:     []errSpec{{403, []string{"csrfFailed", "originRejected"}}}},

		{Method: "POST", Path: "/api/v1/auth/mfa", Id: "completeMFA", Tag: "Auth", Summary: "Complete a sign-in with a second factor", Auth: authNone,
			Permission:  permPublic + " Needs the `mfaToken` from login.",
			Description: "Send the 6-digit TOTP `code` or one `recoveryCode`. Opens the session like login (cookies and access token). A code's time step is accepted once; a recovery code works once. Five wrong codes invalidate the challenge; failures are also throttled per account and client.",
			Request:     mfaRequest{}, Response: authDTO{},
			Errors:  []errSpec{{401, []string{"mfaChallengeInvalid", "invalidMfaCode"}}, {403, []string{"originRejected"}}, {429, []string{"mfaThrottled"}}},
			Example: map[string]any{"mfaToken": "kq3V…", "code": "123456"}},
		{Method: "POST", Path: "/api/v1/auth/password-reset/request", Id: "requestPasswordReset", Tag: "Auth", Summary: "Email a password reset link", Auth: authNone,
			Permission:  permPublic,
			Description: "Always answers 202 with the same body whether or not the address belongs to an active user (enumeration resistant). An active user receives a single-use link `{product.baseUrl}/reset-password?token=…` valid 30 minutes; earlier unused links are revoked. Every request counts toward a limit of 3 per address and 20 per client IP per hour.",
			Request:     passwordResetRequest{}, Response: acceptedDTO{}, Status: 202,
			Errors: []errSpec{{403, []string{"originRejected"}}, {429, []string{"resetThrottled"}}}, Example: map[string]any{"email": "dana@example.com"}},
		{Method: "POST", Path: "/api/v1/auth/password-reset/complete", Id: "completePasswordReset", Tag: "Auth", Summary: "Set a new password with a reset token", Auth: authNone,
			Permission:  permPublic + " Needs a reset token.",
			Description: "The token is single use and expires after 30 minutes. Every session of the user is revoked. MFA stays enabled.",
			Request:     passwordResetCompleteRequest{}, Errors: []errSpec{{422, []string{"validationFailed"}}, {403, []string{"originRejected"}}}},
		{Method: "POST", Path: "/api/v1/auth/invitations/accept", Id: "acceptInvitation", Tag: "Auth", Summary: "Accept an invitation and set a password", Auth: authNone,
			Permission:  permPublic + " Needs an invitation token.",
			Description: "Activates the invited user (status `invited` → `active`, email verified). The token is single use and expires after 72 hours. Sign in afterwards.",
			Request:     acceptInvitationRequest{}, Errors: []errSpec{{422, []string{"validationFailed"}}, {403, []string{"originRejected"}}}},

		// ---- Me ----
		{Method: "GET", Path: "/api/v1/me", Id: "getMe", Tag: "Me", Summary: "The signed-in user", Auth: authUser, Permission: permUser, Response: userDTO{}},
		{Method: "PATCH", Path: "/api/v1/me", Id: "patchMe", Tag: "Me", Summary: "Change my display name", Auth: authUser,
			Permission: permUser, Description: "Self-service profile edit. Only the name parts can change; username, email, roles and status stay administrator-managed.",
			Request: patchMeRequest{}, Response: userDTO{}, IfMatch: ifMatchRequired, Errors: []errSpec{{422, []string{"validationFailed"}}},
			Example: map[string]any{"givenName": "Dana", "familyName": "Okafor"}},
		{Method: "PUT", Path: "/api/v1/me/preferences", Id: "putPreferences", Tag: "Me", Summary: "Theme and email preferences", Auth: authUser,
			Permission: permUser, Request: putPreferencesRequest{}, Response: userDTO{},
			Example: map[string]any{"theme": "dark", "emailPreferences": map[string]any{"watcherUpdates": false}}},
		{Method: "POST", Path: "/api/v1/me/password", Id: "changeMyPassword", Tag: "Me", Summary: "Change my password", Auth: authUser,
			Permission: permUser, Request: changeMyPasswordRequest{}, Errors: []errSpec{{422, []string{"validationFailed"}}}},

		{Method: "GET", Path: "/api/v1/me/mfa", Id: "getMyMFA", Tag: "Me", Summary: "My MFA status", Auth: authUser, Permission: permUser + " Allowed during forced enrolment.",
			Response: mfaStatusDTO{}},
		{Method: "POST", Path: "/api/v1/me/mfa/totp/enrol", Id: "beginTOTPEnrolment", Tag: "Me", Summary: "Start TOTP enrolment", Auth: authUser,
			Permission:  permUser + " Allowed during forced enrolment.",
			Description: "Re-authenticates with the current password and returns a new secret and `otpauth://` URI (shown once; the secret is stored encrypted with the master key). Confirm within 10 minutes.",
			Request:     passwordConfirmRequest{}, Response: totpEnrolmentDTO{},
			Errors: []errSpec{{409, []string{"mfaAlreadyEnabled"}}, {422, []string{"validationFailed"}}, {429, []string{"tooManyAttempts"}}}},
		{Method: "POST", Path: "/api/v1/me/mfa/totp/confirm", Id: "confirmTOTPEnrolment", Tag: "Me", Summary: "Confirm TOTP enrolment", Auth: authUser,
			Permission:  permUser + " Allowed during forced enrolment.",
			Description: "Verifies a code from the new authenticator, enables MFA, revokes the caller's other sessions and returns 10 single-use recovery codes (shown once; stored as digests).",
			Request:     totpConfirmRequest{}, Response: recoveryCodesDTO{},
			Errors: []errSpec{{409, []string{"mfaEnrolmentNotStarted", "mfaAlreadyEnabled"}}, {422, []string{"validationFailed"}}, {429, []string{"mfaThrottled"}}}},
		{Method: "POST", Path: "/api/v1/me/mfa/recovery-codes", Id: "regenerateRecoveryCodes", Tag: "Me", Summary: "Replace my recovery codes", Auth: authUser,
			Permission: permUser, Description: "Re-authenticates with the current password and a current TOTP `code` or an unused `recoveryCode` (a wrong or missing code is a 422 on `code`; repeated wrong codes return 429). Previous codes stop working.",
			Request: mfaStepUpRequest{}, Response: recoveryCodesDTO{}, Errors: []errSpec{{409, []string{"mfaNotEnabled"}}, {422, []string{"validationFailed"}}, {429, []string{"mfaThrottled"}}}},
		{Method: "POST", Path: "/api/v1/me/mfa/disable", Id: "disableMyMFA", Tag: "Me", Summary: "Disable my MFA", Auth: authUser,
			Permission: permUser, Description: "Re-authenticates with the current password and a current TOTP `code` or an unused `recoveryCode` (a wrong or missing code is a 422 on `code`; repeated wrong codes return 429). Refused for administrators while `auth.requireAdminMfa` is on.",
			Request: mfaStepUpRequest{}, Errors: []errSpec{{409, []string{"mfaNotEnabled", "mfaRequiredByPolicy"}}, {422, []string{"validationFailed"}}, {429, []string{"mfaThrottled"}}}},

		// ---- Settings ----
		{Method: "GET", Path: "/api/v1/settings", Id: "getSettings", Tag: "Settings", Summary: "Installation settings (secrets redacted)", Auth: authAdmin,
			Permission: permAdmin, Response: settingsDTO{}},
		{Method: "PUT", Path: "/api/v1/settings", Id: "putSettings", Tag: "Settings", Summary: "Replace installation settings", Auth: authAdmin,
			Permission: permAdmin, Request: putSettingsRequest{}, Response: settingsDTO{}, IfMatch: ifMatchRequired},
		{Method: "GET", Path: "/api/v1/settings/smtp", Id: "getSMTP", Tag: "Settings", Summary: "SMTP settings", Auth: authAdmin, Permission: permAdmin, Response: smtpDTO{}},
		{Method: "PUT", Path: "/api/v1/settings/smtp", Id: "putSMTP", Tag: "Settings", Summary: "Update SMTP settings", Auth: authAdmin, Permission: permAdmin,
			Request: putSMTPRequest{}, Response: smtpDTO{}, IfMatch: ifMatchRequired},
		{Method: "POST", Path: "/api/v1/settings/smtp/test", Id: "testSMTP", Tag: "Settings", Summary: "Queue a test email", Auth: authAdmin, Permission: permAdmin,
			Request: testSMTPRequest{}, Response: deliveryEnvelopeDTO{}, Status: 202},

		// ---- Users ----
		{Method: "GET", Path: "/api/v1/users", Id: "listUsers", Tag: "Users", Summary: "List users", Auth: authAdmin, Permission: permAdmin, Response: userAdminDTO{}, Paged: true},
		{Method: "POST", Path: "/api/v1/users", Id: "createUser", Tag: "Users", Summary: "Create a user", Auth: authAdmin, Permission: permAdmin,
			Request: createUserRequest{}, Response: userAdminDTO{}, Status: 201, Idempotent: true, Errors: []errSpec{{409, []string{"emailTaken", "usernameTaken"}}},
			Example: map[string]any{"email": "dana@example.com", "username": "dana", "givenName": "Dana", "middleName": "N.", "familyName": "Okafor", "password": "a long passphrase"}},
		{Method: "GET", Path: "/api/v1/users/:u", Id: "getUser", Tag: "Users", Summary: "Get a user", Auth: authAdmin, Permission: permAdmin, Response: userAdminDTO{},
			Errors: []errSpec{{404, []string{"userNotFound"}}}},
		{Method: "PATCH", Path: "/api/v1/users/:u", Id: "patchUser", Tag: "Users", Summary: "Update or disable a user", Auth: authAdmin, Permission: permAdmin,
			Request: patchUserRequest{}, Response: userAdminDTO{}, IfMatch: ifMatchRequired, Errors: []errSpec{{404, []string{"userNotFound"}}, {409, []string{"usernameTaken"}}}},
		{Method: "GET", Path: "/api/v1/users/:u/invitation", Id: "getInvitation", Tag: "Users", Summary: "A user's pending invitation", Auth: authAdmin,
			Permission: permAdmin, Response: invitationDTO{}, Errors: []errSpec{{404, []string{"invitationNotFound"}}}},
		{Method: "POST", Path: "/api/v1/users/:u/invitation", Id: "sendInvitation", Tag: "Users", Summary: "Send or resend an invitation email", Auth: authAdmin,
			Permission:  permAdmin,
			Description: "For users with status `invited`. Emails a single-use link `{product.baseUrl}/accept-invitation?token=…` valid 72 hours and revokes earlier links. Delivery is recorded like other notification emails.",
			Response:    invitationDTO{}, Status: 202, Errors: []errSpec{{404, []string{"userNotFound"}}, {409, []string{"userNotInvited"}}}},
		{Method: "DELETE", Path: "/api/v1/users/:u/invitation", Id: "revokeInvitation", Tag: "Users", Summary: "Revoke a pending invitation", Auth: authAdmin,
			Permission: permAdmin, Errors: []errSpec{{404, []string{"userNotFound", "invitationNotFound"}}}},
		{Method: "DELETE", Path: "/api/v1/users/:u/mfa", Id: "resetUserMFA", Tag: "Users", Summary: "Reset another user's MFA", Auth: authAdmin,
			Permission: permAdmin + " Not for your own account.", Description: "For a lost authenticator without recovery codes. The user signs in with the password alone until they enrol again.",
			Errors: []errSpec{{404, []string{"userNotFound"}}, {409, []string{"mfaNotEnabled", "cannotResetOwnMfa"}}}},
		{Method: "PUT", Path: "/api/v1/users/:u/password", Id: "setUserPassword", Tag: "Users", Summary: "Set a user's password", Auth: authAdmin,
			Permission: permAdmin, Request: setUserPasswordRequest{}, Errors: []errSpec{{404, []string{"userNotFound"}}}},
		{Method: "GET", Path: "/api/v1/users/:u/groups", Id: "getUserGroups", Tag: "Users", Summary: "The active groups a user belongs to", Auth: authAdmin,
			Permission: permAdmin, Response: userGroupsDTO{}, Errors: []errSpec{errForbidden, {404, []string{"userNotFound"}}}},
		{Method: "PUT", Path: "/api/v1/users/:u/groups", Id: "putUserGroups", Tag: "Users", Summary: "Set the groups a user belongs to", Auth: authAdmin,
			Permission:  permAdmin + " Removals release the user's ineligible assignments (background job), as on `DELETE /groups/{g}/members/{u}`.",
			Description: "Adds and removes memberships so the user belongs to exactly `groupIds` among the active groups; each change is audited as `groupMembership.added` / `groupMembership.removed`. Every group is checked before any change.",
			Request:     putUserGroupsRequest{}, Response: userGroupsDTO{},
			Errors: []errSpec{errForbidden, {404, []string{"userNotFound", "groupNotFound"}}, {409, []string{"groupInactive"}}, {422, []string{"validationFailed"}}}},

		// ---- Project ----
		{Method: "GET", Path: "/api/v1/project", Id: "getProject", Tag: "Project", Summary: "The deployment's project", Auth: authUserOrToken,
			Permission: permMember + " Integrations: any scope.", Description: "One project per deployment, created by `init`. `name` is the configured product name (settings `product.name`). Tickets are identified by number (`#42`); there is no project key.",
			Response: projectDTO{}},
		{Method: "PATCH", Path: "/api/v1/project", Id: "patchProject", Tag: "Project", Summary: "Update the project", Auth: authUser,
			Permission:  permAdmin,
			Description: "Only `requesterAcknowledgement` is editable. The product name and description are settings (`PUT /settings`, `product.name`, `product.description`); `name`, `description` and `key` are rejected as unknown fields (`422`).",
			Request:     patchProjectRequest{}, Response: projectDTO{}, IfMatch: ifMatchRequired, Errors: []errSpec{errForbidden, {422, []string{"validationFailed"}}},
			Example: map[string]any{"requesterAcknowledgement": false}},
		{Method: "GET", Path: "/api/v1/statuses", Id: "listStatuses", Tag: "Project", Summary: "Ticket statuses", Auth: authUserOrToken,
			Permission: permMember + " Integrations: any scope.", Response: statusDTO{}, List: true},
		{Method: "GET", Path: "/api/v1/request-types", Id: "listRequestTypes", Tag: "Project", Summary: "Request types", Auth: authUserOrToken,
			Permission: permMember + " Integrations: any scope.", Response: requestTypeDTO{}, List: true},

		// ---- Groups ----
		{Method: "GET", Path: "/api/v1/groups", Id: "listGroups", Tag: "Groups", Summary: "List groups", Auth: authUserOrToken,
			Permission: permMember + " Agents see their own groups; admins and staff tokens (`tickets:read`) all. Requester-side tokens: `403 userRequired`.",
			Response:   groupDTO{}, Paged: true},
		{Method: "POST", Path: "/api/v1/groups", Id: "createGroup", Tag: "Groups", Summary: "Create a group", Auth: authUser, Permission: permAdmin,
			Request: createGroupRequest{}, Response: groupDTO{}, Status: 201, Idempotent: true, Errors: []errSpec{errForbidden, {409, []string{"groupNameTaken"}}}},
		{Method: "GET", Path: "/api/v1/groups/:g", Id: "getGroup", Tag: "Groups", Summary: "Get a group", Auth: authUserOrToken, Permission: permMember + permAgentRead,
			Response: groupDTO{}, Errors: []errSpec{{404, []string{"groupNotFound"}}}},
		{Method: "PATCH", Path: "/api/v1/groups/:g", Id: "patchGroup", Tag: "Groups", Summary: "Update or deactivate a group", Auth: authUser,
			Permission: permAdmin, Request: patchGroupRequest{}, Response: groupDTO{}, IfMatch: ifMatchRequired,
			Description: "Names are unique per project (case- and space-insensitive). The protected Triage group may be renamed and described; it cannot be deactivated or deleted (its identity is the project's default group, not its name).",
			Errors:      []errSpec{errForbidden, {404, []string{"groupNotFound"}}, {409, []string{"groupProtected", "groupNameTaken"}}, {422, []string{"validationFailed"}}},
			Example:     map[string]any{"name": "Delivery", "description": "Mail delivery problems"}},
		{Method: "DELETE", Path: "/api/v1/groups/:g", Id: "deleteGroup", Tag: "Groups", Summary: "Delete an empty group", Auth: authUser,
			Permission: permAdmin, Errors: []errSpec{errForbidden, {404, []string{"groupNotFound"}}, {409, []string{"groupProtected", "groupInUse"}}}},
		{Method: "GET", Path: "/api/v1/groups/:g/members", Id: "listGroupMembers", Tag: "Groups", Summary: "List group members", Auth: authUserOrToken,
			Permission: permMember + permAgentRead, Response: groupMemberDTO{}, Paged: true, Errors: []errSpec{{404, []string{"groupNotFound"}}}},
		{Method: "PUT", Path: "/api/v1/groups/:g/members/:u", Id: "putGroupMember", Tag: "Groups", Summary: "Add a user to a group", Auth: authUser,
			Permission: permAdmin, Response: groupMemberDTO{}, Errors: []errSpec{errForbidden, {404, []string{"groupNotFound"}}, {422, []string{"validationFailed"}}}},
		{Method: "DELETE", Path: "/api/v1/groups/:g/members/:u", Id: "deleteGroupMember", Tag: "Groups", Summary: "Remove a user from a group", Auth: authUser,
			Permission: permAdmin + " Releases the user's ineligible assignments (background job).",
			Errors:     []errSpec{errForbidden, {404, []string{"groupNotFound", "memberNotFound"}}}},

		// ---- Members ----
		{Method: "GET", Path: "/api/v1/members", Id: "listMembers", Tag: "Members", Summary: "List users with their role and groups", Auth: authUserOrToken,
			Permission: permMember + permAgentRead, Description: "Read-only directory of every user (for the assignee and watcher pickers). Users are managed under `/users`, group membership under `/groups/{g}/members`.",
			Response: memberDTO{}, Paged: true, Query: []param{
				q("q", "Case-insensitive substring of the name, email or username (≤ 100 characters)."),
				q("groupId", "Only members of this group (must be active)."),
				q("userId", "Comma-separated user IDs (at most 100), e.g. the caller to read their own groups."),
				q("active", "`true`: only active users."),
			}},

		// ---- Integrations ----
		{Method: "GET", Path: "/api/v1/integration-scopes", Id: "listIntegrationScopes", Tag: "Integrations", Summary: "API token scopes", Auth: authUser,
			Permission: permUser, Response: scopeDTO{}, List: true,
			Description: "Every scope a token can hold, with a description (one list; the creator picks any combination). `staff` marks the scopes that make a token act as support staff."},
		{Method: "GET", Path: "/api/v1/integrations", Id: "listIntegrations", Tag: "Integrations", Summary: "List integrations", Auth: authUser,
			Permission: permAdmin, Response: integrationDTO{}, Paged: true, Errors: []errSpec{errForbidden}},
		{Method: "POST", Path: "/api/v1/integrations", Id: "createIntegration", Tag: "Integrations", Summary: "Create an API token (integration)",
			Auth: authUser, Permission: permAdmin, Description: "One kind of token (owner decision 2026-10-31); the creator picks any combination of `scopes`. A token holding `tickets:comment`, `tickets:internal`, `tickets:write` or `tickets:all` is a staff token (acts as support staff under its name); otherwise it is a requester-side token (public content only). `type` is no longer accepted (`422`). The token is returned once and is never replayed by idempotent retries.",
			Request: createIntegrationRequest{}, Response: integrationCreatedDTO{}, Status: 201, Idempotent: true, Errors: []errSpec{errForbidden, {409, []string{"idempotencyKeyUsed"}}},
			Example: map[string]any{"name": "Support bot", "scopes": []string{"tickets:read", "tickets:comment", "tickets:internal"}}},
		{Method: "PATCH", Path: "/api/v1/integrations/:i", Id: "patchIntegration", Tag: "Integrations", Summary: "Rename an integration",
			Auth: authUser, Permission: permAdmin, Description: "Added 2026-10-02. Only `name` changes; scopes and the token stay as issued. Revoked integrations cannot be changed.",
			Request: patchIntegrationRequest{}, Response: integrationDTO{}, IfMatch: ifMatchRequired,
			Errors:  []errSpec{errForbidden, {404, []string{"integrationNotFound"}}, {409, []string{"integrationRevoked"}}, {422, []string{"validationFailed"}}},
			Example: map[string]any{"name": "Billing portal"}},
		{Method: "DELETE", Path: "/api/v1/integrations/:i", Id: "deleteIntegration", Tag: "Integrations", Summary: "Revoke an integration",
			Auth: authUser, Permission: permAdmin, Errors: []errSpec{errForbidden, {404, []string{"integrationNotFound"}}}},

		// ---- Contacts ----
		{Method: "GET", Path: "/api/v1/contacts", Id: "listContacts", Tag: "Customers", Summary: "List contacts", Auth: authUserOrToken,
			Permission: permMember + " API tokens: `contacts:read`.", Response: contactDTO{}, Paged: true, Query: []param{q("q", "Name or email prefix."), q("externalId", "Exact match on the contact's external ID (at most one contact; combined with `q` it narrows further).")}, Errors: []errSpec{errForbidden}},
		{Method: "POST", Path: "/api/v1/contacts", Id: "upsertContact", Tag: "Customers", Summary: "Create or update a contact by email",
			Auth: authUserOrToken, Permission: permMember + " API tokens: `contacts:write`.", Request: upsertContactRequest{}, Response: contactDTO{},
			Idempotent: true, Errors: []errSpec{errForbidden}, Example: map[string]any{"name": "Priya Raman", "email": "priya@acme.example", "externalId": "crm-1042"}},
		{Method: "GET", Path: "/api/v1/contacts/:c", Id: "getContact", Tag: "Customers", Summary: "Get a contact", Auth: authUserOrToken,
			Permission: permMember + " API tokens: `contacts:read`.", Response: contactDTO{}, Errors: []errSpec{errForbidden, {404, []string{"contactNotFound"}}}},

		// ---- Organisations ----
		{Method: "GET", Path: "/api/v1/organisations", Id: "listOrganisations", Tag: "Customers", Summary: "List organisations", Auth: authUserOrToken,
			Permission: permMember + " API tokens: `organisations:read` or `organisations:write`.", Response: organisationDTO{}, Paged: true,
			Query: []param{q("q", "Search the organisation name or external reference (case-insensitive substring, at most 100 characters)."),
				q("externalRef", "Exact match on the external reference (at most one organisation; deactivated ones need `active=all`)."),
				qe("active", "`true` (default; what pickers and filters use), `false` or `all`.", validation.OrganisationActiveFilter)},
			Errors:      []errSpec{errForbidden},
			Description: "By name. `contactCount` and `ticketCount` are reported to callers who manage organisations (admins, `organisations:write`); 0 for everyone else."},
		{Method: "POST", Path: "/api/v1/organisations", Id: "createOrganisation", Tag: "Customers", Summary: "Create an organisation", Auth: authUserOrToken,
			Permission: permAdmin + " API tokens: `organisations:write` (not included in `tickets:all`).", Request: organisationBody{}, Response: organisationDTO{}, Status: 201, Idempotent: true,
			Errors: []errSpec{errForbidden, {409, []string{"organisationExists", "organisationRefTaken"}}},
			Description: "Organisations start active; `PATCH` `active: false` deactivates one (it disappears from pickers, cannot be linked to new tickets or contacts, `422 organisationInactive`, and keeps its existing links). Create the organisation first, then link its `id` to contacts (`organisationId` on `POST /contacts`) and tickets (`organisationId` on `POST /tickets`). " +
				"open-kase never infers an organisation. `name` is required; `externalRef` is your own ID for it. Audited.",
			Example: map[string]any{"name": "Acme Ltd", "externalRef": "CUST-0042"}},
		{Method: "GET", Path: "/api/v1/organisations/:o", Id: "getOrganisation", Tag: "Customers", Summary: "Get an organisation", Auth: authUserOrToken,
			Permission: permMember + " API tokens: `organisations:read` or `organisations:write`.", Response: organisationDTO{}, Errors: []errSpec{errForbidden, {404, []string{"organisationNotFound"}}}},
		{Method: "PATCH", Path: "/api/v1/organisations/:o", Id: "patchOrganisation", Tag: "Customers", Summary: "Update an organisation", Auth: authUserOrToken,
			Permission: permAdmin + " API tokens: `organisations:write`.", Request: organisationBody{}, Response: organisationDTO{}, IfMatch: ifMatchRequired,
			Errors: []errSpec{errForbidden, {404, []string{"organisationNotFound"}}, {409, []string{"organisationExists", "organisationRefTaken"}}, {422, []string{"validationFailed"}}}},
		{Method: "DELETE", Path: "/api/v1/organisations/:o", Id: "deleteOrganisation", Tag: "Customers", Summary: "Delete an organisation", Auth: authUserOrToken,
			Permission: permAdmin + " API tokens: `organisations:write`.", IfMatch: ifMatchRequired,
			Errors:      []errSpec{errForbidden, {404, []string{"organisationNotFound"}}, {409, []string{"organisationInUse"}}},
			Description: "Refused with `409 organisationInUse` while any contact or ticket references the organisation: deactivate it instead (`PATCH` with `active: false`)."},

		// ---- Tickets ----
		{Method: "GET", Path: "/api/v1/queue-counts", Id: "queueCounts", Tag: "Tickets", Summary: "Sidebar queue counts", Auth: authUser,
			Permission: permMember, Response: queueCountsDTO{}},
		{Method: "GET", Path: "/api/v1/tickets", Id: "listTickets", Tag: "Tickets", Summary: "List tickets", Auth: authUserOrToken,
			Permission:  permMember + " Agents see their groups' tickets and tickets assigned to them; admins all. API tokens: `tickets:read`, all tickets (requester-side tokens get the requester-side projection; staff tokens the staff projection). Views `myWork`, `unassigned`, `watching`, `dueSoon` and list filters other than `view` are for users (filters also for staff tokens).",
			Description: "Ordered by last update, newest first. Access filters apply before limits.",
			Response:    ticketDTO{}, Paged: true, Query: ticketListParams, Errors: []errSpec{errForbidden}},
		{Method: "POST", Path: "/api/v1/tickets", Id: "createTicket", Tag: "Tickets", Summary: "Create a ticket in a group (unassigned)",
			Auth: authUserOrToken, Permission: permMember + " API tokens: `tickets:create` (requester-side token: the requester authors the first message and `priority` is recorded as requested; a staff token authors it as the bot only with `tickets:comment` (or `tickets:all`), otherwise the requester authors it; `priority` takes effect for any staff token).",
			Request: createTicketRequest{}, Response: ticketDTO{}, Status: 201, Idempotent: true, Errors: []errSpec{errForbidden, {409, []string{"groupInactive"}}},
			Example: map[string]any{"subject": "Cannot receive external email", "body": "Inbound mail bounces with 550 5.1.1.",
				"requester": map[string]any{"name": "Priya Raman", "email": "priya@acme.example"}, "priority": 2,
				"organisationId": "66f0c0ffee00000000000001"}},
		{Method: "GET", Path: "/api/v1/tickets/:t", Id: "getTicket", Tag: "Tickets", Summary: "Get a ticket", Auth: authUserOrToken,
			Permission: permMember + " API tokens: `tickets:read`.", Response: ticketDTO{}, Errors: []errSpec{errTicket}},
		{Method: "PATCH", Path: "/api/v1/tickets/:t", Id: "patchTicket", Tag: "Tickets", Summary: "Edit priority, request type, tags or organisation (subject: creating requester-side token only)",
			Auth: authUserOrToken, Permission: permMember + " (`ticket.transition`) and staff tokens (`tickets:write`) for priority, request type, tags, `organisationId` (empty clears) and `ccEmails` (replaces the CC list; audited as `ticket.updated` with change `ccEmails`). `subject`: only the requester-side token that created the ticket (`tickets:create`).",
			Description: "The subject is locked after creation: staff (users and staff tokens) sending `subject` receive `403 subjectLocked`. " +
				"The requester-side token that created the ticket (source `api`) may send `subject` alone; it may omit If-Match (or send `0`, the version requester-side tokens see) and the change applies to the current version. " +
				"Any other requester-side token, or a web-created ticket, receives `403 subjectLocked`; a requester-side token sending other fields receives `403 userRequired`. " +
				"Tags must be in the admin-managed catalog (`GET /tags`): adding a tag that is not gives `422 validationFailed` with field code `unknownTag` on `tags[i]`; removing tags always works.",
			Request: patchTicketRequest{}, Response: ticketDTO{}, IfMatch: ifMatchRequired,
			Errors: []errSpec{errTicket, errArchived, errForbidden, {403, []string{"subjectLocked", "userRequired"}}}, Example: map[string]any{"priority": 1, "tags": []string{"vip"}}},
		{Method: "POST", Path: "/api/v1/tickets/:t/claim", Id: "claimTicket", Tag: "Tickets", Summary: "Claim an unassigned ticket (atomic)",
			Auth: authUser, Permission: "Members of the ticket's group (`ticket.claim`).", Response: ticketDTO{}, IfMatch: ifMatchRequired,
			Errors: []errSpec{errTicket, errArchived, errForbidden, {409, []string{"ticketAlreadyClaimed", "invalidTransition"}}}},
		{Method: "POST", Path: "/api/v1/tickets/:t/release", Id: "releaseTicket", Tag: "Tickets", Summary: "Return a ticket to its group queue",
			Auth: authUser, Permission: "The assignee, admins.", Request: releaseTicketRequest{}, Response: ticketDTO{}, IfMatch: ifMatchRequired,
			Description: "`reason` is required (3–500 characters, rule `ticket.releaseReason`; a missing or short reason is `422`). It is recorded on the `ticket.released` audit event and shown in the activity timeline. Automatic releases (assignee disabled or removed from the group) record a system reason.",
			Errors:      []errSpec{errTicket, errArchived, errForbidden, {409, []string{"ticketNotAssigned"}}},
			Example:     map[string]any{"reason": "Going on leave; back to the queue."}},
		{Method: "POST", Path: "/api/v1/tickets/:t/assignments", Id: "assignTicket", Tag: "Tickets", Summary: "Transfer to a group and/or assign",
			Auth: authUserOrToken, Permission: permMember + " (`ticket.assign`) and staff tokens (`tickets:write`; a token is never an assignee). Transfer: send the target `groupId` and optionally an `assigneeId` from that group. Change the assignee: send the ticket's current `groupId` and the new `assigneeId` (or null to unassign). The assignee must be an active member of the group (`422 assigneeId notEligible`).", Request: assignTicketRequest{}, Response: ticketDTO{}, IfMatch: ifMatchRequired,
			Errors:  []errSpec{errTicket, errArchived, errForbidden, {409, []string{"groupInactive"}}},
			Example: map[string]any{"groupId": "665f1c2b8e4a3d0012345678", "assigneeId": nil}},
		{Method: "POST", Path: "/api/v1/tickets/:t/transitions", Id: "transitionTicket", Tag: "Tickets", Summary: "Resolve, reopen, archive, hold or set a status",
			Auth: authUserOrToken, Permission: permMember + " (`ticket.transition`) and staff tokens (`tickets:write`). A requester-side token holding `tickets:replyAsRequester` may send `resolve` only. `If-Match` is required for users and staff tokens (`428 preconditionRequired` without it) and not used for requester-side resolve.", Request: transitionTicketRequest{}, Response: ticketDTO{}, IfMatch: ifMatchOptional,
			Errors: []errSpec{errTicket, errArchived, errForbidden, {409, []string{"invalidTransition"}}, {428, []string{"preconditionRequired"}}},
			Description: "`hold` puts an Open or Awaiting customer ticket On hold: `holdReasonId` is required and the hold lasts exactly the reason's `maxDays` " +
				"(`ticket.hold.until`); an optional `note` is saved as an internal note. Resolved and Archived tickets cannot be put on hold (`409 invalidTransition`). " +
				"The ticket returns to Open on its own at `until` (assignee kept, the assignee is emailed), or earlier when staff change its status, the requester replies or a requester-side token resolves it. " +
				"`setStatus` cannot select On hold. The requester is never notified about holds, and the first-response SLA keeps running. " +
				"A requester-side token (`tickets:replyAsRequester`) closes its ticket with `resolve`: it sees no version, so `If-Match` is not required (and ignored); every other action is `403 userRequired` for it. For the requester side, resolving a ticket that is already Resolved answers `200` with the ticket and writes nothing (staff get `409 invalidTransition`); an archived ticket is `409 ticketArchived`. The requester reopens by replying. With `notifications.requesterEmails` on, a requester-side resolve emails the rest of the customer side (CC addresses and, for a teammate's resolve, the requester), never the person who resolved it. " +
				"`contactId` (requester-side resolve only; any other caller gets `422 notAllowed`) names the contact of the ticket's organisation who resolved it: it must belong to the ticket's organisation (`422 contactNotInOrganisation`; the ticket needs an organisation), and the `ticket.resolved` audit event records it and the `ticket.statusChanged` webhook carries it as `contactId`. Omitted or the requester's own ID: the requester.",
			Example: map[string]any{"action": "resolve", "reason": "Customer confirmed the fix"}},
		{Method: "GET", Path: "/api/v1/tickets/:t/messages", Id: "listMessages", Tag: "Messages", Summary: "Conversation (oldest first)",
			Auth: authUserOrToken, Permission: permMember + " (agent notes need `message.readAgent`). API tokens: `tickets:read` for public messages; staff tokens with `tickets:internal` also agent notes.",
			Response: messageDTO{}, Paged: true, Query: []param{qe("visibility", "Only this audience.", validation.MessageVisibility)}, Errors: []errSpec{errTicket, errForbidden}},
		{Method: "POST", Path: "/api/v1/tickets/:t/messages", Id: "postMessage", Tag: "Messages", Summary: "Post a public reply or an agent note",
			Auth: authUserOrToken, Permission: permMember + " API tokens: `tickets:replyAsRequester` posts public replies as the requester; `tickets:comment` posts public replies as staff (the bot; a staff reply for the SLA); `tickets:internal` posts agent notes (the bot). With both reply scopes, `as` picks the author (default staff).",
			Description: "`visibility` is required and never inferred. `expectedLastPublicId` rejects a reply drafted before a newer public message (`409 newerReplyExists`). `thenStatusId` (users with `ticket.transition`, staff tokens with `tickets:write`) changes the status in the same transaction: Resolved resolves, an open or awaiting-customer status is set; an invalid transition (`409 invalidTransition`, `422 thenStatusId`) fails the request and nothing is posted.",
			Request:     postMessageRequest{}, Response: postedMessageDTO{}, Status: 201, Idempotent: true,
			Errors:  []errSpec{errTicket, errArchived, errForbidden, {409, []string{"newerReplyExists", "invalidTransition", "idempotencyKeyUsed"}}},
			Example: map[string]any{"visibility": "public", "body": "We have fixed the MX record.", "expectedLastPublicId": "665f1c2b8e4a3d0012345679"}},
		{Method: "GET", Path: "/api/v1/tickets/:t/scheduled-messages", Id: "listScheduledMessages", Tag: "Messages", Summary: "Scheduled messages of a ticket",
			Auth: authUser, Permission: "The author sees their own; admins (`project.manage`) see everyone's. Never integrations.",
			Description: "Ordered by send time. `?state=active` (default: scheduled and needsReview) or `all` (also sent and cancelled; newest 100). Scheduled messages are not messages: they change no counts, activity or SLA until sent.",
			Response:    list[scheduledMessageDTO]{}, Query: []param{{Name: "state", Description: "active (default) or all.", Schema: enumOf("active", "all")}}, Errors: []errSpec{errTicket, {403, []string{"userRequired"}}}},
		{Method: "POST", Path: "/api/v1/tickets/:t/scheduled-messages", Id: "scheduleMessage", Tag: "Messages", Summary: "Schedule a reply or note",
			Auth: authUser, Permission: "Users who may write to that audience (and `ticket.transition` with `thenStatusId`). Never integrations.",
			Description: "Stored durably and sent at `sendAt` by a background job through the same command as postMessage, after re-checking the author's access. Not sent (state `cancelled`, the author is emailed a link-only notice) when the ticket is archived or the author lost access; held (state `needsReview`, the author is emailed) when the requester replied after scheduling. Staged attachments are reserved until sent or cancelled. Removes the author's draft for the audience. At most 10 active scheduled messages per author and ticket.",
			Request:     scheduleMessageRequest{}, Response: scheduledMessageDTO{}, Status: 201,
			Errors:  []errSpec{errTicket, errArchived, errForbidden, {409, []string{"newerReplyExists", "tooManyScheduled"}}},
			Example: map[string]any{"visibility": "public", "body": "Good morning! The migration is complete.", "sendAt": "2026-10-05T08:00:00+02:00"}},
		{Method: "PATCH", Path: "/api/v1/tickets/:t/scheduled-messages/:s", Id: "updateScheduledMessage", Tag: "Messages", Summary: "Edit or reschedule",
			Auth: authUser, Permission: "The author.", Request: updateScheduledRequest{}, Response: scheduledMessageDTO{}, IfMatch: ifMatchRequired,
			Description: "Only while `scheduled` or `needsReview`. Any update marks the conversation as reviewed: the state returns to `scheduled` and later requester replies are measured from now. `sendAt` must be in the future.",
			Errors:      []errSpec{errTicket, errArchived, {404, []string{"scheduledMessageNotFound"}}, {409, []string{"versionConflict", "scheduledMessageNotActive"}}}},
		{Method: "POST", Path: "/api/v1/tickets/:t/scheduled-messages/:s/cancel", Id: "cancelScheduledMessage", Tag: "Messages", Summary: "Cancel a scheduled message",
			Auth: authUser, Permission: "The author, or admins.", Response: scheduledMessageDTO{}, IfMatch: ifMatchOptional,
			Description: "Reserved attachments return to the author's staging area and expire after 24 hours. When a administrator cancels someone else's message, the author is emailed a link-only notice.",
			Errors:      []errSpec{errTicket, {404, []string{"scheduledMessageNotFound"}}, {409, []string{"versionConflict", "scheduledMessageNotActive"}}}},
		{Method: "POST", Path: "/api/v1/tickets/:t/scheduled-messages/:s/send", Id: "sendScheduledMessage", Tag: "Messages", Summary: "Send a scheduled message now",
			Auth: authUser, Permission: "The author.", Request: sendScheduledRequest{}, Response: postedMessageDTO{}, Status: 201, IfMatch: ifMatchOptional,
			Description: "Also from `needsReview` (the author reviewed the conversation). Posts through the same command as postMessage, including `thenStatusId`; the scheduled message becomes `sent` in the same transaction. `expectedLastPublicId` guards against newer public replies as on postMessage.",
			Errors:      []errSpec{errTicket, errArchived, errForbidden, {404, []string{"scheduledMessageNotFound"}}, {409, []string{"versionConflict", "scheduledMessageNotActive", "newerReplyExists", "invalidTransition"}}}},
		{Method: "GET", Path: "/api/v1/tickets/:t/watchers", Id: "listWatchers", Tag: "Tickets", Summary: "Watchers who can currently read the ticket",
			Auth: authUser, Permission: "Anyone who can read the ticket. Never integrations (`403 userRequired`).",
			Description: "Oldest watch first, at most 200. Each watcher's access is re-checked now (active account, project membership, group visibility); watchers who lost access are left out, because watching grants no access. `watcherCount` on the ticket counts every watcher row and can be higher.",
			Response:    list[watcherDTO]{}, Errors: []errSpec{errTicket, {403, []string{"userRequired"}}}},
		{Method: "PUT", Path: "/api/v1/tickets/:t/watchers/me", Id: "watchTicket", Tag: "Tickets", Summary: "Watch a ticket (grants no access)",
			Auth: authUser, Permission: "Anyone who can read the ticket.", Response: ticketDTO{}, Errors: []errSpec{errTicket, errArchived}},
		{Method: "DELETE", Path: "/api/v1/tickets/:t/watchers/me", Id: "unwatchTicket", Tag: "Tickets", Summary: "Stop watching", Auth: authUser,
			Permission: "Anyone who can read the ticket.", Response: ticketDTO{}, Errors: []errSpec{errTicket}},
		{Method: "GET", Path: "/api/v1/tickets/:t/drafts/:visibility", Id: "getDraft", Tag: "Messages", Summary: "My draft for an audience",
			Auth: authUser, Permission: "Users who may write to that audience.", Response: draftDTO{}, Errors: []errSpec{errTicket}},
		{Method: "PUT", Path: "/api/v1/tickets/:t/drafts/:visibility", Id: "putDraft", Tag: "Messages", Summary: "Save my draft", Auth: authUser,
			Permission: "Users who may write to that audience.", Request: putDraftRequest{}, Response: draftDTO{}, IfMatch: ifMatchOptional,
			Errors: []errSpec{errTicket, errArchived}},
		{Method: "DELETE", Path: "/api/v1/tickets/:t/drafts/:visibility", Id: "deleteDraft", Tag: "Messages", Summary: "Discard my draft",
			Auth: authUser, Permission: "Users who may write to that audience.", Errors: []errSpec{errTicket}},
		{Method: "POST", Path: "/api/v1/attachments", Id: "uploadAttachment", Tag: "Messages", Summary: "Stage an attachment upload",
			Auth: authUserOrToken, Permission: "Users and API tokens that can post messages or create tickets.",
			Description: "Multipart field `file`. The staged upload expires after 24 hours unless attached to a message by the same uploader. Each uploader may have at most 50 unattached uploads (and 20 × the per-file limit in bytes) per project; further uploads get `429 tooManyPendingUploads`. Every upload appends an `attachment.uploaded` audit event (size and type, never the name or bytes).",
			Multipart:   true, Response: attachmentDTO{}, Status: 201, Errors: []errSpec{errForbidden, {413, []string{"attachmentTooLarge"}}, {415, []string{"multipartRequired"}}, {429, []string{"tooManyPendingUploads"}}}},
		{Method: "GET", Path: "/api/v1/attachments/:a/download", Id: "downloadAttachment", Tag: "Messages", Summary: "Download an attachment",
			Auth: authUserOrToken, Permission: "Anyone who can read the message the file is attached to (or the uploader while staged).",
			Description: "Always `Content-Disposition: attachment`; unsafe types are served as `application/octet-stream`. A file removed by a " +
				"redaction (`removeAttachments`) answers `410 attachmentRemoved` to callers who could read it otherwise; everyone else still gets " +
				"`404 attachmentNotFound`.",
			Media: "application/octet-stream", Errors: []errSpec{{404, []string{"attachmentNotFound"}}, {410, []string{"attachmentRemoved"}}}},

		// ---- Ticket workflow ----
		{Method: "GET", Path: "/api/v1/tickets/:t/links", Id: "listTicketLinks", Tag: "Tickets", Summary: "Linked tickets",
			Auth: authUserOrToken, Permission: permMember + " API tokens: `tickets:read`.",
			Description: "Related-to, duplicate-of and follow-up-to links of the ticket. Each side is authorized independently: a link is listed only when " +
				"the caller may read the other ticket too (live tickets only); links to tickets the caller cannot see are omitted, not masked.",
			Response: linkDTO{}, List: true, Errors: []errSpec{errTicket, errForbidden}},
		{Method: "POST", Path: "/api/v1/tickets/:t/links", Id: "addTicketLink", Tag: "Tickets", Summary: "Link two tickets",
			Auth: authUser, Permission: permMember + " (`ticket.transition`); the caller must be able to read both tickets.",
			Description: "Links are separate records: archived tickets can be linked. `relatedTo` is symmetric. At most 100 links per ticket.",
			Request:     addLinkRequest{}, Response: linkDTO{}, Status: 201,
			Errors:  []errSpec{errTicket, errForbidden, {409, []string{"linkExists", "tooManyLinks"}}},
			Example: map[string]any{"type": "duplicateOf", "ticket": "41"}},
		{Method: "DELETE", Path: "/api/v1/tickets/:t/links/:l", Id: "removeTicketLink", Tag: "Tickets", Summary: "Remove a link",
			Auth: authUser, Permission: permMember + " (`ticket.transition`); the caller must be able to read both tickets.",
			Errors: []errSpec{errTicket, errForbidden, {404, []string{"linkNotFound"}}}},
		{Method: "POST", Path: "/api/v1/tickets/:t/follow-up", Id: "createFollowUpTicket", Tag: "Tickets", Summary: "Create a linked follow-up ticket",
			Auth: authUserOrToken, Permission: permMember + " API tokens: `tickets:create` and `tickets:read`.",
			Description: "The \"Create follow-up ticket\" action of `ticketArchived` problems (also allowed for tickets in any other state). Creates a new " +
				"ticket linked `followUpTo` the original in the same transaction, copying only the subject, requester, request type, CC list and group (Triage when " +
				"that group is inactive) — no messages, agent notes, attachments, watchers, tags or assignee. The requester is the original's unless `contactId` names a teammate. " +
				"`contactId` opens the follow-up as another contact of the original's organisation, only when the first message is written as the requester (a token with neither `tickets:comment` nor `tickets:all`; users and tokens that write as the bot get `422 notAllowed`). It must belong to the original's organisation (`422 contactNotInOrganisation`); that contact becomes the requester of the new ticket and the author of its first message, the copied CC list drops their own address, and no `contactId` is recorded on `ticket.created`.",
			Request: followUpRequest{}, Response: ticketDTO{}, Status: 201, Idempotent: true, Errors: []errSpec{errTicket, errForbidden, {409, []string{"tooManyLinks"}}},
			Example: map[string]any{"body": "The issue is back after the migration."}},
		{Method: "POST", Path: "/api/v1/tickets/:t/copy", Id: "copyTicket", Tag: "Tickets", Summary: "Copy a ticket",
			Auth: authUserOrToken, Permission: permMember + " API tokens: `tickets:create` and `tickets:read` (agent tokens only; requester tokens receive `403 staffOnly`).",
			Description: "Creates a new Open ticket linked `copiedFrom` the original (which may be in any state, including archived) in the same transaction. " +
				"Copies the requester, CC addresses, organisation, priority, request type, group (Triage when inactive), tags that still exist in the catalog, " +
				"the first public message and, unless `includeAttachments` is false, that message's attachments (the bytes are duplicated). Every copied value " +
				"except the requester, CC addresses and organisation can be overridden in the body. The first message is authored by the caller (a user, or the bot for a token with `tickets:comment`/`tickets:all`; otherwise the requester) and gets no " +
				"signature. Not copied: assignee, watchers, status, SLA state (the copy starts its own cycle), later messages, agent notes, history and holds. " +
				"Audited as `ticket.copied`.",
			Request: copyRequest{}, Response: ticketDTO{}, Status: 201, Idempotent: true, Errors: []errSpec{errTicket, errForbidden, {409, []string{"tooManyLinks"}}},
			Example: map[string]any{"subject": "Printer offline (second site)", "priority": 3, "includeAttachments": true}},
		{Method: "GET", Path: "/api/v1/tickets/:t/activity", Id: "listTicketActivity", Tag: "Tickets", Summary: "Activity timeline (oldest first; `order=desc` newest first)", Query: []param{q("order", "`asc` (default) or `desc`; cursors are valid for the order they came from."), q("action", "Only this exact action, e.g. `message.redacted`.")},
			Auth: authUser, Permission: permMember + " API tokens receive `403 userRequired`.",
			Description: "A staff-safe projection of the ticket's audit events (`ticket.*` and `message.*` actions): actor, action, changed field names, " +
				"target, reason and time. Never message bodies or field values.",
			Response: activityDTO{}, Paged: true, Errors: []errSpec{errTicket, {403, []string{"userRequired"}}}},
		{Method: "POST", Path: "/api/v1/tickets/:t/messages/:msg/redact", Id: "redactMessage", Tag: "Messages", Summary: "Redact a message body",
			Auth: authUser, Permission: permAdmin + " (`message.redact`).",
			Description: "Sent messages are immutable; this privacy action replaces the body with `[redacted]`, records actor, time and reason (audit " +
				"`message.redacted`, never the old body) and, in the same transaction, moves the ticket version, re-indexes search, rebuilds unsent webhook payloads, redacts the copy on the scheduled message that sent it " +
				"and drops the stored idempotent-replay response of the request that posted it (a retry with that key then answers " +
				"`409 idempotencyKeyUsed`; nothing is posted twice). Drafts are not affected. " +
				"Attachments are kept unless `removeAttachments` is true: then, in the same transaction, each becomes the placeholder " +
				"`{id, removed: true}` (name, type and size are dropped; who, when and the reason are recorded; the audit event lists " +
				"`attachmentsRemoved:<n>` and `attachment/<id>`), downloads answer `410 attachmentRemoved`, and a durable job deletes the bytes " +
				"after the commit. This cannot be undone. A message redacted earlier with its attachments kept may be redacted again with " +
				"`removeAttachments` to remove them (its body and `redactedAt` stay); otherwise a second redaction is `409 messageAlreadyRedacted`. " +
				"A retry with the same `Idempotency-Key` replays the stored response, unless a later redaction of the message dropped it " +
				"(`409 idempotencyKeyUsed`). Copies already delivered by email or webhook cannot be retracted. Allowed on archived tickets.",
			Request: redactMessageRequest{}, Response: messageDTO{}, Idempotent: true,
			Errors: []errSpec{errTicket, errForbidden, {404, []string{"messageNotFound"}},
				{409, []string{"messageAlreadyRedacted", "idempotencyKeyUsed"}}},
			Example: map[string]any{"reason": "Customer posted a password", "removeAttachments": true}},
		{Method: "GET", Path: "/api/v1/automation", Id: "getAutomation", Tag: "Project", Summary: "Lifecycle automation",
			Auth: authUser, Permission: permMember, Response: automationDTO{}},
		{Method: "PUT", Path: "/api/v1/automation", Id: "putAutomation", Tag: "Project", Summary: "Save lifecycle automation",
			Auth: authUser, Permission: permAdmin,
			Description: "All rules are disabled by default (0). `archiveAfterResolvedDays` archives resolved tickets; `customerFollowupAfterHours` emails the " +
				"requester when a `waitingCustomer` ticket has waited that long since it entered the status (or since the last public staff reply), at most " +
				"`maxReminders` times `reminderCooldownHours` apart; `autoResolveAfterHours` resolves the ticket that long after the last reminder " +
				"(requires reminders, so the requester always gets prior notice). A public requester reply or any status change cancels pending timers. " +
				"Send `If-Match: 0` for the first save.",
			Request: putAutomationRequest{}, Response: automationDTO{}, IfMatch: ifMatchRequired,
			Errors:  []errSpec{errForbidden},
			Example: map[string]any{"archiveAfterResolvedDays": 30, "customerFollowupAfterHours": 48, "maxReminders": 2, "reminderCooldownHours": 48, "autoResolveAfterHours": 72}},
		{Method: "GET", Path: "/api/v1/events", Id: "streamProjectEvents", Tag: "Live updates", Summary: "Live invalidation events (SSE)",
			Auth: authUserOrToken, Permission: permMember + " API tokens: `tickets:read` (requester-side tokens: public-audience changes only, version 0; staff tokens as staff, agent-note changes with `tickets:internal`).",
			Description: "`text/event-stream` for fetch streaming with `Authorization: Bearer` (never a token in the URL). Events: `ready`, then `invalidate` " +
				"with `{type, ticketId, version}` for each committed change of a ticket the caller can read (authorized at delivery), `resync` when changes " +
				"were dropped, `error` (credential or access lost; the stream ends) and `reconnect` (server shutdown); `: ping` comments every heartbeat " +
				"(15 s). Sourced from the outbox (polled every 2 s, Redis pub/sub only shortens the delay). No history: after reconnecting, refetch. Polling " +
				"the list endpoints instead is equally valid. At most 10 concurrent streams per user or integration (`429 tooManyStreams`).",
			Media: "text/event-stream", Errors: []errSpec{errForbidden, {429, []string{"tooManyStreams"}}}},

		// ---- Search ----
		{Method: "GET", Path: "/api/v1/search", Id: "searchTickets", Tag: "Search", Summary: "Full-text ticket search",
			Auth: authUserOrToken, Permission: permMember + " API tokens: `tickets:read` (agent notes only for staff tokens with `tickets:internal`).",
			Description: "Matches ticket subjects and the bodies of messages the caller may read (public replies; agent notes only for staff who can read them). " +
				"Deleted tickets are excluded. `total` counts matching tickets the caller may see, computed after authorization; `totalCapped` is true when the " +
				"candidate bound (1,000) was reached. Snippets are plain text with highlight ranges in UTF-16 code units; render them as text. " +
				"Words are matched without stemming; quote phrases with `\"…\"` and exclude words with `-word`. " +
				"A query that is a ticket number (`42` or `#42`) lists that ticket first when the caller may read it.",
			Response: searchPageDTO{}, Query: []param{q("q", "Search words (required, ≤ 100 characters)."),
				{Name: "cursor", Description: "Opaque cursor from `nextCursor`.", Schema: map[string]any{"type": "string"}},
				{Name: "limit", Description: "Page size (1–100, default 25).", Schema: map[string]any{"type": "integer", "minimum": 1, "maximum": 100}}},
			Errors: []errSpec{errForbidden}},

		// ---- Productivity ----
		{Method: "GET", Path: "/api/v1/tags", Id: "listTags", Tag: "Productivity", Summary: "Tag catalog", Auth: authUserOrToken,
			Permission: permMember + " API tokens: `tags:read` or `tags:write`, or a staff token with `tickets:read`; others `403 insufficientScope`.", Response: tagDTO{}, List: true,
			Description: "Every tag in the admin-managed catalog, by name (at most 500), including unused ones. `count` is the number of live tickets " +
				"carrying the tag that the caller may read as staff; tickets the caller cannot see are not counted (a token that does not read tickets as staff sees 0). Only catalog tags can be added to a ticket."},
		{Method: "POST", Path: "/api/v1/tags", Id: "createTag", Tag: "Productivity", Summary: "Add a tag to the catalog", Auth: authUserOrToken,
			Permission: permAdmin + " API tokens: `tags:write` (not included in `tickets:all`).", Request: createTagRequest{}, Response: tagDTO{}, Status: 201, Idempotent: true,
			Errors:      []errSpec{errForbidden, {409, []string{"tagExists", "tagLimitReached"}}},
			Description: "Agents apply existing tags only (owner decision 2026-10-31); admins and `tags:write` tokens add tags here first. Audited (token writes with the scope).",
			Example:     map[string]any{"name": "billing"}},
		{Method: "DELETE", Path: "/api/v1/tags/:name", Id: "deleteTag", Tag: "Productivity", Summary: "Delete a tag", Auth: authUserOrToken,
			Permission: permAdmin + " API tokens: `tags:write`.", Errors: []errSpec{errForbidden, {404, []string{"tagNotFound"}}},
			Description: "Removes the tag from the catalog and from every ticket of the project that carries it (live, deleted and archived) in one " +
				"transaction; those tickets' versions increase. Audited."},
		{Method: "GET", Path: "/api/v1/hold-reasons", Id: "listHoldReasons", Tag: "Productivity", Summary: "Hold reasons", Auth: authUserOrToken,
			Permission: permMember + " API tokens: `holdReasons:read` or `holdReasons:write`, or a staff token with `tickets:read`; others `403 insufficientScope`.",
			Response:   holdReasonDTO{}, List: true,
			Description: "The reasons a ticket can be put on hold for (`POST /tickets/{t}/transitions` with `hold`), by name. A hold lasts exactly the reason's `maxDays`. " +
				"`inUse` (tickets currently on hold with the reason) is reported to callers who manage hold reasons only."},
		{Method: "POST", Path: "/api/v1/hold-reasons", Id: "createHoldReason", Tag: "Productivity", Summary: "Create a hold reason", Auth: authUserOrToken,
			Permission: permAdmin + " API tokens: `holdReasons:write` (not included in `tickets:all`).", Request: holdReasonBody{}, Response: holdReasonDTO{}, Status: 201, Idempotent: true,
			Errors:      []errSpec{errForbidden, {409, []string{"holdReasonExists", "holdReasonLimitReached"}}},
			Description: "Both `name` and `maxDays` (1 to 365) are required. Audited (token writes with the scope).",
			Example:     map[string]any{"name": "Waiting on Engineering", "maxDays": 5}},
		{Method: "PATCH", Path: "/api/v1/hold-reasons/:h", Id: "patchHoldReason", Tag: "Productivity", Summary: "Update a hold reason", Auth: authUserOrToken,
			Permission: permAdmin + " API tokens: `holdReasons:write`.", Request: holdReasonBody{}, Response: holdReasonDTO{}, IfMatch: ifMatchRequired,
			Errors:      []errSpec{errForbidden, {404, []string{"holdReasonNotFound"}}, {409, []string{"holdReasonExists"}}},
			Description: "A new `maxDays` applies to holds started afterwards; tickets already on hold keep their return time."},
		{Method: "DELETE", Path: "/api/v1/hold-reasons/:h", Id: "deleteHoldReason", Tag: "Productivity", Summary: "Delete a hold reason", Auth: authUserOrToken,
			Permission: permAdmin + " API tokens: `holdReasons:write`.", IfMatch: ifMatchRequired,
			Errors:      []errSpec{errForbidden, {404, []string{"holdReasonNotFound"}}, {409, []string{"holdReasonInUse"}}},
			Description: "Refused with `409 holdReasonInUse` while any ticket is currently On hold with this reason. Past holds keep the reason name on the ticket's activity, so deleting later loses nothing."},
		{Method: "GET", Path: "/api/v1/tickets/:t/sla", Id: "ticketSLA", Tag: "SLA", Summary: "A ticket's SLA state and cycles", Auth: authUser,
			Permission: permMember, Response: ticketSLADTO{}, Errors: []errSpec{errTicket},
			Description: "SLA is first response only, in wall-clock time (24/7). A ticket has one cycle: it starts when the ticket is created and closes " +
				"as `met` with the first public reply by staff (a user or a token posting as staff); resolving or archiving before that stops it (`resolved`, " +
				"`archived`). Reopening starts no new cycle. Turning SLA off stops an open cycle (`slaOff`)."},
		{Method: "GET", Path: "/api/v1/macros", Id: "listMacros", Tag: "Productivity", Summary: "Canned replies", Auth: authUserOrToken,
			Permission: permMember + permAgentRead, Response: macroDTO{}, List: true},
		{Method: "POST", Path: "/api/v1/macros", Id: "createMacro", Tag: "Productivity", Summary: "Create a canned reply", Auth: authUser,
			Permission: permAdmin, Request: macroBody{}, Response: macroDTO{}, Status: 201, Idempotent: true, Errors: []errSpec{errForbidden},
			Description: "A name, a Markdown body (variables `{{requester.name}}`, `{{ticket.number}}` (renders `#1042`), `{{agent.name}}`) and the audience it is meant for.",
			Example:     map[string]any{"name": "Request more info", "body": "Hi {{requester.name}}, could you send the bounce message?", "visibility": "public"}},
		{Method: "GET", Path: "/api/v1/macros/:m", Id: "getMacro", Tag: "Productivity", Summary: "Get a canned reply", Auth: authUserOrToken, Permission: permMember + permAgentRead,
			Response: macroDTO{}, Errors: []errSpec{{404, []string{"macroNotFound"}}}},
		{Method: "PATCH", Path: "/api/v1/macros/:m", Id: "patchMacro", Tag: "Productivity", Summary: "Update a canned reply", Auth: authUser,
			Permission: permAdmin, Request: macroBody{}, Response: macroDTO{}, IfMatch: ifMatchRequired, Errors: []errSpec{errForbidden, {404, []string{"macroNotFound"}}}},
		{Method: "DELETE", Path: "/api/v1/macros/:m", Id: "deleteMacro", Tag: "Productivity", Summary: "Delete a canned reply", Auth: authUser,
			Permission: permAdmin, Errors: []errSpec{errForbidden, {404, []string{"macroNotFound"}}}},
		{Method: "GET", Path: "/api/v1/tickets/:t/macros/:m/preview", Id: "previewMacro", Tag: "Productivity", Summary: "Render a canned reply for a ticket",
			Auth: authUserOrToken, Permission: permMember + permAgentRead, Response: macroPreviewDTO{}, Errors: []errSpec{errTicket, {404, []string{"macroNotFound"}}},
			Description: "Returns the body with its variables filled in (values escaped for Markdown) for the composer. Nothing is posted."},
		{Method: "GET", Path: "/api/v1/sla", Id: "getSLA", Tag: "SLA", Summary: "The SLA", Auth: authUser,
			Permission: permMember, Response: slaSettingsDTO{},
			Description: "Before the first save this returns the default: off, no targets, warning at 80 %, version 0."},
		{Method: "PUT", Path: "/api/v1/sla", Id: "putSLA", Tag: "SLA", Summary: "Save the SLA", Auth: authUser,
			Permission: permAdmin, Request: putSLARequest{}, Response: slaSettingsDTO{}, IfMatch: ifMatchOptional, Errors: []errSpec{errForbidden},
			Description: "One first-response target per priority, in wall-clock minutes (24/7; omit or null for no target). " +
				"A change applies to new cycles (new tickets), never to running ones. Saving with `active: false` after it was on " +
				"turns SLA off: the open cycles of the tickets are stopped (`closedReason: slaOff`; a background job works through " +
				"them in bounded batches right after the save) and their pending warning/breach timers announce nothing; breaches recorded before stay. " +
				"Turning it on again applies to new tickets only; existing tickets get no SLA. Omit If-Match (or send 0) for the first save."},
		{Method: "GET", Path: "/api/v1/reports/summary", Id: "reportSummary", Tag: "Reports", Summary: "Project summary report (bounded live traversal)",
			Auth: authUser, Permission: permAdmin, Response: reportSummaryDTO{},
			Query:  []param{q("from", "First day (YYYY-MM-DD, UTC)."), q("to", "Last day (YYYY-MM-DD, UTC); at most 92 days.")},
			Errors: []errSpec{errForbidden}},

		// ---- Notifications ----
		{Method: "GET", Path: "/api/v1/audit-events", Id: "listAuditEvents", Tag: "Audit", Summary: "Audit log", Auth: authAdmin, Permission: permAdmin,
			Description: "Audit events of the installation, newest first (admin Logs area). Events record changed field names and identifiers only, " +
				"never message bodies, secrets, passwords or token material. Actor and target names are looked up for each page.",
			Response: auditEventDTO{}, Paged: true, Query: auditParams},
		{Method: "GET", Path: "/api/v1/audit-actions", Id: "listAuditActions", Tag: "Audit", Summary: "Audit action catalog", Auth: authAdmin, Permission: permAdmin,
			Description: "Every audit action code the system writes, with a human label and a display category, in display order. " +
				"Use `code` as the `action` filter of GET /audit-events.",
			Response: auditActionDTO{}, List: true},
		{Method: "GET", Path: "/api/v1/audit-actors", Id: "listAuditActors", Tag: "Audit", Summary: "Audit actor catalog", Auth: authAdmin, Permission: permAdmin,
			Description: "Who can appear as the actor of an audit event: users (deactivated and invited ones too), integration tokens (revoked ones too) " +
				"and the fixed system actors. Users first, then tokens, then system actors; at most `limit` users and `limit` tokens, each sorted by name. " +
				"Webhook endpoints are not listed: they deliver events but never perform audited actions. " +
				"Pass an item's `type` and `id` as `actorType` and `actorId` to GET /audit-events.",
			Response: auditActorListDTO{},
			Query: []param{q("q", "Search text (at most 100 characters): part of a user's name, username or email, a token's name, or a system actor's name."),
				q("limit", "Maximum users and maximum tokens returned (1–200, default 50).")},
			Errors: []errSpec{{422, []string{"validationFailed"}}}},
		{Method: "GET", Path: "/api/v1/deliveries", Id: "listDeliveries", Tag: "Notifications", Summary: "Email and webhook deliveries", Auth: authAdmin,
			Permission: permAdmin, Response: deliveryDTO{}, Paged: true, Query: deliveryParams},
		{Method: "GET", Path: "/api/v1/deliveries/:d", Id: "getDelivery", Tag: "Notifications", Summary: "Get a delivery", Auth: authAdmin, Permission: permAdmin,
			Response: deliveryDTO{}, Errors: []errSpec{{404, []string{"deliveryNotFound"}}}},
		{Method: "POST", Path: "/api/v1/deliveries/:d/retry", Id: "retryDelivery", Tag: "Notifications", Summary: "Retry a failed delivery", Auth: authAdmin,
			Permission: permAdmin, Response: deliveryDTO{}, Status: 202, Errors: []errSpec{{404, []string{"deliveryNotFound"}}, {409, []string{"deliveryNotRetryable"}}}},
		{Method: "GET", Path: "/api/v1/webhook-event-types", Id: "listWebhookEventTypes", Tag: "Webhooks", Summary: "Webhook event types", Auth: authUser,
			Permission: permUser, Response: webhookEventTypeDTO{}, List: true},
		{Method: "GET", Path: "/api/v1/webhooks", Id: "listWebhooks", Tag: "Webhooks", Summary: "List webhook endpoints", Auth: authUser,
			Permission: permAdmin, Response: webhookDTO{}, Paged: true, Errors: []errSpec{errForbidden}},
		{Method: "POST", Path: "/api/v1/webhooks", Id: "createWebhook", Tag: "Webhooks", Summary: "Create a webhook endpoint",
			Auth: authUser, Permission: permAdmin + " `includeAgentNotes`: administrators.", Request: webhookBody{}, Response: webhookDTO{},
			Status: 201, Idempotent: true, Errors: []errSpec{errForbidden, {409, []string{"idempotencyKeyUsed"}}},
			Example: map[string]any{"url": "https://hooks.example.com/support", "eventTypes": []string{"ticket.created", "ticket.message.public"}}},
		{Method: "GET", Path: "/api/v1/webhooks/:w", Id: "getWebhook", Tag: "Webhooks", Summary: "Get a webhook endpoint", Auth: authUser,
			Permission: permAdmin, Response: webhookDTO{}, Errors: []errSpec{errForbidden, {404, []string{"webhookNotFound"}}}},
		{Method: "PATCH", Path: "/api/v1/webhooks/:w", Id: "patchWebhook", Tag: "Webhooks", Summary: "Update a webhook endpoint", Auth: authUser,
			Permission: permAdmin, Request: webhookBody{}, Response: webhookDTO{}, IfMatch: ifMatchRequired,
			Errors: []errSpec{errForbidden, {404, []string{"webhookNotFound"}}}},
		{Method: "DELETE", Path: "/api/v1/webhooks/:w", Id: "deleteWebhook", Tag: "Webhooks", Summary: "Delete a webhook endpoint", Auth: authUser,
			Permission: permAdmin, Errors: []errSpec{errForbidden, {404, []string{"webhookNotFound"}}}},
		{Method: "POST", Path: "/api/v1/webhooks/:w/test", Id: "testWebhook", Tag: "Webhooks", Summary: "Send a test event", Auth: authUser,
			Permission: permAdmin, Response: deliveryEnvelopeDTO{}, Status: 202, Errors: []errSpec{errForbidden, {404, []string{"webhookNotFound"}}}},
		{Method: "GET", Path: "/api/v1/webhooks/:w/deliveries", Id: "listWebhookDeliveries", Tag: "Webhooks", Summary: "Deliveries of an endpoint",
			Auth: authUser, Permission: permAdmin, Response: deliveryDTO{}, Paged: true, Query: deliveryParams,
			Errors: []errSpec{errForbidden, {404, []string{"webhookNotFound"}}}},
		{Method: "GET", Path: "/api/v1/webhooks/:w/deliveries/:d", Id: "getWebhookDelivery", Tag: "Webhooks", Summary: "Get an endpoint delivery",
			Auth: authUser, Permission: permAdmin, Response: deliveryDTO{}, Errors: []errSpec{errForbidden, {404, []string{"webhookNotFound", "deliveryNotFound"}}}},
		{Method: "POST", Path: "/api/v1/webhooks/:w/deliveries/:d/retry", Id: "retryWebhookDelivery", Tag: "Webhooks", Summary: "Retry an endpoint delivery",
			Auth: authUser, Permission: permAdmin, Response: deliveryDTO{}, Status: 202,
			Errors: []errSpec{errForbidden, {404, []string{"webhookNotFound", "deliveryNotFound"}}, {409, []string{"deliveryNotRetryable"}}}},
	}
}
