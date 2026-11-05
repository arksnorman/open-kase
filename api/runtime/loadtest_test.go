package runtime

// Load test (Milestone 10). Skipped unless OPEN_KASE_LOADTEST=1:
//
//	OPEN_KASE_TEST_MONGO_URI='mongodb://127.0.0.1:27017/?replicaSet=rs0' OPEN_KASE_LOADTEST=1 \
//	  go test -run TestLoadQueues -timeout 60m -v ./runtime/
//
// It seeds a throwaway database through the application services (so every
// document, index entry, audit event and outbox event is what production
// writes), then drives the real HTTP router over a loopback HTTP server at a
// fixed concurrency and reports latency percentiles. The MongoDB profiler
// records the plans of the queries the requests issue. Knobs:
// OPEN_KASE_LOADTEST_TICKETS (20000), _MESSAGES (200000), _CONCURRENCY
// (20), _REQUESTS (per scenario, 2000), _OUT (write the JSON report here).
// The database is dropped afterwards.

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"math/rand/v2"
	"net/http"
	"net/http/httptest"
	"os"
	"runtime"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo/options"

	"github.com/arksnorman/open-kase/api/internal/access"
	"github.com/arksnorman/open-kase/api/internal/tickets"
	transporthttp "github.com/arksnorman/open-kase/api/internal/transport/http"
	"github.com/arksnorman/open-kase/api/package/logger"
)

func envInt(name string, def int) int {
	if v, err := strconv.Atoi(os.Getenv(name)); err == nil && v > 0 {
		return v
	}
	return def
}

type loadProject struct {
	key     string
	share   float64
	groups  []string
	agents  []session
	scopes  []*access.ProjectScope // agent scopes (for seeding)
	intg    *access.ProjectScope   // integration scope (creates tickets, requester replies)
	tickets []string               // keys
}

type scenarioResult struct {
	Name        string  `json:"name"`
	Requests    int     `json:"requests"`
	Errors      int     `json:"errors"`
	Conflicts   int     `json:"conflicts,omitempty"`
	P50ms       float64 `json:"p50Ms"`
	P95ms       float64 `json:"p95Ms"`
	P99ms       float64 `json:"p99Ms"`
	MaxMs       float64 `json:"maxMs"`
	Throughput  float64 `json:"requestsPerSecond"`
	Concurrency int     `json:"concurrency"`
}

type planSummary struct {
	Collection  string  `json:"collection"`
	Op          string  `json:"op"`
	Shape       string  `json:"shape"`
	Plan        string  `json:"plan"`
	Count       int     `json:"count"`
	AvgMs       float64 `json:"avgMs"`
	AvgKeys     float64 `json:"avgKeysExamined"`
	AvgDocs     float64 `json:"avgDocsExamined"`
	AvgReturned float64 `json:"avgReturned"`
}

var loadVocab = func() []string {
	r := rand.New(rand.NewPCG(1, 2))
	syll := []string{"ka", "lo", "mi", "ne", "su", "ta", "ri", "po", "ve", "zu", "an", "el", "or", "ix", "um", "da", "fe", "gi", "ho", "ju"}
	out := make([]string, 3000)
	for i := range out {
		var b strings.Builder
		for j := 0; j < 2+r.IntN(3); j++ {
			b.WriteString(syll[r.IntN(len(syll))])
		}
		out[i] = b.String()
	}
	return out
}()

// loadText draws Zipf-distributed words, so a few words are common and
// most are rare, like real text.
func loadText(r *rand.Rand, z *rand.Zipf, words int) string {
	var b strings.Builder
	for i := 0; i < words; i++ {
		if i > 0 {
			b.WriteByte(' ')
		}
		b.WriteString(loadVocab[z.Uint64()])
	}
	return b.String()
}

