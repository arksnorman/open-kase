package runtime

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"os/user"
	"slices"
	"strings"
	"time"

	"github.com/arksnorman/open-kase/api/internal/access"
	"github.com/arksnorman/open-kase/api/internal/audit"
	"github.com/arksnorman/open-kase/api/internal/customers"
	"github.com/arksnorman/open-kase/api/internal/integrations"
	"github.com/arksnorman/open-kase/api/internal/macros"
	"github.com/arksnorman/open-kase/api/internal/organisations"
	"github.com/arksnorman/open-kase/api/internal/outbox"
	mongostore "github.com/arksnorman/open-kase/api/internal/persistence/mongo"
	"github.com/arksnorman/open-kase/api/internal/projects"
	"github.com/arksnorman/open-kase/api/internal/sla"
	"github.com/arksnorman/open-kase/api/internal/tickets"
	"github.com/arksnorman/open-kase/api/internal/users"
	"github.com/arksnorman/open-kase/api/package/clock"
)

func runSeedDemo(ctx context.Context, args []string, stdio IO) int {
	boot := bootstrapFromEnv()
	fs := newFlagSet("seed-demo", `Create demo data in the project: groups Delivery and Billing (besides
Triage), agents Dana Okafor and Marco Silva, contacts and sample tickets
with public replies and agent notes. The first administrator joins every
group. Also adds an example first-response SLA (demonstration targets, not a service
commitment), tags, and "Escalated to Delivery" and "Request more info"
canned replies; tickets without an SLA cycle are backfilled.

Safe to run again: existing records are kept. Outbox events written by the
seed are stored as "suppressed" so no notification is ever sent for demo
data. Refuses to run with APP_ENV=production unless --force is given.`)
	boot.registerFlags(fs, false)
	force := fs.Bool("force", false, "allow running with APP_ENV=production")
	pwStdin := fs.Bool("agent-password-stdin", false, "read the demo agents' password from the first line of standard input (default: generate one)")
	if code, stop := parseFlags(fs, args, stdio); stop {
		return code
	}
	if boot.Env == EnvProduction && !*force {
		fmt.Fprintln(stdio.Err, "seed-demo refuses to run with APP_ENV=production; pass --force if you really want demo data here")
		return ExitUsage
	}
	opts := seedOptions{}
	if *pwStdin {
		line, err := bufio.NewReader(stdio.In).ReadString('\n')
		if err != nil && !errors.Is(err, io.EOF) {
			fmt.Fprintf(stdio.Err, "read password: %v\n", err)
			return ExitUsage
		}
		opts.AgentPassword = strings.TrimRight(line, "\r\n")
	}
	c, code := setup(boot, false, stdio)
	if c == nil {
		return code
	}
	defer c.Close(context.Background())
	ctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()
	if err := c.checkMasterKey(ctx); err != nil {
		fmt.Fprintf(stdio.Err, "installation: %v\n", err)
		return ExitFailure
	}
	if err := seedDemo(ctx, c, opts, stdio.Out); err != nil {
		fmt.Fprintf(stdio.Err, "seed-demo: %v\n", err)
		return ExitFailure
	}
	return ExitOK
}

type seedOptions struct {
	AgentPassword string // empty: generate
	Now           time.Time
}

type seedMsg struct {
	ago        time.Duration
	author     string // "contact" or an agent email
	visibility string
	body       string
	file       string // attach a generated file with this name
}

type seedTicket struct {
	number      int64
	subject     string
	requester   string // contact email
	priority    int
	requested   int
	requestType string
	group       string
	status      string // open, awaitingCustomer, onHold, resolved
	holdReason  string // hold reason name (status onHold)
	assignee    string // agent email
	createdAgo  time.Duration
	messages    []seedMsg
	watchers    []string
	finalAgo    time.Duration // time of the closing status change
	tags        []string      // added when the ticket has no tags yet
}

type seedProject struct {
	contacts []seedContact
	tickets  []seedTicket
}

type seedContact struct{ name, email string }

// seedOrgs are the demo organisations (external reference, name) by contact
// e-mail domain; globex.example has none. The seed links each contact and
// ticket explicitly, as an integration would.
var seedOrgs = map[string][2]string{
	"acme-ltd.com":      {"CUST-0042", "Acme Ltd"},
	"northwind.example": {"CUST-0107", "Northwind Traders"},
}

