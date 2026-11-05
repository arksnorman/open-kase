package mongo

import (
	"context"
	"errors"
	"strconv"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"

	"github.com/arksnorman/open-kase/api/internal/audit"
	"github.com/arksnorman/open-kase/api/internal/jobs"
	"github.com/arksnorman/open-kase/api/internal/outbox"
	"github.com/arksnorman/open-kase/api/internal/sla"
	"github.com/arksnorman/open-kase/api/internal/tickets"
)

// slaTargetsDoc is one priority's target.
type slaTargetsDoc struct {
	FirstResponse int32 `bson:"firstResponseMinutes"`
}

func targetsDocs(ts [4]sla.Targets) []slaTargetsDoc {
	out := make([]slaTargetsDoc, 4)
	for i, t := range ts {
		out[i] = slaTargetsDoc{FirstResponse: int32(t.FirstResponseMinutes)}
	}
	return out
}

func targetsFromDocs(ds []slaTargetsDoc) [4]sla.Targets {
	var out [4]sla.Targets
	for i := 0; i < 4 && i < len(ds); i++ {
		out[i] = sla.Targets{FirstResponseMinutes: int(ds[i].FirstResponse)}
	}
	return out
}

type slaSnapshotDoc struct {
	PolicyId       *bson.ObjectID  `bson:"policyId"`
	PolicyVersion  int64           `bson:"policyVersion"`
	Targets        []slaTargetsDoc `bson:"targets"`
	WarningPercent int32           `bson:"warningPercent"`
}

type slaClockDoc struct {
	Kind             string     `bson:"kind"`
	State            string     `bson:"state"`
	BudgetSeconds    int64      `bson:"budgetSeconds"`
	ElapsedSeconds   int64      `bson:"elapsedSeconds"`
	RunningSince     *time.Time `bson:"runningSince"`
	StartedAt        *time.Time `bson:"startedAt"`
	DueAt            *time.Time `bson:"dueAt"`
	WarnAt           *time.Time `bson:"warnAt"`
	MetAt            *time.Time `bson:"metAt"`
	StoppedAt        *time.Time `bson:"stoppedAt"`
	BreachedAt       *time.Time `bson:"breachedAt"`
	WarnedAt         *time.Time `bson:"warnedAt"`
	BreachNotifiedAt *time.Time `bson:"breachNotifiedAt"`
}

type slaBreachDoc struct {
	Kind string    `bson:"kind"`
	At   time.Time `bson:"at"`
}

type slaInstanceDoc struct {
	Id           bson.ObjectID  `bson:"_id"`
	WorkspaceId  bson.ObjectID  `bson:"workspaceId"`
	ProjectId    bson.ObjectID  `bson:"projectId"`
	TicketId     bson.ObjectID  `bson:"ticketId"`
	Cycle        int32          `bson:"cycle"`
	Current      bool           `bson:"current"`
	Snapshot     slaSnapshotDoc `bson:"snapshot"`
	Priority     int32          `bson:"priority"`
	Clocks       []slaClockDoc  `bson:"clocks"`
	Breaches     []slaBreachDoc `bson:"breaches"`
	Generation   int64          `bson:"generation"`
	StartedAt    time.Time      `bson:"startedAt"`
	ClosedAt     *time.Time     `bson:"closedAt"`
	ClosedReason string         `bson:"closedReason,omitempty"`
	Quiet        bool           `bson:"quiet"`
	Rev          int64          `bson:"rev"`
	UpdatedAt    time.Time      `bson:"updatedAt"`
}

