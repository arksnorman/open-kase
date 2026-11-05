package runtime

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/arksnorman/open-kase/api/internal/access"
	"github.com/arksnorman/open-kase/api/internal/auditlog"
	"github.com/arksnorman/open-kase/api/internal/auth"
	"github.com/arksnorman/open-kase/api/internal/automation"
	"github.com/arksnorman/open-kase/api/internal/customers"
	"github.com/arksnorman/open-kase/api/internal/holdreasons"
	"github.com/arksnorman/open-kase/api/internal/installation"
	"github.com/arksnorman/open-kase/api/internal/integrations"
	"github.com/arksnorman/open-kase/api/internal/jobs"
	"github.com/arksnorman/open-kase/api/internal/live"
	"github.com/arksnorman/open-kase/api/internal/macros"
	"github.com/arksnorman/open-kase/api/internal/notifications"
	"github.com/arksnorman/open-kase/api/internal/organisations"
	mongostore "github.com/arksnorman/open-kase/api/internal/persistence/mongo"
	"github.com/arksnorman/open-kase/api/internal/projects"
	"github.com/arksnorman/open-kase/api/internal/reports"
	"github.com/arksnorman/open-kase/api/internal/settings"
	"github.com/arksnorman/open-kase/api/internal/sla"
	"github.com/arksnorman/open-kase/api/internal/statuses"
	"github.com/arksnorman/open-kase/api/internal/tickets"
	transporthttp "github.com/arksnorman/open-kase/api/internal/transport/http"
	transportmcp "github.com/arksnorman/open-kase/api/internal/transport/mcp"
	"github.com/arksnorman/open-kase/api/internal/users"
	"github.com/arksnorman/open-kase/api/internal/webhooks"
	"github.com/arksnorman/open-kase/api/internal/webhooks/endpoints"
	"github.com/arksnorman/open-kase/api/package/clock"
	"github.com/arksnorman/open-kase/api/package/fault"
	"github.com/arksnorman/open-kase/api/package/secrets"
	"github.com/arksnorman/open-kase/api/templates"
)

// passwordParams are the Argon2id parameters for new password hashes.
// Tests lower them; production uses auth.DefaultArgon2Params.
var passwordParams = auth.DefaultArgon2Params

// Container holds explicitly wired dependencies for one process.
type Container struct {
	Log          *slog.Logger
	Boot         Bootstrap
	Clock        clock.Clock
	Keys         *secrets.Keyring
	Store        *mongostore.Store
	Redis        *redis.Client
	Settings     *settings.Service
	SettingsRepo *mongostore.SettingsRepository
	Installation *installation.Service

	Passwords *auth.Passwords
	Auth      *auth.Service
	// SigningKeys manages JWT signing keys (rotation: `keys rotate-jwt`).
	SigningKeys *auth.KeyManager
	// IdentityRepo stores MFA enrolments, challenges and account tokens.
	IdentityRepo *mongostore.IdentityRepository
	usersRepo    *mongostore.UsersRepository
	throttle     *mongostore.Throttle
	Access       *access.Service
	Users        *users.Service
	Projects     *projects.Service
	Customers    *customers.Service
	Integrations *integrations.Service
	Workflow     *statuses.Catalog
	Tickets      *tickets.Service
	TicketsRepo  *mongostore.TicketsRepository
	Idempotency  *mongostore.IdempotencyRepository
	Audit        *mongostore.AuditRepository

	// Milestone 5: durable jobs, notifications and webhooks.
	Jobs          *mongostore.JobsRepository
	NotifyRepo    *mongostore.NotificationsRepository
	WebhooksRepo  *mongostore.WebhooksRepository
	Waker         jobs.Waker
	Notifications *notifications.Service
	Webhooks      *endpoints.Service
	Email         *notifications.Email
	Deliverer     *endpoints.Deliverer
	Renderer      *templates.Renderer
	// Mailers builds the SMTP adapter from settings (tests substitute it).
	Mailers notifications.MailerFactory
	// JobBackoff, JobLeaseTTL and JobGrace tune the job runner (nil/zero =
	// defaults).
	JobBackoff  func(attempt int) time.Duration
	JobLeaseTTL time.Duration
	JobGrace    time.Duration
	// WebhookJitter replaces the ±20 % retry jitter (tests use zero delays).
	WebhookJitter webhooks.Jitter

	// Milestone 8: the /mcp endpoint (built on first use by mcpHandler).
	MCP *transportmcp.Handler

	// Milestone 6: SLA, custom fields, saved views, macros and reports.
	// BizClock drives ticket timestamps, SLA deadlines and SLA timer jobs
	// (tests replace it with a manual clock; authentication keeps Clock).
	BizClock      clock.Clock
	CatalogRepo   *mongostore.CatalogRepository
	SLA           *sla.Service
	Macros        *macros.Service
	HoldReasons   *holdreasons.Service
	Organisations *organisations.Service
	Reports       *reports.Service

	// Ticket workflow: lifecycle automation and live update streams.
	AutomationRepo *mongostore.AutomationRepository
	Automation     *automation.Service
	Live           *live.Hub
	// LiveHeartbeat replaces the SSE heartbeat (tests shorten it).
	LiveHeartbeat time.Duration
}