func emailDomain(email string) string { return email[strings.LastIndex(email, "@")+1:] }

const (
	danaEmail  = "dana@open-kase.local"
	marcoEmail = "marco@open-kase.local"
)

var seedAgents = []struct{ email, name string }{{danaEmail, "Dana Okafor"}, {marcoEmail, "Marco Silva"}} // split into first and last name (users.SplitName)

func demoProject() seedProject {
	h, d := time.Hour, 24*time.Hour
	m := time.Minute
	return seedProject{
		contacts: []seedContact{
			{"Priya Raman", "priya.raman@acme-ltd.com"},
			{"Lena Fischer", "lena.fischer@acme-ltd.com"},
			{"Tom Becker", "tom.becker@northwind.example"},
			{"Sam Ortiz", "sam.ortiz@globex.example"},
		},
		tickets: []seedTicket{
			{number: 1012, subject: "Spam filter quarantining supplier invoices", requester: "priya.raman@acme-ltd.com", priority: 3,
				requestType: "issue", group: "Triage", status: "resolved", assignee: marcoEmail, createdAgo: 12 * d, finalAgo: 9 * d,
				messages: []seedMsg{
					{12 * d, "contact", "public", "Invoices from our supplier billing@parts-supply.example have landed in quarantine since Monday.", ""},
					{11*d + 20*h, marcoEmail, "public", "We have added parts-supply.example to your organisation's allow list. New invoices should arrive normally; please release the quarantined ones from the quarantine digest.", ""},
				}},
			{number: 1028, subject: "DKIM key rotation schedule question", requester: "sam.ortiz@globex.example", priority: 3,
				requestType: "question", group: "Delivery", status: "resolved", assignee: marcoEmail, createdAgo: 4 * d, finalAgo: 2 * d,
				messages: []seedMsg{
					{4 * d, "contact", "public", "How often do you rotate the DKIM keys for globex.example, and do we need to update DNS when you do?", ""},
					{3*d + 2*h, marcoEmail, "agent", "Rotation is every 6 months; selector s2026b goes live next month. No customer DNS change because we publish CNAMEs.", ""},
					{2 * d, marcoEmail, "public", "We rotate DKIM keys every six months. Your DNS uses CNAME records that point to our selectors, so no change is needed on your side.", ""},
				}},
			{number: 1031, subject: "Shared calendar for support desk", requester: "priya.raman@acme-ltd.com", priority: 4,
				requestType: "serviceRequest", group: "Triage", status: "open", createdAgo: 1 * d,
				messages: []seedMsg{
					{1 * d, "contact", "public", "Could you set up a shared calendar for our support desk (support@acme-ltd.com) that the whole team can edit?", ""},
				}},
			{number: 1033, subject: "Password reset emails not arriving", requester: "sam.ortiz@globex.example", priority: 3,
				requestType: "issue", group: "Triage", status: "open", createdAgo: 5 * h,
				messages: []seedMsg{
					{5 * h, "contact", "public", "Our customers are not receiving password reset emails from noreply@globex.example since this morning.", ""},
				}},
			{number: 1036, subject: "Mailbox quota increase for finance team", requester: "lena.fischer@acme-ltd.com", priority: 3,
				requestType: "serviceRequest", group: "Triage", status: "awaitingCustomer", assignee: danaEmail, createdAgo: 6 * h, finalAgo: 3 * h,
				tags: []string{"quota", "billing"},
				messages: []seedMsg{
					{6 * h, "contact", "public", "The finance team's mailboxes are almost full. Can we raise their quota to 50 GB?", ""},
					{3*h + 10*m, danaEmail, "public", "Happy to help. Could you confirm which mailboxes belong to the finance team, and approve the additional cost for the 50 GB plan?", ""},
				}},
			{number: 1039, subject: "SPF record failing after DNS migration", requester: "tom.becker@northwind.example", priority: 2,
				requestType: "issue", group: "Delivery", status: "open", assignee: danaEmail, createdAgo: 3 * h,
				tags: []string{"dns", "spf"},
				messages: []seedMsg{
					{3 * h, "contact", "public", "Since we moved DNS to a new provider, receivers report SPF failures for northwind.example.", ""},
					{1 * h, danaEmail, "agent", "Their new SPF record lacks include:spf.mail-hosting.example; drafting the corrected record for them.", ""},
				}},
			{number: 1041, subject: "Outbound mail delayed to Gmail recipients", requester: "tom.becker@northwind.example", priority: 2,
				requestType: "issue", group: "Delivery", status: "open", createdAgo: 40 * m, tags: []string{"deliverability", "gmail"},
				messages: []seedMsg{
					{40 * m, "contact", "public", "Messages to Gmail addresses are delayed by up to two hours. Other recipients are fine.", ""},
					{26 * m, marcoEmail, "agent", "Queue shows deferrals from Gmail with 421 4.7.28 rate limiting on one outbound IP.", ""},
				}},
			{number: 1042, subject: "Cannot receive external email", requester: "priya.raman@acme-ltd.com", priority: 1, requested: 1,
				requestType: "issue", group: "Triage", status: "open", createdAgo: 60 * m, watchers: []string{danaEmail, marcoEmail},
				tags: []string{"dns", "outage"},
				messages: []seedMsg{
					{60 * m, "contact", "public", "Since about 09:40 all inbound mail from outside our domain is bouncing. Senders get '550 5.1.1 recipient rejected'. Internal mail works. This is affecting the whole company.", "bounce-report.eml"},
					{47 * m, marcoEmail, "agent", "MX for acme-ltd.com still points to mx1.old-provider.net — looks like their DNS change last night. Checking whether the domain was removed from our accepted-domains list.", ""},
					{41 * m, marcoEmail, "public", "Thanks Priya, we're looking into this now. Could you confirm whether anyone changed DNS records for acme-ltd.com in the last 24 hours?", ""},
					{12 * m, "contact", "public", "Yes, our IT contractor moved DNS to a new host yesterday evening.", ""},
				}},
			{number: 1043, subject: "Snapshot restore stuck at 80%", requester: "priya.raman@acme-ltd.com", priority: 2,
				requestType: "issue", group: "Triage", status: "onHold", holdReason: "Waiting on Engineering", assignee: danaEmail, createdAgo: 2 * h, finalAgo: 80 * m,
				messages: []seedMsg{
					{2 * h, "contact", "public", "Restoring last night's snapshot of vm-web-01 has been stuck at 80% for an hour.", ""},
					{90 * m, danaEmail, "agent", "Storage node st-07 shows a degraded volume; restore jobs on it are paused.", ""},
				}},
			{number: 1044, subject: "Additional public IPv4 address", requester: "tom.becker@northwind.example", priority: 4,
				requestType: "serviceRequest", group: "Billing", status: "open", createdAgo: 1 * d,
				messages: []seedMsg{
					{1 * d, "contact", "public", "Please add one more public IPv4 address to our project and let us know the monthly cost.", ""},
				}},
		}}
}

