package runtime

// Pass-2 reliability review (REL): query-plan measurement of the hot read
// paths and background polls on a few thousand tickets. Skipped unless
// OPEN_KASE_REVIEW_PLANS=1 (it seeds data and takes a minute or two):
//
//	OPEN_KASE_TEST_MONGO_URI='mongodb://127.0.0.1:27017/?replicaSet=rs0' OPEN_KASE_REVIEW_PLANS=1 \
//	  go test -run TestReviewRelQueryPlans -timeout 20m -v ./runtime/
//
// It records every operation of each phase with the MongoDB profiler and
// prints, per query shape, the plan, keys and documents examined, documents
// returned and whether the sort ran in memory.

import (
	"context"
	"fmt"
	"math/rand/v2"
	"net/url"
	"os"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo/options"

	"github.com/arksnorman/open-kase/api/internal/access"
	"github.com/arksnorman/open-kase/api/internal/automation"
	"github.com/arksnorman/open-kase/api/internal/jobs"
	"github.com/arksnorman/open-kase/api/internal/mailer"
	"github.com/arksnorman/open-kase/api/internal/notifications"
	mongostore "github.com/arksnorman/open-kase/api/internal/persistence/mongo"
	"github.com/arksnorman/open-kase/api/internal/tickets"
)

// OC-OPS-08: lists that do not filter by status category (the All view for
// staff, an integration's ticket list, and the subject filter on them) must
// read tickets in index order and stop after the page, not read every
// ticket the caller may see and sort them in memory on every page.
func TestReviewAllViewReadsInIndexOrder(t *testing.T) {
	f := newTicketFixture(t)
	ctx := context.Background()
	p, err := f.c.Integrations.Authenticate(ctx, f.integration)
	if err != nil {
		t.Fatal(err)
	}
	intg, err := f.c.Access.Scope(ctx, p)
	if err != nil {
		t.Fatal(err)
	}
	const n, limit = 120, 10
	name, email := "Customer", "customer@example.com"
	for i := 0; i < n; i++ {
		group := f.triage
		if i%2 == 1 {
			group = f.delivery
		}
		if _, err := f.c.Tickets.Create(ctx, intg, tickets.CreateInput{Subject: fmt.Sprintf("Mailbox %d bounces", i), Body: "Bounced.",
			Requester: tickets.RequesterInput{Name: &name, Email: &email}, GroupId: &group}); err != nil {
			t.Fatal(err)
		}
	}
	db := f.coll("tickets").Database()
	// Count only this test's queries: pooled test databases keep
	// system.profile from earlier tests, and the server clock may differ
	// from this host's.
	var hello struct {
		LocalTime time.Time `bson:"localTime"`
	}
	if err := db.RunCommand(ctx, bson.D{{Key: "hello", Value: 1}}).Decode(&hello); err != nil {
		t.Fatal(err)
	}
	if err := db.RunCommand(ctx, bson.D{{Key: "profile", Value: 2}}).Err(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.RunCommand(context.Background(), bson.D{{Key: "profile", Value: 0}}).Err() })
	lim := fmt.Sprint(limit)
	for _, c := range []struct{ who, tok, query string }{
		{"admin", f.admin.Access, "limit=" + lim},
		{"admin, subject filter", f.admin.Access, "limit=" + lim + "&q=bounces"},
		{"agent in one of two groups", f.dana.Access, "limit=" + lim},
		{"agent in both groups", f.marco.Access, "limit=" + lim},
		{"integration", f.integration, "limit=" + lim},
	} {
		res := f.do("GET", "/api/v1/tickets?"+c.query, nil, bearer(c.tok)).expect(200)
		next, _ := res.JSON()["nextCursor"].(string)
		if len(res.Items()) != limit || next == "" {
			t.Fatalf("%s: %d items, next %q", c.who, len(res.Items()), next)
		}
		f.do("GET", "/api/v1/tickets?"+c.query+"&cursor="+url.QueryEscape(next), nil, bearer(c.tok)).expect(200)
	}
	if err := db.RunCommand(ctx, bson.D{{Key: "profile", Value: 0}}).Err(); err != nil {
		t.Fatal(err)
	}
	cur, err := db.Collection("system.profile").Find(ctx, bson.D{{Key: "ns", Value: db.Name() + ".tickets"},
		{Key: "command.find", Value: "tickets"}, {Key: "command.limit", Value: bson.D{{Key: "$exists", Value: true}}},
		{Key: "ts", Value: bson.D{{Key: "$gte", Value: hello.LocalTime}}},
		{Key: "$or", Value: bson.A{bson.D{{Key: "command.sort.updatedAt", Value: -1}}, bson.D{{Key: "command.sort.publicUpdatedAt", Value: -1}}}}})
	if err != nil {
		t.Fatal(err)
	}
	var prof []bson.M
	if err := cur.All(ctx, &prof); err != nil {
		t.Fatal(err)
	}
	if len(prof) != 10 {
		t.Fatalf("profiled %d list queries, want 10", len(prof))
	}
	for _, p := range prof {
		docs, _ := p["docsExamined"].(int32)
		sorted, _ := p["hasSortStage"].(bool)
		// A restricted agent sees half the tickets: up to about two pages of
		// documents per page is expected; the whole project (120) is not.
		if sorted || docs > 4*(limit+1) {
			t.Errorf("list query examined %d documents (in-memory sort %v) for a page of %d: plan %v filter %v",
				docs, sorted, limit, p["planSummary"], toM(p["command"])["filter"])
		}
	}
	// The cursor still pages through ties on the sort field exactly once
	// each: give 30 tickets the same update time and walk every page.
	tie := time.Now().UTC().Truncate(time.Millisecond)
	idc, err := f.coll("tickets").Find(ctx, bson.D{}, options.Find().SetProjection(bson.D{{Key: "_id", Value: 1}}).SetLimit(30))
	if err != nil {
		t.Fatal(err)
	}
	var idDocs []struct {
		Id bson.ObjectID `bson:"_id"`
	}
	if err := idc.All(ctx, &idDocs); err != nil {
		t.Fatal(err)
	}
	var tied bson.A
	for _, d := range idDocs {
		tied = append(tied, d.Id)
	}
	if _, err := f.coll("tickets").UpdateMany(ctx, bson.D{{Key: "_id", Value: bson.D{{Key: "$in", Value: tied}}}},
		bson.D{{Key: "$set", Value: bson.D{{Key: "updatedAt", Value: tie}, {Key: "publicUpdatedAt", Value: tie}}}}); err != nil {
		t.Fatal(err)
	}
	for _, tok := range []string{f.admin.Access, f.integration} {
		seen, cursor := map[string]bool{}, ""
		for page := 0; ; page++ {
			path := "/api/v1/tickets?limit=7"
			if cursor != "" {
				path += "&cursor=" + url.QueryEscape(cursor)
			}
			res := f.do("GET", path, nil, bearer(tok)).expect(200)
			for _, it := range res.Items() {
				k := refOf(it)
				if seen[k] {
					t.Fatalf("page %d repeated %s", page, k)
				}
				seen[k] = true
			}
			if cursor, _ = res.JSON()["nextCursor"].(string); cursor == "" {
				break
			}
		}
		if len(seen) != n {
			t.Fatalf("paged through %d tickets, want %d", len(seen), n)
		}
	}
}

