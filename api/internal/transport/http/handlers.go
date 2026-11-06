package http

import (
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/arksnorman/open-kase/api/internal/access"
	"github.com/arksnorman/open-kase/api/internal/customers"
	"github.com/arksnorman/open-kase/api/internal/integrations"
	"github.com/arksnorman/open-kase/api/internal/projects"
	"github.com/arksnorman/open-kase/api/internal/settings"
	"github.com/arksnorman/open-kase/api/internal/users"
	"github.com/arksnorman/open-kase/api/package/fault"
	"github.com/arksnorman/open-kase/api/package/validation"
)

// ---- Me ----

func (a *api) getMe(c *gin.Context) {
	u, err := a.Users.Me(c.Request.Context(), principal(c))
	respond(c, http.StatusOK, toUser(u), err)
}

// putPreferencesRequest is the request body of putPreferences.
type putPreferencesRequest struct {
	Theme            *string `json:"theme" enum:"system,light,dark"`
	Signature        *string `json:"signature" doc:"Markdown reply signature (up to 1000 characters); empty clears it. Appended by the server to your public replies."`
	EmailPreferences *struct {
		WatcherUpdates *bool `json:"watcherUpdates"`
		QueueUpdates   *bool `json:"queueUpdates"`
	} `json:"emailPreferences"`
}

func (a *api) putPreferences(c *gin.Context) {
	var body putPreferencesRequest
	if !decode(c, &body) {
		return
	}
	in := users.PreferencesInput{Theme: body.Theme, Signature: body.Signature}
	if ep := body.EmailPreferences; ep != nil {
		in.WatcherEmails, in.QueueEmails = ep.WatcherUpdates, ep.QueueUpdates
	}
	u, err := a.Users.SetPreferences(c.Request.Context(), principal(c), in)
	respond(c, http.StatusOK, toUser(u), err)
}

// patchMeRequest is the request body of patchMe.
type patchMeRequest struct {
	GivenName  *string `json:"givenName" doc:"First (given) name (rule user.givenName)."`
	MiddleName *string `json:"middleName" doc:"Middle name (rule user.middleName); empty clears it."`
	FamilyName *string `json:"familyName" doc:"Last (family) name (rule user.familyName)."`
}

func (a *api) patchMe(c *gin.Context) {
	var body patchMeRequest
	if !decode(c, &body) {
		return
	}
	version, ok := ifMatch(c)
	if !ok {
		return
	}
	u, err := a.Users.UpdateProfile(c.Request.Context(), principal(c), version, users.ProfileInput{Name: users.NamePatch{Given: body.GivenName, Middle: body.MiddleName, Family: body.FamilyName}})
	respond(c, http.StatusOK, toUser(u), err)
}

// changeMyPasswordRequest is the request body of changeMyPassword.
type changeMyPasswordRequest struct {
	CurrentPassword string `json:"currentPassword" required:"true"`
	NewPassword     string `json:"newPassword" required:"true"`
}

func (a *api) changeMyPassword(c *gin.Context) {
	var body changeMyPasswordRequest
	if !decode(c, &body) {
		return
	}
	err := a.Users.ChangePassword(c.Request.Context(), principal(c), body.CurrentPassword, body.NewPassword)
	respond(c, http.StatusNoContent, nil, err)
}

// ---- Users (administrators) ----

func (a *api) listUsers(c *gin.Context) {
	after, limit, ok := pageParams(c)
	if !ok {
		return
	}
	p, err := a.Users.List(c.Request.Context(), principal(c), after, limit)
	respond(c, http.StatusOK, page(p, toUserAdmin), err)
}

// createUserRequest is the request body of createUser.
type createUserRequest struct {
	Email          string  `json:"email" required:"true"`
	Username       *string `json:"username" doc:"Sign-in name (3–32 lower-case letters, digits, '.', '_' or '-', starting with a letter; normalized to lower case). Omitted: derived from the email local part, with a numeric suffix when taken."`
	GivenName      string  `json:"givenName" required:"true" doc:"First (given) name (rule user.givenName)."`
	MiddleName     string  `json:"middleName" doc:"Optional middle name (rule user.middleName)."`
	FamilyName     string  `json:"familyName" required:"true" doc:"Last (family) name (rule user.familyName)."`
	Password       *string `json:"password"`
	IsAdmin        bool    `json:"isAdmin"`
	SendInvitation bool    `json:"sendInvitation" doc:"Without a password: email a single-use invitation link (valid 72 hours). Needs SMTP and product.baseUrl."`
}