func instanceDoc(in sla.Instance) slaInstanceDoc {
	d := slaInstanceDoc{Id: mustOID(in.Id), WorkspaceId: mustOID(in.WorkspaceId), ProjectId: mustOID(in.ProjectId), TicketId: mustOID(in.TicketId),
		Cycle: int32(in.Cycle), Current: in.Current, Priority: int32(in.Priority), Generation: in.Generation, StartedAt: in.StartedAt,
		ClosedAt: in.ClosedAt, ClosedReason: in.ClosedReason, Quiet: in.Quiet, Rev: in.Rev, UpdatedAt: in.UpdatedAt, Clocks: []slaClockDoc{}, Breaches: []slaBreachDoc{},
		Snapshot: slaSnapshotDoc{PolicyId: optOID(in.Snapshot.PolicyId), PolicyVersion: in.Snapshot.PolicyVersion,
			Targets:        targetsDocs(in.Snapshot.Targets),
			WarningPercent: int32(in.Snapshot.WarningPercent)}}
	for _, c := range in.Clocks {
		d.Clocks = append(d.Clocks, slaClockDoc{Kind: string(c.Kind), State: string(c.State),
			BudgetSeconds: c.BudgetSeconds, ElapsedSeconds: c.ElapsedSeconds, RunningSince: c.RunningSince,
			StartedAt: c.StartedAt, DueAt: c.DueAt, WarnAt: c.WarnAt, MetAt: c.MetAt, StoppedAt: c.StoppedAt, BreachedAt: c.BreachedAt,
			WarnedAt: c.WarnedAt, BreachNotifiedAt: c.BreachNotifiedAt})
	}
	for _, b := range in.Breaches {
		d.Breaches = append(d.Breaches, slaBreachDoc{Kind: string(b.Kind), At: b.At})
	}
	return d
}

func (d slaInstanceDoc) toDomain() sla.Instance {
	in := sla.Instance{Id: d.Id.Hex(), WorkspaceId: d.WorkspaceId.Hex(), ProjectId: d.ProjectId.Hex(), TicketId: d.TicketId.Hex(),
		Cycle: int(d.Cycle), Current: d.Current, Priority: int(d.Priority), Generation: d.Generation, StartedAt: d.StartedAt,
		ClosedAt: d.ClosedAt, ClosedReason: d.ClosedReason, Quiet: d.Quiet, Rev: d.Rev, UpdatedAt: d.UpdatedAt,
		Snapshot: sla.Snapshot{PolicyId: hexOrEmpty(d.Snapshot.PolicyId), PolicyVersion: d.Snapshot.PolicyVersion,
			Targets:        targetsFromDocs(d.Snapshot.Targets),
			WarningPercent: int(d.Snapshot.WarningPercent)}}
	for _, c := range d.Clocks {
		in.Clocks = append(in.Clocks, sla.Clock{Kind: sla.Kind(c.Kind), State: sla.State(c.State),
			BudgetSeconds: c.BudgetSeconds, ElapsedSeconds: c.ElapsedSeconds, RunningSince: c.RunningSince,
			StartedAt: c.StartedAt, DueAt: c.DueAt, WarnAt: c.WarnAt, MetAt: c.MetAt, StoppedAt: c.StoppedAt, BreachedAt: c.BreachedAt,
			WarnedAt: c.WarnedAt, BreachNotifiedAt: c.BreachNotifiedAt})
	}
	for _, b := range d.Breaches {
		in.Breaches = append(in.Breaches, sla.Breach{Kind: sla.Kind(b.Kind), At: b.At})
	}
	return in
}

// errSLAConflict means the cycle changed between read and write inside a
// transaction; the caller reports a precondition failure.
var errSLAConflict = errors.New("mongo: sla cycle changed")

// errSLAStarted ends a StartSLA transaction whose cycle insert hit a
// duplicate key (a concurrent start won). A write error aborts the MongoDB
// transaction, so returning nil there would make the commit fail as
// transient and the driver re-run the callback until its 120 s limit.
var errSLAStarted = errors.New("mongo: sla cycle already started")

// currentSLA loads the ticket's current cycle (inside the caller's
// transaction), or nil.
func (r *TicketsRepository) currentSLA(ctx context.Context, ticketID bson.ObjectID) (*sla.Instance, error) {
	var d slaInstanceDoc
	err := r.store.col(colSLAInstances).FindOne(ctx, bson.D{{Key: "ticketId", Value: ticketID}, {Key: "current", Value: true}}).Decode(&d)
	if errors.Is(err, mongo.ErrNoDocuments) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	in := d.toDomain()
	return &in, nil
}

