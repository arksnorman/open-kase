package mongo

import (
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"

	"github.com/arksnorman/open-kase/api/internal/audit"
	"github.com/arksnorman/open-kase/api/internal/settings"
)

// BSON document shapes. Domain types never carry BSON tags.

type workspaceDoc struct {
	Id               bson.ObjectID  `bson:"_id"`
	InstallKey       string         `bson:"installKey"`
	Name             string         `bson:"name"`
	KeyCheck         string         `bson:"keyCheck"`
	BootstrapAdminId *bson.ObjectID `bson:"bootstrapAdminId"`
	CreatedAt        time.Time      `bson:"createdAt"`
	UpdatedAt        time.Time      `bson:"updatedAt"`
}

type actorDoc struct {
	Type  string `bson:"type"`
	Id    string `bson:"id,omitempty"`
	Label string `bson:"label,omitempty"`
	// Scopes are the integration token scopes that allowed the action.
	Scopes []string `bson:"scopes,omitempty"`
}

type auditDoc struct {
	Id          bson.ObjectID  `bson:"_id"`
	WorkspaceId bson.ObjectID  `bson:"workspaceId"`
	ProjectId   *bson.ObjectID `bson:"projectId"`
	TicketId    *bson.ObjectID `bson:"ticketId"`
	Action      string         `bson:"action"`
	Actor       actorDoc       `bson:"actor"`
	RequestId   string         `bson:"requestId,omitempty"`
	Target      string         `bson:"target,omitempty"`
	Changes     []string       `bson:"changes,omitempty"`
	Reason      string         `bson:"reason,omitempty"`
	Outcome     string         `bson:"outcome"`
	CreatedAt   time.Time      `bson:"createdAt"`
}

func optionalID(hex string) *bson.ObjectID {
	if hex == "" {
		return nil
	}
	id, err := bson.ObjectIDFromHex(hex)
	if err != nil {
		return nil
	}
	return &id
}

func toAuditDoc(workspaceID bson.ObjectID, e audit.Event) auditDoc {
	outcome := e.Outcome
	if outcome == "" {
		outcome = "success"
	}
	return auditDoc{
		Id: bson.NewObjectID(), WorkspaceId: workspaceID,
		ProjectId: optionalID(e.ProjectId), TicketId: optionalID(e.TicketId),
		Action: string(e.Action), Actor: actorDoc{Type: e.Actor.Type, Id: e.Actor.Id, Label: e.Actor.Label, Scopes: e.Actor.Scopes},
		RequestId: e.RequestId, Target: e.Target, Changes: e.Changes, Reason: e.Reason,
		Outcome: outcome, CreatedAt: e.CreatedAt,
	}
}

type settingsDataDoc struct {
	Product struct {
		Name        string `bson:"name"`
		Description string `bson:"description,omitempty"`
		Signature   string `bson:"signature,omitempty"`
		BaseURL     string `bson:"baseUrl"`
	} `bson:"product"`
	Auth struct {
		AccessTokenTTLSeconds int64 `bson:"accessTokenTtlSeconds"`
		RefreshTokenTTLHours  int64 `bson:"refreshTokenTtlHours"`
		RequireAdminMFA       bool  `bson:"requireAdminMfa,omitempty"`
	} `bson:"auth"`
	Files struct {
		MaxAttachmentBytes       int64 `bson:"maxAttachmentBytes"`
		MaxAttachmentsPerMessage int64 `bson:"maxAttachmentsPerMessage"`
		MaxMessageTotalBytes     int64 `bson:"maxMessageTotalBytes"`
		MaxMessageCodePoints     int64 `bson:"maxMessageCodePoints"`
	} `bson:"files"`
	SMTP struct {
		Enabled            bool   `bson:"enabled"`
		Host               string `bson:"host"`
		Port               int64  `bson:"port"`
		Username           string `bson:"username"`
		PasswordCiphertext string `bson:"passwordCiphertext"`
		FromAddress        string `bson:"fromAddress"`
		FromName           string `bson:"fromName"`
		TLSMode            string `bson:"tlsMode"`
		CACertificate      string `bson:"caCertificate,omitempty"`
		TLSSkipVerify      bool   `bson:"tlsSkipVerify,omitempty"`
	} `bson:"smtp"`
	Notifications struct {
		RequesterLinkBaseURL string `bson:"requesterLinkBaseUrl,omitempty"`
	} `bson:"notifications,omitempty"`
}