func (a *api) createUser(c *gin.Context) {
	var body createUserRequest
	if !decode(c, &body) {
		return
	}
	if body.SendInvitation && body.Password != nil {
		writeError(c, fault.Field("sendInvitation", "invalidChoice", "Send either a password or an invitation, not both."))
		return
	}
	p := principal(c)
	u, err := a.Users.Create(c.Request.Context(), p, users.CreateInput{Email: body.Email, Username: body.Username,
		Name: users.PersonName{Given: body.GivenName, Middle: body.MiddleName, Family: body.FamilyName}, Password: body.Password, IsAdmin: body.IsAdmin})
	if err == nil && body.SendInvitation {
		// The user exists even if queuing the email fails; the
		// administrator can resend with POST /users/{u}/invitation.
		if _, ierr := a.Auth.SendInvitation(c.Request.Context(), p, u.Id); ierr != nil {
			a.log.Warn("invitation not queued for new user", "userId", u.Id, "error", ierr.Error())
		}
	}
	respond(c, http.StatusCreated, toUserAdmin(u), err)
}

func (a *api) getUser(c *gin.Context) {
	u, err := a.Users.Get(c.Request.Context(), principal(c), c.Param("u"))
	respond(c, http.StatusOK, toUserAdmin(u), err)
}

// patchUserRequest is the request body of patchUser.
type patchUserRequest struct {
	GivenName  *string `json:"givenName"`
	MiddleName *string `json:"middleName" doc:"Empty clears the middle name."`
	FamilyName *string `json:"familyName" doc:"Required in the resulting name: a user stored with a one-word name must be given a family name when the name is edited."`
	Username   *string `json:"username" doc:"New sign-in name (same rule as on create)."`
	IsAdmin    *bool   `json:"isAdmin"`
	Status     *string `json:"status" enum:"active,disabled"`
}

func (a *api) patchUser(c *gin.Context) {
	var body patchUserRequest
	if !decode(c, &body) {
		return
	}
	version, ok := ifMatch(c)
	if !ok {
		return
	}
	u, err := a.Users.Update(c.Request.Context(), principal(c), c.Param("u"), version,
		users.UpdateInput{Name: users.NamePatch{Given: body.GivenName, Middle: body.MiddleName, Family: body.FamilyName}, Username: body.Username, IsAdmin: body.IsAdmin, Status: body.Status})
	respond(c, http.StatusOK, toUserAdmin(u), err)
}

// setUserPasswordRequest is the request body of setUserPassword.
type setUserPasswordRequest struct {
	Password string `json:"password" required:"true"`
}

func (a *api) setUserPassword(c *gin.Context) {
	var body setUserPasswordRequest
	if !decode(c, &body) {
		return
	}
	err := a.Users.SetPassword(c.Request.Context(), principal(c), c.Param("u"), body.Password)
	respond(c, http.StatusNoContent, nil, err)
}

// ---- Settings (administrators) ----

func (a *api) getSettings(c *gin.Context) {
	if err := principal(c).RequireAdmin(); err != nil {
		writeError(c, err)
		return
	}
	snap := a.Settings.Current()
	if snap == nil {
		writeError(c, fault.New(fault.KindUnavailable, "settingsUnavailable", "Settings are not loaded yet."))
		return
	}
	c.JSON(http.StatusOK, toSettings(snap.Redacted()))
}

// putNotificationsRequest: every field is optional (absent keeps the stored value).
type putNotificationsRequest struct {
	RequesterLinkBaseURL *string `json:"requesterLinkBaseUrl" doc:"See the settings document. Absent keeps the stored value."`
	RequesterEmails      *bool   `json:"requesterEmails" doc:"Email contacts and CC addresses; absent keeps the stored value."`
}

