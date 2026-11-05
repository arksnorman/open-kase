package runtime

import (
	"context"
	"encoding/base32"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"

	"github.com/arksnorman/open-kase/api/internal/audit"
	"github.com/arksnorman/open-kase/api/internal/auth"
	"github.com/arksnorman/open-kase/api/internal/mailer"
	"github.com/arksnorman/open-kase/api/internal/settings"
	"github.com/arksnorman/open-kase/api/package/clock"
)

// identityFixture is a harness whose authentication service runs on a
// manual clock and whose email goes to a fake mailer.
type identityFixture struct {
	*harness
	clk  *clock.Manual
	mail *mailer.Fake
}

func newIdentityFixture(t *testing.T) *identityFixture {
	t.Helper()
	h := newHarness(t)
	f := &identityFixture{harness: h, clk: clock.NewManual(time.Now().UTC().Truncate(time.Second)), mail: &mailer.Fake{}}
	f.c.Mailers = func(mailer.Config) (mailer.Mailer, error) { return f.mail, nil }
	f.c.JobBackoff = func(int) time.Duration { return time.Millisecond }
	f.settings(func(s *settings.Settings) {
		s.Product.BaseURL = "https://support.example.com"
		s.SMTP = settings.SMTPSettings{Enabled: true, Host: "smtp.example.com", Port: 587, FromAddress: "support@example.com",
			FromName: "Support", TLSMode: "starttls"}
	})
	f.c.UseAuthClock(f.clk)
	f.h = f.router(f.c)
	return f
}

func (f *identityFixture) settings(mutate func(*settings.Settings)) {
	f.t.Helper()
	snap := f.c.Settings.Current()
	if snap == nil {
		var err error
		if snap, err = f.c.Settings.Refresh(context.Background()); err != nil {
			f.t.Fatal(err)
		}
	}
	if _, err := f.c.Settings.Update(context.Background(), settings.Update{Actor: audit.System(audit.SystemCLI, "test"), ExpectedVersion: snap.Version(),
		Mutate: mutate}); err != nil {
		f.t.Fatal(err)
	}
}

// runJobs runs every due job.
func (f *identityFixture) runJobs() {
	f.t.Helper()
	r := f.c.newRunner("test-worker", 1)
	for i := 0; i < 100; i++ {
		ran, err := r.RunOnce(context.Background())
		if err != nil {
			f.t.Fatal(err)
		}
		if !ran {
			return
		}
	}
}

var tokenInLink = regexp.MustCompile(`token=([A-Za-z0-9_-]{43})`)

// linkToken returns the token of the newest email sent to addr.
func (f *identityFixture) linkToken(addr string) string {
	f.t.Helper()
	f.runJobs()
	msgs := sentTo(f.mail, addr)
	if len(msgs) == 0 {
		f.t.Fatalf("no email sent to %s", addr)
	}
	m := tokenInLink.FindStringSubmatch(msgs[len(msgs)-1].Text)
	if m == nil {
		f.t.Fatalf("no token link in email: %s", msgs[len(msgs)-1].Text)
	}
	return m[1]
}

func (f *identityFixture) post(path string, body any, opts ...reqOpt) result {
	f.t.Helper()
	return f.do("POST", path, body, append([]reqOpt{header("Origin", testOrigin)}, opts...)...)
}

// enrol enables TOTP for s and returns the secret and recovery codes.
func (f *identityFixture) enrol(s session, password string) ([]byte, []string) {
	f.t.Helper()
	r := f.post("/api/v1/me/mfa/totp/enrol", map[string]any{"password": password}, bearer(s.Access)).expect(200)
	if !strings.HasPrefix(r.Str("otpauthUri"), "otpauth://totp/") {
		f.t.Fatalf("otpauth uri: %s", r.Body)
	}
	secret, err := base32.StdEncoding.WithPadding(base32.NoPadding).DecodeString(r.Str("secret"))
	if err != nil {
		f.t.Fatal(err)
	}
	f.post("/api/v1/me/mfa/totp/confirm", map[string]any{"code": "000000"}, bearer(s.Access)).expect(422)
	c := f.post("/api/v1/me/mfa/totp/confirm", map[string]any{"code": f.code(secret)}, bearer(s.Access)).expect(200)
	raw, _ := c.JSON()["recoveryCodes"].([]any)
	codes := make([]string, 0, len(raw))
	for _, x := range raw {
		codes = append(codes, x.(string))
	}
	if len(codes) != auth.RecoveryCodeCount {
		f.t.Fatalf("recovery codes: %v", codes)
	}
	return secret, codes
}

