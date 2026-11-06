package http

import (
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/arksnorman/open-kase/api/internal/access"
	"github.com/arksnorman/open-kase/api/internal/notifications"
	"github.com/arksnorman/open-kase/api/internal/settings"
	"github.com/arksnorman/open-kase/api/internal/webhooks"
	"github.com/arksnorman/open-kase/api/internal/webhooks/endpoints"
	"github.com/arksnorman/open-kase/api/package/fault"
	"github.com/arksnorman/open-kase/api/package/pagination"
)

func registerNotifications(p *gin.RouterGroup, a *api) {
	p.GET("/settings/smtp", a.getSMTP)
	p.PUT("/settings/smtp", a.putSMTP)
	p.POST("/settings/smtp/test", a.testSMTP)

	p.GET("/deliveries", a.listDeliveries)
	p.GET("/deliveries/:d", a.getDelivery)
	p.POST("/deliveries/:d/retry", a.retryDelivery)

	p.GET("/webhook-event-types", a.listWebhookEventTypes)
	p.GET("/webhooks", a.listWebhooks)
	p.POST("/webhooks", a.idempotent(a.createWebhook))
	p.GET("/webhooks/:w", a.getWebhook)
	p.PATCH("/webhooks/:w", a.patchWebhook)
	p.DELETE("/webhooks/:w", a.deleteWebhook)
	p.POST("/webhooks/:w/test", a.testWebhook)
	p.GET("/webhooks/:w/deliveries", a.listWebhookDeliveries)
	p.GET("/webhooks/:w/deliveries/:d", a.getWebhookDelivery)
	p.POST("/webhooks/:w/deliveries/:d/retry", a.retryWebhookDelivery)
}

// ---- SMTP settings (administrators) ----