// putSettingsRequest is the request body of putSettings.
type putSettingsRequest struct {
	Product settingsProductRequest `json:"product"`
	Auth    settingsAuthRequest    `json:"auth"`
	Files   settingsFilesDTO       `json:"files"`
	SMTP    struct {
		Enabled       bool    `json:"enabled"`
		Host          string  `json:"host"`
		Port          int64   `json:"port"`
		Username      string  `json:"username"`
		Password      *string `json:"password"`
		FromAddress   string  `json:"fromAddress"`
		FromName      string  `json:"fromName"`
		TLSMode       string  `json:"tlsMode"`
		CACertificate string  `json:"caCertificate" doc:"Optional PEM certificate bundle trusted in addition to the system roots, for a relay with a self-signed or private CA. Ignored when tlsMode is none."`
		TLSSkipVerify bool    `json:"tlsSkipVerify" doc:"Skip server certificate verification. Not recommended: anyone between this server and the relay can read or change mail and see the SMTP password. Ignored when tlsMode is none."`
	} `json:"smtp"`
	// Notifications is optional: absent keeps the stored requester link and email switch.
	Notifications *putNotificationsRequest `json:"notifications"`
	// UpdatedAt is read-only; accepted (and ignored) so a document read
	// with GET can be sent back.
	UpdatedAt *time.Time `json:"updatedAt" doc:"Read-only; ignored."`
}

func (a *api) putSettings(c *gin.Context) {
	p := principal(c)
	if err := p.RequireAdmin(); err != nil {
		writeError(c, err)
		return
	}
	var body putSettingsRequest
	if !decode(c, &body) {
		return
	}
	version, ok := ifMatch(c)
	if !ok {
		return
	}
	next := settings.Settings{
		Product: settings.ProductSettings{Name: body.Product.Name, BaseURL: body.Product.BaseURL},
		Auth:    settings.AuthSettings{AccessTokenTTLSeconds: body.Auth.AccessTokenTTLSeconds, RefreshTokenTTLHours: body.Auth.RefreshTokenTTLHours},
		Files: settings.FileSettings{MaxAttachmentBytes: body.Files.MaxAttachmentBytes, MaxAttachmentsPerMessage: body.Files.MaxAttachmentsPerMessage,
			MaxMessageTotalBytes: body.Files.MaxMessageTotalBytes, MaxMessageCodePoints: body.Files.MaxMessageCodePoints},
		SMTP: settings.SMTPSettings{Enabled: body.SMTP.Enabled, Host: body.SMTP.Host, Port: body.SMTP.Port, Username: body.SMTP.Username,
			FromAddress: body.SMTP.FromAddress, FromName: body.SMTP.FromName, TLSMode: body.SMTP.TLSMode,
			CACertificate: strings.TrimSpace(body.SMTP.CACertificate), TLSSkipVerify: body.SMTP.TLSSkipVerify},
	}
	snap, err := a.Settings.Update(c.Request.Context(), settings.Update{Actor: p.AuditActor(), RequestId: RequestIDFrom(c),
		ExpectedVersion: version, SMTPPassword: body.SMTP.Password, Mutate: func(s *settings.Settings) {
			mfa, notif, desc, sig := s.Auth.RequireAdminMFA, s.Notifications, s.Product.Description, s.Product.Signature
			*s = next
			s.Notifications = notif
			s.Product.Description = desc
			s.Product.Signature = sig
			if body.Product.Signature != nil {
				s.Product.Signature = strings.TrimSpace(*body.Product.Signature)
			}
			if body.Product.Description != nil {
				s.Product.Description = strings.TrimSpace(*body.Product.Description)
			}
			if body.Notifications != nil {
				if body.Notifications.RequesterLinkBaseURL != nil {
					s.Notifications.RequesterLinkBaseURL = *body.Notifications.RequesterLinkBaseURL
				}
				if body.Notifications.RequesterEmails != nil {
					s.Notifications.RequesterEmailsOff = !*body.Notifications.RequesterEmails
				}
			}
			s.Auth.RequireAdminMFA = mfa
			if body.Auth.RequireAdminMFA != nil {
				s.Auth.RequireAdminMFA = *body.Auth.RequireAdminMFA
			}
		}})
	if err != nil {
		writeError(c, err)
		return
	}
	c.JSON(http.StatusOK, toSettings(snap.Redacted()))
}

