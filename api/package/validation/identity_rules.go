package validation

import "regexp"

// Account security: MFA, password reset and invitations.
var (
	// AuthMFACode is a 6-digit TOTP code.
	AuthMFACode = define(Rule{Field: "auth.mfaCode", Kind: KindText, Required: true, MinLen: 6, MaxLen: 6,
		Pattern: regexp.MustCompile(`^[0-9]{6}$`), Message: "Enter the 6-digit code from your authenticator app."})
	// AuthRecoveryCode is a one-time recovery code (xxxx-xxxx-xxxx, base32,
	// case-insensitive; the dashes are optional).
	AuthRecoveryCode = define(Rule{Field: "auth.recoveryCode", Kind: KindText, Required: true, MinLen: 12, MaxLen: 14,
		Pattern: regexp.MustCompile(`^[A-Za-z2-7]{4}-?[A-Za-z2-7]{4}-?[A-Za-z2-7]{4}$`), Message: "Enter one of your recovery codes."})
	// AuthOpaqueToken is a 256-bit base64url token (MFA challenge, password
	// reset or invitation token).
	AuthOpaqueToken = define(Rule{Field: "auth.opaqueToken", Kind: KindText, Required: true, MinLen: 43, MaxLen: 43,
		Pattern: regexp.MustCompile(`^[A-Za-z0-9_-]{43}$`), Message: "This link or token is not valid."})
)