// writeSLA replaces a cycle matched on its revision.
func (r *TicketsRepository) writeSLA(ctx context.Context, in *sla.Instance) error {
	prev := in.Rev
	in.Rev++
	doc := instanceDoc(*in)
	res, err := r.store.col(colSLAInstances).ReplaceOne(ctx, bson.D{{Key: "_id", Value: doc.Id}, {Key: "rev", Value: prev}}, doc)
	if err != nil {
		return err
	}
	if res.MatchedCount == 0 {
		return errSLAConflict
	}
	return nil
}

func (r *TicketsRepository) insertSLA(ctx context.Context, t ticketDoc, in *sla.Instance) error {
	// Number the cycle after the ticket's last one: a cycle stopped when SLA
	// was turned off, or closed while it was off (reopen without a policy),
	// has no successor, so a later start (reopen, backfill) would otherwise
	// reuse its number.
	var last struct {
		Cycle int32 `bson:"cycle"`
	}
	err := r.store.col(colSLAInstances).FindOne(ctx, bson.D{{Key: "ticketId", Value: t.Id}},
		options.FindOne().SetSort(bson.D{{Key: "cycle", Value: -1}}).SetProjection(bson.D{{Key: "cycle", Value: 1}})).Decode(&last)
	if err != nil && !errors.Is(err, mongo.ErrNoDocuments) {
		return err
	}
	if int(last.Cycle) >= in.Cycle {
		in.Cycle = int(last.Cycle) + 1
	}
	in.Id = bson.NewObjectID().Hex()
	in.WorkspaceId, in.ProjectId, in.TicketId, in.Rev = t.WorkspaceId.Hex(), t.ProjectId.Hex(), t.Id.Hex(), 1
	_, err = r.store.col(colSLAInstances).InsertOne(ctx, instanceDoc(*in))
	return err
}

// projectSLA stores the due/warning projection of the current cycle on the
// ticket (used by the due-soon views) and updates doc in memory.
func (r *TicketsRepository) projectSLA(ctx context.Context, doc *ticketDoc, cur *sla.Instance) error {
	var due, warn *time.Time
	var outcome *string
	if cur != nil {
		due, warn = cur.Projection()
		if o := cur.Outcome(); o != "" {
			outcome = &o
		}
	}
	doc.SLADueAt, doc.SLAWarnAt, doc.SLAOutcome = due, warn, outcome
	_, err := r.store.col(colTickets).UpdateOne(ctx, bson.D{{Key: "_id", Value: doc.Id}},
		bson.D{{Key: "$set", Value: bson.D{{Key: "slaDueAt", Value: due}, {Key: "slaWarnAt", Value: warn}, {Key: "slaOutcome", Value: outcome}}}})
	return err
}

// scheduleSLA enqueues the cycle's next timer check, keyed by cycle,
// generation and time so a stale job never collides with a fresh one.
func (r *TicketsRepository) scheduleSLA(ctx context.Context, in *sla.Instance, now time.Time) error {
	next := in.NextCheck()
	if next == nil || !in.Current {
		return nil
	}
	gen := strconv.FormatInt(in.Generation, 10)
	return enqueueJob(ctx, r.store, jobs.NewJob{WorkspaceId: in.WorkspaceId, ProjectId: in.ProjectId, Kind: sla.KindCheck,
		Key:     "sla:" + in.Id + ":" + gen + ":" + strconv.FormatInt(next.UnixMilli(), 10),
		Payload: map[string]string{"instanceId": in.Id, "ticketId": in.TicketId, "generation": gen}, RunAt: *next}, now)
}

