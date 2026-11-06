package tickets

import (
	"context"
	"errors"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/arksnorman/open-kase/api/internal/access"
	"github.com/arksnorman/open-kase/api/internal/audit"
	"github.com/arksnorman/open-kase/api/internal/outbox"
	"github.com/arksnorman/open-kase/api/internal/sla"
	"github.com/arksnorman/open-kase/api/package/fault"
	"github.com/arksnorman/open-kase/api/package/validation"
)

// SLAPolicies supplies the policy snapshot new SLA cycles start with
// (sla.Service). A nil snapshot means SLA is off for the project.
type SLAPolicies interface {
	ActiveSnapshot(ctx context.Context, projectID string) (*sla.Snapshot, error)
}

// ---- Tags ----

// MaxTags bounds the tags on one ticket.
const MaxTags = 20

// NormalizeTag trims, lower-cases and joins inner whitespace with "-".
func NormalizeTag(t string) string {
	return strings.Join(strings.Fields(strings.ToLower(strings.TrimSpace(t))), "-")
}

// NormalizeTags normalizes, validates, deduplicates and sorts tags.
func NormalizeTags(v *validation.Collector, field string, in []string) []string {
	out := make([]string, 0, len(in))
	for i, raw := range in {
		t := NormalizeTag(raw)
		if e := validation.TicketTag.CheckString(field+"["+strconv.Itoa(i)+"]", t); e != nil {
			v.Add(e.Field, e.Code, e.Message)
			continue
		}
		if !slices.Contains(out, t) {
			out = append(out, t)
		}
	}
	if len(out) > MaxTags {
		v.Int(validation.TicketTagCount, field, int64(len(out)))
	}
	slices.Sort(out)
	return out
}

// MaxCatalogTags bounds the tag catalog of a project.
const MaxCatalogTags = 500

// Tags returns the tag catalog (users, tokens with tags:read or tags:write,
// staff tokens with tickets:read) with the number of tickets carrying each
// tag that the caller may read as staff. Counts never include tickets the
// caller cannot see; callers that do not read tickets as staff get zero
// counts (tags are staff data).
func (s *Service) Tags(ctx context.Context, scope *access.ProjectScope) ([]TagCount, error) {
	if err := scope.Require(access.CapTagRead); err != nil {
		return nil, err
	}
	acc := Access{}
	if scope.Principal.IsStaff() && scope.Can(access.CapTicketRead) {
		var err error
		if acc, err = readAccess(scope); err != nil {
			return nil, err
		}
	}
	return s.repo.TagCatalog(ctx, scope.Project.Id, acc)
}

// CreateTag adds a tag to the catalog (admins and tokens with tags:write;
// agents apply existing tags only).
func (s *Service) CreateTag(ctx context.Context, scope *access.ProjectScope, name string) (TagCount, error) {
	if err := scope.Require(access.CapTagManage); err != nil {
		return TagCount{}, err
	}
	name = NormalizeTag(name)
	var v validation.Collector
	v.String(validation.TagName, "name", name)
	if err := v.Err(); err != nil {
		return TagCount{}, err
	}
	now := s.clock.Now()
	ev := audit.New(ctx, s.actor(scope), audit.TagCreated, "tag/"+name, now)
	ev.ProjectId, ev.Changes = scope.Project.Id, []string{"name"}
	err := s.repo.CreateTag(ctx, scope.Principal.WorkspaceId, scope.Project.Id, name, scope.Principal.Id, now, ev)
	switch {
	case errors.Is(err, ErrTagExists):
		return TagCount{}, fault.Conflict("tagExists", "The tag \""+name+"\" already exists.")
	case errors.Is(err, ErrTagLimit):
		return TagCount{}, fault.Conflict("tagLimitReached", "A project can have at most "+strconv.Itoa(MaxCatalogTags)+" tags.")
	case err != nil:
		return TagCount{}, err
	}
	return TagCount{Tag: name, CreatedAt: now}, nil
}

// DeleteTag removes a tag from the catalog and from every ticket carrying
// it (admins and tokens with tags:write). It returns how many tickets
// carried it.
func (s *Service) DeleteTag(ctx context.Context, scope *access.ProjectScope, name string) (int64, error) {
	if err := scope.Require(access.CapTagManage); err != nil {
		return 0, err
	}
	name = NormalizeTag(name)
	if validation.TagName.CheckString("name", name) != nil {
		return 0, fault.NotFound("tagNotFound")
	}
	now := s.clock.Now()
	ev := audit.New(ctx, s.actor(scope), audit.TagDeleted, "tag/"+name, now)
	ev.ProjectId, ev.Changes = scope.Project.Id, []string{"name", "tickets.tags"}
	n, err := s.repo.DeleteTag(ctx, scope.Principal.WorkspaceId, scope.Project.Id, name, ev)
	if errors.Is(err, ErrTagNotFound) {
		return 0, fault.NotFound("tagNotFound")
	}
	return n, err
}

