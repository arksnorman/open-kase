package http

import (
	"net/http"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/arksnorman/open-kase/api/internal/auth"
	"github.com/arksnorman/open-kase/api/internal/users"
	"github.com/arksnorman/open-kase/api/package/fault"
)

// registerIdentity adds MFA, password reset and invitation routes. The
// public ones live under /auth; the signed-in ones under /me and /users.
func registerIdentity(authg, p *gin.RouterGroup, a *api) {
	authg.POST("/mfa", a.completeMFA)
	authg.POST("/password-reset/request", a.requestPasswordReset)
	authg.POST("/password-reset/complete", a.completePasswordReset)
	authg.POST("/invitations/accept", a.acceptInvitation)

	p.GET("/me/mfa", a.getMyMFA)
	p.POST("/me/mfa/totp/enrol", a.beginTOTPEnrolment)
	p.POST("/me/mfa/totp/confirm", a.confirmTOTPEnrolment)
	p.POST("/me/mfa/recovery-codes", a.regenerateRecoveryCodes)
	p.POST("/me/mfa/disable", a.disableMyMFA)

	p.GET("/users/:u/invitation", a.getInvitation)
	p.POST("/users/:u/invitation", a.sendInvitation)
	p.DELETE("/users/:u/invitation", a.revokeInvitation)
	p.DELETE("/users/:u/mfa", a.resetUserMFA)
}

// mfaEnrolmentRoutes are the only authenticated routes an administrator
// who must enrol MFA (auth.requireAdminMfa) may call.
var mfaEnrolmentRoutes = map[string]bool{
	"/api/v1/me":                  true,
	"/api/v1/me/mfa":              true,
	"/api/v1/me/mfa/totp/enrol":   true,
	"/api/v1/me/mfa/totp/confirm": true,
}

// publicForm applies the login protections to unauthenticated POSTs:
// same-origin Origin when present, no cross-site fetch metadata and a JSON
// body only.
func publicForm(a *api, c *gin.Context) bool {
	for _, check := range []func() error{
		func() error { return a.checkOrigin(c, false) },
		func() error { return rejectCrossSite(c) },
		func() error { return requireJSON(c) },
	} {
		if err := check(); err != nil {
			writeError(c, err)
			return false
		}
	}
	return true
}

// mfaRequest is the request body of completeMFA.
type mfaRequest struct {
	MFAToken     string `json:"mfaToken" required:"true" doc:"The mfaToken returned by login."`
	Code         string `json:"code" doc:"6-digit TOTP code (send this or recoveryCode)."`
	RecoveryCode string `json:"recoveryCode" doc:"One-time recovery code such as abcd-efgh-ijkl."`
}

func (a *api) completeMFA(c *gin.Context) {
	if !publicForm(a, c) {
		return
	}
	var body mfaRequest
	if !decode(c, &body) {
		return
	}
	iss, err := a.Auth.CompleteMFA(c.Request.Context(), auth.MFAInput{Token: body.MFAToken, Code: body.Code, RecoveryCode: body.RecoveryCode,
		UserAgent: c.Request.UserAgent(), ClientIP: throttleIP(c)})
	if err != nil {
		writeError(c, err)
		return
	}
	a.setSessionCookies(c, iss)
	c.JSON(http.StatusOK, toAuth(iss))
}

// passwordResetRequest is the request body of requestPasswordReset.
type passwordResetRequest struct {
	Email string `json:"email" required:"true"`
}

// acceptedDTO is the uniform response of enumeration-resistant requests.
type acceptedDTO struct {
	Status string `json:"status" enum:"accepted"`
}

func (a *api) requestPasswordReset(c *gin.Context) {
	if !publicForm(a, c) {
		return
	}
	var body passwordResetRequest
	if !decode(c, &body) {
		return
	}
	err := a.Auth.RequestPasswordReset(c.Request.Context(), body.Email, throttleIP(c))
	respond(c, http.StatusAccepted, acceptedDTO{Status: "accepted"}, err)
}

