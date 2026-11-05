package runtime

import (
	"bytes"
	"strings"
	"testing"
	"time"

	transporthttp "github.com/arksnorman/open-kase/api/internal/transport/http"
	"github.com/arksnorman/open-kase/api/package/logger"
)

const (
	// ghostID is a well-formed identifier that names nothing.
	ghostID = "0123456789abcdef01234567"
	// isolationCanary is written into every piece of hidden content; it must
	// never appear in a response to a principal without access.
	isolationCanary = "zebra-canary-7731"
)

// isolationFixture holds a ticket in the Delivery group (Marco only) with
// every kind of sub-resource; Dana (Triage only) must not be able to reach
// any of it.
type isolationFixture struct {
	*ticketFixture
	hidden, visible map[string]any    // tickets: Delivery (hidden from Dana) and Triage
	mail            map[string]string // real identifiers by path parameter
}

// groupScopedRoute reports whether a documented route is one of the
// ticket-workspace routes (the former project routes) the sweeps probe;
// account, user, settings and delivery routes are covered by their own
// tests.
func groupScopedRoute(path string) bool {
	if !strings.HasPrefix(path, "/api/v1/") {
		return false
	}
	for _, p := range []string{"/api/v1/auth", "/api/v1/me", "/api/v1/users", "/api/v1/settings", "/api/v1/deliveries"} {
		if strings.HasPrefix(path, p) {
			return false
		}
	}
	return true
}

func newIsolationFixture(t *testing.T) *isolationFixture {
	t.Helper()
	f := &isolationFixture{ticketFixture: newTicketFixture(t), mail: map[string]string{}}
	f.c.Boot.Env = EnvDevelopment // plain http to the local webhook receiver
	// The sweeps send several hundred requests; rate limits are covered by
	// TestRateLimits.
	off := map[string]transporthttp.RateLimit{}
	for class := range transporthttp.DefaultRateLimits {
		off[class] = transporthttp.RateLimit{PerMinute: -1}
	}
	r, err := transporthttp.NewRouter(transporthttp.Deps{Logger: logger.Discard(), Readiness: transporthttp.NewReadiness(logger.Discard()),
		Services: f.c.httpServices(), MCP: f.c.mcpHandler(), RateLimits: off})
	if err != nil {
		t.Fatal(err)
	}
	f.h = r
	hs := newHookServer(t)
	admin, marco := bearer(f.admin.Access), bearer(f.marco.Access)

	// Project-level resources.
	f.mail["w"] = f.do("POST", "/api/v1/webhooks", map[string]any{"name": "Test hook", "url": hs.URL + "/hook",
		"eventTypes": []string{"ticket.created"}}, admin).expect(201).Str("id")
	f.mail["m"] = f.do("POST", "/api/v1/macros", map[string]any{"name": "Canned " + isolationCanary,
		"body": "Hello " + isolationCanary}, admin).expect(201).Str("id")
	f.mail["i"] = f.do("GET", "/api/v1/integrations", nil, admin).expect(200).Items()[0]["id"].(string)
	f.mail["g"] = f.delivery
	f.mail["u"] = f.marco.UserId

	// The hidden ticket and everything that can hang off a ticket.
	f.hidden = f.createTicket(f.marco.Access, map[string]any{"groupId": f.delivery, "subject": "Hidden " + isolationCanary,
		"body": "Public text " + isolationCanary}).JSON()
	f.visible = f.createTicket(f.dana.Access, map[string]any{"subject": "Visible triage ticket"}).JSON()
	hk, vk := refOf(f.hidden), refOf(f.visible)
	f.mail["t"], f.mail["c"] = f.hidden["id"].(string), obj(f.hidden, "requester")["id"].(string)
	f.mail["attachment"] = f.upload(f.marco.Access, isolationCanary+".txt", []byte("file "+isolationCanary)).expect(201).Str("id")
	f.mail["msg"] = f.do("POST", tpath(hk, "/messages"), map[string]any{"visibility": "agent", "body": "Note " + isolationCanary,
		"attachmentIds": []string{f.mail["attachment"]}}, marco).expect(201).Str("id")
	f.do("PUT", tpath(hk, "/drafts/agent"), map[string]any{"body": "Draft " + isolationCanary}, marco).expect(200)
	f.watch(f.marco.Access, hk)
	f.mail["l"] = f.do("POST", tpath(vk, "/links"), map[string]any{"type": "relatedTo", "ticket": hk}, bearer(f.lead.Access)).expect(201).Str("id")
	f.drain() // outbox fan-out and webhook deliveries
	deliveries := f.do("GET", "/api/v1/webhooks/"+f.mail["w"]+"/deliveries", nil, admin).expect(200).Items()
	if len(deliveries) == 0 {
		t.Fatal("no webhook delivery was recorded")
	}
	f.mail["d"] = deliveries[0]["id"].(string)
	// Scheduled last: its job stays queued until the send time, which would
	// keep drain() waiting.
	f.mail["s"] = f.do("POST", tpath(hk, "/scheduled-messages"), map[string]any{"visibility": "public", "body": "Later " + isolationCanary,
		"sendAt": time.Now().Add(6 * time.Hour).UTC().Format(time.RFC3339)}, marco).expect(201).Str("id")

	return f
}