func (f *identityFixture) code(secret []byte) string {
	return auth.TOTPCode(secret, auth.TOTPStep(f.clk.Now()))
}

// passwordStep signs in with a password and returns the MFA token.
func (f *identityFixture) passwordStep(email, password string) string {
	f.t.Helper()
	r := f.post("/api/v1/auth/login", map[string]string{"login": email, "password": password}).expect(200)
	if r.JSON()["mfaRequired"] != true || r.Str("mfaToken") == "" || r.JSON()["accessToken"] != nil || len(r.Cookies) != 0 {
		f.t.Fatalf("expected an MFA challenge without a session: %s %v", r.Body, r.Cookies)
	}
	return r.Str("mfaToken")
}

func TestMFATwoStepLoginRecoveryCodesAndThrottle(t *testing.T) {
	f := newIdentityFixture(t)
	admin := f.admin()
	other := f.admin() // a second session: enrolment revokes it
	secret, codes := f.enrol(admin, adminPassword)
	f.do("GET", "/api/v1/me", nil, bearer(admin.Access)).expect(200)
	if f.do("GET", "/api/v1/me", nil, bearer(admin.Access)).JSON()["mfaEnabled"] != true {
		t.Fatal("mfaEnabled not reported")
	}
	f.do("GET", "/api/v1/me", nil, bearer(other.Access)).expect(401, "sessionRevoked")

	// Two-step login: a code of the step used at enrolment is a replay.
	tok := f.passwordStep(adminEmail, adminPassword)
	f.post("/api/v1/auth/mfa", map[string]any{"mfaToken": tok, "code": f.code(secret)}).expect(401, "invalidMfaCode")
	f.clk.Advance(auth.TOTPPeriod)
	code := f.code(secret)
	r := f.post("/api/v1/auth/mfa", map[string]any{"mfaToken": tok, "code": code}).expect(200)
	if r.Str("accessToken") == "" || r.Cookies["kase_refresh"] == nil {
		t.Fatalf("no session after MFA: %s", r.Body)
	}
	f.do("GET", "/api/v1/project", nil, bearer(r.Str("accessToken"))).expect(200)
	// The challenge is single use, and the same code cannot be replayed.
	f.post("/api/v1/auth/mfa", map[string]any{"mfaToken": tok, "code": code}).expect(401, "mfaChallengeInvalid")
	f.post("/api/v1/auth/mfa", map[string]any{"mfaToken": f.passwordStep(adminEmail, adminPassword), "code": code}).expect(401, "invalidMfaCode")

	// A recovery code works once.
	f.post("/api/v1/auth/mfa", map[string]any{"mfaToken": f.passwordStep(adminEmail, adminPassword), "recoveryCode": strings.ToUpper(codes[0])}).expect(200)
	f.post("/api/v1/auth/mfa", map[string]any{"mfaToken": f.passwordStep(adminEmail, adminPassword), "recoveryCode": codes[0]}).expect(401, "invalidMfaCode")
	st := f.do("GET", "/api/v1/me/mfa", nil, bearer(admin.Access))
	if st.Code == 401 { // admin session survives; use a fresh one if not
		t.Fatalf("status: %s", st.Body)
	}
	if n := st.JSON()["recoveryCodesRemaining"]; n != float64(auth.RecoveryCodeCount-1) {
		t.Fatalf("recovery codes remaining %v", n)
	}
	// An expired challenge is refused.
	exp := f.passwordStep(adminEmail, adminPassword)
	f.clk.Advance(auth.MFAChallengeTTL + time.Second)
	f.post("/api/v1/auth/mfa", map[string]any{"mfaToken": exp, "code": f.code(secret)}).expect(401, "mfaChallengeInvalid")

	// A successful sign-in resets the failure counter.
	f.clk.Advance(auth.TOTPPeriod)
	f.post("/api/v1/auth/mfa", map[string]any{"mfaToken": f.passwordStep(adminEmail, adminPassword), "code": f.code(secret)}).expect(200)

	// Wrong codes are throttled: five failures block the account's MFA step.
	tok = f.passwordStep(adminEmail, adminPassword)
	for range 5 {
		f.post("/api/v1/auth/mfa", map[string]any{"mfaToken": tok, "code": "000000"}).expect(401, "invalidMfaCode")
	}
	f.post("/api/v1/auth/mfa", map[string]any{"mfaToken": tok, "code": "000000"}).expect(401, "mfaChallengeInvalid")
	f.clk.Advance(auth.TOTPPeriod)
	blocked := f.post("/api/v1/auth/mfa", map[string]any{"mfaToken": f.passwordStep(adminEmail, adminPassword), "code": f.code(secret)})
	blocked.expect(429, "mfaThrottled")
	if blocked.Header.Get("Retry-After") == "" {
		t.Fatal("no Retry-After")
	}

	// Step-up shares the sign-in guessing budget: while that is exhausted, even a right code is refused.
	f.clk.Advance(auth.TOTPPeriod)
	f.post("/api/v1/me/mfa/recovery-codes", map[string]any{"password": adminPassword, "code": f.code(secret)}, bearer(admin.Access)).expect(429, "mfaThrottled")
	// The throttle runs on the system clock; drop its records instead of waiting 15 minutes.
	if _, err := f.coll("loginThrottle").DeleteMany(context.Background(), map[string]any{}); err != nil {
		t.Fatal(err)
	}

	// Regenerating and disabling need the password and a current second factor.
	f.post("/api/v1/me/mfa/recovery-codes", map[string]any{"password": "wrong password!"}, bearer(admin.Access)).expect(422)
	f.post("/api/v1/me/mfa/recovery-codes", map[string]any{"password": adminPassword}, bearer(admin.Access)).expect(422, "validationFailed")
	f.post("/api/v1/me/mfa/recovery-codes", map[string]any{"password": adminPassword, "code": "000000"}, bearer(admin.Access)).expect(422, "validationFailed")
	f.post("/api/v1/me/mfa/disable", map[string]any{"password": adminPassword}, bearer(admin.Access)).expect(422, "validationFailed")
	f.post("/api/v1/me/mfa/disable", map[string]any{"password": adminPassword, "code": "000000"}, bearer(admin.Access)).expect(422, "validationFailed")
	f.post("/api/v1/me/mfa/disable", map[string]any{"password": "wrong password!", "code": f.code(secret)}, bearer(admin.Access)).expect(422)
	f.post("/api/v1/me/mfa/disable", map[string]any{"password": adminPassword, "recoveryCode": "aaaa-bbbb-cccc"}, bearer(admin.Access)).expect(422, "validationFailed")
	f.clk.Advance(auth.TOTPPeriod)
	code = f.code(secret)
	regen := f.post("/api/v1/me/mfa/recovery-codes", map[string]any{"password": adminPassword, "code": code}, bearer(admin.Access)).expect(200)
	newCodes, _ := regen.JSON()["recoveryCodes"].([]any)
	if len(newCodes) == 0 {
		t.Fatalf("no new recovery codes: %s", regen.Body)
	}
	// The same TOTP step cannot be replayed, and a replaced recovery code is dead.
	f.post("/api/v1/me/mfa/disable", map[string]any{"password": adminPassword, "code": code}, bearer(admin.Access)).expect(422, "validationFailed")
	f.post("/api/v1/me/mfa/disable", map[string]any{"password": adminPassword, "recoveryCode": codes[1]}, bearer(admin.Access)).expect(422, "validationFailed")
	// An unused recovery code is an accepted second factor.
	f.post("/api/v1/me/mfa/disable", map[string]any{"password": adminPassword, "recoveryCode": newCodes[0].(string)}, bearer(admin.Access)).expect(204)
	r = f.post("/api/v1/auth/login", map[string]string{"login": adminEmail, "password": adminPassword}).expect(200)
	if r.JSON()["mfaRequired"] != false || r.Str("accessToken") == "" {
		t.Fatalf("login after disable: %s", r.Body)
	}
	assertAudit(t, f.harness, "user.mfaEnabled", "user.mfaDisabled", "user.mfaRecoveryCodeUsed", "user.mfaRecoveryCodesRegenerated", "auth.mfaChallenged")
}