func TestReviewRelQueryPlans(t *testing.T) {
	if os.Getenv("OPEN_KASE_REVIEW_PLANS") == "" {
		t.Skip("set OPEN_KASE_REVIEW_PLANS=1 to record query plans")
	}
	nTickets := envInt("OPEN_KASE_REVIEW_PLANS_TICKETS", 3000)
	ctx := context.Background()
	f := newNotifyFixture(t)
	admin := f.admin
	billing := f.do("POST", "/api/v1/groups", map[string]any{"name": "Billing"}, bearer(admin.Access)).expect(201).Str("id")
	f.do("PUT", "/api/v1/groups/"+billing+"/members/"+f.marco.UserId, nil, bearer(admin.Access)).expect(200)
	f.do("PUT", "/api/v1/sla", testPolicy(), bearer(admin.Access)).expect(200)
	f.do("PUT", "/api/v1/automation", map[string]any{"archiveAfterResolvedDays": 3,
		"customerFollowupAfterHours": 24, "maxReminders": 2, "reminderCooldownHours": 24, "autoResolveAfterHours": 48},
		bearer(admin.Access), header("If-Match", "0")).expect(200)
	st := f.statusIDs()
	scopeOf := func(tok string, integration bool) *access.ProjectScope {
		var p access.Principal
		var err error
		if integration {
			p, err = f.c.Integrations.Authenticate(ctx, tok)
		} else {
			p, err = f.c.Auth.Authenticate(ctx, tok)
		}
		if err != nil {
			t.Fatal(err)
		}
		sc, err := f.c.Access.Scope(ctx, p)
		if err != nil {
			t.Fatal(err)
		}
		return sc
	}
	intg, marco, dana := scopeOf(f.integration, true), scopeOf(f.marco.Access, false), scopeOf(f.dana.Access, false)
	groups := []string{f.triage, f.delivery, billing}

	// ---- Seed through the services ----
	start := time.Now()
	var created, posted atomic.Int64
	var seedErr atomic.Value
	keys := make([]string, nTickets)
	work := make(chan int)
	var wg sync.WaitGroup
	for w := 0; w < 4; w++ {
		wg.Go(func() {
			for i := range work {
				r := rand.New(rand.NewPCG(uint64(i), 11))
				z := rand.NewZipf(r, 1.1, 2, uint64(len(loadVocab)-1))
				prio := []int{1, 2, 3, 3, 3, 4}[r.IntN(6)]
				group := groups[i%3]
				email := fmt.Sprintf("customer%d@cust%d.example", r.IntN(800), r.IntN(20))
				name := "Customer"
				tk, err := f.c.Tickets.Create(ctx, intg, tickets.CreateInput{Subject: loadText(r, z, 4+r.IntN(5)),
					Body: loadText(r, z, 20+r.IntN(60)), Requester: tickets.RequesterInput{Name: &name, Email: &email},
					Priority: &prio, GroupId: &group})
				if err != nil {
					seedErr.Store(err)
					continue
				}
				created.Add(1)
				keys[i] = tnum(tk)
				for m := 0; m < r.IntN(8); m++ {
					sc, vis := marco, tickets.VisibilityPublic
					switch k := r.IntN(10); {
					case k < 3:
						sc = intg
					case k < 5:
						vis = tickets.VisibilityAgent
					}
					if _, err := f.c.Tickets.PostMessage(ctx, sc, tnum(tk), tickets.PostInput{Visibility: vis, Body: loadText(r, z, 10+r.IntN(40))}); err != nil {
						seedErr.Store(err)
						break
					}
					posted.Add(1)
				}
				claimer := marco
				if group == f.triage && r.IntN(2) == 0 {
					claimer = dana
				}
				cur, err := f.c.Tickets.Get(ctx, claimer, tnum(tk))
				if err != nil {
					seedErr.Store(err)
					continue
				}
				if r.IntN(10) < 6 {
					if cur, err = f.c.Tickets.Claim(ctx, claimer, tnum(tk), cur.Version); err != nil {
						seedErr.Store(err)
						continue
					}
				}
				switch k := r.IntN(20); {
				case k < 4: // resolved
					_, err = f.c.Tickets.Transition(ctx, claimer, tnum(tk), cur.Version, tickets.TransitionInput{Action: "resolve"})
				case k < 6: // archived
					if cur, err = f.c.Tickets.Transition(ctx, claimer, tnum(tk), cur.Version, tickets.TransitionInput{Action: "resolve"}); err == nil {
						_, err = f.c.Tickets.Transition(ctx, claimer, tnum(tk), cur.Version, tickets.TransitionInput{Action: "archive"})
					}
				case k < 9: // waiting for the customer
					id := st["waitingCustomer"]
					_, err = f.c.Tickets.Transition(ctx, claimer, tnum(tk), cur.Version, tickets.TransitionInput{Action: "setStatus", StatusId: &id})
				}
				if err != nil {
					seedErr.Store(fmt.Errorf("transition %s: %w", tnum(tk), err))
				}
				if group == f.triage && r.IntN(10) == 0 {
					if _, err := f.c.Tickets.Watch(ctx, dana, tnum(tk), true); err != nil {
						seedErr.Store(err)
					}
				}
			}
		})
	}
	for i := 0; i < nTickets; i++ {
		work <- i
	}
	close(work)
	wg.Wait()
	if err, _ := seedErr.Load().(error); err != nil {
		t.Logf("seed error (last): %v", err)
	}
	t.Logf("seeded %d tickets, %d messages in %s", created.Load(), created.Load()+posted.Load(), time.Since(start).Round(time.Second))

	db := f.coll("tickets").Database()
	_ = db.Collection("system.profile").Drop(ctx)
	if err := db.CreateCollection(ctx, "system.profile", options.CreateCollection().SetCapped(true).SetSizeInBytes(64<<20)); err != nil {
		t.Fatal(err)
	}
	if err := db.RunCommand(ctx, bson.D{{Key: "profile", Value: 2}}).Err(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.RunCommand(context.Background(), bson.D{{Key: "profile", Value: 0}}).Err() })
	var last time.Time
	report := func(phase string) {
		t.Helper()
		time.Sleep(50 * time.Millisecond)
		cur, err := db.Collection("system.profile").Find(ctx, bson.D{{Key: "ts", Value: bson.D{{Key: "$gt", Value: last}}}},
			options.Find().SetSort(bson.D{{Key: "ts", Value: 1}}))
		if err != nil {
			t.Fatal(err)
		}
		var prof []bson.M
		if err := cur.All(ctx, &prof); err != nil {
			t.Fatal(err)
		}
		type agg struct {
			n                       int
			keys, docs, ret, ms     float64
			maxKeys, maxDocs, sorts float64
		}
		rows := map[string]*agg{}
		num := func(p bson.M, k string) float64 {
			switch v := p[k].(type) {
			case int32:
				return float64(v)
			case int64:
				return float64(v)
			case float64:
				return v
			}
			return 0
		}
		for _, p := range prof {
			if ts, ok := p["ts"].(bson.DateTime); ok && ts.Time().After(last) {
				last = ts.Time()
			}
			ns, _ := p["ns"].(string)
			col := ns[strings.LastIndex(ns, ".")+1:]
			if strings.HasPrefix(col, "system.") || col == "$cmd" {
				continue
			}
			cmd := toM(p["command"])
			kind := ""
			for _, k := range []string{"find", "aggregate", "count", "findAndModify", "update", "distinct", "delete", "insert", "getMore"} {
				if _, ok := cmd[k]; ok {
					kind = k
					break
				}
			}
			if kind == "" || kind == "insert" || kind == "getMore" {
				continue
			}
			shape := ""
			switch {
			case toM(cmd["filter"]) != nil:
				shape = shapeOf(toM(cmd["filter"]))
			case toM(cmd["query"]) != nil:
				shape = shapeOf(toM(cmd["query"]))
			default:
				if pl, ok := cmd["pipeline"].(bson.A); ok && len(pl) > 0 {
					if m := toM(toM(pl[0])["$match"]); m != nil {
						shape = "$match " + shapeOf(m)
					}
				}
				if u, ok := cmd["updates"].(bson.A); ok && len(u) > 0 {
					shape = shapeOf(toM(toM(u[0])["q"]))
				}
			}
			if s := toM(cmd["sort"]); s != nil {
				shape += " sort " + shapeOf(s)
			}
			plan, _ := p["planSummary"].(string)
			key := fmt.Sprintf("%-22s %-13s %s\n      plan=%s", col, kind, shape, plan)
			a := rows[key]
			if a == nil {
				a = &agg{}
				rows[key] = a
			}
			a.n++
			k, d := num(p, "keysExamined"), num(p, "docsExamined")
			a.keys += k
			a.docs += d
			a.ret += num(p, "nreturned")
			a.ms += num(p, "millis")
			a.maxKeys, a.maxDocs = max(a.maxKeys, k), max(a.maxDocs, d)
			if v, _ := p["hasSortStage"].(bool); v {
				a.sorts++
			}
		}
		ks := make([]string, 0, len(rows))
		for k := range rows {
			ks = append(ks, k)
		}
		sort.Strings(ks)
		var b strings.Builder
		for _, k := range ks {
			a := rows[k]
			n := float64(a.n)
			flag := ""
			if strings.Contains(k, "COLLSCAN") {
				flag += " COLLSCAN"
			}
			if a.sorts > 0 {
				flag += fmt.Sprintf(" IN-MEMORY-SORT(%d/%d)", int(a.sorts), a.n)
			}
			fmt.Fprintf(&b, "  %s\n      n=%d keys=%.0f (max %.0f) docs=%.0f (max %.0f) returned=%.1f ms=%.1f%s\n", k, a.n,
				a.keys/n, a.maxKeys, a.docs/n, a.maxDocs, a.ret/n, a.ms/n, flag)
		}
		t.Logf("==== %s (%d operations)\n%s", phase, len(prof), b.String())
	}
	report("baseline")

	// ---- Queue lists and counts ----
	views := []string{"all", "myWork", "unassigned", "watching", "awaitingCustomer", "onHold", "dueSoon", "resolved", "archived"}
	for _, who := range []struct {
		name string
		s    session
	}{{"dana (one group of three)", f.dana}, {"marco (all three groups)", f.marco}, {"admin (all tickets)", admin}} {
		for _, v := range views {
			res := f.do("GET", "/api/v1/tickets?limit=50&view="+v, nil, bearer(who.s.Access))
			if res.Code != 200 {
				t.Logf("%s view %s: %d %s", who.name, v, res.Code, res.Body)
				continue
			}
			if c, _ := res.JSON()["nextCursor"].(string); c != "" {
				f.do("GET", "/api/v1/tickets?limit=50&view="+v+"&cursor="+url.QueryEscape(c), nil, bearer(who.s.Access)).expect(200)
			}
		}
		report("queue lists, page 1 and 2: " + who.name)
		f.do("GET", "/api/v1/queue-counts", nil, bearer(who.s.Access)).expect(200)
		report("queue counts: " + who.name)
	}

	res := f.do("GET", "/api/v1/tickets?limit=50", nil, bearer(f.integration)).expect(200)
	if c, _ := res.JSON()["nextCursor"].(string); c != "" {
		f.do("GET", "/api/v1/tickets?limit=50&cursor="+url.QueryEscape(c), nil, bearer(f.integration)).expect(200)
	}
	report("integration ticket list, page 1 and 2")

	// ---- Ticket detail and messages (first page and a cursor page) ----
	var busy []string
	for _, k := range keys {
		if k == "" {
			continue
		}
		if n := f.messageCount(k); n >= 6 {
			busy = append(busy, k)
		}
		if len(busy) == 10 {
			break
		}
	}
	report("(messageCount helper)")
	for _, k := range busy {
		f.do("GET", "/api/v1/tickets/"+k, nil, bearer(f.marco.Access)).expect(200)
	}
	report("ticket detail x10 (marco)")
	for _, k := range busy {
		res := f.do("GET", "/api/v1/tickets/"+k+"/messages?limit=3", nil, bearer(f.marco.Access)).expect(200)
		if c, _ := res.JSON()["nextCursor"].(string); c != "" {
			f.do("GET", "/api/v1/tickets/"+k+"/messages?limit=3&cursor="+url.QueryEscape(c), nil, bearer(f.marco.Access)).expect(200)
		}
	}
	report("messages page 1 + cursor page x10 (marco)")

	// ---- Search (common and rare terms; both passes for a restricted agent) ----
	common, rare := loadVocab[0], loadVocab[1500]
	for _, who := range []struct {
		name string
		s    session
	}{{"dana", f.dana}, {"marco", f.marco}, {"admin", admin}} {
		for _, q := range []string{common, rare} {
			f.do("GET", "/api/v1/search?q="+q, nil, bearer(who.s.Access)).expect(200)
		}
		report("search common+rare: " + who.name)
		f.do("GET", "/api/v1/tickets?limit=50&q="+common, nil, bearer(who.s.Access)).expect(200)
		report("subject filter (q=): " + who.name)
	}

	// ---- Live update poll ----
	src := mongostore.NewLiveSource(f.c.Store)
	projectID := intg.Project.Id
	if _, err := src.ChangesSince(ctx, []string{projectID}, time.Now().Add(-10*time.Second), 200); err != nil {
		t.Fatal(err)
	}
	if _, err := src.ChangesSince(ctx, []string{projectID}, start, 200); err != nil {
		t.Fatal(err)
	}
	report("live poll (last 10 s; since seed start)")

	// ---- Background work: outbox dispatch, job acquisition, SLA checks, automation scans ----
	d := f.c.newDispatcher("plan-dispatcher")
	rn := f.c.newRunner("plan-worker", 1)
	for i := 0; i < 150; i++ {
		if _, err := d.DispatchOnce(ctx); err != nil {
			t.Fatal(err)
		}
	}
	report("outbox dispatch x150")
	for i := 0; i < 150; i++ {
		if _, err := rn.RunOnce(ctx); err != nil {
			t.Fatal(err)
		}
	}
	report("job acquisition + handlers x150")
	if _, err := f.c.Automation.EnsureScheduled(ctx); err != nil {
		t.Fatal(err)
	}
	report("automation EnsureScheduled")
	for i := 0; i < 20; i++ {
		if worked, err := rn.RunOnce(ctx); err != nil {
			t.Fatal(err)
		} else if !worked {
			break
		}
	}
	report("automation scans (and remaining due jobs) x20")
	var stats bson.M
	_ = db.RunCommand(ctx, bson.D{{Key: "collStats", Value: "jobs"}}).Decode(&stats)
	t.Logf("jobs collection: count=%v", stats["count"])
}