// isolationBody returns a request body that passes validation for the
// route, so that a handler which authorized after validating would be seen
// acting on the foreign resource.
func (f *isolationFixture) isolationBody(rt transporthttp.RouteAuth) any {
	group, linkTo := f.triage, refOf(f.visible)
	sweep := "isolation sweep"
	switch route := rt.Method + " " + strings.TrimPrefix(rt.Path, "/api/v1"); route {
	case "PATCH /tickets/:t":
		return map[string]any{"priority": 1}
	case "DELETE /tickets/:t", "POST /tickets/:t/release", "POST /tickets/:t/messages/:msg/redact":
		return map[string]any{"reason": sweep}
	case "POST /tickets/:t/assignments":
		return map[string]any{"groupId": group}
	case "POST /tickets/:t/transitions":
		return map[string]any{"action": "resolve"}
	case "POST /tickets/:t/messages":
		return map[string]any{"visibility": "public", "body": sweep}
	case "POST /tickets/:t/scheduled-messages":
		return map[string]any{"visibility": "public", "body": sweep, "sendAt": time.Now().Add(time.Hour).UTC().Format(time.RFC3339)}
	case "PATCH /tickets/:t/scheduled-messages/:s", "PUT /tickets/:t/drafts/:visibility", "POST /tickets/:t/follow-up", "POST /tickets/:t/copy":
		return map[string]any{"body": sweep}
	case "POST /tickets/:t/links":
		return map[string]any{"type": "relatedTo", "ticket": linkTo}
	case "PATCH /groups/:g", "PATCH /integrations/:i", "PATCH /macros/:m":
		return map[string]any{"name": "Renamed by the sweep"}
	case "PATCH /webhooks/:w":
		return map[string]any{"active": false}
	case "POST /tickets/:t/scheduled-messages/:s/send", "POST /tickets/:t/scheduled-messages/:s/cancel":
		return nil
	}
	switch rt.Method {
	case "POST", "PUT", "PATCH":
		if rt.Example != nil {
			return rt.Example
		}
		return map[string]any{}
	}
	return nil
}

// isolationCase is one principal probing identifiers it has no access to.
type isolationCase struct {
	name string
	tok  string
	// ids are the real identifiers by path parameter ("attachment" for
	// :a).
	ids map[string]string
	// foreign lists the parameters the principal must not be able to tell
	// from missing ones; a route is probed when it carries at least one.
	foreign []string
}

func isolationParam(name string) string {
	if name == "a" {
		return "attachment"
	}
	return name
}

// paths fills the route's parameters twice: with the real identifiers and
// with the foreign ones replaced by well-formed identifiers that do not
// exist. n is the number of foreign parameters in the route.
func (c isolationCase) paths(route string) (realPath, ghostPath string, n int) {
	segs := strings.Split(route, "/")
	ghost := make([]string, len(segs))
	copy(ghost, segs)
	for i, s := range segs {
		if !strings.HasPrefix(s, ":") {
			continue
		}
		name := isolationParam(s[1:])
		switch name {
		case "visibility":
			segs[i], ghost[i] = "agent", "agent"
			continue
		}
		v, ok := c.ids[name]
		if !ok {
			v = ghostID
		}
		segs[i], ghost[i] = v, v
		for _, fp := range c.foreign {
			if fp != name || !ok {
				continue
			}
			n++
			ghost[i] = ghostID
		}
	}
	return strings.Join(segs, "/"), strings.Join(ghost, "/"), n
}

