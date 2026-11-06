package http

import (
	"time"

	"github.com/arksnorman/open-kase/api/internal/access"
	"github.com/arksnorman/open-kase/api/internal/auth"
	"github.com/arksnorman/open-kase/api/internal/customers"
	"github.com/arksnorman/open-kase/api/internal/integrations"
	"github.com/arksnorman/open-kase/api/internal/projects"
	"github.com/arksnorman/open-kase/api/internal/settings"
	"github.com/arksnorman/open-kase/api/internal/statuses"
	"github.com/arksnorman/open-kase/api/internal/users"
	"github.com/arksnorman/open-kase/api/package/product"
)

// Response DTOs. Domain objects are never serialized directly.

func nullable(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

type refDTO struct {
	Id   string `json:"id"`
	Name string `json:"name"`
}

type userRefDTO struct {
	Id       string `json:"id"`
	Name     string `json:"name"`
	Email    string `json:"email"`
	Username string `json:"username"`
}

// userDTO is the contract's User (the signed-in user).
type userDTO struct {
	Id       string `json:"id"`
	Email    string `json:"email"`
	Username string `json:"username" doc:"Sign-in name (alternative to the email address)."`
	Name     string `json:"name" doc:"Display name: the name parts joined with single spaces (read-only)."`
	userNameParts
	IsAdmin   bool   `json:"isAdmin"`
	Theme     string `json:"theme"`
	Signature string `json:"signature" doc:"The user's Markdown reply signature (may be empty)."`
	// EmailPreferences (added in Milestone 5).
	EmailPreferences emailPrefsDTO `json:"emailPreferences"`
	MFAEnabled       bool          `json:"mfaEnabled"`
	// Version is the If-Match value for PATCH /me.
	Version int64 `json:"version"`
}

// userNameParts are a user's name parts.
type userNameParts struct {
	GivenName  string `json:"givenName"`
	MiddleName string `json:"middleName"`
	FamilyName string `json:"familyName"`
}

func namePartsOf(u users.User) userNameParts {
	return userNameParts{GivenName: u.GivenName, MiddleName: u.MiddleName, FamilyName: u.FamilyName}
}

func toUser(u users.User) userDTO {
	return userDTO{Id: u.Id, Email: u.Email, Username: u.Username, Name: u.Name, userNameParts: namePartsOf(u), IsAdmin: u.IsAdmin(), Theme: u.EffectiveTheme(), Signature: u.Signature,
		EmailPreferences: emailPrefsDTO{WatcherUpdates: !u.EmailPrefs.MuteWatcherUpdates, QueueUpdates: !u.EmailPrefs.MuteQueueUpdates},
		MFAEnabled:       u.MFAEnabled(), Version: u.Version}
}

// emailPrefsDTO: true means the optional email is sent.
type emailPrefsDTO struct {
	WatcherUpdates bool `json:"watcherUpdates"`
	QueueUpdates   bool `json:"queueUpdates"`
}

// userAdminDTO is the administrator view of a user.
type userAdminDTO struct {
	Id       string `json:"id"`
	Email    string `json:"email"`
	Username string `json:"username"`
	Name     string `json:"name" doc:"Display name: the name parts joined with single spaces (read-only)."`
	userNameParts
	IsAdmin    bool      `json:"isAdmin"`
	Status     string    `json:"status"`
	MFAEnabled bool      `json:"mfaEnabled"`
	CreatedAt  time.Time `json:"createdAt"`
	UpdatedAt  time.Time `json:"updatedAt"`
	Version    int64     `json:"version"`
}

func toUserAdmin(u users.User) userAdminDTO {
	return userAdminDTO{Id: u.Id, Email: u.Email, Username: u.Username, Name: u.Name, userNameParts: namePartsOf(u), IsAdmin: u.IsAdmin(), Status: u.Status, MFAEnabled: u.MFAEnabled(),
		CreatedAt: u.CreatedAt, UpdatedAt: u.UpdatedAt, Version: u.Version}
}

type authDTO struct {
	AccessToken string  `json:"accessToken"`
	ExpiresIn   int     `json:"expiresIn"`
	User        userDTO `json:"user"`
	// MFAEnrolmentRequired: the session can only enrol TOTP
	// (auth.requireAdminMfa) until enrolment is confirmed.
	MFAEnrolmentRequired bool `json:"mfaEnrolmentRequired"`
}

// loginDTO is the login response: either a session (accessToken, user)
// or, for users with MFA, a challenge to complete at POST /auth/mfa.
type loginDTO struct {
	MFARequired          bool     `json:"mfaRequired" doc:"True when the password was correct and a second factor is needed; no session was opened."`
	MFAToken             *string  `json:"mfaToken,omitempty" doc:"Single-use challenge for POST /auth/mfa (MFA users only)."`
	MFAExpiresIn         *int     `json:"mfaExpiresIn,omitempty" doc:"Seconds until mfaToken expires."`
	AccessToken          *string  `json:"accessToken,omitempty"`
	ExpiresIn            *int     `json:"expiresIn,omitempty"`
	User                 *userDTO `json:"user,omitempty"`
	MFAEnrolmentRequired bool     `json:"mfaEnrolmentRequired"`
}

func toAuth(iss auth.Issued) authDTO {
	return authDTO{AccessToken: iss.AccessToken, ExpiresIn: iss.ExpiresIn, User: toUser(iss.User), MFAEnrolmentRequired: iss.MFAEnrolmentRequired}
}

type projectDTO struct {
	Id             string    `json:"id"`
	Name           string    `json:"name" doc:"The product name (settings product.name, Configuration → General); read-only here."`
	DefaultGroupId string    `json:"defaultGroupId"`
	RequesterAck   bool      `json:"requesterAcknowledgement"`
	HasSignature   bool      `json:"hasSignature" doc:"A workspace reply signature is configured (its text is not exposed here); lets the reply composer offer the signature switch."`
	CreatedAt      time.Time `json:"createdAt"`
	UpdatedAt      time.Time `json:"updatedAt"`
	Version        int64     `json:"version"`
}

// toProject is the project with the configured product name.
func (a *api) toProject(p projects.Project) projectDTO {
	name, hasSig := product.Name, false
	if snap := a.Settings.Current(); snap != nil {
		name, hasSig = snap.Product().Name, snap.Product().Signature != ""
	}
	return projectDTO{Id: p.Id, Name: name, HasSignature: hasSig,
		DefaultGroupId: p.DefaultGroupId, RequesterAck: p.RequesterAcknowledgement, CreatedAt: p.CreatedAt, UpdatedAt: p.UpdatedAt, Version: p.Version}
}

func toRefs(gs []projects.GroupRef) []refDTO {
	out := make([]refDTO, 0, len(gs))
	for _, g := range gs {
		out = append(out, refDTO{Id: g.Id, Name: g.Name})
	}
	return out
}

// userGroupsDTO is the active groups a user belongs to (admin user page).
type userGroupsDTO struct {
	Groups []refDTO `json:"groups"`
}

func toUserGroups(gs []projects.GroupRef) userGroupsDTO { return userGroupsDTO{Groups: toRefs(gs)} }

type groupDTO struct {
	Id          string    `json:"id"`
	ProjectId   string    `json:"projectId"`
	Name        string    `json:"name"`
	Description *string   `json:"description"`
	Protected   bool      `json:"protected"`
	Active      bool      `json:"active"`
	MemberCount int       `json:"memberCount"`
	CreatedAt   time.Time `json:"createdAt"`
	UpdatedAt   time.Time `json:"updatedAt"`
	Version     int64     `json:"version"`
}

func toGroup(g projects.Group) groupDTO {
	return groupDTO{Id: g.Id, ProjectId: g.ProjectId, Name: g.Name, Description: nullable(g.Description), Protected: g.Protected,
		Active: g.Active, MemberCount: g.MemberCount, CreatedAt: g.CreatedAt, UpdatedAt: g.UpdatedAt, Version: g.Version}
}

type memberDTO struct {
	User      userRefDTO `json:"user"`
	UserId    string     `json:"userId"`
	Role      string     `json:"role" enum:"admin,agent"`
	Groups    []refDTO   `json:"groups"`
	Active    bool       `json:"active"`
	CreatedAt time.Time  `json:"createdAt"`
}

func toMember(m projects.Member) memberDTO {
	return memberDTO{User: userRefDTO{Id: m.UserId, Name: m.UserName, Email: m.UserEmail, Username: m.UserUsername}, UserId: m.UserId, Role: m.Role,
		Groups: toRefs(m.Groups), Active: m.UserStatus == users.StatusActive, CreatedAt: m.CreatedAt}
}

type groupMemberDTO struct {
	GroupId   string     `json:"groupId"`
	User      userRefDTO `json:"user"`
	CreatedAt time.Time  `json:"createdAt"`
}

func toGroupMember(m projects.GroupMember) groupMemberDTO {
	return groupMemberDTO{GroupId: m.GroupId, User: userRefDTO{Id: m.UserId, Name: m.Name, Email: m.Email, Username: m.Username}, CreatedAt: m.CreatedAt}
}

type integrationDTO struct {
	Id         string     `json:"id"`
	ProjectId  string     `json:"projectId"`
	Name       string     `json:"name"`
	Scopes     []string   `json:"scopes"`
	Status     string     `json:"status"`
	TokenHint  string     `json:"tokenHint"`
	CreatedAt  time.Time  `json:"createdAt"`
	LastUsedAt *time.Time `json:"lastUsedAt"`
	RevokedAt  *time.Time `json:"revokedAt"`
	// Version is the If-Match value for PATCH.
	Version int64 `json:"version"`
}

func toIntegration(i integrations.Integration) integrationDTO {
	scopes := i.Scopes
	if scopes == nil {
		scopes = []string{}
	}
	return integrationDTO{Id: i.Id, ProjectId: i.ProjectId, Name: i.Name, Scopes: scopes, Status: i.Status, TokenHint: i.TokenHint,
		CreatedAt: i.CreatedAt, LastUsedAt: i.LastUsedAt, RevokedAt: i.RevokedAt, Version: i.Version}
}

type integrationCreatedDTO struct {
	integrationDTO
	Token string `json:"token"`
}

type scopeDTO struct {
	Name        string `json:"name"`
	Staff       bool   `json:"staff" doc:"A token holding this scope acts as support staff (a bot under its own name): staff ticket view, groups and agents, tickets created as staff."`
	Description string `json:"description"`
}

// scopeDescriptions describes each integration scope (one list for every
// token; owner decision 2026-10-31).
var scopeDescriptions = map[string]string{
	access.ScopeTicketsRead:             "Read tickets and their public messages (with a staff scope also groups, agents and canned replies)",
	access.ScopeTicketsCreate:           "Create tickets for a requester contact (with a staff scope the token writes the first message as staff)",
	access.ScopeTicketsReplyAsRequester: "Post public replies as the ticket's requester (like a customer reply: reopens, owes a response)",
	access.ScopeTicketsComment:          "Post public replies as staff under the integration's name (a staff reply; meets the first response target)",
	access.ScopeTicketsInternal:         "Read and write internal notes",
	access.ScopeTicketsWrite:            "Change status, priority, tags, group and assignee",
	access.ScopeTicketsAll:              "All ticket scopes except replying as the requester",
	access.ScopeContactsRead:            "Read contacts",
	access.ScopeContactsWrite:           "Create and update contacts",
	access.ScopeTagsRead:                "List the tag catalog",
	access.ScopeTagsWrite:               "Create and delete catalog tags (deleting removes the tag from every ticket)",
	access.ScopeHoldReasonsRead:         "List hold reasons (the reasons a ticket can be put on hold for)",
	access.ScopeHoldReasonsWrite:        "Create, change and delete hold reasons (a reason in use by a ticket on hold cannot be deleted)",
	access.ScopeOrganisationsRead:       "List and read organisations (the customers an integration serves)",
	access.ScopeOrganisationsWrite:      "Create, change and delete organisations (an organisation in use by a contact or ticket cannot be deleted)",
}

type contactDTO struct {
	Id             string    `json:"id"`
	ProjectId      string    `json:"projectId"`
	Name           string    `json:"name"`
	Email          string    `json:"email"`
	ExternalId     *string   `json:"externalId"`
	OrganisationId *string   `json:"organisationId" doc:"The organisation the integration linked the contact to; null when none."`
	CreatedAt      time.Time `json:"createdAt"`
	UpdatedAt      time.Time `json:"updatedAt"`
	Version        int64     `json:"version"`
}

func toContact(c customers.Contact) contactDTO {
	out := contactDTO{Id: c.Id, ProjectId: c.ProjectId, Name: c.Name, Email: c.Email, ExternalId: nullable(c.ExternalId),
		OrganisationId: nullable(c.OrganisationId), CreatedAt: c.CreatedAt, UpdatedAt: c.UpdatedAt, Version: c.Version}
	return out
}

type statusDTO struct {
	Id       string `json:"id"`
	Name     string `json:"name"`
	Category string `json:"category"`
	Builtin  bool   `json:"builtin"`
}

func toStatus(s statuses.Status) statusDTO {
	return statusDTO{Id: s.Id, Name: s.Name, Category: string(s.Category), Builtin: s.Builtin}
}

type requestTypeDTO struct {
	Id   string `json:"id"`
	Name string `json:"name"`
}

func toRequestType(t statuses.RequestType) requestTypeDTO {
	return requestTypeDTO{Id: t.Id, Name: t.Name}
}

// Settings (administrator view; secrets redacted).

type settingsProductDTO struct {
	Name        string `json:"name"`
	Description string `json:"description" doc:"Optional description of this installation (up to 1000 characters)."`
	Signature   string `json:"signature" doc:"Optional Markdown workspace signature (up to 1000 characters) the server appends to staff public replies."`
	BaseURL     string `json:"baseUrl"`
}

// settingsProductRequest is the product section of PUT /settings.
type settingsProductRequest struct {
	Name        string  `json:"name"`
	Description *string `json:"description" doc:"Optional; absent keeps the stored description."`
	Signature   *string `json:"signature" doc:"Optional; absent keeps the stored signature, empty clears it."`
	BaseURL     string  `json:"baseUrl"`
}

type settingsAuthDTO struct {
	AccessTokenTTLSeconds int64 `json:"accessTokenTtlSeconds"`
	RefreshTokenTTLHours  int64 `json:"refreshTokenTtlHours"`
	RequireAdminMFA       bool  `json:"requireAdminMfa" doc:"Administrators without TOTP MFA must enrol before any other API use. Enable before internet-facing use."`
}

// settingsAuthRequest is settingsAuthDTO with an optional MFA policy
// (absent keeps the stored value).
type settingsAuthRequest struct {
	AccessTokenTTLSeconds int64 `json:"accessTokenTtlSeconds"`
	RefreshTokenTTLHours  int64 `json:"refreshTokenTtlHours"`
	RequireAdminMFA       *bool `json:"requireAdminMfa" doc:"Absent keeps the stored value."`
}

type settingsFilesDTO struct {
	MaxAttachmentBytes       int64 `json:"maxAttachmentBytes"`
	MaxAttachmentsPerMessage int64 `json:"maxAttachmentsPerMessage"`
	MaxMessageTotalBytes     int64 `json:"maxMessageTotalBytes"`
	MaxMessageCodePoints     int64 `json:"maxMessageCodePoints"`
}

type settingsSMTPDTO struct {
	Enabled            bool   `json:"enabled"`
	Host               string `json:"host"`
	Port               int64  `json:"port"`
	Username           string `json:"username"`
	PasswordConfigured bool   `json:"passwordConfigured"`
	FromAddress        string `json:"fromAddress"`
	FromName           string `json:"fromName"`
	TLSMode            string `json:"tlsMode"`
	CACertificate      string `json:"caCertificate" doc:"PEM bundle trusted in addition to the system roots; empty when none."`
	TLSSkipVerify      bool   `json:"tlsSkipVerify" doc:"Server certificate verification is skipped (not recommended)."`
}

type settingsDTO struct {
	ConfigVersion int64                    `json:"configVersion"`
	UpdatedAt     time.Time                `json:"updatedAt" doc:"When the settings were last changed."`
	Product       settingsProductDTO       `json:"product"`
	Auth          settingsAuthDTO          `json:"auth"`
	Files         settingsFilesDTO         `json:"files"`
	SMTP          settingsSMTPDTO          `json:"smtp"`
	Notifications settingsNotificationsDTO `json:"notifications"`
}

type settingsNotificationsDTO struct {
	RequesterLinkBaseURL string `json:"requesterLinkBaseUrl" doc:"Absolute http(s) base URL of the system that submits tickets. Requester emails link to {requesterLinkBaseUrl}/{ticket number} and never include message content or a link to this application. Empty: no link; the email asks the requester to check the system they used."`
	RequesterEmails      bool   `json:"requesterEmails" doc:"Email contacts and CC addresses (default true). When false, no email goes to contacts or CC addresses (ticket received, replies, status notices, follow-up reminders; each is recorded as a skipped delivery with class requesterEmailsOff); staff emails are unchanged. Use webhooks to notify requesters instead."`
}

func toSettings(r settings.Redacted) settingsDTO {
	return settingsDTO{
		ConfigVersion: r.Version, UpdatedAt: r.UpdatedAt,
		Product: settingsProductDTO{Name: r.Product.Name, Description: r.Product.Description, Signature: r.Product.Signature, BaseURL: r.Product.BaseURL},
		Auth: settingsAuthDTO{AccessTokenTTLSeconds: r.Auth.AccessTokenTTLSeconds, RefreshTokenTTLHours: r.Auth.RefreshTokenTTLHours,
			RequireAdminMFA: r.Auth.RequireAdminMFA},
		Files: settingsFilesDTO{MaxAttachmentBytes: r.Files.MaxAttachmentBytes, MaxAttachmentsPerMessage: r.Files.MaxAttachmentsPerMessage,
			MaxMessageTotalBytes: r.Files.MaxMessageTotalBytes, MaxMessageCodePoints: r.Files.MaxMessageCodePoints},
		SMTP: settingsSMTPDTO{Enabled: r.SMTP.Enabled, Host: r.SMTP.Host, Port: r.SMTP.Port, Username: r.SMTP.Username,
			PasswordConfigured: r.SMTP.PasswordConfigured, FromAddress: r.SMTP.FromAddress, FromName: r.SMTP.FromName, TLSMode: r.SMTP.TLSMode,
			CACertificate: r.SMTP.CACertificate, TLSSkipVerify: r.SMTP.TLSSkipVerify},
		Notifications: settingsNotificationsDTO{RequesterLinkBaseURL: r.Notifications.RequesterLinkBaseURL, RequesterEmails: r.Notifications.RequesterEmailsEnabled()},
	}
}