// checkTagsKnown rejects tags being added to a ticket that are not in the
// catalog (422 unknownTag, one error per tag naming it). Tags the ticket
// already carries are not checked, so removing tags always works.
func (s *Service) checkTagsKnown(ctx context.Context, projectID string, raw, tags, current []string) error {
	var added []string
	for _, t := range tags {
		if !slices.Contains(current, t) {
			added = append(added, t)
		}
	}
	if len(added) == 0 {
		return nil
	}
	unknown, err := s.repo.UnknownTags(ctx, projectID, added)
	if err != nil || len(unknown) == 0 {
		return err
	}
	var v validation.Collector
	var seen []string
	for i, r := range raw {
		t := NormalizeTag(r)
		if slices.Contains(unknown, t) && !slices.Contains(seen, t) {
			seen = append(seen, t)
			v.Add("tags["+strconv.Itoa(i)+"]", "unknownTag", "The tag \""+t+"\" does not exist. Ask an admin to add it to the tag list.")
		}
	}
	return v.Err()
}

// ---- Filters (list parameters) ----

// Filter narrows a ticket list. Every filter is intersected with the
// caller's access; none can widen it.
type Filter struct {
	Assignee        string // "", "me", "unassigned" or a user ID
	Priorities      []int
	StatusIds       []string
	Tags            []string // tickets carrying every tag
	RequestTypeIds  []string
	OrganisationIds []string
	RequesterId     string // a contact ID: that contact's tickets
	Due             string // "", "dueSoon", "breached", "met", "missed" or "running"
}

// requesterSide reports whether the filter uses only the parameters that
// requester-side tokens may send (statusId, organisationId, requesterId);
// they only narrow what the token can already read.
func (f Filter) requesterSide() bool {
	return f.Assignee == "" && len(f.Priorities) == 0 && len(f.Tags) == 0 && len(f.RequestTypeIds) == 0 && f.Due == ""
}

// MaxFilterValues bounds each filter list.
const MaxFilterValues = 20

// Empty reports whether the filter selects everything.
func (f Filter) Empty() bool {
	return f.Assignee == "" && len(f.Priorities) == 0 && len(f.StatusIds) == 0 &&
		len(f.Tags) == 0 && len(f.RequestTypeIds) == 0 && len(f.OrganisationIds) == 0 && f.RequesterId == "" && f.Due == ""
}

// Normalize validates the filter and normalizes its tags.
func (f *Filter) Normalize(v *validation.Collector) {
	const prefix = ""
	bound := func(name string, n int) {
		if n > MaxFilterValues {
			v.Add(prefix+name, validation.CodeTooLong, "Use at most 20 values.")
		}
	}
	ids := func(name string, list []string) {
		bound(name, len(list))
		for i, id := range list {
			v.String(validation.ObjectID, prefix+name+"["+strconv.Itoa(i)+"]", id)
		}
	}
	ids("statusIds", f.StatusIds)
	ids("requestTypeIds", f.RequestTypeIds)
	ids("organisationIds", f.OrganisationIds)
	if f.RequesterId != "" {
		v.String(validation.ObjectID, prefix+"requesterId", f.RequesterId)
	}
	if f.Assignee != "" {
		v.String(validation.ViewAssignee, prefix+"assignee", f.Assignee)
	}
	bound("priorities", len(f.Priorities))
	for i, p := range f.Priorities {
		v.Int(validation.TicketPriority, prefix+"priorities["+strconv.Itoa(i)+"]", int64(p))
	}
	f.Tags = NormalizeTags(v, prefix+"tags", f.Tags)
	if f.Due != "" {
		v.String(validation.ViewDue, prefix+"due", f.Due)
	}
	slices.Sort(f.Priorities)
	f.Priorities = slices.Compact(f.Priorities)
}

// apply narrows q with the filter for the viewing user.
func (f Filter) apply(q *Query, me string, now time.Time) {
	switch f.Assignee {
	case "":
	case "me":
		q.AssigneeId = me
	case "unassigned":
		q.Unassigned = true
	default:
		q.AssigneeId = f.Assignee
	}
	q.Priorities = slices.Clone(f.Priorities)
	q.StatusIds = slices.Clone(f.StatusIds)
	q.Tags = slices.Clone(f.Tags)
	q.RequestTypeIds = slices.Clone(f.RequestTypeIds)
	q.OrganisationIds = slices.Clone(f.OrganisationIds)
	q.RequesterId = f.RequesterId
	q.SLA, q.Now = f.Due, now
}

// ---- SLA ----

// slaFor returns an SLAUpdate that applies fn to a copy of the current,
// open cycle; nothing happens when the ticket has no cycle.
func slaFor(fn func(in *sla.Instance)) SLAUpdate {
	return func(cur *sla.Instance) SLAWrite {
		if cur == nil {
			return SLAWrite{}
		}
		next := cur.Clone()
		fn(&next)
		return SLAWrite{Update: &next}
	}
}

func (s *Service) slaSnapshot(ctx context.Context, projectID string) (*sla.Snapshot, error) {
	if s.sla == nil {
		return nil, nil
	}
	return s.sla.ActiveSnapshot(ctx, projectID)
}