func assertAudit(t *testing.T, h *harness, actions ...string) {
	t.Helper()
	evs, err := h.c.Audit.RecentAudit(context.Background(), 5000)
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	for _, e := range evs {
		seen[e.Action] = true
	}
	for _, a := range actions {
		if !seen[a] {
			t.Errorf("no audit event %s", a)
		}
	}
}

func TestRequireAdminMFAForcesEnrolment(t *testing.T) {
	f := newIdentityFixture(t)
	f.createUser(f.admin(), "agent@example.com", "Agent")
	f.settings(func(s *settings.Settings) { s.Auth.RequireAdminMFA = true })

	r := f.post("/api/v1/auth/login", map[string]string{"login": adminEmail, "password": adminPassword}).expect(200)
	if r.JSON()["mfaEnrolmentRequired"] != true {
		t.Fatalf("enrolment not required: %s", r.Body)
	}
	tok := r.Str("accessToken")
	f.do("GET", "/api/v1/project", nil, bearer(tok)).expect(403, "mfaEnrolmentRequired")
	f.do("GET", "/api/v1/users", nil, bearer(tok)).expect(403, "mfaEnrolmentRequired")
	f.do("POST", "/mcp", map[string]any{"jsonrpc": "2.0", "id": 1, "method": "tools/list"}, bearer(tok)).expect(403, "mfaEnrolmentRequired")
	f.do("GET", "/api/v1/me", nil, bearer(tok)).expect(200)
	if f.do("GET", "/api/v1/me/mfa", nil, bearer(tok)).expect(200).JSON()["enrolmentRequired"] != true {
		t.Fatal("status does not report enrolmentRequired")
	}
	// Non-administrators are not affected.
	agent := f.login("agent@example.com", "agent password 1")
	f.do("GET", "/api/v1/project", nil, bearer(agent.Access)).expect(200)

	s := session{Access: tok}
	f.enrol(s, adminPassword)
	f.do("GET", "/api/v1/project", nil, bearer(tok)).expect(200)
	f.post("/api/v1/me/mfa/disable", map[string]any{"password": adminPassword}, bearer(tok)).expect(409, "mfaRequiredByPolicy")
	settingsDTO := f.do("GET", "/api/v1/settings", nil, bearer(tok)).expect(200).JSON()
	if obj(settingsDTO, "auth")["requireAdminMfa"] != true {
		t.Fatalf("settings: %v", settingsDTO)
	}
}