type settingsDoc struct {
	Id            bson.ObjectID   `bson:"_id"`
	WorkspaceId   bson.ObjectID   `bson:"workspaceId"`
	Scope         string          `bson:"scope"`
	ConfigVersion int64           `bson:"configVersion"`
	Data          settingsDataDoc `bson:"data"`
	UpdatedAt     time.Time       `bson:"updatedAt"`
	UpdatedBy     actorDoc        `bson:"updatedBy"`
}

func toSettingsData(s settings.Settings) settingsDataDoc {
	var d settingsDataDoc
	d.Product.Name, d.Product.Description, d.Product.BaseURL = s.Product.Name, s.Product.Description, s.Product.BaseURL
	d.Product.Signature = s.Product.Signature
	d.Auth.AccessTokenTTLSeconds, d.Auth.RefreshTokenTTLHours = s.Auth.AccessTokenTTLSeconds, s.Auth.RefreshTokenTTLHours
	d.Auth.RequireAdminMFA = s.Auth.RequireAdminMFA
	d.Files.MaxAttachmentBytes = s.Files.MaxAttachmentBytes
	d.Files.MaxAttachmentsPerMessage = s.Files.MaxAttachmentsPerMessage
	d.Files.MaxMessageTotalBytes = s.Files.MaxMessageTotalBytes
	d.Files.MaxMessageCodePoints = s.Files.MaxMessageCodePoints
	d.SMTP.Enabled, d.SMTP.Host, d.SMTP.Port = s.SMTP.Enabled, s.SMTP.Host, s.SMTP.Port
	d.SMTP.Username, d.SMTP.PasswordCiphertext = s.SMTP.Username, s.SMTP.PasswordCiphertext
	d.SMTP.FromAddress, d.SMTP.FromName, d.SMTP.TLSMode = s.SMTP.FromAddress, s.SMTP.FromName, s.SMTP.TLSMode
	d.SMTP.CACertificate, d.SMTP.TLSSkipVerify = s.SMTP.CACertificate, s.SMTP.TLSSkipVerify
	d.Notifications.RequesterLinkBaseURL = s.Notifications.RequesterLinkBaseURL
	return d
}

func (d settingsDataDoc) toDomain() settings.Settings {
	var s settings.Settings
	s.Product = settings.ProductSettings{Name: d.Product.Name, Description: d.Product.Description, Signature: d.Product.Signature, BaseURL: d.Product.BaseURL}
	s.Auth = settings.AuthSettings{AccessTokenTTLSeconds: d.Auth.AccessTokenTTLSeconds, RefreshTokenTTLHours: d.Auth.RefreshTokenTTLHours,
		RequireAdminMFA: d.Auth.RequireAdminMFA}
	s.Files = settings.FileSettings{
		MaxAttachmentBytes: d.Files.MaxAttachmentBytes, MaxAttachmentsPerMessage: d.Files.MaxAttachmentsPerMessage,
		MaxMessageTotalBytes: d.Files.MaxMessageTotalBytes, MaxMessageCodePoints: d.Files.MaxMessageCodePoints,
	}
	s.SMTP = settings.SMTPSettings{
		Enabled: d.SMTP.Enabled, Host: d.SMTP.Host, Port: d.SMTP.Port, Username: d.SMTP.Username,
		PasswordCiphertext: d.SMTP.PasswordCiphertext, FromAddress: d.SMTP.FromAddress,
		FromName: d.SMTP.FromName, TLSMode: d.SMTP.TLSMode,
		CACertificate: d.SMTP.CACertificate, TLSSkipVerify: d.SMTP.TLSSkipVerify,
	}
	s.Notifications.RequesterLinkBaseURL = d.Notifications.RequesterLinkBaseURL
	return s
}