// passwordResetCompleteRequest is the request body of completePasswordReset.
type passwordResetCompleteRequest struct {
	Token    string `json:"token" required:"true" doc:"The token from the emailed link."`
	Password string `json:"password" required:"true"`
}

func (a *api) completePasswordReset(c *gin.Context) {
	if !publicForm(a, c) {
		return
	}
	var body passwordResetCompleteRequest
	if !decode(c, &body) {
		return
	}
	err := a.Auth.CompletePasswordReset(c.Request.Context(), body.Token, body.Password)
	respond(c, http.StatusNoContent, nil, err)
}

// acceptInvitationRequest is the request body of acceptInvitation.
type acceptInvitationRequest struct {
	Token      string  `json:"token" required:"true" doc:"The token from the invitation link."`
	GivenName  *string `json:"givenName" doc:"Optional corrected name: when any name part is sent, givenName and familyName are required and replace the stored name."`
	MiddleName *string `json:"middleName"`
	FamilyName *string `json:"familyName"`
	Password   string  `json:"password" required:"true"`
}

func (a *api) acceptInvitation(c *gin.Context) {
	if !publicForm(a, c) {
		return
	}
	var body acceptInvitationRequest
	if !decode(c, &body) {
		return
	}
	err := a.Auth.AcceptInvitation(c.Request.Context(), body.Token, users.NamePatch{Given: body.GivenName, Middle: body.MiddleName, Family: body.FamilyName}, body.Password)
	respond(c, http.StatusNoContent, nil, err)
}

// ---- Own MFA ----

// mfaStatusDTO is the caller's MFA state.
type mfaStatusDTO struct {
	Enabled               bool       `json:"enabled"`
	EnabledAt             *time.Time `json:"enabledAt"`
	RecoveryCodesLeft     int        `json:"recoveryCodesRemaining"`
	Required              bool       `json:"required" doc:"The installation requires MFA for this user (administrators when auth.requireAdminMfa is on)."`
	EnrolmentRequired     bool       `json:"enrolmentRequired" doc:"Required but not enabled: other API calls are refused until enrolment is confirmed."`
	PendingEnrolment      bool       `json:"pendingEnrolment"`
	PendingEnrolmentUntil *time.Time `json:"pendingEnrolmentExpiresAt"`
}

func (a *api) getMyMFA(c *gin.Context) {
	st, err := a.Auth.Status(c.Request.Context(), principal(c))
	respond(c, http.StatusOK, mfaStatusDTO{Enabled: st.Enabled, EnabledAt: st.EnabledAt, RecoveryCodesLeft: st.RecoveryCodesLeft,
		Required: st.Required, EnrolmentRequired: st.EnrolmentRequired, PendingEnrolment: st.PendingEnrolment,
		PendingEnrolmentUntil: st.PendingExpiresAt}, err)
}

// passwordConfirmRequest re-authenticates a sensitive account change.
type passwordConfirmRequest struct {
	Password string `json:"password" required:"true" doc:"The caller's current password."`
}

// mfaStepUpRequest re-authenticates disabling MFA or regenerating recovery codes.
type mfaStepUpRequest struct {
	Password     string `json:"password" required:"true" doc:"The caller's current password."`
	Code         string `json:"code" doc:"Current 6-digit TOTP code (send this or recoveryCode)."`
	RecoveryCode string `json:"recoveryCode" doc:"An unused recovery code such as abcd-efgh-ijkl; it is consumed."`
}

// totpEnrolmentDTO is a new, unconfirmed TOTP secret (shown once).
type totpEnrolmentDTO struct {
	Secret     string    `json:"secret" doc:"Base32 secret for manual entry."`
	OTPAuthURI string    `json:"otpauthUri" doc:"otpauth:// URI to render as a QR code."`
	ExpiresAt  time.Time `json:"expiresAt" doc:"Confirm before this time."`
}