// sweep probes every documented workspace route that carries a foreign
// parameter. The answer for a real identifier must be an error, must equal
// the answer for an identifier that names nothing (so existence is not
// revealed), and must not contain hidden content.
func (f *isolationFixture) sweep(t *testing.T, c isolationCase) int {
	t.Helper()
	probed := 0
	for _, rt := range transporthttp.RouteAuthTable() {
		if !groupScopedRoute(rt.Path) || rt.Multipart {
			continue
		}
		realPath, ghostPath, n := c.paths(rt.Path)
		if n == 0 {
			continue
		}
		body := f.isolationBody(rt)
		real := f.do(rt.Method, realPath, body, bearer(c.tok), header("If-Match", "1"))
		ghost := f.do(rt.Method, ghostPath, body, bearer(c.tok), header("If-Match", "1"))
		probed++
		name := c.name + ": " + rt.Method + " " + rt.Path
		if real.Code < 400 {
			t.Errorf("%s succeeded with status %d: %s", name, real.Code, real.Body)
			continue
		}
		if real.Code != ghost.Code || real.Problem().Code != ghost.Problem().Code {
			t.Errorf("%s reveals the resource: %d %s for the real identifier, %d %s for a missing one",
				name, real.Code, real.Problem().Code, ghost.Code, ghost.Problem().Code)
		}
		if bytes.Contains(real.Body, []byte(isolationCanary)) {
			t.Errorf("%s leaks hidden content: %s", name, real.Body)
		}
	}
	return probed
}

// TestIsolationSweep is the authorization matrix for the cells that the
// per-route authentication sweep (TestEveryRouteRequiresAuthentication) does
// not reach: an agent outside the ticket's group.
func TestIsolationSweep(t *testing.T) {
	f := newIsolationFixture(t)

	// What privileged readers see before the sweeps, to prove afterwards
	// that nothing was changed.
	snapshot := func() map[string]string {
		out := map[string]string{}
		get := func(tok, path string) {
			out[path] = string(f.do("GET", path, nil, bearer(tok)).expect(200).Body)
		}
		hk := refOf(f.hidden)
		for _, p := range []string{tpath(hk), tpath(hk, "/messages"), tpath(hk, "/scheduled-messages?state=all"), tpath(hk, "/links"),
			tpath(hk, "/drafts/agent"), tpath(hk, "/watchers")} {
			get(f.marco.Access, p)
		}
		for _, p := range []string{"/api/v1/groups", "/api/v1/members", "/api/v1/integrations",
			"/api/v1/webhooks", "/api/v1/macros", "/api/v1/contacts",
			"/api/v1/groups/" + f.delivery + "/members", "/api/v1/tickets?view=all"} {
			get(f.admin.Access, p)
		}
		return out
	}
	before := snapshot()

	// Groups are a staff directory (agents transfer tickets to any group), so
	// an agent outside a group may read it; its tickets stay hidden.
	hiddenParams := []string{"t", "s", "l", "msg", "attachment"}
	withNumber := map[string]string{}
	for k, v := range f.mail {
		withNumber[k] = v
	}
	withNumber["t"] = refOf(f.hidden)
	onVisible := map[string]string{}
	for k, v := range f.mail {
		onVisible[k] = v
	}
	onVisible["t"] = f.visible["id"].(string)
	everything := []string{"t", "s", "l", "msg", "attachment", "g", "u", "i", "c", "m", "w", "d"}

	cases := []isolationCase{
		// An agent who is not in the ticket's group.
		{name: "agent outside the group (ticket ID)", tok: f.dana.Access, ids: f.mail, foreign: hiddenParams},
		{name: "agent outside the group (ticket number)", tok: f.dana.Access, ids: withNumber, foreign: hiddenParams},
		// The hidden ticket's sub-resources addressed through a ticket the agent can read.
		{name: "agent via a visible ticket", tok: f.dana.Access, ids: onVisible, foreign: []string{"s", "l", "msg"}},
	}
	for _, c := range cases {
		if n := f.sweep(t, c); n < 5 {
			t.Errorf("%s: only %d routes probed", c.name, n)
		} else {
			t.Logf("%s: %d routes", c.name, n)
		}
	}

	// Positive control: the identifiers are real. Every GET route the sweeps
	// probed answers 200 for a principal who does have access, so the
	// refusals above are access decisions, not broken paths.
	control := isolationCase{ids: f.mail, foreign: everything}
	for _, rt := range transporthttp.RouteAuthTable() {
		if rt.Method != "GET" || !groupScopedRoute(rt.Path) {
			continue
		}
		path, _, n := control.paths(rt.Path)
		if n == 0 {
			continue
		}
		if a, b := f.do("GET", path, nil, bearer(f.marco.Access)), f.do("GET", path, nil, bearer(f.admin.Access)); a.Code != 200 && b.Code != 200 {
			t.Errorf("positive control GET %s: %d %s (member), %d %s (admin)", rt.Path, a.Code, a.Problem().Code, b.Code, b.Problem().Code)
		}
	}

	// Nothing changed.
	after := snapshot()
	for path, want := range before {
		if after[path] != want {
			t.Errorf("the sweeps changed %s:\nbefore %s\nafter  %s", path, want, after[path])
		}
	}
}