func TestPasswordResetSingleUseExpiryAndEnumeration(t *testing.T) {
	f := newIdentityFixture(t)
	admin := f.admin()
	f.createUser(admin, "dana@example.com", "Dana Okafor")
	dana := f.login("dana@example.com", "agent password 1")

	known := f.post("/api/v1/auth/password-reset/request", map[string]any{"email": "Dana@Example.com"}).expect(202)
	unknown := f.post("/api/v1/auth/password-reset/request", map[string]any{"email": "nobody@example.com"}).expect(202)
	if string(known.Body) != string(unknown.Body) {
		t.Fatalf("responses differ: %s vs %s", known.Body, unknown.Body)
	}
	tok := f.linkToken("dana@example.com")
	if msgs := sentTo(f.mail, "dana@example.com"); !strings.Contains(msgs[0].Text, "https://support.example.com/reset-password?token=") || msgs[0].HTML == "" {
		t.Fatalf("email: %+v", msgs[0])
	}
	if len(sentTo(f.mail, "nobody@example.com")) != 0 {
		t.Fatal("email sent to unknown address")
	}
	f.post("/api/v1/auth/password-reset/complete", map[string]any{"token": tok, "password": "short"}).expect(422)
	f.post("/api/v1/auth/password-reset/complete", map[string]any{"token": tok, "password": "a brand new password"}).expect(204)
	// All sessions revoked; the token is single use; the new password works.
	f.do("GET", "/api/v1/me", nil, bearer(dana.Access)).expect(401, "sessionRevoked")
	f.refresh(dana).expect(401)
	f.post("/api/v1/auth/password-reset/complete", map[string]any{"token": tok, "password": "another new password"}).expect(422, "validationFailed")
	f.login("dana@example.com", "a brand new password")

	// Expiry after 30 minutes.
	f.post("/api/v1/auth/password-reset/request", map[string]any{"email": "dana@example.com"}).expect(202)
	tok2 := f.linkToken("dana@example.com")
	f.clk.Advance(auth.PasswordResetTTL + time.Second)
	f.post("/api/v1/auth/password-reset/complete", map[string]any{"token": tok2, "password": "a third new password"}).expect(422)
	f.post("/api/v1/auth/password-reset/complete", map[string]any{"token": strings.Repeat("A", 43), "password": "a third new password"}).expect(422)

	// Throttled identically for known and unknown addresses (3 per hour).
	f.post("/api/v1/auth/password-reset/request", map[string]any{"email": "dana@example.com"}).expect(202)
	f.post("/api/v1/auth/password-reset/request", map[string]any{"email": "dana@example.com"}).expect(429, "resetThrottled")
	f.post("/api/v1/auth/password-reset/request", map[string]any{"email": "nobody@example.com"}).expect(202)
	f.post("/api/v1/auth/password-reset/request", map[string]any{"email": "nobody@example.com"}).expect(202)
	f.post("/api/v1/auth/password-reset/request", map[string]any{"email": "nobody@example.com"}).expect(429, "resetThrottled")
	assertAudit(t, f.harness, "auth.passwordResetRequested", "auth.passwordResetCompleted")
}