// enqueueJob inserts j inside the caller's transaction unless a job with
// its key exists.
func enqueueJob(ctx context.Context, s *Store, j jobs.NewJob, now time.Time) error {
	_, err := s.col(colJobs).UpdateOne(ctx, bson.D{{Key: "key", Value: j.Key}},
		bson.D{{Key: "$setOnInsert", Value: newJobDoc(j, now)}}, options.UpdateOne().SetUpsert(true))
	return err
}

// applySLA runs a mutation's SLAUpdate inside its transaction.
func (r *TicketsRepository) applySLA(ctx context.Context, doc *ticketDoc, fn tickets.SLAUpdate, now time.Time) error {
	cur, err := r.currentSLA(ctx, doc.Id)
	if err != nil {
		return err
	}
	w := fn(cur)
	if w.Update == nil {
		return nil
	}
	if err := r.writeSLA(ctx, w.Update); err != nil {
		return err
	}
	current := w.Update
	if !current.Current {
		current = nil
	} else if cur != nil && current.Generation != cur.Generation {
		if err := r.scheduleSLA(ctx, current, now); err != nil {
			return err
		}
	}
	return r.projectSLA(ctx, doc, current)
}

// currentSLAs loads the current cycles of the given tickets.
func (r *TicketsRepository) currentSLAs(ctx context.Context, ids bson.A) (map[bson.ObjectID]*sla.Instance, error) {
	docs, err := findAll[slaInstanceDoc](ctx, r.store.col(colSLAInstances),
		bson.D{{Key: "ticketId", Value: bson.D{{Key: "$in", Value: ids}}}, {Key: "current", Value: true}}, options.Find())
	if err != nil {
		return nil, err
	}
	out := make(map[bson.ObjectID]*sla.Instance, len(docs))
	for _, d := range docs {
		in := d.toDomain()
		out[d.TicketId] = &in
	}
	return out, nil
}

// SLACycles returns every cycle of a ticket, oldest first.
func (r *TicketsRepository) SLACycles(ctx context.Context, ticketID string) ([]sla.Instance, error) {
	docs, err := findAll[slaInstanceDoc](ctx, r.store.col(colSLAInstances), bson.D{{Key: "ticketId", Value: mustOID(ticketID)}},
		options.Find().SetSort(bson.D{{Key: "cycle", Value: 1}}).SetLimit(100))
	if err != nil {
		return nil, err
	}
	out := make([]sla.Instance, 0, len(docs))
	for _, d := range docs {
		out = append(out, d.toDomain())
	}
	return out, nil
}

// LastSLACycle returns the ticket's latest cycle, or nil.
func (r *TicketsRepository) LastSLACycle(ctx context.Context, ticketID string) (*sla.Instance, error) {
	var d slaInstanceDoc
	err := r.store.col(colSLAInstances).FindOne(ctx, bson.D{{Key: "ticketId", Value: mustOID(ticketID)}},
		options.FindOne().SetSort(bson.D{{Key: "cycle", Value: -1}})).Decode(&d)
	if errors.Is(err, mongo.ErrNoDocuments) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	in := d.toDomain()
	return &in, nil
}

// StartSLA stores a first cycle for a ticket without one. The unique
// partial index on (ticket_id, current) makes concurrent starts safe.
func (r *TicketsRepository) StartSLA(ctx context.Context, t tickets.Ticket, inst sla.Instance) (bool, error) {
	started := false
	err := r.store.transaction(ctx, func(ctx context.Context) error {
		started = false
		var doc ticketDoc
		err := r.store.col(colTickets).FindOne(ctx, bson.D{{Key: "_id", Value: mustOID(t.Id)}}).Decode(&doc)
		if errors.Is(err, mongo.ErrNoDocuments) {
			return nil
		}
		if err != nil {
			return err
		}
		cur, err := r.currentSLA(ctx, doc.Id)
		if err != nil || cur != nil {
			return err
		}
		in := inst.Clone()
		if err := r.insertSLA(ctx, doc, &in); err != nil {
			if mongo.IsDuplicateKeyError(err) {
				return errSLAStarted
			}
			return err
		}
		if err := r.scheduleSLA(ctx, &in, in.UpdatedAt); err != nil {
			return err
		}
		started = true
		return r.projectSLA(ctx, &doc, &in)
	})
	if errors.Is(err, errSLAStarted) {
		return false, nil
	}
	return started, err
}