// blockingMailer holds every send until its context ends (a slow SMTP
// server during a shutdown).
type blockingMailer struct{ entered chan struct{} }

func (b *blockingMailer) Send(ctx context.Context, _ mailer.Message) error {
	select {
	case b.entered <- struct{}{}:
	default:
	}
	<-ctx.Done()
	return &mailer.Error{Class: mailer.ClassTemporary, Phase: mailer.PhaseConnect, Err: ctx.Err()}
}

// releasedMailer holds every send until release is closed (then the send
// succeeds) or its context ends.
type releasedMailer struct{ entered, release chan struct{} }

func (m *releasedMailer) Send(ctx context.Context, _ mailer.Message) error {
	select {
	case m.entered <- struct{}{}:
	default:
	}
	select {
	case <-m.release:
		return nil
	case <-ctx.Done():
		return &mailer.Error{Class: mailer.ClassTemporary, Phase: mailer.PhaseConnect, Err: ctx.Err()}
	}
}

type shutdownJob struct {
	State    string `bson:"state"`
	Attempts int    `bson:"attempts"`
	History  []struct {
		Outcome string `bson:"outcome"`
		Error   string `bson:"error"`
	} `bson:"history"`
}

// P-ASYNC-4 (implemented in pass 3): at shutdown the runner takes no new
// job, lets a send in progress finish within the grace period, and only
// then cancels it (recorded as a failed attempt with a retry delay). It
// used to cancel the send at once, so every deployment or restart turned
// sends in progress into failed attempts (dead-lettered on a last attempt).
func TestShutdownLetsAnAttemptInProgressFinish(t *testing.T) {
	f := newNotifyFixture(t)
	ctx := context.Background()
	rm := &releasedMailer{entered: make(chan struct{}, 1), release: make(chan struct{})}
	f.c.Mailers = func(mailer.Config) (mailer.Mailer, error) { return rm, nil }
	f.createTicket(f.integration, map[string]any{})
	f.createTicket(f.integration, map[string]any{}) // more email jobs than the one worker can start
	for {
		ok, err := f.c.newDispatcher("d").DispatchOnce(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if !ok {
			break
		}
	}
	emails := func(filter bson.D) []shutdownJob {
		t.Helper()
		cur, err := f.coll("jobs").Find(ctx, append(bson.D{{Key: "kind", Value: notifications.KindEmail}}, filter...))
		if err != nil {
			t.Fatal(err)
		}
		var out []shutdownJob
		if err := cur.All(ctx, &out); err != nil {
			t.Fatal(err)
		}
		return out
	}
	queued := len(emails(bson.D{{Key: "state", Value: "queued"}}))
	if queued < 2 {
		t.Fatalf("%d queued email jobs, want at least 2", queued)
	}

	// Within the grace period: the send in progress completes.
	f.c.JobGrace = 10 * time.Second
	rctx, stop := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() { f.c.newRunner("shutting-down", 1).Run(rctx); close(done) }()
	select {
	case <-rm.entered:
	case <-time.After(10 * time.Second):
		t.Fatal("no send started")
	}
	stop() // SIGTERM: serve cancels the workers' context
	time.Sleep(300 * time.Millisecond)
	close(rm.release) // the SMTP server answers
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("runner did not stop after the send finished")
	}
	started := emails(bson.D{{Key: "attempts", Value: bson.D{{Key: "$gte", Value: 1}}}})
	if len(started) != 1 || started[0].State != "succeeded" || len(started[0].History) != 1 || started[0].History[0].Outcome != "succeeded" {
		t.Fatalf("jobs started around the shutdown: %+v (want exactly one, succeeded)", started)
	}

	// Beyond the grace period the attempt is cancelled and retried later.
	f.c.JobGrace = 300 * time.Millisecond
	bm := &blockingMailer{entered: make(chan struct{}, 1)}
	f.c.Mailers = func(mailer.Config) (mailer.Mailer, error) { return bm, nil }
	rctx, stop = context.WithCancel(ctx)
	done = make(chan struct{})
	go func() { f.c.newRunner("shutting-down-2", 1).Run(rctx); close(done) }()
	select {
	case <-bm.entered:
	case <-time.After(10 * time.Second):
		t.Fatal("no send started")
	}
	stop()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("runner did not stop after the grace period")
	}
	cut := emails(bson.D{{Key: "history.outcome", Value: "retry"}})
	if len(cut) != 1 || cut[0].State != "queued" || !strings.Contains(cut[0].History[0].Error, "context canceled") {
		t.Fatalf("attempt cut by the end of the grace period: %+v", cut)
	}
}