// SLAHistory returns every SLA cycle of a ticket (staff only).
func (s *Service) SLAHistory(ctx context.Context, scope *access.ProjectScope, ref string) (Ticket, []sla.Instance, error) {
	if err := scope.Principal.RequireUser(); err != nil {
		return Ticket{}, nil, err
	}
	t, err := s.load(ctx, scope, ref)
	if err != nil {
		return Ticket{}, nil, err
	}
	cycles, err := s.repo.SLACycles(ctx, t.Id)
	return s.present(scope, t), cycles, err
}

// maxBackfill bounds one backfill run; run it again for more.
const maxBackfill = 2000

// BackfillResult reports a backfill run.
type BackfillResult struct {
	Started int
	// More is true when the bound was reached; run the backfill again.
	More bool
}

// BackfillSLA starts SLA cycles for live open, active and waiting tickets
// that have none (tickets created before the policy existed). Each cycle is
// replayed from the ticket's creation and public messages: the first staff
// public reply meets it. Deadlines that already passed are recorded as
// breaches but announced to nobody. A ticket whose earlier cycle ended
// without a successor (stopped when SLA was turned off) gets a new cycle
// that starts now when its first response is still owed, and none
// otherwise. Project administrators only.
func (s *Service) BackfillSLA(ctx context.Context, scope *access.ProjectScope) (BackfillResult, error) {
	if err := scope.Principal.RequireUser(); err != nil {
		return BackfillResult{}, err
	}
	if err := scope.Require(access.CapProjectManage); err != nil {
		return BackfillResult{}, err
	}
	snap, err := s.slaSnapshot(ctx, scope.Project.Id)
	if err != nil {
		return BackfillResult{}, err
	}
	if snap == nil {
		return BackfillResult{}, fault.Conflict("slaInactive", "SLA is off.")
	}
	var res BackfillResult
	after := ""
	for res.Started < maxBackfill {
		batch, err := s.repo.SLABackfillCandidates(ctx, scope.Project.Id, after, 100)
		if err != nil {
			return res, err
		}
		if len(batch) == 0 {
			break
		}
		for _, t := range batch {
			after = t.Id
			quiet := outbox.Suppressed(ctx)
			last, err := s.repo.LastSLACycle(ctx, t.Id)
			if err != nil {
				return res, err
			}
			var inst sla.Instance
			if last == nil {
				if inst, err = s.replaySLA(ctx, *snap, t, quiet); err != nil {
					return res, err
				}
			} else {
				// Restart: the time without a cycle (SLA off) does not
				// count and the earlier breaches are not recorded again.
				if t.FirstResponseAt != nil {
					continue // answered: nothing is owed
				}
				inst = sla.Start(*snap, t.Priority, 1, s.clock.Now(), sla.StartOptions{Quiet: quiet || last.Quiet})
			}
			ok, err := s.repo.StartSLA(ctx, t, inst)
			if err != nil {
				return res, err
			}
			if ok {
				res.Started++
			}
		}
	}
	if res.Started >= maxBackfill {
		res.More = true
	}
	now := s.clock.Now()
	ev := audit.New(ctx, s.actor(scope), audit.SLABackfilled, "", now)
	ev.ProjectId, ev.Changes = scope.Project.Id, []string{"started:" + strconv.Itoa(res.Started)}
	if err := s.repo.RecordAudit(ctx, scope.Principal.WorkspaceId, ev); err != nil {
		return res, err
	}
	return res, nil
}

// replaySLA rebuilds a first cycle from the ticket's history up to now:
// the first staff public reply after the opening message meets it.
func (s *Service) replaySLA(ctx context.Context, snap sla.Snapshot, t Ticket, quiet bool) (sla.Instance, error) {
	now := s.clock.Now()
	in := sla.Start(snap, t.Priority, 1, t.CreatedAt, sla.StartOptions{Quiet: quiet})
	msgs, err := s.repo.ListMessages(ctx, t.Id, []string{VisibilityPublic}, nil, 500)
	if err != nil {
		return sla.Instance{}, err
	}
	for i, m := range msgs {
		if i == 0 || m.CreatedAt.After(now) {
			continue // the opening message is the request itself
		}
		if m.Author.Staff() {
			in.StaffPublicReply(m.CreatedAt)
			break
		}
	}
	in.Advance(now)
	in.Check(now) // the past is recorded, not announced
	return in, nil
}

// CheckSLA is the timer job's entry point (see Repository.CheckSLA).
func (s *Service) CheckSLA(ctx context.Context, instanceID string, generation int64) error {
	return s.repo.CheckSLA(ctx, instanceID, generation, s.clock.Now())
}

// StopSLA is the deactivation sweep's entry point (see Repository.StopSLA).
func (s *Service) StopSLA(ctx context.Context, r sla.StopRun) error {
	return s.repo.StopSLA(ctx, r, sla.StopBatch, s.clock.Now())
}

// Now returns the service clock's time (timer jobs use it so tests can
// drive SLA deadlines with a manual clock).
func (s *Service) Now() time.Time { return s.clock.Now() }