// ---- Project ----

func (a *api) getProject(c *gin.Context) {
	s, ok := a.scope(c)
	if !ok {
		return
	}
	pr, err := a.Projects.GetProject(c.Request.Context(), s)
	respond(c, http.StatusOK, a.toProject(pr), err)
}

// patchProjectRequest is the request body of patchProject. The product name
// and description are settings (PUT /settings); name, description and key
// are rejected as unknown fields.
type patchProjectRequest struct {
	// RequesterAcknowledgement toggles the "ticket received" email.
	RequesterAcknowledgement *bool `json:"requesterAcknowledgement"`
}

func (a *api) patchProject(c *gin.Context) {
	s, ok := a.scope(c)
	if !ok {
		return
	}
	var body patchProjectRequest
	if !decode(c, &body) {
		return
	}
	version, ok := ifMatch(c)
	if !ok {
		return
	}
	pr, err := a.Projects.UpdateProject(c.Request.Context(), s, version, projects.UpdateProjectInput{
		RequesterAcknowledgement: body.RequesterAcknowledgement})
	respond(c, http.StatusOK, a.toProject(pr), err)
}

// ---- Groups ----

func (a *api) listGroups(c *gin.Context) {
	s, ok := a.scope(c)
	if !ok {
		return
	}
	after, limit, ok := pageParams(c)
	if !ok {
		return
	}
	p, err := a.Projects.ListGroups(c.Request.Context(), s, after, limit)
	respond(c, http.StatusOK, page(p, toGroup), err)
}

// createGroupRequest is the request body of createGroup.
type createGroupRequest struct {
	Name        string `json:"name" required:"true"`
	Description string `json:"description"`
}

func (a *api) createGroup(c *gin.Context) {
	s, ok := a.scope(c)
	if !ok {
		return
	}
	var body createGroupRequest
	if !decode(c, &body) {
		return
	}
	g, err := a.Projects.CreateGroup(c.Request.Context(), s, projects.CreateGroupInput{Name: body.Name, Description: body.Description})
	respond(c, http.StatusCreated, toGroup(g), err)
}

func (a *api) getGroup(c *gin.Context) {
	s, ok := a.scope(c)
	if !ok {
		return
	}
	g, err := a.Projects.GetGroup(c.Request.Context(), s, c.Param("g"))
	respond(c, http.StatusOK, toGroup(g), err)
}

// patchGroupRequest is the request body of patchGroup.
type patchGroupRequest struct {
	Name        *string `json:"name"`
	Description *string `json:"description"`
	Active      *bool   `json:"active"`
}

func (a *api) patchGroup(c *gin.Context) {
	s, ok := a.scope(c)
	if !ok {
		return
	}
	var body patchGroupRequest
	if !decode(c, &body) {
		return
	}
	version, ok := ifMatch(c)
	if !ok {
		return
	}
	g, err := a.Projects.UpdateGroup(c.Request.Context(), s, c.Param("g"), version, projects.UpdateGroupInput{Name: body.Name, Description: body.Description, Active: body.Active})
	respond(c, http.StatusOK, toGroup(g), err)
}

func (a *api) deleteGroup(c *gin.Context) {
	s, ok := a.scope(c)
	if !ok {
		return
	}
	respond(c, http.StatusNoContent, nil, a.Projects.DeleteGroup(c.Request.Context(), s, c.Param("g")))
}

func (a *api) listGroupMembers(c *gin.Context) {
	s, ok := a.scope(c)
	if !ok {
		return
	}
	after, limit, ok := pageParams(c)
	if !ok {
		return
	}
	p, err := a.Projects.ListGroupMembers(c.Request.Context(), s, c.Param("g"), after, limit)
	respond(c, http.StatusOK, page(p, toGroupMember), err)
}

func (a *api) putGroupMember(c *gin.Context) {
	s, ok := a.scope(c)
	if !ok {
		return
	}
	gm, err := a.Projects.AddGroupMember(c.Request.Context(), s, c.Param("g"), c.Param("u"))
	respond(c, http.StatusOK, toGroupMember(gm), err)
}