type smtpDTO struct {
	ConfigVersion      int64  `json:"configVersion"`
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

func toSMTP(r settings.Redacted) smtpDTO {
	return smtpDTO{ConfigVersion: r.Version, Enabled: r.SMTP.Enabled, Host: r.SMTP.Host, Port: r.SMTP.Port, Username: r.SMTP.Username,
		PasswordConfigured: r.SMTP.PasswordConfigured, FromAddress: r.SMTP.FromAddress, FromName: r.SMTP.FromName, TLSMode: r.SMTP.TLSMode,
		CACertificate: r.SMTP.CACertificate, TLSSkipVerify: r.SMTP.TLSSkipVerify}
}

func (a *api) getSMTP(c *gin.Context) {
	if err := principal(c).RequireAdmin(); err != nil {
		writeError(c, err)
		return
	}
	snap := a.Settings.Current()
	if snap == nil {
		writeError(c, fault.New(fault.KindUnavailable, "settingsUnavailable", "Settings are not loaded yet."))
		return
	}
	c.JSON(http.StatusOK, toSMTP(snap.Redacted()))
}

// putSMTPRequest is the request body of putSMTP.
type putSMTPRequest struct {
	Enabled       bool    `json:"enabled"`
	Host          string  `json:"host"`
	Port          int64   `json:"port"`
	Username      string  `json:"username"`
	Password      *string `json:"password"` // write-only: absent keeps, "" clears
	FromAddress   string  `json:"fromAddress"`
	FromName      string  `json:"fromName"`
	TLSMode       string  `json:"tlsMode"`
	CACertificate string  `json:"caCertificate" doc:"Optional PEM certificate bundle trusted in addition to the system roots, for a relay with a self-signed or private CA. Ignored when tlsMode is none."`
	TLSSkipVerify bool    `json:"tlsSkipVerify" doc:"Skip server certificate verification. Not recommended: anyone between this server and the relay can read or change mail and see the SMTP password. Ignored when tlsMode is none."`
}

func (a *api) putSMTP(c *gin.Context) {
	p := principal(c)
	if err := p.RequireAdmin(); err != nil {
		writeError(c, err)
		return
	}
	var body putSMTPRequest
	if !decode(c, &body) {
		return
	}
	version, ok := ifMatch(c)
	if !ok {
		return
	}
	snap, err := a.Settings.Update(c.Request.Context(), settings.Update{Actor: p.AuditActor(), RequestId: RequestIDFrom(c),
		ExpectedVersion: version, SMTPPassword: body.Password, Mutate: func(s *settings.Settings) {
			s.SMTP = settings.SMTPSettings{Enabled: body.Enabled, Host: body.Host, Port: body.Port, Username: body.Username,
				FromAddress: body.FromAddress, FromName: body.FromName, TLSMode: body.TLSMode,
				CACertificate: strings.TrimSpace(body.CACertificate), TLSSkipVerify: body.TLSSkipVerify}
		}})
	if err != nil {
		writeError(c, err)
		return
	}
	c.JSON(http.StatusOK, toSMTP(snap.Redacted()))
}

// testSMTPRequest is the request body of testSMTP.
type testSMTPRequest struct {
	To string `json:"to" required:"true"`
}

// deliveryEnvelopeDTO wraps a delivery created by a test send.
type deliveryEnvelopeDTO struct {
	Delivery deliveryDTO `json:"delivery"`
}

func (a *api) testSMTP(c *gin.Context) {
	var body testSMTPRequest
	if !decode(c, &body) {
		return
	}
	d, err := a.Notifications.SendTestEmail(c.Request.Context(), principal(c), body.To)
	respond(c, http.StatusAccepted, deliveryEnvelopeDTO{Delivery: toDelivery(d)}, err)
}

// ---- Deliveries ----

type deliveryRecipientDTO struct {
	Kind    string  `json:"kind"`
	Id      *string `json:"id"`
	Address *string `json:"address"`
	Name    *string `json:"name" doc:"Current display name of the user or contact recipient, or the webhook endpoint name (null when unknown or deleted)."`
	Email   *string `json:"email" doc:"Current email address of the user or contact recipient (address is the one used when sending)."`
	URL     *string `json:"url" doc:"The webhook endpoint's URL (webhook deliveries; null when the endpoint was deleted)."`
}

type deliveryDTO struct {
	Id              string               `json:"id"`
	Channel         string               `json:"channel"`
	EventId         *string              `json:"eventId"`
	EventType       string               `json:"eventType"`
	ProjectId       *string              `json:"projectId"`
	TicketId        *string              `json:"ticketId"`
	TicketNumber    *int64               `json:"ticketNumber" doc:"Number of the ticket the delivery concerns (null when the delivery has no ticket)."`
	EndpointId      *string              `json:"endpointId"`
	Recipient       deliveryRecipientDTO `json:"recipient"`
	Reason          string               `json:"reason"`
	Template        *string              `json:"template"`
	Status          string               `json:"status"`
	Attempts        int                  `json:"attempts"`
	NextAttemptAt   *time.Time           `json:"nextAttemptAt"`
	LastAttemptAt   *time.Time           `json:"lastAttemptAt"`
	ErrorClass      *string              `json:"errorClass"`
	Error           *string              `json:"error"`
	StatusCode      *int                 `json:"statusCode"`
	ResponseSnippet *string              `json:"responseSnippet"`
	ResponseMs      *int                 `json:"responseMs" doc:"How long the receiver took to answer the last attempt in milliseconds (webhooks; null without a response)."`
	MessageId       *string              `json:"messageId"`
	CreatedAt       time.Time            `json:"createdAt"`
	UpdatedAt       time.Time            `json:"updatedAt"`
	CompletedAt     *time.Time           `json:"completedAt"`
}

func toDelivery(d notifications.Delivery) deliveryDTO {
	out := deliveryDTO{Id: d.Id, Channel: d.Channel, EventId: nullable(d.EventId), EventType: d.EventType, ProjectId: nullable(d.ProjectId),
		TicketId: nullable(d.TicketId), EndpointId: nullable(d.EndpointId),
		Recipient: deliveryRecipientDTO{Kind: d.Recipient.Kind, Id: nullable(d.Recipient.Id), Address: nullable(d.Recipient.Address),
			Name: nullable(d.Recipient.Name), Email: nullable(d.Recipient.Email), URL: nullable(d.Recipient.URL)},
		Reason: d.Reason, Template: nullable(d.Template), Status: d.Status, Attempts: d.Attempts, NextAttemptAt: d.NextAttemptAt,
		LastAttemptAt: d.LastAttemptAt, ErrorClass: nullable(d.ErrorClass), Error: nullable(d.Error),
		ResponseSnippet: nullable(d.ResponseSnippet), MessageId: nullable(d.MessageId), CreatedAt: d.CreatedAt, UpdatedAt: d.UpdatedAt,
		CompletedAt: d.CompletedAt}
	if d.TicketNumber != 0 {
		n := d.TicketNumber
		out.TicketNumber = &n
	}
	if d.ResponseMs > 0 {
		ms := d.ResponseMs
		out.ResponseMs = &ms
	}
	if d.StatusCode != 0 {
		sc := d.StatusCode
		out.StatusCode = &sc
	}
	if d.Status != notifications.StatusRetrying && d.Status != notifications.StatusPending {
		out.NextAttemptAt = nil
	}
	return out
}

func deliveryQuery(c *gin.Context) (notifications.ListInput, bool) {
	after, limit, ok := pageParams(c)
	if !ok {
		return notifications.ListInput{}, false
	}
	return notifications.ListInput{Channel: c.Query("channel"), Status: c.Query("status"), EventId: c.Query("eventId"),
		After: after, Limit: limit}, true
}

func (a *api) listDeliveries(c *gin.Context) {
	in, ok := deliveryQuery(c)
	if !ok {
		return
	}
	p, err := a.Notifications.ListDeliveries(c.Request.Context(), principal(c), in)
	respond(c, http.StatusOK, page(p, toDelivery), err)
}

func (a *api) getDelivery(c *gin.Context) {
	d, err := a.Notifications.GetDelivery(c.Request.Context(), principal(c), c.Param("d"))
	respond(c, http.StatusOK, toDelivery(d), err)
}

func (a *api) retryDelivery(c *gin.Context) {
	d, err := a.Notifications.RetryDelivery(c.Request.Context(), principal(c), c.Param("d"))
	respond(c, http.StatusAccepted, toDelivery(d), err)
}

// ---- Webhook endpoints ----

// webhookHeaderDTO is a configured header; its value is never returned.
type webhookHeaderDTO struct {
	Name            string `json:"name"`
	ValueConfigured bool   `json:"valueConfigured" doc:"A value is stored (encrypted). Values are write-only."`
}

// webhookAuthDTO is the endpoint's authentication without its secrets.
type webhookAuthDTO struct {
	Type               string `json:"type" enum:"none,basic,token"`
	Username           string `json:"username" doc:"Basic auth user name (empty otherwise)."`
	PasswordConfigured bool   `json:"passwordConfigured"`
	TokenConfigured    bool   `json:"tokenConfigured"`
}

// webhookCertDTO summarizes a certificate.
type webhookCertDTO struct {
	Subject   string    `json:"subject"`
	Issuer    string    `json:"issuer"`
	NotBefore time.Time `json:"notBefore"`
	NotAfter  time.Time `json:"notAfter"`
}

// webhookTLSDTO shows the endpoint's certificate settings. Certificates
// are public; the client key is write-only.
type webhookTLSDTO struct {
	CACertificate         string           `json:"caCertificate" doc:"Extra trusted CA certificates (PEM), empty when none."`
	CACertificates        []webhookCertDTO `json:"caCertificates"`
	ClientCertificate     string           `json:"clientCertificate" doc:"Client certificate for mutual TLS (PEM), empty when none."`
	ClientCertificateInfo *webhookCertDTO  `json:"clientCertificateInfo"`
	ClientKeyConfigured   bool             `json:"clientKeyConfigured"`
}

// webhookStripDTO is one recent delivery of a webhook's health strip.
type webhookStripDTO struct {
	Status string    `json:"status" doc:"Delivery status: sent, retrying, pending or failed."`
	At     time.Time `json:"at" doc:"Time of the last attempt."`
}

// webhookStatsDTO summarizes a webhook's deliveries of the last 24 hours.
type webhookStatsDTO struct {
	SentLast24h           int               `json:"sentLast24h" doc:"Deliveries attempted at least once in the last 24 hours (skipped ones excluded)."`
	DeliveredFirstTryRate *float64          `json:"deliveredFirstTryRate" doc:"Share (0-1) of those delivered on the first attempt; null without deliveries."`
	Retrying              int               `json:"retrying" doc:"Deliveries waiting for another attempt."`
	MedianResponseMs      *int              `json:"medianResponseMs" doc:"Median receiver response time in milliseconds over 24 hours; null without data."`
	LastSentAt            *time.Time        `json:"lastSentAt" doc:"Last attempt in the last 24 hours."`
	LastDeliveries        []webhookStripDTO `json:"lastDeliveries" doc:"Up to 10 most recent deliveries within 24 hours, newest first."`
	PausedAt              *time.Time        `json:"pausedAt" doc:"When the webhook was paused; null while active."`
}

func toWebhookStats(e endpoints.Endpoint, s endpoints.Stats) webhookStatsDTO {
	out := webhookStatsDTO{SentLast24h: s.Sent, Retrying: s.Retrying, MedianResponseMs: s.MedianResponseMs, LastSentAt: s.LastSentAt,
		LastDeliveries: make([]webhookStripDTO, 0, len(s.Recent)), PausedAt: e.PausedAt}
	if s.Sent > 0 {
		r := float64(s.FirstTry) / float64(s.Sent)
		out.DeliveredFirstTryRate = &r
	}
	for _, it := range s.Recent {
		out.LastDeliveries = append(out.LastDeliveries, webhookStripDTO{Status: it.Status, At: it.At})
	}
	return out
}

// webhooksWithStats converts endpoints, adding the delivery statistics
// with one query for all of them.
func (a *api) webhooksWithStats(c *gin.Context, s *access.ProjectScope, es []endpoints.Endpoint) ([]webhookDTO, error) {
	stats, err := a.Webhooks.Stats(c.Request.Context(), s, es)
	if err != nil {
		// The statistics are secondary: answer with empty ones.
		a.log.Warn("webhook statistics failed", "error", err.Error())
		stats = nil
	}
	out := make([]webhookDTO, 0, len(es))
	for _, e := range es {
		d := toWebhook(e)
		d.Stats = toWebhookStats(e, stats[e.Id])
		out = append(out, d)
	}
	return out, nil
}

type webhookDTO struct {
	Id                string             `json:"id"`
	ProjectId         string             `json:"projectId"`
	PrincipalId       string             `json:"principalId"`
	Name              string             `json:"name"`
	URL               string             `json:"url"`
	EventTypes        []string           `json:"eventTypes"`
	Active            bool               `json:"active"`
	IncludeAgentNotes bool               `json:"includeAgentNotes"`
	Headers           []webhookHeaderDTO `json:"headers"`
	Auth              webhookAuthDTO     `json:"auth"`
	TLS               webhookTLSDTO      `json:"tls"`
	CreatedAt         time.Time          `json:"createdAt"`
	UpdatedAt         time.Time          `json:"updatedAt"`
	Version           int64              `json:"version"`
	Stats             webhookStatsDTO    `json:"stats"`
}

func certDTOs(pem string) []webhookCertDTO {
	out := []webhookCertDTO{}
	for _, c := range endpoints.Certificates(pem) {
		out = append(out, webhookCertDTO{Subject: c.Subject, Issuer: c.Issuer, NotBefore: c.NotBefore, NotAfter: c.NotAfter})
	}
	return out
}

func toWebhook(e endpoints.Endpoint) webhookDTO {
	out := webhookDTO{Id: e.Id, ProjectId: e.ProjectId, PrincipalId: e.PrincipalId, Name: e.Name, URL: e.URL, EventTypes: e.EventTypes, Active: e.Active,
		IncludeAgentNotes: e.IncludeAgentNotes, Headers: []webhookHeaderDTO{}, CreatedAt: e.CreatedAt, UpdatedAt: e.UpdatedAt, Version: e.Version,
		Stats: toWebhookStats(e, endpoints.Stats{}),
		Auth: webhookAuthDTO{Type: e.Auth.Type, Username: e.Auth.Username, PasswordConfigured: e.Auth.SealedPassword != "",
			TokenConfigured: e.Auth.SealedToken != ""},
		TLS: webhookTLSDTO{CACertificate: e.TLS.CACertificate, CACertificates: certDTOs(e.TLS.CACertificate),
			ClientCertificate: e.TLS.ClientCertificate, ClientKeyConfigured: e.TLS.SealedClientKey != ""}}
	for _, h := range e.Headers {
		out.Headers = append(out.Headers, webhookHeaderDTO{Name: h.Name, ValueConfigured: h.SealedValue != ""})
	}
	if cs := certDTOs(e.TLS.ClientCertificate); len(cs) > 0 {
		out.TLS.ClientCertificateInfo = &cs[0]
	}
	return out
}

// webhookEventTypeDTO describes a webhook event type.
type webhookEventTypeDTO struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	AgentNotes  bool   `json:"requiresAgentNotes"`
}

