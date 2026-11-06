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
	"github.com/arksnorman/open-kase/api/internal/holdreasons"
	"github.com/arksnorman/open-kase/api/internal/idempotency"
	"github.com/arksnorman/open-kase/api/internal/jobs"
	"github.com/arksnorman/open-kase/api/internal/outbox"
	"github.com/arksnorman/open-kase/api/internal/sla"
	"github.com/arksnorman/open-kase/api/internal/statuses"
	"github.com/arksnorman/open-kase/api/internal/tickets"
	"github.com/arksnorman/open-kase/api/package/pagination"
)

type ticketDoc struct {
	Id          bson.ObjectID `bson:"_id"`
	WorkspaceId bson.ObjectID `bson:"workspaceId"`
	ProjectId   bson.ObjectID `bson:"projectId"`
	Number      int64         `bson:"number"`
	Subject     string        `bson:"subject"`
	RequesterId bson.ObjectID `bson:"requesterId"`
	// Organisation the creator linked explicitly (never inferred).
	OrganisationId    *bson.ObjectID `bson:"organisationId,omitempty"`
	CCEmails          []string       `bson:"ccEmails,omitempty"`
	GroupId           bson.ObjectID  `bson:"groupId"`
	AssigneeId        *bson.ObjectID `bson:"assigneeId"`
	Priority          int32          `bson:"priority"`
	RequestedPriority *int32         `bson:"requestedPriority"`
	StatusId          bson.ObjectID  `bson:"statusId"`
	StatusCategory    string         `bson:"statusCategory"`
	RequestTypeId     bson.ObjectID  `bson:"requestTypeId"`
	Source            string         `bson:"source"`
	WatcherCount      int32          `bson:"watcherCount"`
	LastPublicId      *bson.ObjectID `bson:"lastPublicMessageId"`
	CreatedBy         actorDoc       `bson:"createdBy"`
	CreatedAt         time.Time      `bson:"createdAt"`
	UpdatedAt         time.Time      `bson:"updatedAt"`
	PublicUpdatedAt   time.Time      `bson:"publicUpdatedAt"`
	PublicActivityAt  time.Time      `bson:"publicActivityAt"`
	StaffActivityAt   time.Time      `bson:"staffActivityAt"`
	ResolvedAt        *time.Time     `bson:"resolvedAt"`
	ArchivedAt        *time.Time     `bson:"archivedAt"`
	Version           int64          `bson:"version"`
	// Milestone 6.
	Tags            []string   `bson:"tags,omitempty"`
	FirstResponseAt *time.Time `bson:"firstResponseAt,omitempty"`
	SLADueAt        *time.Time `bson:"slaDueAt,omitempty"`
	SLAWarnAt       *time.Time `bson:"slaWarnAt,omitempty"`
	// SLAOutcome is "met" or "missed" once the current cycle's first response happened.
	SLAOutcome *string `bson:"slaOutcome,omitempty"`
	// Lifecycle automation.
	Automation *automationDoc `bson:"automation,omitempty"`
	// On hold (all three set together).
	HoldReasonId   *bson.ObjectID `bson:"holdReasonId,omitempty"`
	HoldReasonName string         `bson:"holdReasonName,omitempty"`
	HoldUntil      *time.Time     `bson:"holdUntil,omitempty"`
}

// automationDoc is the ticket's lifecycle automation bookkeeping.
type automationDoc struct {
	Generation     int64      `bson:"gen"`
	WaitingSince   *time.Time `bson:"waitingSince"`
	Reminders      int32      `bson:"reminders"`
	LastReminderAt *time.Time `bson:"lastReminderAt"`
	// Notice: a reminder of this waiting period announced auto-resolution.
	Notice bool `bson:"notice,omitempty"`
}

func (d *automationDoc) toDomain() tickets.AutomationState {
	if d == nil {
		return tickets.AutomationState{}
	}
	return tickets.AutomationState{Generation: d.Generation, WaitingSince: d.WaitingSince, Reminders: int(d.Reminders), LastReminderAt: d.LastReminderAt,
		NoticeGiven: d.Notice}
}

// redactionDoc records a message redaction (never the old body).
type redactionDoc struct {
	At     time.Time `bson:"at"`
	By     actorDoc  `bson:"by"`
	Reason string    `bson:"reason"`
}

type messageAuthorDoc struct {
	Kind  string         `bson:"kind"`
	Id    bson.ObjectID  `bson:"id"`
	ViaId *bson.ObjectID `bson:"viaId,omitempty"`
}

type messageDoc struct {
	Id              bson.ObjectID    `bson:"_id"`
	WorkspaceId     bson.ObjectID    `bson:"workspaceId"`
	ProjectId       bson.ObjectID    `bson:"projectId"`
	TicketId        bson.ObjectID    `bson:"ticketId"`
	Visibility      string           `bson:"visibility"`
	Author          messageAuthorDoc `bson:"author"`
	Body            string           `bson:"body"`
	AttachmentCount int32            `bson:"attachmentCount"`
	Seq             int64            `bson:"seq"`
	CreatedAt       time.Time        `bson:"createdAt"`
	Redacted        *redactionDoc    `bson:"redacted,omitempty"`
	// ScheduledID links the delivery of a scheduled message (unique index
	// scheduled_unique: a scheduled message produces at most one message).
	ScheduledId *bson.ObjectID `bson:"scheduledMessageId,omitempty"`
}

type draftDoc struct {
	Id               bson.ObjectID  `bson:"_id"`
	TicketId         bson.ObjectID  `bson:"ticketId"`
	AuthorId         bson.ObjectID  `bson:"authorId"`
	Visibility       string         `bson:"visibility"`
	Body             string         `bson:"body"`
	BaseLastPublicId *bson.ObjectID `bson:"baseLastPublicId"`
	CreatedAt        time.Time      `bson:"createdAt"`
	UpdatedAt        time.Time      `bson:"updatedAt"`
	Version          int64          `bson:"version"`
}

type watcherDoc struct {
	Id          bson.ObjectID `bson:"_id"`
	WorkspaceId bson.ObjectID `bson:"workspaceId"`
	ProjectId   bson.ObjectID `bson:"projectId"`
	TicketId    bson.ObjectID `bson:"ticketId"`
	UserId      bson.ObjectID `bson:"userId"`
	CreatedAt   time.Time     `bson:"createdAt"`
}

type outboxDoc struct {
	Id            bson.ObjectID      `bson:"_id"`
	WorkspaceId   bson.ObjectID      `bson:"workspaceId"`
	ProjectId     *bson.ObjectID     `bson:"projectId"`
	TicketId      *bson.ObjectID     `bson:"ticketId"`
	MessageId     *bson.ObjectID     `bson:"messageId,omitempty"`
	Type          string             `bson:"type"`
	Visibility    string             `bson:"visibility,omitempty"`
	TicketVersion int64              `bson:"ticketVersion"`
	Actor         actorDoc           `bson:"actor"`
	MessageAuthor *messageAuthorDoc  `bson:"messageAuthor,omitempty"`
	Transition    string             `bson:"transition,omitempty"`
	Changes       []string           `bson:"changes,omitempty"`
	UserId        *bson.ObjectID     `bson:"userId,omitempty"`
	GroupId       *bson.ObjectID     `bson:"groupId,omitempty"`
	Snapshot      *outboxSnapshotDoc `bson:"snapshot,omitempty"`
	SLA           *outboxSLADoc      `bson:"sla,omitempty"`
	State         string             `bson:"state"`
	Attempts      int32              `bson:"attempts"`
	SchemaVersion int32              `bson:"schemaVersion"`
	LeaseOwner    string             `bson:"leaseOwner,omitempty"`
	LeaseUntil    *time.Time         `bson:"leaseUntil,omitempty"`
	LeaseGen      int64              `bson:"leaseGeneration,omitempty"`
	FanoutCount   int32              `bson:"fanoutCount,omitempty"`
	DispatchedAt  *time.Time         `bson:"dispatchedAt,omitempty"`
	LastError     string             `bson:"lastError,omitempty"`
	OccurredAt    time.Time          `bson:"occurredAt"`
	CreatedAt     time.Time          `bson:"createdAt"`
}

// outboxSLADoc describes an SLA warning or breach event.
type outboxSLADoc struct {
	Kind  string    `bson:"kind"`
	DueAt time.Time `bson:"dueAt"`
}

// outboxSnapshotDoc is the ticket state right after the change.
type outboxSnapshotDoc struct {
	Subject        string         `bson:"subject"`
	Priority       int32          `bson:"priority"`
	StatusId       bson.ObjectID  `bson:"statusId"`
	StatusCategory string         `bson:"statusCategory"`
	GroupId        bson.ObjectID  `bson:"groupId"`
	AssigneeId     *bson.ObjectID `bson:"assigneeId"`
	RequesterId    bson.ObjectID  `bson:"requesterId"`
	RequestTypeId  bson.ObjectID  `bson:"requestTypeId"`
	OrganisationId *bson.ObjectID `bson:"organisationId,omitempty"`
}