// seeder carries the state of one seed run.
type seeder struct {
	c       *Container
	out     io.Writer
	repo    *mongostore.DemoSeedRepository
	users   *mongostore.UsersRepository
	clk     *clock.Manual
	tickets *tickets.Service
	ws      string
	admin   access.Principal
	agents  map[string]access.Principal // by email
	opts    seedOptions
	label   string
	orgs    map[string]string // organisation ID by contact e-mail domain
}

func seedDemo(ctx context.Context, c *Container, opts seedOptions, out io.Writer) error {
	ctx = outbox.Suppress(ctx)
	snap, err := c.Settings.Refresh(ctx)
	if err != nil {
		return fmt.Errorf("load settings: %w", err)
	}
	if opts.Now.IsZero() {
		opts.Now = c.Clock.Now()
	}
	s := &seeder{c: c, out: out, repo: mongostore.NewDemoSeedRepository(c.Store), users: mongostore.NewUsersRepository(c.Store),
		clk: clock.NewManual(opts.Now), ws: snap.WorkspaceID(), agents: map[string]access.Principal{}, opts: opts, label: "cli seed-demo"}
	if u, err := user.Current(); err == nil {
		s.label += " (os user " + u.Username + ")"
	}
	// A tickets service on a manual clock gives the demo story realistic,
	// backdated timestamps (and SLA deadlines) while running the real
	// command paths.
	s.tickets = c.newTicketsService(s.clk)

	adminID, err := s.repo.BootstrapAdminID(ctx)
	if err != nil {
		return err
	}
	adminUser, err := s.users.Get(ctx, s.ws, adminID)
	if err != nil {
		return fmt.Errorf("load administrator: %w", err)
	}
	op := audit.Operator(audit.SystemCLI, s.label)
	s.admin = access.Principal{Kind: access.KindUser, Id: adminID, WorkspaceId: s.ws, Name: adminUser.Name,
		Email: adminUser.Email, Admin: true, Actor: &op}

	if err := s.ensureAgents(ctx); err != nil {
		return err
	}
	if err := s.seedProject(ctx, demoProject(), adminID); err != nil {
		return err
	}
	fmt.Fprintln(out, "Demo data ready.")
	return nil
}