func TestInvitationAcceptanceSingleUseExpiryRevoke(t *testing.T) {
	f := newIdentityFixture(t)
	admin := f.admin()
	r := f.do("POST", "/api/v1/users", map[string]any{"email": "ines@example.com", "givenName": "Ines", "familyName": "Duarte", "sendInvitation": true}, bearer(admin.Access)).expect(201)
	if r.Str("status") != "invited" {
		t.Fatalf("status %s", r.Body)
	}
	uid := r.Str("id")
	f.do("POST", "/api/v1/users", map[string]any{"email": "x@example.com", "givenName": "X", "familyName": "Y", "sendInvitation": true, "password": "a long password 1"},
		bearer(admin.Access)).expect(422)
	f.do("GET", "/api/v1/users/"+uid+"/invitation", nil, bearer(admin.Access)).expect(200)
	tok := f.linkToken("ines@example.com")
	msg := sentTo(f.mail, "ines@example.com")[0]
	if !strings.Contains(msg.Text, "/accept-invitation?token=") || !strings.Contains(msg.Text, "Ada Admin") {
		t.Fatalf("invitation email: %s", msg.Text)
	}
	f.post("/api/v1/auth/login", map[string]string{"login": "ines@example.com", "password": "her chosen password"}).expect(401, "invalidCredentials")
	f.post("/api/v1/auth/invitations/accept", map[string]any{"token": tok, "givenName": "Inès", "familyName": "Duarte"}).expect(422)
	f.post("/api/v1/auth/invitations/accept", map[string]any{"token": tok, "givenName": "Inès", "password": "her chosen password"}).expect(422)
	f.post("/api/v1/auth/invitations/accept", map[string]any{"token": tok, "givenName": "Inès", "middleName": "M.", "familyName": "Duarte", "password": "her chosen password"}).expect(204)
	f.post("/api/v1/auth/invitations/accept", map[string]any{"token": tok, "password": "her chosen password"}).expect(422)
	s := f.login("ines@example.com", "her chosen password")
	if me := f.do("GET", "/api/v1/me", nil, bearer(s.Access)).JSON(); me["name"] != "Inès M. Duarte" || me["middleName"] != "M." {
		t.Fatalf("me: %v", me)
	}
	f.do("POST", "/api/v1/users/"+uid+"/invitation", nil, bearer(admin.Access)).expect(409, "userNotInvited")

	// Expiry after 72 hours.
	uid2 := f.do("POST", "/api/v1/users", map[string]any{"email": "late@example.com", "givenName": "Late", "familyName": "User", "sendInvitation": true}, bearer(admin.Access)).expect(201).Str("id")
	late := f.linkToken("late@example.com")
	f.clk.Advance(auth.InvitationTTL + time.Second)
	f.post("/api/v1/auth/invitations/accept", map[string]any{"token": late, "password": "late password 123"}).expect(422)
	admin = f.admin() // the earlier access token expired meanwhile
	// Resend issues a new link and revokes older ones.
	f.do("POST", "/api/v1/users/"+uid2+"/invitation", nil, bearer(admin.Access)).expect(202)
	fresh := f.linkToken("late@example.com")
	// Revoke invalidates it.
	f.do("DELETE", "/api/v1/users/"+uid2+"/invitation", nil, bearer(admin.Access)).expect(204)
	f.do("DELETE", "/api/v1/users/"+uid2+"/invitation", nil, bearer(admin.Access)).expect(404, "invitationNotFound")
	f.post("/api/v1/auth/invitations/accept", map[string]any{"token": fresh, "password": "late password 123"}).expect(422)
	assertAudit(t, f.harness, "user.invitationSent", "user.invitationAccepted", "user.invitationRevoked")
}