func snapshotOf(d ticketDoc) *outboxSnapshotDoc {
	return &outboxSnapshotDoc{Subject: d.Subject, Priority: d.Priority, StatusId: d.StatusId, StatusCategory: d.StatusCategory,
		GroupId: d.GroupId, AssigneeId: d.AssigneeId, RequesterId: d.RequesterId, RequestTypeId: d.RequestTypeId,
		OrganisationId: d.OrganisationId}
}

var (
	_ tickets.Repository = (*TicketsRepository)(nil)
	_ tickets.FileStore  = (*AttachmentsRepository)(nil)
)

// TicketsRepository implements tickets.Repository.
type TicketsRepository struct {
	store    *Store
	onChange func(projectID string)
}

// NewTicketsRepository returns a tickets repository.
func NewTicketsRepository(s *Store) *TicketsRepository { return &TicketsRepository{store: s} }

func counterID(projectID bson.ObjectID) string { return "ticket_number:" + projectID.Hex() }

// EnsureNumberAtLeast moves the project's ticket counter so the next
// ticket number is at least next. Numbers are never reused; this only
// skips ahead (used by the demo seed).
func (r *TicketsRepository) EnsureNumberAtLeast(ctx context.Context, projectID string, next int64) error {
	_, err := r.store.col(colCounters).UpdateOne(ctx, bson.D{{Key: "_id", Value: counterID(mustOID(projectID))}},
		bson.D{{Key: "$max", Value: bson.D{{Key: "seq", Value: next - 1}}}}, options.UpdateOne().SetUpsert(true))
	return err
}

func toActorDoc(a audit.Actor) actorDoc { return actorDoc{Type: a.Type, Id: a.Id, Label: a.Label} }

// insertOutbox stores the events of a ticket change with a snapshot of the
// ticket after the change (doc) and, for message events, the author.
func (r *TicketsRepository) insertOutbox(ctx context.Context, doc ticketDoc, events []outbox.Event, msg *messageDoc, now time.Time) error {
	if len(events) == 0 {
		return nil
	}
	state := outbox.StatePending
	if outbox.Suppressed(ctx) {
		state = outbox.StateSuppressed
	}
	docs := make([]any, 0, len(events))
	for _, e := range events {
		tid := doc.Id
		d := outboxDoc{Id: bson.NewObjectID(), WorkspaceId: doc.WorkspaceId, ProjectId: optOID(e.ProjectId), TicketId: &tid,
			Type: e.Type, Visibility: e.Visibility, TicketVersion: doc.Version, Actor: actorDoc{Type: e.Actor.Type, Id: e.Actor.Id, Label: e.Actor.Label, Scopes: e.Actor.Scopes}, State: state,
			Transition: e.Transition, Changes: e.Changes, Snapshot: snapshotOf(doc),
			SchemaVersion: outbox.SchemaVersion, OccurredAt: e.OccurredAt, CreatedAt: now}
		if e.UserId != "" {
			d.UserId = optOID(e.UserId)
		}
		if e.SLAKind != "" && e.SLADueAt != nil {
			d.SLA = &outboxSLADoc{Kind: e.SLAKind, DueAt: *e.SLADueAt}
		}
		if msg != nil && (e.Type == outbox.MessageCreated || e.Type == outbox.TicketCreated) {
			mid := msg.Id
			d.MessageId = &mid
			a := msg.Author
			d.MessageAuthor = &a
		}
		docs = append(docs, d)
	}
	_, err := r.store.col(colOutbox).InsertMany(ctx, docs)
	return err
}

// attach links staged uploads to a new message. Every ID must still be a
// staged, unexpired upload by the same uploader in the project.
func (r *TicketsRepository) attach(ctx context.Context, projectID, ticketID, messageID bson.ObjectID, visibility string, ids []string, up tickets.Uploader, now time.Time, scheduledID string) error {
	if len(ids) == 0 {
		return nil
	}
	oids := make(bson.A, 0, len(ids))
	for _, id := range ids {
		oids = append(oids, mustOID(id))
	}
	// Unreserved uploads must not have expired; uploads reserved by the
	// scheduled message being sent have no expiry.
	filter := bson.D{
		{Key: "_id", Value: bson.D{{Key: "$in", Value: oids}}}, {Key: "projectId", Value: projectID},
		{Key: "state", Value: tickets.AttachmentStaged},
		{Key: "uploader.kind", Value: up.Kind}, {Key: "uploader.id", Value: mustOID(up.Id)},
	}
	if scheduledID != "" {
		filter = append(filter, bson.E{Key: "scheduledMessageId", Value: mustOID(scheduledID)})
	} else {
		filter = append(filter, bson.E{Key: "expiresAt", Value: bson.D{{Key: "$gt", Value: now}}},
			bson.E{Key: "scheduledMessageId", Value: bson.D{{Key: "$exists", Value: false}}})
	}
	res, err := r.store.col(colAttachments).UpdateMany(ctx, filter, bson.D{{Key: "$set", Value: bson.D{
		{Key: "state", Value: tickets.AttachmentAttached}, {Key: "ticketId", Value: ticketID}, {Key: "messageId", Value: messageID},
		{Key: "visibility", Value: visibility}, {Key: "attachedAt", Value: now}, {Key: "expiresAt", Value: nil},
	}}, {Key: "$unset", Value: bson.D{{Key: "scheduledMessageId", Value: ""}}}})
	if err != nil {
		return err
	}
	if res.ModifiedCount != int64(len(ids)) {
		return tickets.ErrAttachmentsGone
	}
	return nil
}

func authorDoc(a tickets.Author) messageAuthorDoc {
	return messageAuthorDoc{Kind: a.Kind, Id: mustOID(a.Id), ViaId: optOID(a.ViaId)}
}

// ---- Create ----

// Create allocates the next project number and inserts the ticket, its
// first message, attachment links, audit events, outbox records and the
// idempotency marker in one transaction. A retried command whose marker
// shows a committed ticket returns that ticket instead of a duplicate.
func (r *TicketsRepository) Create(ctx context.Context, cmd tickets.CreateCommand) (tickets.Ticket, error) {
	t := cmd.Ticket
	ws, pid := mustOID(t.WorkspaceId), mustOID(t.ProjectId)
	var created bson.ObjectID
	err := r.store.transaction(ctx, func(ctx context.Context) error {
		created = bson.ObjectID{}
		if cmd.Idempotency != nil {
			res, err := r.IdempotentResource(ctx, *cmd.Idempotency)
			if err != nil {
				return err
			}
			if id, ok := cutPrefix(res, "ticket/"); ok {
				created = mustOID(id)
				return nil
			}
		}
		var counter struct {
			Seq int64 `bson:"seq"`
		}
		if err := r.store.col(colCounters).FindOneAndUpdate(ctx, bson.D{{Key: "_id", Value: counterID(pid)}},
			bson.D{{Key: "$inc", Value: bson.D{{Key: "seq", Value: int64(1)}}}},
			options.FindOneAndUpdate().SetUpsert(true).SetReturnDocument(options.After)).Decode(&counter); err != nil {
			return err
		}
		if err := r.touchGroup(ctx, pid, mustOID(t.Group.Id)); err != nil {
			return err
		}
		var orgID *bson.ObjectID
		if t.Organisation != nil {
			id := mustOID(t.Organisation.Id)
			ok, active, err := r.store.touchOrganisation(ctx, pid, id, t.CreatedAt)
			if err != nil {
				return err
			}
			if !ok {
				return tickets.ErrOrganisationGone
			}
			if !active && !cmd.KeepOrganisation {
				return tickets.ErrOrganisationInactive
			}
			orgID = &id
		}
		tid, mid := bson.NewObjectID(), bson.NewObjectID()
		doc := ticketDoc{
			Id: tid, WorkspaceId: ws, ProjectId: pid, Number: counter.Seq,
			Subject: t.Subject, CCEmails: t.CCEmails, Tags: t.Tags, RequesterId: mustOID(t.Requester.Id), GroupId: mustOID(t.Group.Id), OrganisationId: orgID,
			Priority: int32(t.Priority), StatusId: mustOID(t.Status.Id), StatusCategory: string(t.StatusCategory),
			RequestTypeId: mustOID(t.RequestType.Id), Source: t.Source, LastPublicId: &mid, CreatedBy: toActorDoc(cmd.CreatedBy),
			CreatedAt: t.CreatedAt, UpdatedAt: t.UpdatedAt, PublicUpdatedAt: t.PublicUpdatedAt, PublicActivityAt: t.PublicActivityAt, StaffActivityAt: t.StaffActivityAt, Version: 1,
		}
		if t.RequestedPriority > 0 {
			rp := int32(t.RequestedPriority)
			doc.RequestedPriority = &rp
		}
		var cycle *sla.Instance
		if cmd.SLA != nil {
			c := cmd.SLA.Clone()
			cycle = &c
			doc.SLADueAt, doc.SLAWarnAt = cycle.Projection()
		}
		if _, err := r.store.col(colTickets).InsertOne(ctx, doc); err != nil {
			return err
		}
		if cycle != nil {
			if err := r.insertSLA(ctx, doc, cycle); err != nil {
				return err
			}
			if err := r.scheduleSLA(ctx, cycle, t.CreatedAt); err != nil {
				return err
			}
		}
		msg := messageDoc{Id: mid, WorkspaceId: ws, ProjectId: pid, TicketId: tid, Visibility: cmd.Message.Visibility,
			Author: authorDoc(cmd.Message.Author), Body: cmd.Message.Body, AttachmentCount: int32(len(cmd.AttachmentIds)), Seq: 1, CreatedAt: cmd.Message.CreatedAt}
		if _, err := r.store.col(colMessages).InsertOne(ctx, msg); err != nil {
			return err
		}
		if err := r.attach(ctx, pid, tid, mid, cmd.Message.Visibility, cmd.AttachmentIds, cmd.Uploader, t.CreatedAt, ""); err != nil {
			return err
		}
		events := make([]audit.Event, len(cmd.Audit))
		for i, ev := range cmd.Audit {
			ev.ProjectId, ev.TicketId, ev.Target = t.ProjectId, tid.Hex(), "ticket/"+tid.Hex()
			events[i] = ev
		}
		if err := r.store.insertAudit(ctx, ws, events...); err != nil {
			return err
		}
		if err := r.insertOutbox(ctx, doc, cmd.Outbox, &msg, t.CreatedAt); err != nil {
			return err
		}
		if cmd.Link != nil {
			l := *cmd.Link
			l.SourceId = tid.Hex()
			if err := r.insertLinkTx(ctx, ws, l, cmd.LinkAudit, cmd.LinkOutbox); err != nil {
				return err
			}
		}
		if cmd.Idempotency != nil {
			if _, err := r.store.col(colIdempotency).UpdateOne(ctx,
				bson.D{{Key: "scope", Value: cmd.Idempotency.Scope}, {Key: "key", Value: cmd.Idempotency.Key}},
				bson.D{{Key: "$set", Value: bson.D{{Key: "resource", Value: "ticket/" + tid.Hex()}}}}); err != nil {
				return err
			}
		}
		created = tid
		return nil
	})
	if err != nil {
		return tickets.Ticket{}, err
	}
	r.changed(t.ProjectId)
	return r.Get(ctx, t.ProjectId, created.Hex(), cmd.ViewerId)
}