func (s *seeder) printf(format string, args ...any) { fmt.Fprintf(s.out, format+"\n", args...) }

func randomPassword() string {
	b := make([]byte, 15)
	_, _ = rand.Read(b)
	return "demo-" + base64.RawURLEncoding.EncodeToString(b)
}

func (s *seeder) ensureAgents(ctx context.Context) error {
	password := s.opts.AgentPassword
	for _, a := range seedAgents {
		u, err := s.users.GetByEmail(ctx, s.ws, users.NormalizeEmail(a.email))
		switch {
		case err == nil:
			s.printf("agent %s <%s>: already present (kept; password unchanged)", u.Name, u.Email)
		case errors.Is(err, users.ErrNotFound):
			generated := password == ""
			if generated {
				password = randomPassword()
			}
			pw := password
			if u, err = s.c.Users.Create(ctx, s.admin, users.CreateInput{Email: a.email, Name: users.SplitName(a.name), Password: &pw}); err != nil {
				return fmt.Errorf("create agent %s: %w", a.email, err)
			}
			if generated {
				s.printf("agent %s <%s>: created with generated password %s (shown once)", u.Name, u.Email, pw)
			} else {
				s.printf("agent %s <%s>: created with the password from standard input", u.Name, u.Email)
			}
		default:
			return err
		}
		op := audit.Operator(audit.SystemCLI, s.label+" as "+u.Name)
		s.agents[a.email] = access.Principal{Kind: access.KindUser, Id: u.Id, WorkspaceId: s.ws, Name: u.Name, Email: u.Email, Actor: &op}
	}
	return nil
}