func TestLoadQueues(t *testing.T) {
	if os.Getenv("OPEN_KASE_LOADTEST") == "" {
		t.Skip("set OPEN_KASE_LOADTEST=1 to run the load test")
	}
	nTickets := envInt("OPEN_KASE_LOADTEST_TICKETS", 20000)
	nMessages := envInt("OPEN_KASE_LOADTEST_MESSAGES", 200000)
	conc := envInt("OPEN_KASE_LOADTEST_CONCURRENCY", 20)
	perScenario := envInt("OPEN_KASE_LOADTEST_REQUESTS", 2000)
	ctx := context.Background()

	h := newHarness(t)
	admin := h.admin()
	projects := []*loadProject{{key: "LDA", share: 0.5}, {key: "LDB", share: 0.3}, {key: "LDC", share: 0.2}}
	agentNo := 0
	for _, p := range projects {
		// Three segments of the single project, each with its own two
		// groups, agents and integration (formerly three projects).
		g1 := h.do("POST", "/api/v1/groups", map[string]any{"name": "Load " + p.key}, bearer(admin.Access)).expect(201).Str("id")
		g2 := h.do("POST", "/api/v1/groups", map[string]any{"name": "Second line " + p.key}, bearer(admin.Access)).expect(201).Str("id")
		p.groups = []string{g1, g2}
		for i := 0; i < 8; i++ {
			agentNo++
			email := fmt.Sprintf("agent%02d@load.example", agentNo)
			uid := h.createUser(admin, email, fmt.Sprintf("Agent %02d", agentNo))
			groups := []string{g1}
			if i%2 == 1 {
				groups = append(groups, g2)
			}
			for _, g := range groups {
				h.do("PUT", "/api/v1/groups/"+g+"/members/"+uid, nil, bearer(admin.Access)).expect(200)
			}
			s := h.login(email, agentPassword)
			p.agents = append(p.agents, s)
			pr, err := h.c.Auth.Authenticate(ctx, s.Access)
			if err != nil {
				t.Fatal(err)
			}
			sc, err := h.c.Access.Scope(ctx, pr)
			if err != nil {
				t.Fatal(err)
			}
			p.scopes = append(p.scopes, sc)
		}
		tok := h.do("POST", "/api/v1/integrations", map[string]any{"name": "Gateway",
			"scopes": []string{"tickets:read", "tickets:create", "tickets:replyAsRequester"}}, bearer(admin.Access)).expect(201).Str("token")
		pr, err := h.c.Integrations.Authenticate(ctx, tok)
		if err != nil {
			t.Fatal(err)
		}
		if p.intg, err = h.c.Access.Scope(ctx, pr); err != nil {
			t.Fatal(err)
		}
	}

	// ---- Seed through the services ----
	seedStart := time.Now()
	type job struct {
		p    *loadProject
		msgs int
		seed uint64
	}
	jobs := make(chan job, 256)
	var created, posted, claimed atomic.Int64
	var seedErr atomic.Value
	var mu sync.Mutex
	workers := max(8, runtime.NumCPU()*4)
	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		wg.Go(func() {
			for j := range jobs {
				r := rand.New(rand.NewPCG(j.seed, 7))
				z := rand.NewZipf(r, 1.1, 2, uint64(len(loadVocab)-1))
				prio := []int{1, 2, 2, 3, 3, 3, 3, 3, 3, 4}[r.IntN(10)]
				group := j.p.groups[r.IntN(2)]
				reqEmail := fmt.Sprintf("customer%d@cust%d.example", r.IntN(4000), r.IntN(40))
				name := "Customer"
				tk, err := h.c.Tickets.Create(ctx, j.p.intg, tickets.CreateInput{Subject: loadText(r, z, 4+r.IntN(6)),
					Body: loadText(r, z, 30+r.IntN(120)), Requester: tickets.RequesterInput{Name: &name, Email: &reqEmail},
					Priority: &prio, GroupId: &group})
				if err != nil {
					seedErr.Store(err)
					continue
				}
				created.Add(1)
				mu.Lock()
				j.p.tickets = append(j.p.tickets, tnum(tk))
				mu.Unlock()
				for m := 1; m < j.msgs; m++ {
					var eligible []*access.ProjectScope
					for _, a := range j.p.scopes {
						if a.InGroup(group) {
							eligible = append(eligible, a)
						}
					}
					sc, vis := eligible[r.IntN(len(eligible))], tickets.VisibilityPublic
					switch k := r.IntN(10); {
					case k < 3:
						sc = j.p.intg // requester reply
					case k < 5:
						vis = tickets.VisibilityAgent
					}
					if _, err := h.c.Tickets.PostMessage(ctx, sc, tnum(tk), tickets.PostInput{Visibility: vis, Body: loadText(r, z, 15+r.IntN(80))}); err != nil {
						seedErr.Store(fmt.Errorf("post on %s: %w", tnum(tk), err))
						break
					}
					posted.Add(1)
				}
				// About half of the tickets are claimed by an eligible agent.
				if r.IntN(2) == 0 {
					for _, sc := range j.p.scopes {
						if sc.InGroup(group) {
							cur, err := h.c.Tickets.Get(ctx, sc, tnum(tk))
							if err == nil {
								if _, err := h.c.Tickets.Claim(ctx, sc, tnum(tk), cur.Version); err == nil {
									claimed.Add(1)
								}
							}
							break
						}
					}
				}
			}
		})
	}
	extra := nMessages - nTickets // the create call posts the first message
	r := rand.New(rand.NewPCG(42, 42))
	plan := make([]int, nTickets)
	for i := range plan {
		plan[i] = 1
	}
	// Spread the remaining messages with log-normal weights: most tickets
	// get a handful of messages, a few get many times the mean.
	weights := make([]float64, nTickets)
	var sum float64
	for i := range weights {
		weights[i] = math.Exp(r.NormFloat64() * 0.9)
		sum += weights[i]
	}
	given := 0
	for i := range plan {
		n := int(math.Floor(weights[i] / sum * float64(extra)))
		plan[i] += n
		given += n
	}
	for i := 0; given < extra; i, given = (i+1)%nTickets, given+1 {
		plan[i]++
	}
	progressDone := make(chan struct{})
	go func() {
		tk := time.NewTicker(30 * time.Second)
		defer tk.Stop()
		for {
			select {
			case <-progressDone:
				return
			case <-tk.C:
				t.Logf("seeding: %d tickets, %d messages, %s", created.Load(), posted.Load()+created.Load(), time.Since(seedStart).Round(time.Second))
			}
		}
	}()
	for i := 0; i < nTickets; i++ {
		u := r.Float64()
		p := projects[0]
		if u > projects[0].share {
			p = projects[1]
			if u > projects[0].share+projects[1].share {
				p = projects[2]
			}
		}
		jobs <- job{p: p, msgs: plan[i], seed: uint64(i)}
	}
	close(jobs)
	wg.Wait()
	close(progressDone)
	if err, _ := seedErr.Load().(error); err != nil {
		t.Logf("seed error (last): %v", err)
	}
	seedDur := time.Since(seedStart)
	maxMsgs := slices.Max(plan)
	t.Logf("seeded %d tickets, %d messages (max %d on one ticket), %d claimed in %s (%d workers)",
		created.Load(), created.Load()+posted.Load(), maxMsgs, claimed.Load(), seedDur.Round(time.Second), workers)

	// ---- Drive the HTTP API ----
	router, err := transporthttp.NewRouter(transporthttp.Deps{Logger: logger.Discard(), Readiness: transporthttp.NewReadiness(logger.Discard()),
		Services: h.c.httpServices(), RateLimits: map[string]transporthttp.RateLimit{
			transporthttp.LimitSearch: {PerMinute: -1}, transporthttp.LimitTicketCreate: {PerMinute: -1}, transporthttp.LimitMessagePost: {PerMinute: -1}}})
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(router)
	defer srv.Close()
	client := &http.Client{Transport: &http.Transport{MaxIdleConnsPerHost: conc * 2}, Timeout: 30 * time.Second}

	db := h.coll("tickets").Database()
	// Level 2 profiles every operation of this database only. slowms is
	// server-wide (it also controls the mongod log), so it is never
	// changed here.
	setProfile := func(level int) {
		if err := db.RunCommand(ctx, bson.D{{Key: "profile", Value: level}}).Err(); err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(func() { _ = db.RunCommand(context.Background(), bson.D{{Key: "profile", Value: 0}}).Err() })
	// A profile collection large enough to keep every operation of the run
	// (the default is 1 MiB, which keeps only the last few hundred).
	_ = db.Collection("system.profile").Drop(ctx)
	if err := db.CreateCollection(ctx, "system.profile", options.CreateCollection().SetCapped(true).SetSizeInBytes(512<<20)); err != nil {
		t.Fatal(err)
	}
	main := projects[0]
	var results []scenarioResult
	run := func(name string, n int, req func(worker, i int) (*http.Request, error), ok func(*http.Response) (bool, bool)) {
		lat := make([]time.Duration, 0, n)
		var lmu sync.Mutex
		var errs, conflicts atomic.Int64
		var next atomic.Int64
		start := time.Now()
		var wg sync.WaitGroup
		for w := 0; w < conc; w++ {
			wg.Go(func() {
				for {
					i := int(next.Add(1)) - 1
					if i >= n {
						return
					}
					rq, err := req(w, i)
					if err != nil {
						errs.Add(1)
						continue
					}
					t0 := time.Now()
					res, err := client.Do(rq)
					if err != nil {
						errs.Add(1)
						continue
					}
					io.Copy(io.Discard, res.Body)
					res.Body.Close()
					d := time.Since(t0)
					good, conflict := ok(res)
					switch {
					case conflict:
						conflicts.Add(1)
					case !good:
						errs.Add(1)
					}
					lmu.Lock()
					lat = append(lat, d)
					lmu.Unlock()
				}
			})
		}
		wg.Wait()
		el := time.Since(start)
		sort.Slice(lat, func(i, j int) bool { return lat[i] < lat[j] })
		pct := func(p float64) float64 {
			if len(lat) == 0 {
				return 0
			}
			return float64(lat[min(len(lat)-1, int(math.Ceil(p*float64(len(lat))))-1)].Microseconds()) / 1000
		}
		res := scenarioResult{Name: name, Requests: n, Errors: int(errs.Load()), Conflicts: int(conflicts.Load()), P50ms: pct(0.50), P95ms: pct(0.95),
			P99ms: pct(0.99), MaxMs: pct(1), Throughput: math.Round(float64(n)/el.Seconds()*10) / 10, Concurrency: conc}
		results = append(results, res)
		t.Logf("%-34s n=%d err=%d conflicts=%d p50=%.1fms p95=%.1fms p99=%.1fms max=%.1fms %.0f req/s",
			name, n, res.Errors, res.Conflicts, res.P50ms, res.P95ms, res.P99ms, res.MaxMs, res.Throughput)
	}
	get := func(tok, path string) (*http.Request, error) {
		rq, err := http.NewRequest("GET", srv.URL+path, nil)
		if err == nil {
			rq.Header.Set("Authorization", "Bearer "+tok)
		}
		return rq, err
	}
	is200 := func(res *http.Response) (bool, bool) { return res.StatusCode == 200, false }
	agentTok := func(w int) string { return main.agents[w%len(main.agents)].Access }
	views := []string{"all", "unassigned", "myWork", "all&priority=1,2", "all&category=open"}

	setProfile(2)
	run("queue list (5 views, limit 50)", perScenario, func(w, i int) (*http.Request, error) {
		return get(agentTok(w), "/api/v1/tickets?limit=50&view="+views[i%len(views)])
	}, is200)
	run("queue counts", perScenario/4, func(w, i int) (*http.Request, error) {
		return get(agentTok(w), "/api/v1/queue-counts")
	}, is200)
	rr := rand.New(rand.NewPCG(9, 9))
	pick := make([]string, perScenario)
	for i := range pick {
		pick[i] = main.tickets[rr.IntN(len(main.tickets))]
	}
	run("ticket detail", perScenario, func(w, i int) (*http.Request, error) {
		return get(agentTok(w), "/api/v1/tickets/"+pick[i])
	}, func(res *http.Response) (bool, bool) { return res.StatusCode == 200 || res.StatusCode == 404, false })
	run("ticket messages (first page of 50)", perScenario, func(w, i int) (*http.Request, error) {
		return get(agentTok(w), "/api/v1/tickets/"+pick[i]+"/messages?limit=50")
	}, func(res *http.Response) (bool, bool) { return res.StatusCode == 200 || res.StatusCode == 404, false })
	zr := rand.New(rand.NewPCG(5, 5))
	zz := rand.NewZipf(zr, 1.1, 2, uint64(len(loadVocab)-1))
	terms := make([]string, perScenario)
	for i := range terms {
		terms[i] = loadVocab[zz.Uint64()]
	}
	run("full-text search (Zipf terms)", perScenario, func(w, i int) (*http.Request, error) {
		return get(agentTok(w), "/api/v1/search?q="+terms[i])
	}, is200)
	run("subject filter (q=, list)", perScenario/2, func(w, i int) (*http.Request, error) {
		return get(agentTok(w), "/api/v1/tickets?limit=50&q="+terms[i])
	}, is200)

	// Claim race: all workers claim the same ticket; exactly one may win.
	var raceKey string
	var raceVersion int64
	for _, a := range main.agents[:1] {
		items := h.do("GET", "/api/v1/tickets?view=unassigned&limit=1", nil, bearer(a.Access)).expect(200).Items()
		if len(items) > 0 {
			raceKey, raceVersion = refOf(items[0]), int64(items[0]["version"].(float64))
		}
	}
	if raceKey != "" {
		var wins atomic.Int64
		eligible := []session{}
		for _, a := range main.agents {
			if h.do("GET", "/api/v1/tickets/"+raceKey, nil, bearer(a.Access)).Code == 200 {
				eligible = append(eligible, a)
			}
		}
		run(fmt.Sprintf("claim race (%d agents, one ticket)", len(eligible)), len(eligible), func(w, i int) (*http.Request, error) {
			rq, err := http.NewRequest("POST", srv.URL+"/api/v1/tickets/"+raceKey+"/claim", nil)
			if err == nil {
				rq.Header.Set("Authorization", "Bearer "+eligible[i].Access)
				rq.Header.Set("If-Match", strconv.FormatInt(raceVersion, 10))
			}
			return rq, err
		}, func(res *http.Response) (bool, bool) {
			if res.StatusCode == 200 {
				wins.Add(1)
			}
			return res.StatusCode == 200, res.StatusCode == 409
		})
		if wins.Load() != 1 {
			t.Errorf("claim race: %d winners, want exactly 1", wins.Load())
		}
	}
	// Claims: each worker claims distinct unassigned tickets from its groups.
	type cand struct {
		key     string
		version int64
		tok     string
	}
	var cands []cand
	taken := map[string]bool{}
	perAgent := perScenario/len(main.agents) + 1
	for _, a := range main.agents {
		mine, cursor := 0, ""
		for mine < perAgent {
			path := "/api/v1/tickets?view=unassigned&limit=100"
			if cursor != "" {
				path += "&cursor=" + cursor
			}
			res := h.do("GET", path, nil, bearer(a.Access)).expect(200).JSON()
			items, _ := res["items"].([]any)
			for _, it := range items {
				m := it.(map[string]any)
				k := refOf(m)
				if taken[k] || mine >= perAgent {
					continue
				}
				taken[k] = true
				mine++
				cands = append(cands, cand{key: k, version: int64(m["version"].(float64)), tok: a.Access})
			}
			cursor, _ = res["nextCursor"].(string)
			if cursor == "" || len(items) == 0 {
				break
			}
		}
	}
	rand.New(rand.NewPCG(3, 3)).Shuffle(len(cands), func(i, j int) { cands[i], cands[j] = cands[j], cands[i] })
	nClaims := min(len(cands), perScenario)
	run("claim (distinct unassigned tickets)", nClaims, func(w, i int) (*http.Request, error) {
		c := cands[i]
		rq, err := http.NewRequest("POST", srv.URL+"/api/v1/tickets/"+c.key+"/claim", nil)
		if err == nil {
			rq.Header.Set("Authorization", "Bearer "+c.tok)
			rq.Header.Set("If-Match", strconv.FormatInt(c.version, 10))
		}
		return rq, err
	}, func(res *http.Response) (bool, bool) { return res.StatusCode == 200, res.StatusCode == 409 })

	setProfile(0)

	// ---- Query plans from the profiler ----
	cur, err := db.Collection("system.profile").Find(ctx, bson.D{{Key: "ns", Value: bson.D{{Key: "$in", Value: bson.A{
		db.Name() + ".tickets", db.Name() + ".messages"}}}}, {Key: "op", Value: bson.D{{Key: "$in", Value: bson.A{"query", "command", "update"}}}}},
		options.Find().SetProjection(bson.D{{Key: "ns", Value: 1}, {Key: "op", Value: 1}, {Key: "command", Value: 1}, {Key: "planSummary", Value: 1},
			{Key: "millis", Value: 1}, {Key: "keysExamined", Value: 1}, {Key: "docsExamined", Value: 1}, {Key: "nreturned", Value: 1}}))
	if err != nil {
		t.Fatal(err)
	}
	var prof []bson.M
	if err := cur.All(ctx, &prof); err != nil {
		t.Fatal(err)
	}
	agg := map[string]*planSummary{}
	for _, p := range prof {
		ns, _ := p["ns"].(string)
		col := ns[strings.LastIndex(ns, ".")+1:]
		cmd := toM(p["command"])
		kind, shape := "", ""
		for _, k := range []string{"find", "aggregate", "count", "findAndModify", "update", "distinct"} {
			if _, ok := cmd[k]; ok {
				kind = k
				break
			}
		}
		if kind == "" {
			continue
		}
		if f := toM(cmd["filter"]); f != nil {
			shape = shapeOf(f)
		} else if q := toM(cmd["query"]); q != nil {
			shape = shapeOf(q)
		} else if pl, ok := cmd["pipeline"].(bson.A); ok && len(pl) > 0 {
			if m := toM(toM(pl[0])["$match"]); m != nil {
				shape = "$match " + shapeOf(m)
			}
		}
		if srt := toM(cmd["sort"]); srt != nil {
			shape += " sort " + shapeOf(srt)
		}
		plan, _ := p["planSummary"].(string)
		key := col + "|" + kind + "|" + shape + "|" + plan
		ps := agg[key]
		if ps == nil {
			ps = &planSummary{Collection: col, Op: kind, Shape: shape, Plan: plan}
			agg[key] = ps
		}
		num := func(k string) float64 {
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
		ps.Count++
		ps.AvgMs += num("millis")
		ps.AvgKeys += num("keysExamined")
		ps.AvgDocs += num("docsExamined")
		ps.AvgReturned += num("nreturned")
	}
	var plans []planSummary
	for _, ps := range agg {
		n := float64(ps.Count)
		ps.AvgMs, ps.AvgKeys, ps.AvgDocs, ps.AvgReturned = round1(ps.AvgMs/n), round1(ps.AvgKeys/n), round1(ps.AvgDocs/n), round1(ps.AvgReturned/n)
		plans = append(plans, *ps)
	}
	sort.Slice(plans, func(i, j int) bool { return plans[i].Count > plans[j].Count })
	collscans := 0
	for _, ps := range plans {
		t.Logf("plan %-8s %-13s n=%-5d %-60.60s %-70.70s keys=%.0f docs=%.0f ret=%.0f %.1fms", ps.Collection, ps.Op, ps.Count, ps.Shape, ps.Plan,
			ps.AvgKeys, ps.AvgDocs, ps.AvgReturned, ps.AvgMs)
		if strings.Contains(ps.Plan, "COLLSCAN") {
			collscans++
		}
	}
	if collscans > 0 {
		t.Errorf("%d query shapes on tickets/messages used a collection scan", collscans)
	}

	var stats bson.M
	_ = db.RunCommand(ctx, bson.D{{Key: "dbStats", Value: 1}, {Key: "scale", Value: 1 << 20}}).Decode(&stats)
	report := map[string]any{
		"tickets": created.Load(), "messages": created.Load() + posted.Load(), "claimedDuringSeed": claimed.Load(),
		"maxMessagesOnOneTicket": maxMsgs, "seed_seconds": math.Round(seedDur.Seconds()), "seed_workers": workers,
		"projects":  map[string]int{projects[0].key: len(projects[0].tickets), projects[1].key: len(projects[1].tickets), projects[2].key: len(projects[2].tickets)},
		"scenarios": results, "plans": plans, "dbStatsMib": map[string]any{"dataSize": stats["dataSize"], "storageSize": stats["storageSize"], "indexSize": stats["indexSize"]},
		"go": runtime.Version(), "cpus": runtime.NumCPU(),
	}
	if out := os.Getenv("OPEN_KASE_LOADTEST_OUT"); out != "" {
		b, _ := json.MarshalIndent(report, "", "  ")
		if err := os.WriteFile(out, b, 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

func round1(f float64) float64 { return math.Round(f*10) / 10 }

// toM converts a decoded document (bson.D or bson.M) to bson.M.
func toM(v any) bson.M {
	switch d := v.(type) {
	case bson.M:
		return d
	case bson.D:
		m := bson.M{}
		for _, e := range d {
			m[e.Key] = e.Value
		}
		return m
	}
	return nil
}

// shapeOf renders a filter's field structure without values.
func shapeOf(m bson.M) string {
	keys := make([]string, 0, len(m))
	for k, v := range m {
		if sub := toM(v); sub != nil {
			keys = append(keys, k+":{"+shapeOf(sub)+"}")
			continue
		}
		switch vv := v.(type) {
		case bson.A:
			var parts []string
			for _, e := range vv {
				if em := toM(e); em != nil {
					parts = append(parts, shapeOf(em))
				}
			}
			if len(parts) > 0 {
				keys = append(keys, k+":["+strings.Join(parts, "|")+"]")
			} else {
				keys = append(keys, k)
			}
		default:
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)
	return strings.Join(keys, ",")
}