func cutPrefix(s, prefix string) (string, bool) {
	if len(s) > len(prefix) && s[:len(prefix)] == prefix {
		return s[len(prefix):], true
	}
	return "", false
}

// ---- Reads ----

// Organisation resolves a project organisation for linking to a ticket.
func (r *TicketsRepository) Organisation(ctx context.Context, projectID, id string) (tickets.OrgRef, error) {
	oidv, err := oid(id)
	if err != nil {
		return tickets.OrgRef{}, tickets.ErrNotFound
	}
	var d organisationDoc
	err = r.store.col(colOrganisations).FindOne(ctx, bson.D{{Key: "_id", Value: oidv}, {Key: "projectId", Value: mustOID(projectID)}}).Decode(&d)
	if errors.Is(err, mongo.ErrNoDocuments) {
		return tickets.OrgRef{}, tickets.ErrNotFound
	}
	if err != nil {
		return tickets.OrgRef{}, err
	}
	return tickets.OrgRef{Id: d.Id.Hex(), Name: d.Name, ExternalRef: d.ExternalRef, Active: d.Active}, nil
}

// Get finds a ticket by ObjectID within the project.
func (r *TicketsRepository) Get(ctx context.Context, projectID, id, viewerID string) (tickets.Ticket, error) {
	tid, err := oid(id)
	if err != nil {
		return tickets.Ticket{}, tickets.ErrNotFound
	}
	return r.getOne(ctx, bson.D{{Key: "projectId", Value: mustOID(projectID)}, {Key: "_id", Value: tid}}, viewerID)
}

// GetByNumber finds a ticket by number within the project.
func (r *TicketsRepository) GetByNumber(ctx context.Context, projectID string, number int64, viewerID string) (tickets.Ticket, error) {
	return r.getOne(ctx, bson.D{{Key: "projectId", Value: mustOID(projectID)}, {Key: "number", Value: number}}, viewerID)
}

func (r *TicketsRepository) getOne(ctx context.Context, filter bson.D, viewerID string) (tickets.Ticket, error) {
	var doc ticketDoc
	err := r.store.col(colTickets).FindOne(ctx, filter).Decode(&doc)
	if errors.Is(err, mongo.ErrNoDocuments) {
		return tickets.Ticket{}, tickets.ErrNotFound
	}
	if err != nil {
		return tickets.Ticket{}, err
	}
	out, err := r.hydrate(ctx, []ticketDoc{doc}, viewerID)
	if err != nil {
		return tickets.Ticket{}, err
	}
	return out[0], nil
}

// accessClause restricts a ticket query to what acc may read (nil: every
// ticket of the project). It is the query form of tickets.Access.allows:
// tickets in the reader's groups, or open/active/waiting tickets assigned
// to the reader (never a resolved or archived ticket through a leftover
// assignment alone).
func accessClause(acc tickets.Access) bson.D {
	if acc.All {
		return nil
	}
	or := bson.A{bson.D{{Key: "groupId", Value: bson.D{{Key: "$in", Value: oidList(acc.GroupIds)}}}}}
	if acc.UserId != "" {
		workable := bson.A{string(statuses.CategoryOpen), string(statuses.CategoryWaitingCustomer), string(statuses.CategoryOnHold)}
		or = append(or, bson.D{{Key: "assigneeId", Value: mustOID(acc.UserId)},
			{Key: "statusCategory", Value: bson.D{{Key: "$in", Value: workable}}}})
	}
	return bson.D{{Key: "$or", Value: or}}
}

// maxWatched bounds the watched-ticket ID list used by the watching view.
const maxWatched = 5000