func (s *seeder) seedProject(ctx context.Context, sp seedProject, adminID string) error {
	scope, err := s.c.Access.Scope(ctx, s.admin)
	if err != nil {
		return err
	}

	// Groups.
	groups := map[string]string{}
	page, err := s.c.Projects.ListGroups(ctx, scope, nil, 100)
	if err != nil {
		return err
	}
	for _, g := range page.Items {
		groups[g.Name] = g.Id
	}
	for _, name := range []string{"Delivery", "Billing"} {
		if groups[name] != "" {
			continue
		}
		g, err := s.c.Projects.CreateGroup(ctx, scope, projects.CreateGroupInput{Name: name})
		if err != nil {
			return err
		}
		groups[name] = g.Id
	}

	// Group memberships: the administrator in every group; agents in theirs.
	members := []struct {
		id     string
		groups []string
	}{
		{adminID, []string{"Triage", "Delivery", "Billing"}},
		{s.agents[danaEmail].Id, []string{"Triage", "Delivery"}},
		{s.agents[marcoEmail].Id, []string{"Triage", "Delivery"}},
	}
	for _, m := range members {
		for _, g := range m.groups {
			if _, err := s.c.Projects.AddGroupMember(ctx, scope, groups[g], m.id); err != nil {
				return err
			}
		}
	}

	// Organisations, then contacts linked to them.
	s.orgs = map[string]string{}
	existing, err := s.c.Organisations.List(ctx, scope, "", "all", nil, 100)
	if err != nil {
		return err
	}
	for domain, o := range seedOrgs {
		ref, name := o[0], o[1]
		for _, e := range existing.Items {
			if e.ExternalRef == ref {
				s.orgs[domain] = e.Id
			}
		}
		if s.orgs[domain] != "" {
			continue
		}
		org, err := s.c.Organisations.Create(ctx, scope, organisations.Input{Name: &name, ExternalRef: &ref})
		if err != nil {
			return err
		}
		s.orgs[domain] = org.Id
	}
	contacts := map[string]string{}
	for _, ct := range sp.contacts {
		in := customers.ContactInput{Name: ct.name, Email: ct.email}
		if id, ok := s.orgs[emailDomain(ct.email)]; ok {
			in.OrganisationId = &id
		}
		res, err := s.c.Customers.UpsertContact(ctx, scope, in)
		if err != nil {
			return err
		}
		contacts[ct.email] = res.Contact.Id
	}

	// The requester side posts through a demo integration (revoked at the
	// end; its token is never shown).
	gateway, err := s.gateway(ctx, scope)
	if err != nil {
		return err
	}
	defer func() {
		if gateway.Status == integrations.StatusActive {
			_ = s.c.Integrations.Revoke(context.WithoutCancel(ctx), scope, gateway.Id)
		}
	}()
	op := audit.Operator(audit.SystemCLI, s.label+" via "+gateway.Name)
	requester := access.Principal{Kind: access.KindIntegration, Id: gateway.Id, WorkspaceId: s.ws, Name: gateway.Name,
		ProjectId: scope.Project.Id, Scopes: gateway.Scopes, Actor: &op}

	rts, err := s.c.Workflow.RequestTypes(ctx, scope)
	if err != nil {
		return err
	}
	types := map[string]string{}
	for _, rt := range rts {
		types[rt.Code] = rt.Id
	}
	sts, err := s.c.Workflow.Statuses(ctx, scope)
	if err != nil {
		return err
	}
	waiting := ""
	for _, st := range sts {
		if st.Code == "waitingCustomer" {
			waiting = st.Id
		}
	}
	// Before the tickets, so new demo tickets get SLA cycles naturally.
	if err := s.ensureProductivity(ctx, scope, groups, waiting); err != nil {
		return err
	}
	if err := s.ensureTags(ctx, scope, sp.tickets); err != nil {
		return err
	}
	for _, t := range sp.tickets {
		key, err := s.repo.TicketRefBySubject(ctx, scope.Project.Id, t.subject)
		if err != nil {
			return err
		}
		if key != "" {
			s.printf("ticket %s %q: already present (kept)", key, t.subject)
			if err := s.ensureTicketExtras(ctx, key, t); err != nil {
				return fmt.Errorf("ticket %s: %w", key, err)
			}
			continue
		}
		if err := s.seedTicket(ctx, requester, t, groups, contacts, types, waiting); err != nil {
			return fmt.Errorf("ticket %q: %w", t.subject, err)
		}
	}
	// Tickets created before the policy existed (an earlier seed run) get a
	// cycle replayed from their history.
	s.clk.Set(s.opts.Now)
	res, err := s.tickets.BackfillSLA(ctx, scope)
	if err != nil {
		return fmt.Errorf("sla backfill: %w", err)
	}
	s.printf("SLA cycles backfilled for %d existing ticket(s)", res.Started)
	return nil
}

// demoSLA returns the example first-response targets (wall-clock, 24/7):
// P1 30 min, P2 2 h, P3 8 h, P4 24 h.
func demoSLA() sla.PolicyInput {
	mins := func(n int) *int { return &n }
	return sla.PolicyInput{Active: true,
		Targets: map[string]sla.TargetsInput{
			"1": {FirstResponseMinutes: mins(30)},
			"2": {FirstResponseMinutes: mins(2 * 60)},
			"3": {FirstResponseMinutes: mins(8 * 60)},
			"4": {FirstResponseMinutes: mins(24 * 60)},
		}}
}

// ensureProductivity adds the demo SLA and canned replies when missing.
func (s *seeder) ensureProductivity(ctx context.Context, scope *access.ProjectScope, groups map[string]string, waiting string) error {
	if cur, err := s.c.SLA.Get(ctx, scope); err != nil {
		return err
	} else if cur.Version == 0 {
		if _, err := s.c.SLA.Put(ctx, scope, 0, demoSLA()); err != nil {
			return fmt.Errorf("sla: %w", err)
		}
		s.printf("example SLA turned on (first response: P1 30 min, P2 2 h, P3 8 h, P4 24 h)")
	} else {
		s.printf("SLA already saved (kept)")
	}

	existing, err := s.c.Macros.List(ctx, scope)
	if err != nil {
		return err
	}
	have := func(name string) bool {
		return slices.ContainsFunc(existing, func(m macros.Macro) bool { return m.Name == name })
	}
	str := func(v string) *string { return &v }
	if name := "Escalated to Delivery"; !have(name) {
		body := "Hi {{requester.name}},\n\nThanks for the details. We have escalated {{ticket.number}} to our Delivery team, who will follow up shortly.\n\nBest regards,\n{{agent.name}}"
		if _, err := s.c.Macros.Create(ctx, scope, macros.Input{Name: str(name), Body: &body, Visibility: str("public")}); err != nil {
			return fmt.Errorf("canned reply %q: %w", name, err)
		}
		s.printf("canned reply %q created", name)
	}
	if name := "Request more info"; !have(name) {
		body := "Hi {{requester.name}},\n\nTo look into {{ticket.number}} we need a little more information:\n\n" +
			"- When did the problem start?\n- Which addresses or mailboxes are affected?\n- Any error message or bounce text you received\n\n" +
			"Thanks,\n{{agent.name}}"
		if _, err := s.c.Macros.Create(ctx, scope, macros.Input{Name: str(name), Body: &body, Visibility: str("public")}); err != nil {
			return fmt.Errorf("canned reply %q: %w", name, err)
		}
		s.printf("canned reply %q created", name)
	}
	return nil
}