// SLABackfillCandidates lists live workable tickets without a current cycle.
func (r *TicketsRepository) SLABackfillCandidates(ctx context.Context, projectID, afterID string, limit int) ([]tickets.Ticket, error) {
	pid := mustOID(projectID)
	last := afterID
	var out []tickets.Ticket
	for len(out) < limit {
		filter := bson.D{{Key: "projectId", Value: pid},
			{Key: "statusCategory", Value: bson.D{{Key: "$in", Value: bson.A{"open", "waitingCustomer", "onHold"}}}}}
		if last != "" {
			filter = append(filter, bson.E{Key: "_id", Value: bson.D{{Key: "$gt", Value: mustOID(last)}}})
		}
		docs, err := findAll[ticketDoc](ctx, r.store.col(colTickets), filter, options.Find().SetSort(bson.D{{Key: "_id", Value: 1}}).SetLimit(int64(limit)))
		if err != nil {
			return nil, err
		}
		if len(docs) == 0 {
			break
		}
		last = docs[len(docs)-1].Id.Hex()
		ids := bson.A{}
		for _, d := range docs {
			ids = append(ids, d.Id)
		}
		have, err := r.currentSLAs(ctx, ids)
		if err != nil {
			return nil, err
		}
		var missing []ticketDoc
		for _, d := range docs {
			if have[d.Id] == nil {
				missing = append(missing, d)
			}
		}
		ts, err := r.hydrate(ctx, missing, "")
		if err != nil {
			return nil, err
		}
		out = append(out, ts...)
	}
	return out, nil
}

// RecordAudit stores audit events.
func (r *TicketsRepository) RecordAudit(ctx context.Context, workspaceID string, events ...audit.Event) error {
	return r.store.insertAudit(ctx, mustOID(workspaceID), events...)
}

// slaActor is the audit/outbox actor of SLA timers.
var slaActor = audit.System(audit.SystemSLA, "sla timer")

// CheckSLA runs one timer check inside the caller's transaction (the
// job's fenced completion). A cycle whose generation moved on is left
// alone: a newer job carries the current schedule. The policy is read in
// the same transaction: when SLA was turned off after the cycle started,
// the timer announces nothing and stops the cycle itself (the stop job
// may not have reached it yet, or it started in a race with the
// deactivation).
func (r *TicketsRepository) CheckSLA(ctx context.Context, instanceID string, generation int64, now time.Time) error {
	iid, err := oid(instanceID)
	if err != nil {
		return nil
	}
	var d slaInstanceDoc
	err = r.store.col(colSLAInstances).FindOne(ctx, bson.D{{Key: "_id", Value: iid}, {Key: "generation", Value: generation},
		{Key: "current", Value: true}}).Decode(&d)
	if errors.Is(err, mongo.ErrNoDocuments) {
		return nil // stale timer
	}
	if err != nil {
		return err
	}
	p, err := getSLAPolicy(ctx, r.store, d.ProjectId)
	if err != nil && !errors.Is(err, sla.ErrNotFound) {
		return err
	}
	if in := d.toDomain(); !in.Closed() && p.Stops(in.Snapshot) { // no policy: SLA is off as well
		return r.deactivateSLA(ctx, &in, p.StopTime(now))
	}
	var t ticketDoc
	err = r.store.col(colTickets).FindOne(ctx, bson.D{{Key: "_id", Value: d.TicketId}}).Decode(&t)
	if errors.Is(err, mongo.ErrNoDocuments) {
		return nil // deleted tickets have suspended clocks
	}
	if err != nil {
		return err
	}
	in := d.toDomain()
	notices := in.Check(now)
	if err := r.writeSLA(ctx, &in); err != nil {
		return err
	}
	if len(notices) > 0 {
		events := make([]outbox.Event, 0, len(notices))
		for _, n := range notices {
			typ := outbox.TicketSLAWarning
			if n.Breached {
				typ = outbox.TicketSLABreached
			}
			due := n.DueAt
			events = append(events, outbox.Event{Type: typ, ProjectId: t.ProjectId.Hex(), TicketId: t.Id.Hex(), Visibility: "agent",
				Actor: slaActor, OccurredAt: now, SLAKind: string(n.Kind), SLADueAt: &due})
		}
		octx := ctx
		if in.Quiet {
			octx = outbox.Suppress(ctx)
		}
		if err := r.insertOutbox(octx, t, events, nil, now); err != nil {
			return err
		}
	}
	if err := r.scheduleSLA(ctx, &in, now); err != nil {
		return err
	}
	return r.projectSLA(ctx, &t, &in)
}