// newContainer wires services around an already loaded keyring.
func newContainer(log *slog.Logger, boot Bootstrap, keys *secrets.Keyring) (*Container, error) {
	store, err := mongostore.Connect(boot.MongoURI)
	if err != nil {
		return nil, err
	}
	c := &Container{Log: log, Boot: boot, Clock: clock.System{}, Keys: keys, Store: store}
	if boot.RedisURI != "" {
		opts, err := redis.ParseURL(boot.RedisURI)
		if err != nil {
			_ = store.Close(context.Background())
			return nil, errors.New("invalid REDIS_URI: the URI could not be parsed")
		}
		c.Redis = redis.NewClient(opts)
	}
	c.SettingsRepo = mongostore.NewSettingsRepository(store)
	c.Settings = settings.NewService(c.SettingsRepo, keys, c.Clock)
	c.Installation = installation.NewService(mongostore.NewInstallationRepository(store), keys,
		func(pw string) (string, error) { return auth.HashPassword(pw, passwordParams) }, c.Clock)
	if c.Passwords, err = auth.NewPasswords(passwordParams); err != nil {
		_ = store.Close(context.Background())
		return nil, err
	}

	usersRepo := mongostore.NewUsersRepository(store)
	projectsRepo := mongostore.NewProjectsRepository(store)
	throttle := mongostore.NewThrottle(store, c.Clock.Now, nil)
	c.usersRepo, c.throttle = usersRepo, throttle
	c.Audit = mongostore.NewAuditRepository(store)
	c.Idempotency = mongostore.NewIdempotencyRepository(store)
	c.Access = access.NewService(projectsRepo)
	c.Users = users.NewService(usersRepo, c.Passwords, throttle, c.Clock)
	c.Projects = projects.NewService(projectsRepo, c.Clock)
	c.Customers = customers.NewService(mongostore.NewCustomersRepository(store), c.Clock)
	c.Integrations = integrations.NewService(mongostore.NewIntegrationsRepository(store), c.Clock)
	c.CatalogRepo = mongostore.NewCatalogRepository(store)
	c.Workflow = statuses.NewCatalog(c.CatalogRepo)
	c.TicketsRepo = mongostore.NewTicketsRepository(store)
	c.Jobs = mongostore.NewJobsRepository(store)
	c.wireBusiness(c.Clock)
	if c.Renderer, err = templates.NewRenderer(); err != nil {
		_ = store.Close(context.Background())
		return nil, err
	}
	c.Mailers = notifications.SMTPMailer
	c.NotifyRepo = mongostore.NewNotificationsRepository(store, c.Jobs)
	c.WebhooksRepo = mongostore.NewWebhooksRepository(store)
	c.IdentityRepo = mongostore.NewIdentityRepository(store, c.Jobs)
	c.wireNotifications()
	c.UseAuthClock(c.Clock)
	c.AutomationRepo = mongostore.NewAutomationRepository(store)
	c.wireAutomation()
	c.Live = live.NewHub(live.Config{Source: mongostore.NewLiveSource(store), Log: log})
	c.TicketsRepo.SetChangeHook(c.liveHint)
	return c, nil
}