func TestJWTKeyRotationKeepsOldTokensUntilExpiry(t *testing.T) {
	f := newIdentityFixture(t)
	old := f.admin()
	ws := f.c.Settings.Current().WorkspaceID()
	ttl := f.c.authPolicy().AccessTTL
	rot, err := f.c.SigningKeys.Rotate(context.Background(), ws, auth.RotationOverlap(ttl), audit.Operator(audit.SystemCLI, "test"))
	if err != nil || rot.OldKeyId == "" {
		t.Fatalf("rotate: %+v %v", rot, err)
	}
	f.do("GET", "/api/v1/me", nil, bearer(old.Access)).expect(200)
	fresh := f.admin()
	if kidOf(t, fresh.Access) != rot.NewKeyId || kidOf(t, old.Access) != rot.OldKeyId {
		t.Fatal("tokens not signed with the expected keys")
	}
	// A separate process (fresh container) also verifies the old token.
	other := f.container()
	other.UseAuthClock(f.clk)
	h2 := f.router(other)
	f.doWith(h2, "GET", "/api/v1/me", nil, bearer(old.Access)).expect(200)
	f.clk.Advance(ttl - time.Minute)
	f.do("GET", "/api/v1/me", nil, bearer(old.Access)).expect(200)
	f.clk.Advance(2 * time.Minute)
	f.do("GET", "/api/v1/me", nil, bearer(old.Access)).expect(401, "tokenExpired")
	// Tokens signed by the new key keep working.
	f.do("GET", "/api/v1/me", nil, bearer(f.admin().Access)).expect(200)
	assertAudit(t, f.harness, "signingKey.rotated")
}

func kidOf(t *testing.T, raw string) string {
	t.Helper()
	tk, _, err := jwt.NewParser().ParseUnverified(raw, &auth.AccessClaims{})
	if err != nil {
		t.Fatal(err)
	}
	kid, _ := tk.Header["kid"].(string)
	return kid
}