func (r *TicketsRepository) filter(ctx context.Context, q tickets.Query) (bson.D, error) {
	pid := mustOID(q.ProjectId)
	f := bson.D{{Key: "projectId", Value: pid}}
	var and bson.A
	if c := accessClause(q.Access); c != nil {
		and = append(and, c)
	}
	if len(q.Categories) > 0 {
		cats := make(bson.A, 0, len(q.Categories))
		for _, c := range q.Categories {
			cats = append(cats, string(c))
		}
		f = append(f, bson.E{Key: "statusCategory", Value: bson.D{{Key: "$in", Value: cats}}})
	}
	if q.AssigneeId != "" {
		f = append(f, bson.E{Key: "assigneeId", Value: mustOID(q.AssigneeId)})
	}
	if len(q.Priorities) > 0 {
		ps := bson.A{}
		for _, p := range q.Priorities {
			ps = append(ps, int32(p))
		}
		f = append(f, bson.E{Key: "priority", Value: bson.D{{Key: "$in", Value: ps}}})
	}
	if len(q.StatusIds) > 0 {
		f = append(f, bson.E{Key: "statusId", Value: bson.D{{Key: "$in", Value: oidList(q.StatusIds)}}})
	}
	if len(q.OrganisationIds) > 0 {
		f = append(f, bson.E{Key: "organisationId", Value: bson.D{{Key: "$in", Value: oidList(q.OrganisationIds)}}})
	}
	if q.RequesterId != "" {
		f = append(f, bson.E{Key: "requesterId", Value: mustOID(q.RequesterId)})
	}
	if len(q.RequestTypeIds) > 0 {
		f = append(f, bson.E{Key: "requestTypeId", Value: bson.D{{Key: "$in", Value: oidList(q.RequestTypeIds)}}})
	}
	if len(q.Tags) > 0 {
		tags := bson.A{}
		for _, t := range q.Tags {
			tags = append(tags, t)
		}
		f = append(f, bson.E{Key: "tags", Value: bson.D{{Key: "$all", Value: tags}}})
	}
	if q.WarnBefore != nil {
		f = append(f, bson.E{Key: "slaWarnAt", Value: bson.D{{Key: "$lte", Value: *q.WarnBefore}}})
	}
	switch q.SLA {
	case "dueSoon": // warned, not yet past due
		f = append(f, bson.E{Key: "slaWarnAt", Value: bson.D{{Key: "$lte", Value: q.Now}}},
			bson.E{Key: "slaDueAt", Value: bson.D{{Key: "$gt", Value: q.Now}}})
	case "breached":
		f = append(f, bson.E{Key: "slaDueAt", Value: bson.D{{Key: "$lte", Value: q.Now}}})
	case "running": // before the warning time (or no warning), not past due
		and = append(and, bson.D{{Key: "slaDueAt", Value: bson.D{{Key: "$gt", Value: q.Now}}}},
			bson.D{{Key: "$or", Value: bson.A{bson.D{{Key: "slaWarnAt", Value: nil}}, bson.D{{Key: "slaWarnAt", Value: bson.D{{Key: "$gt", Value: q.Now}}}}}}})
	case "met", "missed":
		f = append(f, bson.E{Key: "slaOutcome", Value: q.SLA})
	}
	if q.Unassigned {
		f = append(f, bson.E{Key: "assigneeId", Value: nil})
	}
	if q.GroupIds != nil {
		and = append(and, bson.D{{Key: "groupId", Value: bson.D{{Key: "$in", Value: oidList(q.GroupIds)}}}})
	}
	if q.GroupFilter != "" {
		and = append(and, bson.D{{Key: "groupId", Value: mustOID(q.GroupFilter)}})
	}
	if q.WatchedBy != "" {
		ws, err := findAll[watcherDoc](ctx, r.store.col(colWatchers),
			bson.D{{Key: "projectId", Value: pid}, {Key: "userId", Value: mustOID(q.WatchedBy)}},
			options.Find().SetProjection(bson.D{{Key: "ticketId", Value: 1}}).SetLimit(maxWatched))
		if err != nil {
			return nil, err
		}
		ids := make(bson.A, 0, len(ws))
		for _, w := range ws {
			ids = append(ids, w.TicketId)
		}
		and = append(and, bson.D{{Key: "_id", Value: bson.D{{Key: "$in", Value: ids}}}})
	}
	if s := q.Search; s.Number > 0 || s.Subject != "" {
		var or bson.A
		if s.Number > 0 {
			or = append(or, bson.D{{Key: "number", Value: s.Number}})
		}
		if s.Subject != "" {
			or = append(or, bson.D{{Key: "subject", Value: bson.Regex{Pattern: regexpQuote(s.Subject), Options: "i"}}})
		}
		and = append(and, bson.D{{Key: "$or", Value: or}})
	}
	if q.After != nil {
		at, err := time.Parse(time.RFC3339Nano, q.After.Sort)
		id, ierr := oid(q.After.Id)
		if err != nil || ierr != nil {
			return nil, errInvalidCursor
		}
		field := sortField(q)
		// (field < at) or (field = at and _id < id), written with a plain
		// upper bound on the sort field so that the planner can start the
		// index scan at the cursor instead of reading every earlier page.
		and = append(and, bson.D{{Key: field, Value: bson.D{{Key: "$lte", Value: at}}}}, bson.D{{Key: "$or", Value: bson.A{
			bson.D{{Key: field, Value: bson.D{{Key: "$lt", Value: at}}}},
			bson.D{{Key: "_id", Value: bson.D{{Key: "$lt", Value: id}}}},
		}}})
	}
	if len(and) > 0 {
		f = append(f, bson.E{Key: "$and", Value: and})
	}
	return f, nil
}

// sortField is the list order: requester-visible updates for integrations,
// any update for staff.
func sortField(q tickets.Query) string {
	if q.PublicSort {
		return "publicUpdatedAt"
	}
	return "updatedAt"
}

func oidList(ids []string) bson.A {
	out := make(bson.A, 0, len(ids))
	for _, id := range ids {
		out = append(out, mustOID(id))
	}
	return out
}

// List returns one page of tickets, most recently updated first. The
// access filter is part of the query, so the limit applies after it.
func (r *TicketsRepository) List(ctx context.Context, q tickets.Query) ([]tickets.Ticket, error) {
	f, err := r.filter(ctx, q)
	if err != nil {
		return nil, err
	}
	sort := bson.D{{Key: sortField(q), Value: -1}, {Key: "_id", Value: -1}}
	opts := options.Find().SetSort(sort).SetLimit(int64(q.Limit))
	if unfiltered(q) {
		// With only the access clause and the cursor, reading the sort index
		// in order is always right. Left alone, the planner sometimes picks
		// the group and assignee queue indexes for a restricted agent and
		// sorts the whole visible set in memory for every page.
		idx := "projectAllUpdated"
		if q.PublicSort {
			idx = "projectAllPublicUpdated"
		}
		opts.SetHint(idx)
	}
	docs, err := findAll[ticketDoc](ctx, r.store.col(colTickets), f, opts)
	if err != nil {
		return nil, err
	}
	return r.hydrate(ctx, docs, q.ViewerId)
}

// unfiltered reports whether q narrows the tickets only by access (the All
// view without filters).
func unfiltered(q tickets.Query) bool {
	return len(q.Categories) == 0 && q.AssigneeId == "" && !q.Unassigned && q.GroupIds == nil && q.WatchedBy == "" &&
		q.GroupFilter == "" && q.Search == (tickets.Search{}) && len(q.Priorities) == 0 && len(q.StatusIds) == 0 &&
		len(q.Tags) == 0 && len(q.RequestTypeIds) == 0 && len(q.OrganisationIds) == 0 && q.RequesterId == "" && q.WarnBefore == nil && q.SLA == "" && !q.None
}

// Count counts the tickets matching an authorized query.
func (r *TicketsRepository) Count(ctx context.Context, q tickets.Query) (int64, error) {
	q.After = nil
	f, err := r.filter(ctx, q)
	if err != nil {
		return 0, err
	}
	return r.store.col(colTickets).CountDocuments(ctx, f)
}

// hydrate resolves names for groups, users, contacts,
// statuses and request types with one query per collection.
func (r *TicketsRepository) hydrate(ctx context.Context, docs []ticketDoc, viewerID string) ([]tickets.Ticket, error) {
	out := make([]tickets.Ticket, 0, len(docs))
	if len(docs) == 0 {
		return out, nil
	}
	ids := map[string]map[bson.ObjectID]bool{}
	add := func(kind string, id *bson.ObjectID) {
		if id == nil || id.IsZero() {
			return
		}
		if ids[kind] == nil {
			ids[kind] = map[bson.ObjectID]bool{}
		}
		ids[kind][*id] = true
	}
	ticketIDs := make(bson.A, 0, len(docs))
	for i := range docs {
		d := &docs[i]
		add(colGroups, &d.GroupId)
		add(colUsers, d.AssigneeId)
		add(colContacts, &d.RequesterId)
		add(colStatuses, &d.StatusId)
		add(colRequestTypes, &d.RequestTypeId)
		add(colOrganisations, d.OrganisationId)
		ticketIDs = append(ticketIDs, d.Id)
	}
	names, err := r.names(ctx, ids)
	if err != nil {
		return nil, err
	}
	cycles, err := r.currentSLAs(ctx, ticketIDs)
	if err != nil {
		return nil, err
	}
	watched := map[bson.ObjectID]bool{}
	if viewerID != "" {
		ws, err := findAll[watcherDoc](ctx, r.store.col(colWatchers),
			bson.D{{Key: "userId", Value: mustOID(viewerID)}, {Key: "ticketId", Value: bson.D{{Key: "$in", Value: ticketIDs}}}}, options.Find())
		if err != nil {
			return nil, err
		}
		for _, w := range ws {
			watched[w.TicketId] = true
		}
	}
	for _, d := range docs {
		t := tickets.Ticket{
			Id: d.Id.Hex(), WorkspaceId: d.WorkspaceId.Hex(), ProjectId: d.ProjectId.Hex(), Number: d.Number,
			Subject: d.Subject, CCEmails: d.CCEmails, Group: tickets.Ref{Id: d.GroupId.Hex(), Name: names[d.GroupId].name},
			Priority: int(d.Priority), Status: tickets.Ref{Id: d.StatusId.Hex(), Name: names[d.StatusId].name},
			StatusCategory: statuses.Category(d.StatusCategory),
			RequestType:    tickets.Ref{Id: d.RequestTypeId.Hex(), Name: names[d.RequestTypeId].name},
			Source:         d.Source, CreatedBy: audit.Actor{Type: d.CreatedBy.Type, Id: d.CreatedBy.Id, Label: d.CreatedBy.Label}, WatcherCount: int(d.WatcherCount), Watching: watched[d.Id], LastPublicId: hexOrEmpty(d.LastPublicId),
			CreatedAt: d.CreatedAt, UpdatedAt: d.UpdatedAt, PublicUpdatedAt: d.PublicUpdatedAt, PublicActivityAt: d.PublicActivityAt, StaffActivityAt: d.StaffActivityAt,
			ResolvedAt: d.ResolvedAt, ArchivedAt: d.ArchivedAt, Version: d.Version,
			Tags: d.Tags, FirstResponseAt: d.FirstResponseAt, SLA: cycles[d.Id],
			Automation: d.Automation.toDomain(),
		}
		if t.Tags == nil {
			t.Tags = []string{}
		}
		if t.CCEmails == nil {
			t.CCEmails = []string{}
		}
		if d.HoldReasonId != nil && d.HoldUntil != nil {
			t.Hold = &tickets.Hold{ReasonId: d.HoldReasonId.Hex(), ReasonName: d.HoldReasonName, Until: *d.HoldUntil}
		}
		c := names[d.RequesterId]
		t.Requester = tickets.ContactRef{Id: d.RequesterId.Hex(), Name: c.name, Email: c.email, ExternalId: c.externalID}
		if d.AssigneeId != nil {
			t.Assignee = &tickets.Ref{Id: d.AssigneeId.Hex(), Name: names[*d.AssigneeId].name}
		}
		if d.OrganisationId != nil {
			o := names[*d.OrganisationId]
			t.Organisation = &tickets.OrgRef{Id: d.OrganisationId.Hex(), Name: o.name, ExternalRef: o.externalRef}
		}
		if d.RequestedPriority != nil {
			t.RequestedPriority = int(*d.RequestedPriority)
		}
		out = append(out, t)
	}
	return out, nil
}