// UseAuthClock (re)builds the signing keys and the authentication service
// on clk (tests drive token, MFA and reset-link expiry with a manual clock).
func (c *Container) UseAuthClock(clk clock.Clock) {
	c.SigningKeys = auth.NewKeyManager(mongostore.NewSigningKeysRepository(c.Store), c.Keys, clk)
	c.Auth = auth.NewService(auth.Deps{
		Log: c.Log, Users: c.usersRepo, Audit: c.Audit, Sessions: mongostore.NewSessionsRepository(c.Store),
		Tokens: auth.NewTokens(c.SigningKeys, clk), Passwords: c.Passwords, Throttle: c.throttle, Clock: clk,
		Workspace: c.workspaceID, Policy: c.authPolicy,
		MFA: c.IdentityRepo, Accounts: c.IdentityRepo, Sealer: c.Keys, Waker: c.Waker,
	})
}

// wireBusiness builds the ticket-facing services on clk.
func (c *Container) wireBusiness(clk clock.Clock) {
	c.BizClock = clk
	// Jobs enqueued without a run time are stamped with the clock the runner
	// acquires with (tests drive both with a manual clock).
	c.Jobs.SetNow(clk.Now)
	c.SLA = sla.NewService(mongostore.NewSLAPolicyRepository(c.Store), clk, c.wakeJobs)
	c.Tickets = c.newTicketsService(clk)
	c.Macros = macros.NewService(mongostore.NewMacrosRepository(c.Store), c.Tickets, clk)
	c.HoldReasons = holdreasons.NewService(mongostore.NewHoldReasonsRepository(c.Store), clk)
	c.Organisations = organisations.NewService(mongostore.NewOrganisationsRepository(c.Store), clk)
	c.Reports = reports.NewService(mongostore.NewReportsRepository(c.Store), clk)
}

// newTicketsService returns a tickets service on clk with the SLA
// collaborator.
func (c *Container) newTicketsService(clk clock.Clock) *tickets.Service {
	return tickets.NewService(tickets.Deps{
		Repo: c.TicketsRepo, Files: mongostore.NewAttachmentsRepository(c.Store), Catalog: c.CatalogRepo,
		Requesters: c.Customers, Clock: clk, Limits: c.fileLimits, SLA: c.SLA,
		Jobs: c.Jobs, Wake: c.wakeJobs, Authors: authorScopes{c}, Signatures: signatureSource{c},
	})
}

// wakeJobs nudges the job workers after enqueueing (best effort).
func (c *Container) wakeJobs(ctx context.Context) {
	if c.Waker != nil {
		c.Waker.Notify(ctx)
	}
}

// authorScopes re-authorizes the author of a scheduled message at send
// time from current storage (account status, membership, groups).
type authorScopes struct{ c *Container }

func (a authorScopes) UserScope(ctx context.Context, workspaceID, projectID, userID string) (*access.ProjectScope, error) {
	u, err := a.c.usersRepo.Get(ctx, workspaceID, userID)
	if errors.Is(err, users.ErrNotFound) {
		return nil, fault.NotFound("userNotFound")
	}
	if err != nil {
		return nil, err
	}
	if !u.Active() {
		return nil, fault.Forbidden("userInactive", "The author's account is not active.")
	}
	p := access.Principal{Kind: access.KindUser, Id: u.Id, WorkspaceId: u.WorkspaceId, Name: u.Name, Email: u.Email, Admin: u.IsAdmin()}
	return a.c.Access.Scope(ctx, p)
}

// signatureSource reads the author's and the workspace's signatures.
type signatureSource struct{ c *Container }

func (s signatureSource) Signatures(ctx context.Context, workspaceID, userID string) (string, string, error) {
	u, err := s.c.usersRepo.Get(ctx, workspaceID, userID)
	if err != nil {
		return "", "", err
	}
	workspace := ""
	if snap := s.c.Settings.Current(); snap != nil {
		workspace = snap.Product().Signature
	}
	return u.Signature, workspace, nil
}

// UseBusinessClock rewires the ticket-facing services and notification
// workers onto clk (tests drive SLA deadlines with a manual clock).
func (c *Container) UseBusinessClock(clk clock.Clock) {
	c.wireBusiness(clk)
	c.wireNotifications()
	c.wireAutomation()
	c.MCP = nil // rebuilt with the new services on first use
}