func (a *api) listWebhookEventTypes(c *gin.Context) {
	// Documented for signed-in users only (catalogue for the admin pages).
	if err := principal(c).RequireUser(); err != nil {
		writeError(c, err)
		return
	}
	desc := map[string]string{
		webhooks.TypeTicketCreated:       "A ticket was created.",
		webhooks.TypeTicketUpdated:       "Subject, priority or request type changed.",
		webhooks.TypeMessagePublic:       "A public reply was added (staff or requester).",
		webhooks.TypeMessageAgentNote:    "An agent-only note was added (requires includeAgentNotes).",
		webhooks.TypeTicketAssigned:      "The ticket was taken (claimed), released, assigned or moved to another group.",
		webhooks.TypeTicketStatusChanged: "The ticket's status changed (resolve, reopen, archive, set status, requester reply).",
		webhooks.TypeSLAWarning:          "The first response (the SLA target) is due soon.",
		webhooks.TypeSLABreached:         "The first-response deadline (the SLA target) was missed.",
		webhooks.TypeFollowupReminder:    "A follow-up reminder was sent to the requester (autoResolveNotice is true on the last one before auto-resolve).",
	}
	out := []webhookEventTypeDTO{}
	for _, t := range webhooks.EventTypes() {
		out = append(out, webhookEventTypeDTO{Name: t, Description: desc[t], AgentNotes: t == webhooks.TypeMessageAgentNote})
	}
	c.JSON(http.StatusOK, itemsOf(out))
}