func (a *api) deleteGroupMember(c *gin.Context) {
	s, ok := a.scope(c)
	if !ok {
		return
	}
	respond(c, http.StatusNoContent, nil, a.Projects.RemoveGroupMember(c.Request.Context(), s, c.Param("g"), c.Param("u")))
}

// ---- A user's groups (admin user page) ----

func (a *api) getUserGroups(c *gin.Context) {
	s, ok := a.scope(c)
	if !ok {
		return
	}
	if _, err := a.Users.Get(c.Request.Context(), principal(c), c.Param("u")); err != nil {
		writeError(c, err)
		return
	}
	gs, err := a.Projects.UserGroups(c.Request.Context(), s, c.Param("u"))
	respond(c, http.StatusOK, toUserGroups(gs), err)
}

// putUserGroupsRequest is the request body of putUserGroups.
type putUserGroupsRequest struct {
	GroupIds []string `json:"groupIds" required:"true" doc:"Every active group the user should belong to (at most 200); memberships of other active groups are removed."`
}

func (a *api) putUserGroups(c *gin.Context) {
	var body putUserGroupsRequest
	if !decode(c, &body) {
		return
	}
	s, ok := a.scope(c)
	if !ok {
		return
	}
	// Authorize (admins only, via the user lookup) before validating.
	if _, err := a.Users.Get(c.Request.Context(), principal(c), c.Param("u")); err != nil {
		writeError(c, err)
		return
	}
	if body.GroupIds == nil {
		writeError(c, fault.Field("groupIds", validation.CodeRequired, "Send the list of group IDs (empty removes every group)."))
		return
	}
	gs, err := a.Projects.SetUserGroups(c.Request.Context(), s, c.Param("u"), body.GroupIds)
	respond(c, http.StatusOK, toUserGroups(gs), err)
}

// ---- Project members ----

// splitIDs splits a comma-separated ID list (nil when empty).
func splitIDs(raw string) []string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil
	}
	parts := strings.Split(raw, ",")
	for i := range parts {
		parts[i] = strings.TrimSpace(parts[i])
	}
	return parts
}

func (a *api) listMembers(c *gin.Context) {
	s, ok := a.scope(c)
	if !ok {
		return
	}
	after, limit, ok := pageParams(c)
	if !ok {
		return
	}
	f := projects.MemberFilter{Q: c.Query("q"), GroupId: c.Query("groupId"), UserIds: splitIDs(c.Query("userId")), ActiveOnly: c.Query("active") == "true"}
	p, err := a.Projects.ListMembers(c.Request.Context(), s, f, after, limit)
	respond(c, http.StatusOK, page(p, toMember), err)
}

// ---- Integrations ----

func (a *api) listIntegrationScopes(c *gin.Context) {
	// Documented for signed-in users only (catalogue for the admin pages).
	if err := principal(c).RequireUser(); err != nil {
		writeError(c, err)
		return
	}
	out := []scopeDTO{}
	for _, s := range access.Scopes() {
		out = append(out, scopeDTO{Name: s, Staff: access.StaffScopes([]string{s}), Description: scopeDescriptions[s]})
	}
	c.JSON(http.StatusOK, itemsOf(out))
}

func (a *api) listIntegrations(c *gin.Context) {
	s, ok := a.scope(c)
	if !ok {
		return
	}
	after, limit, ok := pageParams(c)
	if !ok {
		return
	}
	p, err := a.Integrations.List(c.Request.Context(), s, after, limit)
	respond(c, http.StatusOK, page(p, toIntegration), err)
}

// createIntegrationRequest is the request body of createIntegration.
type createIntegrationRequest struct {
	Name   string   `json:"name" required:"true"`
	Scopes []string `json:"scopes" required:"true" enum:"tickets:read,tickets:create,tickets:replyAsRequester,tickets:comment,tickets:internal,tickets:write,tickets:all,contacts:read,contacts:write,tags:read,tags:write,holdReasons:read,holdReasons:write,organisations:read,organisations:write" doc:"Any combination of scopes (GET /integration-scopes describes each). tickets:comment, tickets:internal, tickets:write or tickets:all make the token act as support staff under its name; without them it acts for the requester side and sees public content only."`
}