// wireAutomation builds the lifecycle automation service on the business
// clock.
func (c *Container) wireAutomation() {
	c.Automation = automation.NewService(automation.Deps{Repo: c.AutomationRepo, Tickets: c.Tickets, Jobs: c.Jobs,
		Waker: c.Waker, Clock: c.BizClock, Log: c.Log})
}

// workspaceID returns the installation's workspace from the settings
// snapshot; requests fail with 503 until the installation is initialized
// and the snapshot is loaded.
func (c *Container) workspaceID(context.Context) (string, error) {
	if snap := c.Settings.Current(); snap != nil {
		return snap.WorkspaceID(), nil
	}
	return "", fault.New(fault.KindUnavailable, "notInitialized", "The installation is not ready yet.")
}

// authPolicy is the narrow settings view for token lifetimes.
func (c *Container) authPolicy() auth.Policy {
	a, name := settings.Defaults().Auth, settings.Defaults().Product.Name
	if snap := c.Settings.Current(); snap != nil {
		a, name = snap.Auth(), snap.Product().Name
	}
	return auth.Policy{AccessTTL: time.Duration(a.AccessTokenTTLSeconds) * time.Second, RefreshTTL: time.Duration(a.RefreshTokenTTLHours) * time.Hour,
		RequireAdminMFA: a.RequireAdminMFA, Issuer: name}
}

// fileLimits is the narrow settings view for message and upload sizes.
func (c *Container) fileLimits() tickets.Limits {
	f := settings.Defaults().Files
	if snap := c.Settings.Current(); snap != nil {
		f = snap.Files()
	}
	return tickets.Limits{MaxAttachmentBytes: f.MaxAttachmentBytes, MaxAttachmentsPerMessage: f.MaxAttachmentsPerMessage,
		MaxMessageTotalBytes: f.MaxMessageTotalBytes, MaxMessageCodePoints: f.MaxMessageCodePoints}
}

// httpServices exposes the application services to the REST adapter.
func (c *Container) httpServices() transporthttp.Services {
	return transporthttp.Services{
		Auth: c.Auth, Integrations: c.Integrations, Access: c.Access, Users: c.Users, Projects: c.Projects,
		Customers: c.Customers, Settings: c.Settings, Workflow: c.Workflow, Tickets: c.Tickets, Idempotency: c.Idempotency, Clock: c.Clock,
		Notifications: c.Notifications, Webhooks: c.Webhooks,
		SLA: c.SLA, Macros: c.Macros, HoldReasons: c.HoldReasons, Organisations: c.Organisations, Reports: c.Reports, AuditLog: auditlog.NewService(mongostore.NewAuditLogRepository(c.Store)),
		Automation: c.Automation, Live: c.Live, LiveHeartbeat: c.LiveHeartbeat,
		SecureCookies: c.Boot.Env != EnvDevelopment,
		BaseURL: func() string {
			if snap := c.Settings.Current(); snap != nil {
				return snap.Product().BaseURL
			}
			return ""
		},
	}
}

// Close releases external connections.
func (c *Container) Close(ctx context.Context) {
	if c.Live != nil {
		c.Live.Close()
	}
	if c.Redis != nil {
		_ = c.Redis.Close()
	}
	if err := c.Store.Close(ctx); err != nil {
		c.Log.Warn("mongodb disconnect", "error", err.Error())
	}
}

// Readiness check functions shared by serve and doctor.

func (c *Container) checkMongo(ctx context.Context) error {
	_, err := c.Store.ReplicaSet(ctx)
	return err
}

func (c *Container) checkMasterKey(ctx context.Context) error {
	st, err := c.Installation.State(ctx)
	if err != nil {
		return err
	}
	if !st.Initialized {
		return errors.New("installation is not initialized; run `init`")
	}
	return nil
}

func (c *Container) checkSettings(context.Context) error {
	if c.Settings.Current() == nil {
		return errors.New("settings snapshot not loaded")
	}
	return nil
}

func (c *Container) checkRedis(ctx context.Context) error {
	if c.Redis == nil {
		return errors.New("redis is not configured")
	}
	return c.Redis.Ping(ctx).Err()
}