type nameRec struct{ name, email, externalID, externalRef string }

// names loads display names for IDs grouped by collection.
func (r *TicketsRepository) names(ctx context.Context, ids map[string]map[bson.ObjectID]bool) (map[bson.ObjectID]nameRec, error) {
	out := map[bson.ObjectID]nameRec{}
	for col, set := range ids {
		in := make(bson.A, 0, len(set))
		for id := range set {
			in = append(in, id)
		}
		cur, err := r.store.col(col).Find(ctx, bson.D{{Key: "_id", Value: bson.D{{Key: "$in", Value: in}}}},
			options.Find().SetProjection(bson.D{{Key: "name", Value: 1}, {Key: "displayName", Value: 1}, {Key: "label", Value: 1},
				{Key: "email", Value: 1}, {Key: "externalId", Value: 1}, {Key: "externalRef", Value: 1}}))
		if err != nil {
			return nil, err
		}
		var rows []struct {
			Id          bson.ObjectID `bson:"_id"`
			Name        string        `bson:"name"`
			DisplayName string        `bson:"displayName"`
			Label       string        `bson:"label"`
			Email       string        `bson:"email"`
			ExternalId  string        `bson:"externalId"`
			ExternalRef string        `bson:"externalRef"`
		}
		if err := cur.All(ctx, &rows); err != nil {
			return nil, err
		}
		for _, row := range rows {
			n := row.Name
			switch col {
			case colUsers:
				n = row.DisplayName
			case colStatuses:
				n = row.Label
			}
			out[row.Id] = nameRec{name: n, email: row.Email, externalID: row.ExternalId, externalRef: row.ExternalRef}
		}
	}
	return out, nil
}

// ---- Eligibility and groups ----

// eligibilityFilter matches the user's membership in the group.
func eligibilityFilter(pid, gid, uid bson.ObjectID) bson.D {
	return bson.D{{Key: "projectId", Value: pid}, {Key: "groupId", Value: gid}, {Key: "userId", Value: uid}}
}

func (r *TicketsRepository) eligibleUserAndGroup(ctx context.Context, pid, gid, uid bson.ObjectID) (bool, error) {
	n, err := r.store.col(colUsers).CountDocuments(ctx, bson.D{{Key: "_id", Value: uid}, {Key: "status", Value: "active"}}, options.Count().SetLimit(1))
	if err != nil || n == 0 {
		return false, err
	}
	n, err = r.store.col(colGroups).CountDocuments(ctx, bson.D{{Key: "_id", Value: gid}, {Key: "projectId", Value: pid}, {Key: "active", Value: true}}, options.Count().SetLimit(1))
	return n > 0, err
}

// IsEligible reports whether the user is an active member of the group.
func (r *TicketsRepository) IsEligible(ctx context.Context, projectID, groupID, userID string) (bool, error) {
	pid, gid, uid := mustOID(projectID), mustOID(groupID), mustOID(userID)
	n, err := r.store.col(colGroupMemberships).CountDocuments(ctx, eligibilityFilter(pid, gid, uid), options.Count().SetLimit(1))
	if err != nil || n == 0 {
		return false, err
	}
	return r.eligibleUserAndGroup(ctx, pid, gid, uid)
}

// touchEligible proves eligibility inside a transaction. Writing the group
// membership makes a concurrent removal of that membership conflict with
// the assignment.
func (r *TicketsRepository) touchEligible(ctx context.Context, pid, gid, uid bson.ObjectID) error {
	res, err := r.store.col(colGroupMemberships).UpdateOne(ctx, eligibilityFilter(pid, gid, uid),
		bson.D{{Key: "$inc", Value: bson.D{{Key: "assignmentRevision", Value: int64(1)}}}})
	if err != nil {
		return err
	}
	if res.MatchedCount == 0 {
		return tickets.ErrIneligible
	}
	if err := r.touchGroup(ctx, pid, gid); errors.Is(err, tickets.ErrGroupUnavailable) {
		return tickets.ErrIneligible
	} else if err != nil {
		return err
	}
	ok, err := r.eligibleUserAndGroup(ctx, pid, gid, uid)
	if err != nil {
		return err
	}
	if !ok {
		return tickets.ErrIneligible
	}
	return nil
}

// touchGroup proves inside a transaction that the group exists and is
// active. Writing the group document makes a concurrent deactivation or
// deletion (which write the same document) conflict with the ticket change.
func (r *TicketsRepository) touchGroup(ctx context.Context, pid, gid bson.ObjectID) error {
	res, err := r.store.col(colGroups).UpdateOne(ctx, bson.D{{Key: "_id", Value: gid}, {Key: "projectId", Value: pid}, {Key: "active", Value: true}},
		bson.D{{Key: "$inc", Value: bson.D{{Key: "ticketRevision", Value: int64(1)}}}})
	if err != nil {
		return err
	}
	if res.MatchedCount == 0 {
		return tickets.ErrGroupUnavailable
	}
	return nil
}

// AssignedTickets lists live tickets assigned to a user that are not
// resolved or archived, ordered by ID for resumable paging.
func (r *TicketsRepository) AssignedTickets(ctx context.Context, workspaceID, projectID, userID, afterID string, limit int) ([]tickets.Ticket, error) {
	filter := bson.D{{Key: "workspaceId", Value: mustOID(workspaceID)}, {Key: "assigneeId", Value: mustOID(userID)},
		{Key: "statusCategory", Value: bson.D{{Key: "$nin", Value: bson.A{string(statuses.CategoryResolved), string(statuses.CategoryArchived)}}}}}
	if projectID != "" {
		filter = append(filter, bson.E{Key: "projectId", Value: mustOID(projectID)})
	}
	if afterID != "" {
		filter = append(filter, bson.E{Key: "_id", Value: bson.D{{Key: "$gt", Value: mustOID(afterID)}}})
	}
	docs, err := findAll[ticketDoc](ctx, r.store.col(colTickets), filter, options.Find().SetSort(bson.D{{Key: "_id", Value: 1}}).SetLimit(int64(limit)))
	if err != nil {
		return nil, err
	}
	return r.hydrate(ctx, docs, "")
}

// ActiveGroup returns a project group and whether it is active.
func (r *TicketsRepository) ActiveGroup(ctx context.Context, projectID, groupID string) (tickets.Ref, bool, error) {
	var g groupDoc
	err := r.store.col(colGroups).FindOne(ctx, bson.D{{Key: "_id", Value: mustOID(groupID)}, {Key: "projectId", Value: mustOID(projectID)}}).Decode(&g)
	if errors.Is(err, mongo.ErrNoDocuments) {
		return tickets.Ref{}, false, tickets.ErrNotFound
	}
	if err != nil {
		return tickets.Ref{}, false, err
	}
	return tickets.Ref{Id: g.Id.Hex(), Name: g.Name}, g.Active, nil
}

// DefaultGroup returns the project's protected Triage group.
func (r *TicketsRepository) DefaultGroup(ctx context.Context, projectID string) (tickets.Ref, error) {
	var g groupDoc
	err := r.store.col(colGroups).FindOne(ctx, bson.D{{Key: "projectId", Value: mustOID(projectID)}, {Key: "isDefault", Value: true}}).Decode(&g)
	if err != nil {
		return tickets.Ref{}, err
	}
	return tickets.Ref{Id: g.Id.Hex(), Name: g.Name}, nil
}

// ---- Mutations ----