// deactivateSLA stops an open cycle because SLA was turned off at the given
// time (or at the cycle's last change, when that is later, so no time runs
// backwards) and clears its ticket's due-soon projection. Its pending
// timers find it no longer current and do nothing.
func (r *TicketsRepository) deactivateSLA(ctx context.Context, in *sla.Instance, at time.Time) error {
	if in.UpdatedAt.After(at) {
		at = in.UpdatedAt
	}
	in.Deactivate(at)
	if err := r.writeSLA(ctx, in); err != nil {
		return err
	}
	_, err := r.store.col(colTickets).UpdateOne(ctx, bson.D{{Key: "_id", Value: mustOID(in.TicketId)}},
		bson.D{{Key: "$set", Value: bson.D{{Key: "slaDueAt", Value: nil}, {Key: "slaWarnAt", Value: nil}, {Key: "slaOutcome", Value: nil}}}})
	return err
}

// slaStopActor is the audit actor of the deactivation sweep.
var slaStopActor = audit.System(audit.SystemSLA, "SLA turned off")

// StopSLA runs one part of a deactivation sweep inside the caller's
// transaction (the stop job's fenced completion). Stopped cycles no longer
// match, so each part takes the next open ones (index project_open_cycles);
// cycles started after a reactivation have a newer version and stay.
func (r *TicketsRepository) StopSLA(ctx context.Context, run sla.StopRun, limit int, now time.Time) error {
	docs, err := findAll[slaInstanceDoc](ctx, r.store.col(colSLAInstances), bson.D{{Key: "projectId", Value: mustOID(run.ProjectId)},
		{Key: "current", Value: true}, {Key: "closedAt", Value: nil}, {Key: "snapshot.policyVersion", Value: bson.D{{Key: "$lt", Value: run.BeforeVersion}}}},
		options.Find().SetLimit(int64(limit+1)))
	if err != nil {
		return err
	}
	more := len(docs) > limit
	if more {
		docs = docs[:limit]
	}
	for _, d := range docs {
		in := d.toDomain()
		if err := r.deactivateSLA(ctx, &in, run.At); err != nil {
			return err
		}
	}
	run.Stopped += len(docs)
	if more {
		run.Part++
		return enqueueJob(ctx, r.store, sla.StopJob(run, now), now)
	}
	ev := audit.New(ctx, slaStopActor, audit.SLACyclesStopped, "", now)
	ev.ProjectId, ev.Changes = run.ProjectId, []string{"stopped:" + strconv.Itoa(run.Stopped)}
	return r.store.insertAudit(ctx, mustOID(run.WorkspaceId), ev)
}

// ---- Policies ----

type slaPolicyDoc struct {
	Id             bson.ObjectID   `bson:"_id"`
	WorkspaceId    bson.ObjectID   `bson:"workspaceId"`
	ProjectId      bson.ObjectID   `bson:"projectId"`
	Active         bool            `bson:"active"`
	Targets        []slaTargetsDoc `bson:"targets"`
	WarningPercent int32           `bson:"warningPercent"`
	Version        int64           `bson:"version"`
	// The latest save that turned SLA off (absent: never).
	DeactivatedVersion int64      `bson:"deactivatedVersion,omitempty"`
	DeactivatedAt      *time.Time `bson:"deactivatedAt,omitempty"`
	CreatedAt          time.Time  `bson:"createdAt"`
	UpdatedAt          time.Time  `bson:"updatedAt"`
}