func (a *api) createIntegration(c *gin.Context) {
	s, ok := a.scope(c)
	if !ok {
		return
	}
	var body createIntegrationRequest
	if !decode(c, &body) {
		return
	}
	created, err := a.Integrations.Create(c.Request.Context(), s, integrations.CreateInput{Name: body.Name, Scopes: body.Scopes})
	if err != nil {
		writeError(c, err)
		return
	}
	// The token is returned exactly once and must never be replayed from
	// an idempotency record.
	c.Header("Cache-Control", "no-store")
	c.Set(ctxNoReplayStore, true)
	c.JSON(http.StatusCreated, integrationCreatedDTO{integrationDTO: toIntegration(created.Integration), Token: created.Token})
}

// patchIntegrationRequest is the request body of patchIntegration.
type patchIntegrationRequest struct {
	Name *string `json:"name" required:"true" doc:"New name (rule integration.name). Scopes cannot be changed."`
}

func (a *api) patchIntegration(c *gin.Context) {
	s, ok := a.scope(c)
	if !ok {
		return
	}
	var body patchIntegrationRequest
	if !decode(c, &body) {
		return
	}
	version, ok := ifMatch(c)
	if !ok {
		return
	}
	ig, err := a.Integrations.Update(c.Request.Context(), s, c.Param("i"), version, integrations.UpdateInput{Name: body.Name})
	respond(c, http.StatusOK, toIntegration(ig), err)
}

func (a *api) deleteIntegration(c *gin.Context) {
	s, ok := a.scope(c)
	if !ok {
		return
	}
	respond(c, http.StatusNoContent, nil, a.Integrations.Revoke(c.Request.Context(), s, c.Param("i")))
}

// ---- Contacts ----

func (a *api) listContacts(c *gin.Context) {
	s, ok := a.scope(c)
	if !ok {
		return
	}
	after, limit, ok := pageParams(c)
	if !ok {
		return
	}
	p, err := a.Customers.ListContacts(c.Request.Context(), s, c.Query("q"), c.Query("externalId"), after, limit)
	respond(c, http.StatusOK, page(p, toContact), err)
}

// upsertContactRequest is the request body of upsertContact.
type upsertContactRequest struct {
	Name           string  `json:"name" required:"true"`
	Email          string  `json:"email" required:"true"`
	ExternalId     *string `json:"externalId"`
	OrganisationId *string `json:"organisationId" doc:"Optional: link the contact to an organisation (from POST /organisations). Omit to leave the link unchanged; an empty string clears it."`
}

func (a *api) upsertContact(c *gin.Context) {
	s, ok := a.scope(c)
	if !ok {
		return
	}
	var body upsertContactRequest
	if !decode(c, &body) {
		return
	}
	res, err := a.Customers.UpsertContact(c.Request.Context(), s, customers.ContactInput{Name: body.Name, Email: body.Email,
		ExternalId: body.ExternalId, OrganisationId: body.OrganisationId})
	status := http.StatusOK
	if res.Created {
		status = http.StatusCreated
	}
	respond(c, status, toContact(res.Contact), err)
}

func (a *api) getContact(c *gin.Context) {
	s, ok := a.scope(c)
	if !ok {
		return
	}
	ct, err := a.Customers.GetContact(c.Request.Context(), s, c.Param("c"))
	respond(c, http.StatusOK, toContact(ct), err)
}

// ---- Workflow configuration ----

func (a *api) listStatuses(c *gin.Context) {
	s, ok := a.scope(c)
	if !ok {
		return
	}
	rows, err := a.Workflow.Statuses(c.Request.Context(), s)
	respond(c, http.StatusOK, items(rows, toStatus), err)
}

func (a *api) listRequestTypes(c *gin.Context) {
	s, ok := a.scope(c)
	if !ok {
		return
	}
	rows, err := a.Workflow.RequestTypes(c.Request.Context(), s)
	respond(c, http.StatusOK, items(rows, toRequestType), err)
}