func changeSet(ch tickets.Changes, messageID bson.ObjectID) bson.D {
	set := bson.D{{Key: "updatedAt", Value: ch.UpdatedAt}}
	if ch.Subject != nil {
		set = append(set, bson.E{Key: "subject", Value: *ch.Subject})
	}
	if ch.OrganisationId != nil && *ch.OrganisationId != "" {
		set = append(set, bson.E{Key: "organisationId", Value: mustOID(*ch.OrganisationId)})
	}
	if ch.Priority != nil {
		set = append(set, bson.E{Key: "priority", Value: int32(*ch.Priority)})
	}
	if ch.RequestTypeId != nil {
		set = append(set, bson.E{Key: "requestTypeId", Value: mustOID(*ch.RequestTypeId)})
	}
	if ch.GroupId != nil {
		set = append(set, bson.E{Key: "groupId", Value: mustOID(*ch.GroupId)})
	}
	if ch.AssigneeId != nil {
		set = append(set, bson.E{Key: "assigneeId", Value: optOID(*ch.AssigneeId)})
	}
	if ch.StatusId != nil {
		set = append(set, bson.E{Key: "statusId", Value: mustOID(*ch.StatusId)}, bson.E{Key: "statusCategory", Value: string(*ch.StatusCategory)})
	}
	if ch.ResolvedAt != nil {
		set = append(set, bson.E{Key: "resolvedAt", Value: *ch.ResolvedAt})
	}
	if ch.ArchivedAt != nil {
		set = append(set, bson.E{Key: "archivedAt", Value: *ch.ArchivedAt})
	}
	if ch.PublicActivityAt != nil {
		set = append(set, bson.E{Key: "publicActivityAt", Value: *ch.PublicActivityAt})
	}
	if ch.StaffActivityAt != nil {
		set = append(set, bson.E{Key: "staffActivityAt", Value: *ch.StaffActivityAt})
	}
	if ch.PublicUpdatedAt != nil {
		set = append(set, bson.E{Key: "publicUpdatedAt", Value: *ch.PublicUpdatedAt})
	}
	if ch.SetLastPublic {
		set = append(set, bson.E{Key: "lastPublicMessageId", Value: messageID})
	}
	if ch.Tags != nil {
		set = append(set, bson.E{Key: "tags", Value: append([]string{}, (*ch.Tags)...)})
	}
	if ch.CCEmails != nil {
		set = append(set, bson.E{Key: "ccEmails", Value: append([]string{}, (*ch.CCEmails)...)})
	}
	if ch.FirstResponseAt != nil {
		set = append(set, bson.E{Key: "firstResponseAt", Value: *ch.FirstResponseAt})
	}
	return set
}

func changeUpdate(ch tickets.Changes, messageID bson.ObjectID) bson.D {
	set, inc := changeSet(ch, messageID), bson.D{{Key: "version", Value: int64(1)}}
	if a := ch.Automation; a != nil {
		set = append(set, bson.E{Key: "automation.waitingSince", Value: a.WaitingSince}, bson.E{Key: "automation.reminders", Value: int32(0)},
			bson.E{Key: "automation.lastReminderAt", Value: nil}, bson.E{Key: "automation.notice", Value: false})
		inc = append(inc, bson.E{Key: "automation.gen", Value: int64(1)})
	}
	if h := ch.Hold; h != nil {
		set = append(set, bson.E{Key: "holdReasonId", Value: mustOID(h.ReasonId)}, bson.E{Key: "holdReasonName", Value: h.ReasonName},
			bson.E{Key: "holdUntil", Value: h.Until})
	}
	update := bson.D{{Key: "$set", Value: set}, {Key: "$inc", Value: inc}}
	var unset bson.D
	if ch.ClearHold {
		unset = append(unset, bson.E{Key: "holdReasonId", Value: ""}, bson.E{Key: "holdReasonName", Value: ""}, bson.E{Key: "holdUntil", Value: ""})
	}
	if ch.OrganisationId != nil && *ch.OrganisationId == "" {
		unset = append(unset, bson.E{Key: "organisationId", Value: ""})
	}
	if len(unset) > 0 {
		update = append(update, bson.E{Key: "$unset", Value: unset})
	}
	return update
}

// Mutate applies one conditional ticket change with its message,
// attachments, draft removal, audit events and outbox
// records in a single transaction. A failed condition returns
// tickets.ErrPrecondition and changes nothing.
func (r *TicketsRepository) Mutate(ctx context.Context, m tickets.Mutation) (tickets.Ticket, tickets.Message, error) {
	pid, tid := mustOID(m.ProjectId), mustOID(m.TicketId)
	var doc ticketDoc
	var msg *messageDoc
	err := r.store.transaction(ctx, func(ctx context.Context) error {
		msg = nil
		if m.Eligible != nil {
			if err := r.touchEligible(ctx, pid, mustOID(m.Eligible.GroupId), mustOID(m.Eligible.UserId)); err != nil {
				return err
			}
		}
		if g := m.Changes.GroupId; g != nil {
			if err := r.touchGroup(ctx, pid, mustOID(*g)); err != nil {
				return err
			}
		}
		if o := m.Changes.OrganisationId; o != nil && *o != "" {
			ok, active, err := r.store.touchOrganisation(ctx, pid, mustOID(*o), m.Changes.UpdatedAt)
			if err != nil {
				return err
			}
			if !ok {
				return tickets.ErrOrganisationGone
			}
			// Only a changed organisation reaches here (the service skips an unchanged one).
			if !active {
				return tickets.ErrOrganisationInactive
			}
		}
		filter := bson.D{{Key: "_id", Value: tid}, {Key: "projectId", Value: pid}}
		if m.ExpectedVersion > 0 {
			filter = append(filter, bson.E{Key: "version", Value: m.ExpectedVersion})
		}
		if m.RequireUnassigned {
			filter = append(filter, bson.E{Key: "assigneeId", Value: nil})
		}
		if m.RequireAssignee != "" {
			filter = append(filter, bson.E{Key: "assigneeId", Value: mustOID(m.RequireAssignee)})
		}
		if g := m.RequireAutomationGen; g != nil {
			filter = append(filter, automationGenFilter(*g))
		}
		if len(m.RequireCategories) > 0 {
			cats := bson.A{}
			for _, c := range m.RequireCategories {
				cats = append(cats, string(c))
			}
			filter = append(filter, bson.E{Key: "statusCategory", Value: bson.D{{Key: "$in", Value: cats}}})
		}
		messageID := bson.NewObjectID()
		update := changeUpdate(m.Changes, messageID)
		err := r.store.col(colTickets).FindOneAndUpdate(ctx, filter, update, options.FindOneAndUpdate().SetReturnDocument(options.After)).Decode(&doc)
		if errors.Is(err, mongo.ErrNoDocuments) {
			return tickets.ErrPrecondition
		}
		if err != nil {
			return err
		}
		now := m.Changes.UpdatedAt
		if err := r.applyHold(ctx, doc, m.Changes, now); err != nil {
			return err
		}
		if m.SLA != nil {
			if err := r.applySLA(ctx, &doc, m.SLA, now); errors.Is(err, errSLAConflict) {
				return tickets.ErrPrecondition
			} else if err != nil {
				return err
			}
		}
		var msgRef *bson.ObjectID
		if m.Message != nil {
			md := messageDoc{Id: messageID, WorkspaceId: doc.WorkspaceId, ProjectId: pid, TicketId: tid, Visibility: m.Message.Visibility,
				Author: authorDoc(m.Message.Author), Body: m.Message.Body, AttachmentCount: int32(len(m.AttachmentIds)), Seq: doc.Version, CreatedAt: m.Message.CreatedAt,
				ScheduledId: optOID(m.ScheduledId)}
			if _, err := r.store.col(colMessages).InsertOne(ctx, md); err != nil {
				if isDuplicate(err, "scheduledUnique") {
					return tickets.ErrScheduledSent
				}
				return err
			}
			if err := r.attach(ctx, pid, tid, messageID, md.Visibility, m.AttachmentIds, m.Uploader, now, m.ScheduledId); err != nil {
				return err
			}
			if m.ClearDraftOf != "" {
				if _, err := r.store.col(colDrafts).DeleteOne(ctx, bson.D{{Key: "ticketId", Value: tid},
					{Key: "authorId", Value: mustOID(m.ClearDraftOf)}, {Key: "visibility", Value: md.Visibility}}); err != nil {
					return err
				}
			}
			msg, msgRef = &md, &messageID
			if ref := m.Idempotency; ref != nil {
				if _, err := r.store.col(colIdempotency).UpdateOne(ctx, bson.D{{Key: "scope", Value: ref.Scope}, {Key: "key", Value: ref.Key}},
					bson.D{{Key: "$set", Value: bson.D{{Key: "resource", Value: "message/" + messageID.Hex()}}}}); err != nil {
					return err
				}
			}
		}
		events := make([]audit.Event, len(m.Audit))
		for i, ev := range m.Audit {
			if ev.Action == audit.MessageCreated && msgRef != nil {
				ev.Target = "message/" + messageID.Hex()
			}
			ev.TicketId = tid.Hex()
			events[i] = ev
		}
		if err := r.store.insertAudit(ctx, doc.WorkspaceId, events...); err != nil {
			return err
		}
		return r.insertOutbox(ctx, doc, m.Outbox, msg, now)
	})
	if err != nil {
		return tickets.Ticket{}, tickets.Message{}, err
	}
	r.changed(m.ProjectId)
	out, err := r.hydrate(ctx, []ticketDoc{doc}, m.ViewerId)
	if err != nil {
		return tickets.Ticket{}, tickets.Message{}, err
	}
	var created tickets.Message
	if msg != nil {
		ms, err := r.hydrateMessages(ctx, []messageDoc{*msg})
		if err != nil {
			return tickets.Ticket{}, tickets.Message{}, err
		}
		created = ms[0]
	}
	return out[0], created, nil
}