// webhookHeaderBody is one extra request header.
type webhookHeaderBody struct {
	Name  string  `json:"name" required:"true" doc:"HTTP header name. Reserved: Host, Content-Length, Content-Type, Transfer-Encoding, Connection and other hop-by-hop headers, X-Open-Kase-Event-Id, X-Open-Kase-Event-Type, and Authorization unless auth.type is none."`
	Value *string `json:"value" doc:"Header value (write-only, stored encrypted). Omitted or empty on update: keep the stored value of the header with the same name."`
}

// webhookAuthBody sets the Authorization header.
type webhookAuthBody struct {
	Type     string  `json:"type" required:"true" enum:"none,basic,token" doc:"none; basic (Authorization: Basic); token (Authorization: Bearer)."`
	Username *string `json:"username" doc:"Basic: user name (no ':'). Omitted: keep the stored one when the type stays basic."`
	Password *string `json:"password" doc:"Basic: password (write-only). Omitted or empty: keep the stored one when the type stays basic."`
	Token    *string `json:"token" doc:"Token: bearer token (write-only). Omitted or empty: keep the stored one when the type stays token."`
}

// webhookTLSBody changes certificate settings; omitted members are unchanged.
type webhookTLSBody struct {
	CACertificate     *string `json:"caCertificate" doc:"PEM CA certificates trusted in addition to the system roots. Empty string removes them."`
	ClientCertificate *string `json:"clientCertificate" doc:"PEM client certificate for mutual TLS. Empty string removes the certificate and its key."`
	ClientKey         *string `json:"clientKey" doc:"PEM private key of the client certificate (write-only). Omitted or empty: keep the stored key, which must match the certificate."`
}