// ensureTags adds the tags the demo tickets use to the tag catalog (only
// catalog tags can be applied to tickets).
func (s *seeder) ensureTags(ctx context.Context, scope *access.ProjectScope, ts []seedTicket) error {
	existing, err := s.tickets.Tags(ctx, scope)
	if err != nil {
		return err
	}
	for _, t := range ts {
		for _, name := range t.tags {
			if slices.ContainsFunc(existing, func(c tickets.TagCount) bool { return c.Tag == name }) {
				continue
			}
			tg, err := s.tickets.CreateTag(ctx, scope, name)
			if err != nil {
				return fmt.Errorf("tag %q: %w", name, err)
			}
			existing = append(existing, tg)
			s.printf("tag %q created", name)
		}
	}
	return nil
}

// ensureTicketExtras adds the demo tags to an existing seeded ticket that
// has none (re-runs on older seeds).
func (s *seeder) ensureTicketExtras(ctx context.Context, key string, t seedTicket) error {
	if len(t.tags) == 0 {
		return nil
	}
	scope, err := s.scopeFor(ctx, s.admin)
	if err != nil {
		return err
	}
	cur, err := s.tickets.Get(ctx, scope, key)
	if err != nil || cur.Archived() {
		return err
	}
	if len(cur.Tags) > 0 {
		return nil
	}
	tags := slices.Clone(t.tags)
	in := tickets.UpdateInput{Tags: &tags}
	s.clk.Set(s.opts.Now)
	if _, err := s.tickets.Update(ctx, scope, key, cur.Version, in); err != nil {
		return err
	}
	s.printf("ticket %s: demo tags added", key)
	return nil
}

// gateway finds or creates the project's demo integration principal.
func (s *seeder) gateway(ctx context.Context, scope *access.ProjectScope) (integrations.Integration, error) {
	const name = "Demo mail gateway (seed)"
	page, err := s.c.Integrations.List(ctx, scope, nil, 100)
	if err != nil {
		return integrations.Integration{}, err
	}
	for _, ig := range page.Items {
		if ig.Name == name {
			return ig, nil
		}
	}
	created, err := s.c.Integrations.Create(ctx, scope, integrations.CreateInput{Name: name,
		Scopes: []string{access.ScopeTicketsRead, access.ScopeTicketsCreate, access.ScopeTicketsReplyAsRequester}})
	if err != nil {
		return integrations.Integration{}, err
	}
	return created.Integration, nil
}

func (s *seeder) scopeFor(ctx context.Context, p access.Principal) (*access.ProjectScope, error) {
	return s.c.Access.Scope(ctx, p)
}

func (s *seeder) at(ago time.Duration) { s.clk.Set(s.opts.Now.Add(-ago)) }