// ---- Watchers ----

// SetWatch adds or removes the user's watcher row and adjusts the ticket's
// watcher count in one transaction. New watchers are refused on archived
// tickets.
func (r *TicketsRepository) SetWatch(ctx context.Context, t tickets.Ticket, userID string, watch bool, ev audit.Event) (tickets.Ticket, error) {
	ws, pid, tid, uid := mustOID(t.WorkspaceId), mustOID(t.ProjectId), mustOID(t.Id), mustOID(userID)
	err := r.store.transaction(ctx, func(ctx context.Context) error {
		ticketFilter := bson.D{{Key: "_id", Value: tid}, {Key: "projectId", Value: pid}}
		var delta int32
		if watch {
			_, err := r.store.col(colWatchers).InsertOne(ctx, watcherDoc{Id: bson.NewObjectID(), WorkspaceId: ws, ProjectId: pid,
				TicketId: tid, UserId: uid, CreatedAt: ev.CreatedAt})
			if mongo.IsDuplicateKeyError(err) {
				// A write error aborts the MongoDB transaction: returning nil
				// here would make the commit fail with a transient error and
				// the driver re-run this callback until its 120 s limit. End
				// the transaction with an error and treat it as success below.
				return tickets.ErrAlreadyWatching
			}
			if err != nil {
				return err
			}
			ticketFilter = append(ticketFilter, bson.E{Key: "statusCategory", Value: bson.D{{Key: "$ne", Value: string(statuses.CategoryArchived)}}})
			delta = 1
		} else {
			res, err := r.store.col(colWatchers).DeleteOne(ctx, bson.D{{Key: "ticketId", Value: tid}, {Key: "userId", Value: uid}})
			if err != nil {
				return err
			}
			if res.DeletedCount == 0 {
				return nil
			}
			delta = -1
		}
		res, err := r.store.col(colTickets).UpdateOne(ctx, ticketFilter, bson.D{{Key: "$inc", Value: bson.D{{Key: "watcherCount", Value: delta}}}})
		if err != nil {
			return err
		}
		if res.MatchedCount == 0 {
			if watch {
				return tickets.ErrTicketArchived
			}
			return tickets.ErrNotFound
		}
		return r.store.insertAudit(ctx, ws, ev)
	})
	if err != nil && !errors.Is(err, tickets.ErrAlreadyWatching) {
		return tickets.Ticket{}, err
	}
	return r.Get(ctx, t.ProjectId, t.Id, userID)
}

// TicketWatchers returns the users watching a ticket, oldest watch first
// (at most limit), with display name and username. Access is not checked
// here; the service filters by each watcher's current access.
func (r *TicketsRepository) TicketWatchers(ctx context.Context, ticketID string, limit int) ([]tickets.Watcher, error) {
	ws, err := findAll[watcherDoc](ctx, r.store.col(colWatchers), bson.D{{Key: "ticketId", Value: mustOID(ticketID)}},
		options.Find().SetSort(bson.D{{Key: "createdAt", Value: 1}, {Key: "_id", Value: 1}}).SetLimit(int64(limit)))
	if err != nil || len(ws) == 0 {
		return []tickets.Watcher{}, err
	}
	ids := make(bson.A, 0, len(ws))
	for _, w := range ws {
		ids = append(ids, w.UserId)
	}
	us, err := findAll[userDoc](ctx, r.store.col(colUsers), bson.D{{Key: "_id", Value: bson.D{{Key: "$in", Value: ids}}}},
		options.Find().SetProjection(bson.D{{Key: "displayName", Value: 1}, {Key: "username", Value: 1}, {Key: "email", Value: 1}}))
	if err != nil {
		return nil, err
	}
	byID := make(map[bson.ObjectID]userDoc, len(us))
	for _, u := range us {
		byID[u.Id] = u
	}
	out := make([]tickets.Watcher, 0, len(ws))
	for _, w := range ws {
		u, ok := byID[w.UserId]
		if !ok {
			continue
		}
		name := u.DisplayName
		if name == "" {
			name = u.Email
		}
		out = append(out, tickets.Watcher{Id: u.Id.Hex(), Name: name, Username: u.Username})
	}
	return out, nil
}

// ---- Messages ----

// ListMessages pages through a ticket's messages in the given audiences,
// oldest first.
func (r *TicketsRepository) ListMessages(ctx context.Context, ticketID string, visibilities []string, after *pagination.Cursor, limit int) ([]tickets.Message, error) {
	vis := make(bson.A, 0, len(visibilities))
	for _, v := range visibilities {
		vis = append(vis, v)
	}
	f := bson.D{{Key: "ticketId", Value: mustOID(ticketID)}, {Key: "visibility", Value: bson.D{{Key: "$in", Value: vis}}}}
	if after != nil {
		// The cursor names the last message of the previous page; its
		// position is read here, restricted to the audiences the reader may
		// see, so a cursor never reveals or probes a hidden message's
		// sequence number.
		id, err := oid(after.Id)
		if err != nil {
			return nil, errInvalidCursor
		}
		var base messageDoc
		err = r.store.col(colMessages).FindOne(ctx, bson.D{{Key: "_id", Value: id}, {Key: "ticketId", Value: mustOID(ticketID)},
			{Key: "visibility", Value: bson.D{{Key: "$in", Value: vis}}}}, options.FindOne().SetProjection(bson.D{{Key: "seq", Value: 1}})).Decode(&base)
		if errors.Is(err, mongo.ErrNoDocuments) {
			return nil, errInvalidCursor
		}
		if err != nil {
			return nil, err
		}
		f = append(f, bson.E{Key: "seq", Value: bson.D{{Key: "$gt", Value: base.Seq}}})
	}
	docs, err := findAll[messageDoc](ctx, r.store.col(colMessages), f,
		options.Find().SetSort(bson.D{{Key: "seq", Value: 1}}).SetLimit(int64(limit)))
	if err != nil {
		return nil, err
	}
	return r.hydrateMessages(ctx, docs)
}

func (r *TicketsRepository) hydrateMessages(ctx context.Context, docs []messageDoc) ([]tickets.Message, error) {
	out := make([]tickets.Message, 0, len(docs))
	if len(docs) == 0 {
		return out, nil
	}
	ids := map[string]map[bson.ObjectID]bool{colUsers: {}, colContacts: {}, colServicePrincipals: {}}
	withFiles := bson.A{}
	for _, d := range docs {
		switch d.Author.Kind {
		case tickets.AuthorContact:
			ids[colContacts][d.Author.Id] = true
		case tickets.AuthorIntegration:
			ids[colServicePrincipals][d.Author.Id] = true
		default:
			ids[colUsers][d.Author.Id] = true
		}
		if d.AttachmentCount > 0 {
			withFiles = append(withFiles, d.Id)
		}
	}
	for k, v := range ids {
		if len(v) == 0 {
			delete(ids, k)
		}
	}
	names, err := r.names(ctx, ids)
	if err != nil {
		return nil, err
	}
	files := map[bson.ObjectID][]tickets.Attachment{}
	if len(withFiles) > 0 {
		// Removed attachments are listed as placeholders (ID only).
		atts, err := findAll[attachmentDoc](ctx, r.store.col(colAttachments),
			bson.D{{Key: "messageId", Value: bson.D{{Key: "$in", Value: withFiles}}},
				{Key: "state", Value: bson.D{{Key: "$in", Value: bson.A{tickets.AttachmentAttached, tickets.AttachmentRemoved}}}}},
			options.Find().SetSort(bson.D{{Key: "_id", Value: 1}}))
		if err != nil {
			return nil, err
		}
		for _, a := range atts {
			files[*a.MessageId] = append(files[*a.MessageId], a.toDomain())
		}
	}
	for _, d := range docs {
		m := tickets.Message{Id: d.Id.Hex(), TicketId: d.TicketId.Hex(), ProjectId: d.ProjectId.Hex(), Visibility: d.Visibility,
			Author: tickets.Author{Kind: d.Author.Kind, Id: d.Author.Id.Hex(), Name: names[d.Author.Id].name, ViaId: hexOrEmpty(d.Author.ViaId)},
			Body:   d.Body, Attachments: files[d.Id], Seq: d.Seq, CreatedAt: d.CreatedAt}
		if r := d.Redacted; r != nil {
			m.Redacted = &tickets.Redaction{At: r.At, ActorType: r.By.Type, ActorId: r.By.Id, Reason: r.Reason}
		}
		if m.Attachments == nil {
			m.Attachments = []tickets.Attachment{}
		}
		out = append(out, m)
	}
	return out, nil
}