type webhookBody struct {
	Name              *string              `json:"name" doc:"Display name (1-80 characters; required on create)."`
	URL               *string              `json:"url"`
	EventTypes        *[]string            `json:"eventTypes"`
	Active            *bool                `json:"active"`
	IncludeAgentNotes *bool                `json:"includeAgentNotes"`
	Headers           *[]webhookHeaderBody `json:"headers" doc:"Replaces the extra headers (at most 20)."`
	Auth              *webhookAuthBody     `json:"auth"`
	TLS               *webhookTLSBody      `json:"tls"`
}

func (b webhookBody) input() endpoints.Input {
	in := endpoints.Input{Name: b.Name, URL: b.URL, EventTypes: b.EventTypes, Active: b.Active, IncludeAgentNotes: b.IncludeAgentNotes}
	if b.Headers != nil {
		hs := make([]endpoints.HeaderInput, 0, len(*b.Headers))
		for _, h := range *b.Headers {
			hs = append(hs, endpoints.HeaderInput{Name: h.Name, Value: h.Value})
		}
		in.Headers = &hs
	}
	if b.Auth != nil {
		in.Auth = &endpoints.AuthInput{Type: b.Auth.Type, Username: b.Auth.Username, Password: b.Auth.Password, Token: b.Auth.Token}
	}
	if b.TLS != nil {
		in.TLS = &endpoints.TLSInput{CACertificate: b.TLS.CACertificate, ClientCertificate: b.TLS.ClientCertificate, ClientKey: b.TLS.ClientKey}
	}
	return in
}

