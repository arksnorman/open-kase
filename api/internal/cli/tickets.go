package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/arksnorman/open-kase/api/package/product"
)

const ticketsUsage = `usage:
  %[1]s tickets list [--view VIEW] [--group NAME|ID] [--limit N] [--json]
  %[1]s tickets show TICKET-NUMBER [--json]
  %[1]s tickets claim TICKET-NUMBER [--json]
  %[1]s tickets reply TICKET-NUMBER --file reply.md --audience public|agent [--json]`

func (r *runner) tickets(ctx context.Context, args []string) error {
	if len(args) == 0 {
		return usagef(ticketsUsage, product.Binary)
	}
	switch args[0] {
	case "list":
		return r.ticketsList(ctx, args[1:])
	case "show":
		return r.ticketsShow(ctx, args[1:])
	case "claim":
		return r.ticketsClaim(ctx, args[1:])
	case "reply":
		return r.ticketsReply(ctx, args[1:])
	}
	return usagef(ticketsUsage, product.Binary)
}

// ticketPath returns the API path of a ticket number such as 1042 or #1042
func ticketPath(ref string) (string, error) {
	n := strings.TrimPrefix(ref, "#")
	if v, err := strconv.ParseInt(n, 10, 64); err != nil || v <= 0 {
		return "", usagef("%q is not a ticket number such as 1042", ref)
	}
	return "/tickets/" + n, nil
}

type ticketRow struct {
	Id       string `json:"id"`
	Number   int64  `json:"number"`
	Subject  string `json:"subject"`
	Priority int    `json:"priority"`
	Version  int64  `json:"version"`
	Group    struct {
		Id   string `json:"id"`
		Name string `json:"name"`
	} `json:"group"`
	Assignee *struct {
		Name string `json:"name"`
	} `json:"assignee"`
	Status struct {
		Name string `json:"name"`
	} `json:"status"`
	Requester struct {
		Name  string `json:"name"`
		Email string `json:"email"`
	} `json:"requester"`
	UpdatedAt time.Time `json:"updatedAt"`
}

func (t ticketRow) assignee() string {
	if t.Assignee == nil {
		return "-"
	}
	return t.Assignee.Name
}

func (r *runner) ticketsList(ctx context.Context, args []string) error {
	fs, c := r.flags("tickets list", "tickets list [--view VIEW] [--group NAME|ID] [--limit N]")
	view := fs.String("view", "", "queue view: all, myWork, unassigned, watching, awaitingCustomer, onHold, dueSoon, resolved, archived")
	group := fs.String("group", "", "only tickets in this group (name or ID)")
	limit := fs.Int("limit", 50, "maximum number of tickets (1-1000)")
	if _, err := parse(fs, args, 0); err != nil {
		return err
	}
	if *limit < 1 || *limit > 1000 {
		return usagef("--limit must be between 1 and 1000")
	}
	cl, err := r.client(c)
	if err != nil {
		return err
	}
	q := url.Values{}
	if *view != "" {
		q.Set("view", *view)
	}
	if *group != "" {
		id, err := r.resolveGroup(ctx, cl, *group)
		if err != nil {
			return err
		}
		q.Set("groupId", id)
	}
	rows, err := all[json.RawMessage](ctx, cl, "/tickets", q, *limit)
	if err != nil {
		return err
	}
	if c.json {
		return r.printJSON(mustJSON(rows))
	}
	tw := r.table()
	fmt.Fprintln(tw, "NUMBER\tPRIORITY\tSTATUS\tGROUP\tASSIGNEE\tSUBJECT")
	for _, raw := range rows {
		var t ticketRow
		_ = json.Unmarshal(raw, &t)
		fmt.Fprintf(tw, "#%d\tP%d\t%s\t%s\t%s\t%s\n", t.Number, t.Priority, t.Status.Name, t.Group.Name, t.assignee(), t.Subject)
	}
	return tw.Flush()
}

// resolveGroup accepts a group ID or a (case-insensitive) group name.
func (r *runner) resolveGroup(ctx context.Context, cl *client, group string) (string, error) {
	groups, err := all[struct {
		Id   string `json:"id"`
		Name string `json:"name"`
	}](ctx, cl, "/groups", nil, 1000)
	if err != nil {
		return "", err
	}
	for _, g := range groups {
		if g.Id == group || strings.EqualFold(g.Name, group) {
			return g.Id, nil
		}
	}
	return "", usagef("no group %q", group)
}

type messageRow struct {
	Visibility string `json:"visibility"`
	Author     struct {
		Name string `json:"name"`
	} `json:"author"`
	Body      string    `json:"body"`
	CreatedAt time.Time `json:"createdAt"`
}