// GetMessage returns one message of a ticket.
func (r *TicketsRepository) GetMessage(ctx context.Context, ticketID, id string) (tickets.Message, error) {
	var d messageDoc
	err := r.store.col(colMessages).FindOne(ctx, bson.D{{Key: "_id", Value: mustOID(id)}, {Key: "ticketId", Value: mustOID(ticketID)}}).Decode(&d)
	if errors.Is(err, mongo.ErrNoDocuments) {
		return tickets.Message{}, tickets.ErrNotFound
	}
	if err != nil {
		return tickets.Message{}, err
	}
	out, err := r.hydrateMessages(ctx, []messageDoc{d})
	if err != nil {
		return tickets.Message{}, err
	}
	return out[0], nil
}

// IdempotentResource returns the resource recorded on a reservation by a
// committed command, or "".
func (r *TicketsRepository) IdempotentResource(ctx context.Context, ref idempotency.Ref) (string, error) {
	var rec struct {
		Resource string `bson:"resource"`
	}
	err := r.store.col(colIdempotency).FindOne(ctx, bson.D{{Key: "scope", Value: ref.Scope}, {Key: "key", Value: ref.Key}}).Decode(&rec)
	if errors.Is(err, mongo.ErrNoDocuments) {
		return "", nil
	}
	return rec.Resource, err
}

// ---- Drafts ----

func (d draftDoc) toDomain() tickets.Draft {
	return tickets.Draft{TicketId: d.TicketId.Hex(), AuthorId: d.AuthorId.Hex(), Visibility: d.Visibility, Body: d.Body,
		BaseLastPublicId: hexOrEmpty(d.BaseLastPublicId), UpdatedAt: d.UpdatedAt, Version: d.Version}
}

func draftKey(ticketID, authorID, visibility string) bson.D {
	return bson.D{{Key: "ticketId", Value: mustOID(ticketID)}, {Key: "authorId", Value: mustOID(authorID)}, {Key: "visibility", Value: visibility}}
}

// GetDraft returns the author's draft or tickets.ErrNotFound.
func (r *TicketsRepository) GetDraft(ctx context.Context, ticketID, authorID, visibility string) (tickets.Draft, error) {
	var d draftDoc
	err := r.store.col(colDrafts).FindOne(ctx, draftKey(ticketID, authorID, visibility)).Decode(&d)
	if errors.Is(err, mongo.ErrNoDocuments) {
		return tickets.Draft{}, tickets.ErrNotFound
	}
	if err != nil {
		return tickets.Draft{}, err
	}
	return d.toDomain(), nil
}

// PutDraft upserts a draft; with expectedVersion > 0 it only updates that
// version.
func (r *TicketsRepository) PutDraft(ctx context.Context, d tickets.Draft, expectedVersion int64) (tickets.Draft, error) {
	key := draftKey(d.TicketId, d.AuthorId, d.Visibility)
	set := bson.D{{Key: "body", Value: d.Body}, {Key: "baseLastPublicId", Value: optOID(d.BaseLastPublicId)}, {Key: "updatedAt", Value: d.UpdatedAt}}
	update := bson.D{{Key: "$set", Value: set}, {Key: "$inc", Value: bson.D{{Key: "version", Value: int64(1)}}}}
	col := r.store.col(colDrafts)
	if expectedVersion > 0 {
		filter := append(key, bson.E{Key: "version", Value: expectedVersion})
		res, err := col.UpdateOne(ctx, filter, update)
		if err != nil {
			return tickets.Draft{}, err
		}
		if res.MatchedCount == 0 {
			return tickets.Draft{}, tickets.ErrDraftConflict
		}
	} else {
		update = append(update, bson.E{Key: "$setOnInsert", Value: bson.D{{Key: "_id", Value: bson.NewObjectID()}, {Key: "createdAt", Value: d.UpdatedAt}}})
		_, err := col.UpdateOne(ctx, key, update, options.UpdateOne().SetUpsert(true))
		if mongo.IsDuplicateKeyError(err) {
			// A concurrent first save inserted the draft; apply on top.
			_, err = col.UpdateOne(ctx, key, update[:2])
		}
		if err != nil {
			return tickets.Draft{}, err
		}
	}
	return r.GetDraft(ctx, d.TicketId, d.AuthorId, d.Visibility)
}

// DeleteDraft removes a draft (idempotent).
func (r *TicketsRepository) DeleteDraft(ctx context.Context, ticketID, authorID, visibility string) error {
	_, err := r.store.col(colDrafts).DeleteOne(ctx, draftKey(ticketID, authorID, visibility))
	return err
}

// StagedAttachments returns attachment records with the given IDs in the
// project, whatever their state (the service checks state and uploader).
func (r *TicketsRepository) StagedAttachments(ctx context.Context, projectID string, ids []string) ([]tickets.Attachment, error) {
	docs, err := findAll[attachmentDoc](ctx, r.store.col(colAttachments),
		bson.D{{Key: "_id", Value: bson.D{{Key: "$in", Value: oidList(ids)}}}, {Key: "projectId", Value: mustOID(projectID)}}, options.Find())
	if err != nil {
		return nil, err
	}
	out := make([]tickets.Attachment, 0, len(docs))
	for _, d := range docs {
		out = append(out, d.toDomain())
	}
	return out, nil
}

// holdJobPrefix and holdJobKey name a ticket's hold-expiry job. The key
// carries the expiry time, so a later hold never collides with an earlier one.
func holdJobPrefix(ticketID string) string { return "hold:" + ticketID + ":" }

func holdJobKey(ticketID string, until time.Time) string {
	return holdJobPrefix(ticketID) + strconv.FormatInt(until.UnixMilli(), 10)
}

// applyHold runs inside a ticket mutation. Putting a ticket on hold proves
// the hold reason still exists (the write conflicts with its deletion) and
// schedules the job that returns the ticket to Open; ending a hold cancels
// the queued job.
func (r *TicketsRepository) applyHold(ctx context.Context, doc ticketDoc, ch tickets.Changes, now time.Time) error {
	if h := ch.Hold; h != nil {
		res, err := r.store.col(colHoldReasons).UpdateOne(ctx, bson.D{{Key: "_id", Value: mustOID(h.ReasonId)}, {Key: "projectId", Value: doc.ProjectId}},
			bson.D{{Key: "$set", Value: bson.D{{Key: "lastHoldAt", Value: now}}}})
		if err != nil {
			return err
		}
		if res.MatchedCount == 0 {
			return tickets.ErrHoldReasonGone
		}
		tid := doc.Id.Hex()
		return enqueueJob(ctx, r.store, jobs.NewJob{WorkspaceId: doc.WorkspaceId.Hex(), ProjectId: doc.ProjectId.Hex(), Kind: tickets.KindHoldExpire,
			Key: holdJobKey(tid, h.Until), Payload: map[string]string{"ticketId": tid, "until": strconv.FormatInt(h.Until.UnixMilli(), 10)},
			RunAt: h.Until, MaxAttempts: 3}, now)
	}
	if ch.ClearHold {
		_, err := r.store.col(colJobs).DeleteMany(ctx, bson.D{{Key: "kind", Value: tickets.KindHoldExpire},
			{Key: "key", Value: bson.D{{Key: "$regex", Value: "^" + holdJobPrefix(doc.Id.Hex())}}}, {Key: "state", Value: string(jobs.StateQueued)}})
		return err
	}
	return nil
}

// ExpiredHolds lists tickets still On hold past their holdUntil (the sweep
// that backs up the expiry job).
func (r *TicketsRepository) ExpiredHolds(ctx context.Context, before time.Time, limit int) ([]tickets.Ticket, error) {
	docs, err := findAll[ticketDoc](ctx, r.store.col(colTickets), bson.D{{Key: "statusCategory", Value: string(statuses.CategoryOnHold)},
		{Key: "holdUntil", Value: bson.D{{Key: "$lt", Value: before}}}}, options.Find().SetSort(bson.D{{Key: "holdUntil", Value: 1}}).SetLimit(int64(limit)))
	if err != nil || len(docs) == 0 {
		return nil, err
	}
	return r.hydrate(ctx, docs, "")
}

// HoldReason returns one hold reason of the project (tickets.Repository).
func (r *TicketsRepository) HoldReason(ctx context.Context, projectID, id string) (holdreasons.Reason, error) {
	var d holdReasonDoc
	err := r.store.col(colHoldReasons).FindOne(ctx, bson.D{{Key: "_id", Value: mustOID(id)}, {Key: "projectId", Value: mustOID(projectID)}}).Decode(&d)
	if errors.Is(err, mongo.ErrNoDocuments) {
		return holdreasons.Reason{}, holdreasons.ErrNotFound
	}
	if err != nil {
		return holdreasons.Reason{}, err
	}
	return d.toDomain(), nil
}