func (a *api) listWebhooks(c *gin.Context) {
	s, ok := a.scope(c)
	if !ok {
		return
	}
	after, limit, ok := pageParams(c)
	if !ok {
		return
	}
	p, err := a.Webhooks.List(c.Request.Context(), s, after, limit)
	if err != nil {
		writeError(c, err)
		return
	}
	items, err := a.webhooksWithStats(c, s, p.Items)
	respond(c, http.StatusOK, pagination.Page[webhookDTO]{Items: items, NextCursor: p.NextCursor}, err)
}

// respondWebhook answers with one webhook including its statistics.
func (a *api) respondWebhook(c *gin.Context, s *access.ProjectScope, status int, e endpoints.Endpoint, err error) {
	if err != nil {
		writeError(c, err)
		return
	}
	items, err := a.webhooksWithStats(c, s, []endpoints.Endpoint{e})
	if err != nil {
		writeError(c, err)
		return
	}
	c.JSON(status, items[0])
}

func (a *api) createWebhook(c *gin.Context) {
	s, ok := a.scope(c)
	if !ok {
		return
	}
	var body webhookBody
	if !decode(c, &body) {
		return
	}
	out, err := a.Webhooks.Create(c.Request.Context(), s, body.input())
	if err != nil {
		writeError(c, err)
		return
	}
	c.Header("Location", "/api/v1/webhooks/"+out.Id)
	c.JSON(http.StatusCreated, toWebhook(out))
}

func (a *api) getWebhook(c *gin.Context) {
	s, ok := a.scope(c)
	if !ok {
		return
	}
	e, err := a.Webhooks.Get(c.Request.Context(), s, c.Param("w"))
	a.respondWebhook(c, s, http.StatusOK, e, err)
}

func (a *api) patchWebhook(c *gin.Context) {
	s, ok := a.scope(c)
	if !ok {
		return
	}
	var body webhookBody
	if !decode(c, &body) {
		return
	}
	version, ok := ifMatch(c)
	if !ok {
		return
	}
	e, err := a.Webhooks.Update(c.Request.Context(), s, c.Param("w"), version, body.input())
	a.respondWebhook(c, s, http.StatusOK, e, err)
}

func (a *api) deleteWebhook(c *gin.Context) {
	s, ok := a.scope(c)
	if !ok {
		return
	}
	respond(c, http.StatusNoContent, nil, a.Webhooks.Delete(c.Request.Context(), s, c.Param("w")))
}

func (a *api) testWebhook(c *gin.Context) {
	s, ok := a.scope(c)
	if !ok {
		return
	}
	d, err := a.Webhooks.Test(c.Request.Context(), s, c.Param("w"))
	respond(c, http.StatusAccepted, deliveryEnvelopeDTO{Delivery: toDelivery(d)}, err)
}

func (a *api) listWebhookDeliveries(c *gin.Context) {
	s, ok := a.scope(c)
	if !ok {
		return
	}
	in, ok := deliveryQuery(c)
	if !ok {
		return
	}
	p, err := a.Webhooks.Deliveries(c.Request.Context(), s, c.Param("w"), in)
	respond(c, http.StatusOK, page(p, toDelivery), err)
}

func (a *api) getWebhookDelivery(c *gin.Context) {
	s, ok := a.scope(c)
	if !ok {
		return
	}
	d, err := a.Webhooks.Delivery(c.Request.Context(), s, c.Param("w"), c.Param("d"))
	respond(c, http.StatusOK, toDelivery(d), err)
}

func (a *api) retryWebhookDelivery(c *gin.Context) {
	s, ok := a.scope(c)
	if !ok {
		return
	}
	d, err := a.Webhooks.Retry(c.Request.Context(), s, c.Param("w"), c.Param("d"))
	respond(c, http.StatusAccepted, toDelivery(d), err)
}