func (s *seeder) seedTicket(ctx context.Context, requester access.Principal, t seedTicket,
	groups, contacts, types map[string]string, waiting string) error {
	reqScope, err := s.scopeFor(ctx, requester)
	if err != nil {
		return err
	}
	if err := s.c.TicketsRepo.EnsureNumberAtLeast(ctx, reqScope.Project.Id, t.number); err != nil {
		return err
	}
	first := t.messages[0]
	s.at(t.createdAgo)
	var attIDs []string
	if first.file != "" {
		a, err := s.tickets.Upload(ctx, reqScope, first.file, strings.NewReader(demoBounceReport()))
		if err != nil {
			return err
		}
		attIDs = []string{a.Id}
	}
	contactID, groupID, rtID := contacts[t.requester], groups[t.group], types[t.requestType]
	in := tickets.CreateInput{Subject: t.subject, Body: first.body, Requester: tickets.RequesterInput{ContactId: &contactID},
		GroupId: &groupID, RequestTypeId: &rtID, AttachmentIds: attIDs}
	if id, ok := s.orgs[emailDomain(t.requester)]; ok {
		in.OrganisationId = &id
	}
	if t.requested > 0 {
		rp := t.requested
		in.Priority = &rp
	}
	tk, err := s.tickets.Create(ctx, reqScope, in)
	if err != nil {
		return err
	}
	key := tk.Ref()
	adminScope, err := s.scopeFor(ctx, s.admin)
	if err != nil {
		return err
	}
	// The integration's view hides the version; continue with the staff view.
	if tk, err = s.tickets.Get(ctx, adminScope, key); err != nil {
		return err
	}
	if t.priority != tk.Priority {
		s.at(t.createdAgo - time.Minute)
		prio := t.priority
		if tk, err = s.tickets.Update(ctx, adminScope, key, tk.Version, tickets.UpdateInput{Priority: &prio}); err != nil {
			return err
		}
	}
	if len(t.tags) > 0 {
		s.at(t.createdAgo - time.Minute)
		tags := slices.Clone(t.tags)
		if tk, err = s.tickets.Update(ctx, adminScope, key, tk.Version, tickets.UpdateInput{Tags: &tags}); err != nil {
			return err
		}
	}
	for _, w := range t.watchers {
		sc, err := s.scopeFor(ctx, s.agents[w])
		if err != nil {
			return err
		}
		if _, err := s.tickets.Watch(ctx, sc, key, true); err != nil {
			return err
		}
	}
	var assigneeScope *access.ProjectScope
	if t.assignee != "" {
		if assigneeScope, err = s.scopeFor(ctx, s.agents[t.assignee]); err != nil {
			return err
		}
		s.at(t.createdAgo - 2*time.Minute)
		if tk, err = s.tickets.Claim(ctx, assigneeScope, key, tk.Version); err != nil {
			return err
		}
	}
	for _, m := range t.messages[1:] {
		s.at(m.ago)
		sc := reqScope
		if m.author != "contact" {
			if sc, err = s.scopeFor(ctx, s.agents[m.author]); err != nil {
				return err
			}
		}
		if _, err := s.tickets.PostMessage(ctx, sc, key, tickets.PostInput{Visibility: m.visibility, Body: m.body}); err != nil {
			return err
		}
	}
	if (t.status == "awaitingCustomer" || t.status == "resolved" || t.status == "onHold") && assigneeScope != nil {
		s.at(t.finalAgo)
		cur, err := s.tickets.Get(ctx, assigneeScope, key)
		if err != nil {
			return err
		}
		in := tickets.TransitionInput{Action: "resolve", Reason: "demo data"}
		switch t.status {
		case "awaitingCustomer":
			in = tickets.TransitionInput{Action: "setStatus", StatusId: &waiting, Reason: "waiting for the requester's confirmation"}
		case "onHold":
			reasons, err := s.c.HoldReasons.List(ctx, assigneeScope)
			if err != nil {
				return err
			}
			var rid string
			for _, r := range reasons {
				if r.Name == t.holdReason {
					rid = r.Id
				}
			}
			if rid == "" {
				return fmt.Errorf("hold reason %q does not exist; run init first", t.holdReason)
			}
			in = tickets.TransitionInput{Action: "hold", HoldReasonId: rid, Reason: "demo data"}
		}
		if _, err := s.tickets.Transition(ctx, assigneeScope, key, cur.Version, in); err != nil {
			return err
		}
	}
	s.printf("ticket %s %q: created", key, t.subject)
	return nil
}

func demoBounceReport() string {
	var b strings.Builder
	b.WriteString("Return-Path: <>\r\nFrom: Mail Delivery System <mailer-daemon@mx1.old-provider.net>\r\n")
	b.WriteString("Subject: Undelivered Mail Returned to Sender\r\nContent-Type: text/plain; charset=utf-8\r\n\r\n")
	b.WriteString("This is a demo bounce report generated by open-kase seed-demo.\r\n\r\n")
	for i := range 160 {
		fmt.Fprintf(&b, "<user%03d@acme-ltd.com>: host mx1.old-provider.net said: 550 5.1.1 recipient rejected\r\n", i)
	}
	return b.String()
}
