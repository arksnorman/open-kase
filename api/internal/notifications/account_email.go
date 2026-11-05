package notifications

import (
	"context"
	"errors"
	"net/url"
	"strings"
	"time"

	"github.com/arksnorman/open-kase/api/internal/jobs"
	"github.com/arksnorman/open-kase/api/internal/mailer"
	"github.com/arksnorman/open-kase/api/internal/settings"
	"github.com/arksnorman/open-kase/api/templates"
)

// KindAccountEmail delivers password-reset and invitation emails.
const KindAccountEmail = "email.account"

// ReasonAccount is the delivery reason of account emails.
const ReasonAccount = "account"

// Account token purposes (auth.PurposePasswordReset, auth.PurposeInvitation).
const (
	accountPurposeReset      = "passwordReset"
	accountPurposeInvitation = "invitation"
)

// AccountTokenInfo is what the account email worker needs about a token.
type AccountTokenInfo struct {
	Purpose   string
	Valid     bool // unused, unrevoked and unexpired at the time of the check
	ExpiresAt time.Time
	UserEmail string
	UserName  string
	// InvitedBy is the display name of the administrator who sent an
	// invitation (empty for resets).
	InvitedBy string
}

// Accounts reads account tokens for the email worker.
type Accounts interface {
	AccountTokenInfo(ctx context.Context, tokenID string, now time.Time) (AccountTokenInfo, error)
}

// AccountTokenContext must equal auth.AccountTokenContext (the sealed
// token is bound to its token ID).
func accountTokenContext(tokenID string) string { return "auth.accountToken:" + tokenID }

// HandleAccount is the jobs.Handler for KindAccountEmail. The plain token
// exists only sealed in the job payload; the worker re-checks that the
// token is still valid immediately before sending.
func (e *Email) HandleAccount(ctx context.Context, j jobs.Job, _ jobs.Lease) jobs.Result {
	return e.run(ctx, j, e.buildAccount)
}

func (e *Email) buildAccount(ctx context.Context, j jobs.Job, d Delivery, snap *settings.Snapshot) (mailer.Message, error) {
	if e.d.Accounts == nil {
		return mailer.Message{}, skipped("accountsUnavailable")
	}
	tokenID := j.Payload["tokenId"]
	info, err := e.d.Accounts.AccountTokenInfo(ctx, tokenID, e.d.Now())
	if errors.Is(err, ErrNotFound) {
		return mailer.Message{}, skipped("tokenMissing")
	}
	if err != nil {
		return mailer.Message{}, err
	}
	if !info.Valid {
		return mailer.Message{}, skipped("tokenNoLongerValid")
	}
	product := snap.Product()
	if product.BaseURL == "" {
		return mailer.Message{}, skipped("baseUrlNotConfigured")
	}
	plain, err := e.d.Keys.Open(j.Payload["tokenSealed"], accountTokenContext(tokenID))
	if err != nil {
		return mailer.Message{}, err
	}
	base := strings.TrimRight(product.BaseURL, "/")
	common := templates.Common{ProductName: product.Name, RecipientName: templates.Line(info.UserName)}
	var data templates.Data
	switch info.Purpose {
	case accountPurposeReset:
		data = templates.PasswordResetData{Common: common, URL: base + "/reset-password?token=" + url.QueryEscape(string(plain)),
			ExpiresAt: info.ExpiresAt}
	case accountPurposeInvitation:
		data = templates.InvitationData{Common: common, URL: base + "/accept-invitation?token=" + url.QueryEscape(string(plain)),
			InvitedBy: info.InvitedBy, Email: info.UserEmail, ExpiresAt: info.ExpiresAt}
	default:
		return mailer.Message{}, skipped("unknownPurpose")
	}
	r, err := e.d.Renderer.Render(data)
	if err != nil {
		return mailer.Message{}, err
	}
	return mailer.Message{To: mailer.Address{Name: common.RecipientName, Email: info.UserEmail}, Subject: r.Subject, Text: r.Text, HTML: r.HTML,
		MessageId: messageID("account."+d.Id, snap.SMTP().FromAddress),
		Headers:   map[string]string{"X-Open-Kase-Template": r.TemplateId}}, nil
}