func (a *api) beginTOTPEnrolment(c *gin.Context) {
	var body passwordConfirmRequest
	if !decode(c, &body) {
		return
	}
	e, err := a.Auth.BeginEnrolment(c.Request.Context(), principal(c), body.Password)
	respond(c, http.StatusOK, totpEnrolmentDTO{Secret: e.Secret, OTPAuthURI: e.URI, ExpiresAt: e.ExpiresAt}, err)
}

// totpConfirmRequest is the request body of confirmTOTPEnrolment.
type totpConfirmRequest struct {
	Code string `json:"code" required:"true" doc:"Current 6-digit code from the new authenticator."`
}

// recoveryCodesDTO carries recovery codes (shown once).
type recoveryCodesDTO struct {
	RecoveryCodes []string `json:"recoveryCodes" doc:"Single-use codes; store them safely. They are not shown again."`
}

func (a *api) confirmTOTPEnrolment(c *gin.Context) {
	var body totpConfirmRequest
	if !decode(c, &body) {
		return
	}
	codes, err := a.Auth.ConfirmEnrolment(c.Request.Context(), principal(c), body.Code)
	respond(c, http.StatusOK, recoveryCodesDTO{RecoveryCodes: codes}, err)
}

func (a *api) regenerateRecoveryCodes(c *gin.Context) {
	var body mfaStepUpRequest
	if !decode(c, &body) {
		return
	}
	codes, err := a.Auth.RegenerateRecoveryCodes(c.Request.Context(), principal(c), body.Password, body.Code, body.RecoveryCode)
	respond(c, http.StatusOK, recoveryCodesDTO{RecoveryCodes: codes}, err)
}

func (a *api) disableMyMFA(c *gin.Context) {
	var body mfaStepUpRequest
	if !decode(c, &body) {
		return
	}
	err := a.Auth.DisableMFA(c.Request.Context(), principal(c), body.Password, body.Code, body.RecoveryCode)
	respond(c, http.StatusNoContent, nil, err)
}

// ---- Administrators: invitations and MFA reset ----

// invitationDTO is a pending invitation.
type invitationDTO struct {
	UserId     string    `json:"userId"`
	ExpiresAt  time.Time `json:"expiresAt"`
	DeliveryId string    `json:"deliveryId" doc:"Email delivery record (GET /notifications/deliveries/{d})."`
}

func toInvitation(inv auth.Invitation) invitationDTO {
	return invitationDTO{UserId: inv.UserId, ExpiresAt: inv.ExpiresAt, DeliveryId: inv.DeliveryId}
}

func (a *api) getInvitation(c *gin.Context) {
	inv, err := a.Auth.PendingInvitation(c.Request.Context(), principal(c), c.Param("u"))
	respond(c, http.StatusOK, toInvitation(inv), err)
}

func (a *api) sendInvitation(c *gin.Context) {
	inv, err := a.Auth.SendInvitation(c.Request.Context(), principal(c), c.Param("u"))
	respond(c, http.StatusAccepted, toInvitation(inv), err)
}

func (a *api) revokeInvitation(c *gin.Context) {
	err := a.Auth.RevokeInvitation(c.Request.Context(), principal(c), c.Param("u"))
	respond(c, http.StatusNoContent, nil, err)
}

func (a *api) resetUserMFA(c *gin.Context) {
	err := a.Auth.ResetUserMFA(c.Request.Context(), principal(c), c.Param("u"))
	respond(c, http.StatusNoContent, nil, err)
}

// errMFAEnrolmentRequired is returned for every other route while an
// administrator must enrol MFA.
func errMFAEnrolmentRequired() error {
	return fault.Forbidden("mfaEnrolmentRequired",
		"This installation requires administrators to use multi-factor authentication. Enrol an authenticator app to continue.")
}