func (r *runner) ticketsShow(ctx context.Context, args []string) error {
	fs, c := r.flags("tickets show", "tickets show TICKET-NUMBER [--json]")
	pos, err := parse(fs, args, 1)
	if err != nil {
		return err
	}
	path, err := ticketPath(pos[0])
	if err != nil {
		return err
	}
	cl, err := r.client(c)
	if err != nil {
		return err
	}
	var t ticketRow
	rawTicket, err := cl.json(ctx, request{method: http.MethodGet, path: path}, &t)
	if err != nil {
		return err
	}
	msgs, err := all[json.RawMessage](ctx, cl, path+"/messages", nil, 1000)
	if err != nil {
		return err
	}
	if c.json {
		return r.printJSON(mustJSON(map[string]any{"ticket": json.RawMessage(rawTicket), "messages": msgs}))
	}
	fmt.Fprintf(r.io.Out, "#%d  %s\n", t.Number, t.Subject)
	fmt.Fprintf(r.io.Out, "%s priority · %s · group %s · assignee %s\n", [5]string{"", "Critical", "High", "Normal", "Low"}[min(max(t.Priority, 0), 4)], t.Status.Name, t.Group.Name, t.assignee())
	fmt.Fprintf(r.io.Out, "Requester %s <%s> · updated %s\n", t.Requester.Name, t.Requester.Email, t.UpdatedAt.Local().Format(time.RFC1123))
	for _, raw := range msgs {
		var m messageRow
		_ = json.Unmarshal(raw, &m)
		audience := "PUBLIC REPLY"
		if m.Visibility == "agent" {
			audience = "AGENT NOTE (staff only)"
		}
		fmt.Fprintf(r.io.Out, "\n--- %s · %s · %s\n%s\n", audience, m.Author.Name, m.CreatedAt.Local().Format(time.RFC1123), m.Body)
	}
	return nil
}

func (r *runner) ticketsClaim(ctx context.Context, args []string) error {
	fs, c := r.flags("tickets claim", "tickets claim TICKET-NUMBER [--json]")
	pos, err := parse(fs, args, 1)
	if err != nil {
		return err
	}
	path, err := ticketPath(pos[0])
	if err != nil {
		return err
	}
	cl, err := r.client(c)
	if err != nil {
		return err
	}
	var t ticketRow
	if _, err := cl.json(ctx, request{method: http.MethodGet, path: path}, &t); err != nil {
		return err
	}
	raw, err := cl.json(ctx, request{method: http.MethodPost, path: path + "/claim",
		headers: map[string]string{"If-Match": strconv.FormatInt(t.Version, 10)}}, &t)
	if err != nil {
		return err
	}
	if c.json {
		return r.printJSON(raw)
	}
	fmt.Fprintf(r.io.Out, "Claimed #%d (group %s, assignee %s)\n", t.Number, t.Group.Name, t.assignee())
	return nil
}

func (r *runner) ticketsReply(ctx context.Context, args []string) error {
	fs, c := r.flags("tickets reply", "tickets reply TICKET-NUMBER --file reply.md --audience public|agent")
	file := fs.String("file", "", "Markdown body file, or - for standard input (required)")
	audience := fs.String("audience", "", "who sees the message: public (requester) or agent (staff-only note); required, never inferred")
	pos, err := parse(fs, args, 1)
	if err != nil {
		return err
	}
	if *audience != "public" && *audience != "agent" {
		return usagef("--audience is required and must be public or agent")
	}
	if *file == "" {
		return usagef("--file is required (use - for standard input)")
	}
	path, err := ticketPath(pos[0])
	if err != nil {
		return err
	}
	var body []byte
	if *file == "-" {
		if r.in == nil {
			return usagef("no standard input")
		}
		body, err = io.ReadAll(io.LimitReader(r.in, 4<<20))
	} else {
		body, err = os.ReadFile(*file)
	}
	if err != nil {
		return usagef("read %s: %v", *file, err)
	}
	if strings.TrimSpace(string(body)) == "" {
		return usagef("the message body is empty")
	}
	cl, err := r.client(c)
	if err != nil {
		return err
	}
	var m struct {
		Id         string `json:"id"`
		Visibility string `json:"visibility"`
	}
	raw, err := cl.json(ctx, request{method: http.MethodPost, path: path + "/messages",
		body: map[string]any{"visibility": *audience, "body": string(body)}}, &m)
	if err != nil {
		return err
	}
	if c.json {
		return r.printJSON(raw)
	}
	what := "public reply"
	if m.Visibility == "agent" {
		what = "agent note"
	}
	fmt.Fprintf(r.io.Out, "Posted %s %s on %s\n", what, m.Id, strings.ToUpper(pos[0]))
	return nil
}