// OC-ASYNC-08: an automation action job that became a no-op kept its
// single-use key, so the same action for the same waiting period could
// never be scheduled again. Here the administrator switches the policy off
// between a scan and its action, then on again: the due reminder must still
// be sent, exactly once.
func TestAutomationActionKeyReusedAfterNoOp(t *testing.T) {
	f := newAutomationFixture(t)
	path := "/api/v1/automation"
	on := map[string]any{"archiveAfterResolvedDays": 0, "customerFollowupAfterHours": 24, "maxReminders": 2,
		"reminderCooldownHours": 24, "autoResolveAfterHours": 0}
	off := map[string]any{"archiveAfterResolvedDays": 0, "customerFollowupAfterHours": 0, "maxReminders": 2,
		"reminderCooldownHours": 24, "autoResolveAfterHours": 0}
	f.do("PUT", path, on, bearer(f.admin.Access), header("If-Match", "0")).expect(200)
	key := f.createTicket(f.integration, map[string]any{}).Ref()
	f.waiting(f.dana.Access, key)
	f.runDue()
	// 25 hours later a scan finds the reminder due and queues its action.
	f.clk.Advance(25 * time.Hour)
	scans := jobs.NewRunner(f.c.Jobs, jobs.Config{Owner: "scan-only", Now: f.clk.Now, Backoff: f.c.JobBackoff})
	scans.Handle(automation.KindScan, f.c.Automation.HandleScan)
	for i := 0; i < 20; i++ {
		if worked, err := scans.RunOnce(context.Background()); err != nil {
			t.Fatal(err)
		} else if !worked {
			break
		}
	}
	queued, err := f.coll("jobs").CountDocuments(context.Background(), bson.D{{Key: "kind", Value: automation.KindAct}, {Key: "state", Value: "queued"}})
	if err != nil || queued != 1 {
		t.Fatalf("queued actions: %d %v", queued, err)
	}
	// Before it runs, the administrator pauses automation, then resumes it.
	f.do("PUT", path, off, bearer(f.admin.Access), header("If-Match", "1")).expect(200)
	f.runDue() // the action finds nothing due: a no-op
	f.do("PUT", path, on, bearer(f.admin.Access), header("If-Match", "2")).expect(200)
	for i := 0; i < 4; i++ { // the next scan windows
		f.advance(16 * time.Minute)
	}
	if n := f.reminders(key); n != 1 {
		t.Errorf("reminders after automation was resumed: %d, want 1 (the due reminder was blocked by its used action key)", n)
	}
}