func (d slaPolicyDoc) toDomain() sla.Policy {
	return sla.Policy{Id: d.Id.Hex(), WorkspaceId: d.WorkspaceId.Hex(), ProjectId: d.ProjectId.Hex(), Active: d.Active,
		Targets: targetsFromDocs(d.Targets), WarningPercent: int(d.WarningPercent),
		Version: d.Version, DeactivatedVersion: d.DeactivatedVersion, DeactivatedAt: d.DeactivatedAt,
		CreatedAt: d.CreatedAt, UpdatedAt: d.UpdatedAt}
}

// getSLAPolicy reads a project's policy (inside the caller's transaction,
// if any); sla.ErrNotFound when the project has none.
func getSLAPolicy(ctx context.Context, s *Store, projectID bson.ObjectID) (sla.Policy, error) {
	var d slaPolicyDoc
	err := s.col(colSLAPolicies).FindOne(ctx, bson.D{{Key: "projectId", Value: projectID}}).Decode(&d)
	if errors.Is(err, mongo.ErrNoDocuments) {
		return sla.Policy{}, sla.ErrNotFound
	}
	if err != nil {
		return sla.Policy{}, err
	}
	return d.toDomain(), nil
}

// SLAPolicyRepository implements sla.Repository.
type SLAPolicyRepository struct{ store *Store }

var _ sla.Repository = (*SLAPolicyRepository)(nil)

// NewSLAPolicyRepository returns the policy repository.
func NewSLAPolicyRepository(s *Store) *SLAPolicyRepository { return &SLAPolicyRepository{store: s} }

// GetPolicy returns the project's policy.
func (r *SLAPolicyRepository) GetPolicy(ctx context.Context, projectID string) (sla.Policy, error) {
	return getSLAPolicy(ctx, r.store, mustOID(projectID))
}

// SavePolicy inserts or replaces the policy with its audit event and, when
// the save turned SLA off, the stop job.
func (r *SLAPolicyRepository) SavePolicy(ctx context.Context, p sla.Policy, expectedVersion int64, ev audit.Event, stop *jobs.NewJob) (sla.Policy, error) {
	id := bson.NewObjectID()
	if p.Id != "" {
		id = mustOID(p.Id)
	}
	doc := slaPolicyDoc{Id: id, WorkspaceId: mustOID(p.WorkspaceId), ProjectId: mustOID(p.ProjectId), Active: p.Active,
		Targets: targetsDocs(p.Targets), WarningPercent: int32(p.WarningPercent),
		Version: p.Version, DeactivatedVersion: p.DeactivatedVersion, DeactivatedAt: p.DeactivatedAt,
		CreatedAt: p.CreatedAt, UpdatedAt: p.UpdatedAt}
	err := r.store.transaction(ctx, func(ctx context.Context) error {
		col := r.store.col(colSLAPolicies)
		if expectedVersion == 0 {
			if _, err := col.InsertOne(ctx, doc); err != nil {
				if mongo.IsDuplicateKeyError(err) {
					return sla.ErrConflict
				}
				return err
			}
		} else {
			res, err := col.ReplaceOne(ctx, bson.D{{Key: "_id", Value: id}, {Key: "version", Value: expectedVersion}}, doc)
			if err != nil {
				return err
			}
			if res.MatchedCount == 0 {
				return sla.ErrConflict
			}
		}
		if stop != nil {
			if err := enqueueJob(ctx, r.store, *stop, p.UpdatedAt); err != nil {
				return err
			}
		}
		ev.Target = "sla/" + id.Hex()
		return r.store.insertAudit(ctx, doc.WorkspaceId, ev)
	})
	if err != nil {
		return sla.Policy{}, err
	}
	return doc.toDomain(), nil
}
