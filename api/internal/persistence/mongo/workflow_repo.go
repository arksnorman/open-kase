package mongo

import (
	"context"
	"errors"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"

	"github.com/arksnorman/open-kase/api/internal/audit"
	"github.com/arksnorman/open-kase/api/internal/notifications"
	"github.com/arksnorman/open-kase/api/internal/outbox"
	"github.com/arksnorman/open-kase/api/internal/statuses"
	"github.com/arksnorman/open-kase/api/internal/tickets"
	"github.com/arksnorman/open-kase/api/package/pagination"
)

// SetChangeHook registers a callback invoked with the project ID after a
// ticket change was written (a latency hint for live update streams; the
// streams also poll the outbox, so a missed call only delays an update).
func (r *TicketsRepository) SetChangeHook(fn func(projectID string)) { r.onChange = fn }

func (r *TicketsRepository) changed(projectID string) {
	if r.onChange != nil && projectID != "" {
		r.onChange(projectID)
	}
}

// automationGenFilter matches the ticket's automation generation; tickets
// that never had automation state have generation 0.
func automationGenFilter(gen int64) bson.E {
	if gen == 0 {
		return bson.E{Key: "automation.gen", Value: bson.D{{Key: "$in", Value: bson.A{int64(0), nil}}}}
	}
	return bson.E{Key: "automation.gen", Value: gen}
}

// ---- Links ----

type linkDoc struct {
	Id          bson.ObjectID `bson:"_id"`
	WorkspaceId bson.ObjectID `bson:"workspaceId"`
	ProjectId   bson.ObjectID `bson:"projectId"`
	SourceId    bson.ObjectID `bson:"sourceTicketId"`
	TargetId    bson.ObjectID `bson:"targetTicketId"`
	Type        string        `bson:"type"`
	CreatedBy   actorDoc      `bson:"createdBy"`
	CreatedAt   time.Time     `bson:"createdAt"`
}

func (d linkDoc) toDomain() tickets.Link {
	return tickets.Link{Id: d.Id.Hex(), ProjectId: d.ProjectId.Hex(), SourceId: d.SourceId.Hex(), TargetId: d.TargetId.Hex(),
		Type: d.Type, CreatedBy: audit.Actor{Type: d.CreatedBy.Type, Id: d.CreatedBy.Id, Label: d.CreatedBy.Label}, CreatedAt: d.CreatedAt}
}

// ticketEvents writes outbox events for tickets that did not change
// themselves (link changes), each with its ticket's current snapshot.
func (r *TicketsRepository) ticketEvents(ctx context.Context, events []outbox.Event, now time.Time) error {
	for _, e := range events {
		var doc ticketDoc
		if err := r.store.col(colTickets).FindOne(ctx, bson.D{{Key: "_id", Value: mustOID(e.TicketId)}}).Decode(&doc); err != nil {
			return err
		}
		if err := r.insertOutbox(ctx, doc, []outbox.Event{e}, nil, now); err != nil {
			return err
		}
	}
	return nil
}

func (r *TicketsRepository) countLinks(ctx context.Context, ticketID bson.ObjectID) (int64, error) {
	return r.store.col(colTicketLinks).CountDocuments(ctx, bson.D{{Key: "$or", Value: bson.A{
		bson.D{{Key: "sourceTicketId", Value: ticketID}}, bson.D{{Key: "targetTicketId", Value: ticketID}}}}})
}

// insertLinkTx inserts a link with its audit and outbox records inside the
// caller's transaction.
func (r *TicketsRepository) insertLinkTx(ctx context.Context, ws bson.ObjectID, l tickets.Link, events []audit.Event, ob []outbox.Event) error {
	src, dst := mustOID(l.SourceId), mustOID(l.TargetId)
	for _, id := range []bson.ObjectID{src, dst} {
		n, err := r.countLinks(ctx, id)
		if err != nil {
			return err
		}
		if n >= tickets.MaxLinksPerTicket {
			return tickets.ErrTooManyLinks
		}
	}
	doc := linkDoc{Id: mustOID(l.Id), WorkspaceId: ws, ProjectId: mustOID(l.ProjectId), SourceId: src, TargetId: dst, Type: l.Type,
		CreatedBy: toActorDoc(l.CreatedBy), CreatedAt: l.CreatedAt}
	if doc.Id.IsZero() {
		doc.Id = bson.NewObjectID()
	}
	if _, err := r.store.col(colTicketLinks).InsertOne(ctx, doc); err != nil {
		if mongo.IsDuplicateKeyError(err) {
			return tickets.ErrLinkExists
		}
		return err
	}
	for i := range events {
		events[i].Target = "ticketLink/" + doc.Id.Hex()
	}
	if err := r.store.insertAudit(ctx, ws, events...); err != nil {
		return err
	}
	return r.ticketEvents(ctx, ob, l.CreatedAt)
}

// InsertLink stores a link between two tickets of one project.
func (r *TicketsRepository) InsertLink(ctx context.Context, l tickets.Link, events []audit.Event, ob []outbox.Event) (tickets.Link, error) {
	l.Id = bson.NewObjectID().Hex()
	err := r.store.transaction(ctx, func(ctx context.Context) error {
		var proj struct {
			WorkspaceId bson.ObjectID `bson:"workspaceId"`
		}
		if err := r.store.col(colTickets).FindOne(ctx, bson.D{{Key: "_id", Value: mustOID(l.SourceId)}, {Key: "projectId", Value: mustOID(l.ProjectId)}},
			options.FindOne().SetProjection(bson.D{{Key: "workspaceId", Value: 1}})).Decode(&proj); err != nil {
			if errors.Is(err, mongo.ErrNoDocuments) {
				return tickets.ErrNotFound
			}
			return err
		}
		return r.insertLinkTx(ctx, proj.WorkspaceId, l, events, ob)
	})
	if err != nil {
		return tickets.Link{}, err
	}
	r.changed(l.ProjectId)
	return l, nil
}

// DeleteLink removes a link with its audit and outbox records.
func (r *TicketsRepository) DeleteLink(ctx context.Context, l tickets.Link, events []audit.Event, ob []outbox.Event) error {
	err := r.store.transaction(ctx, func(ctx context.Context) error {
		var d linkDoc
		err := r.store.col(colTicketLinks).FindOneAndDelete(ctx, bson.D{{Key: "_id", Value: mustOID(l.Id)}, {Key: "projectId", Value: mustOID(l.ProjectId)}}).Decode(&d)
		if errors.Is(err, mongo.ErrNoDocuments) {
			return tickets.ErrNotFound
		}
		if err != nil {
			return err
		}
		for i := range events {
			events[i].Target = "ticketLink/" + d.Id.Hex()
		}
		if err := r.store.insertAudit(ctx, d.WorkspaceId, events...); err != nil {
			return err
		}
		now := time.Now().UTC()
		if len(ob) > 0 {
			now = ob[0].OccurredAt
		}
		return r.ticketEvents(ctx, ob, now)
	})
	if err == nil {
		r.changed(l.ProjectId)
	}
	return err
}

// GetLink returns one link of the project.
func (r *TicketsRepository) GetLink(ctx context.Context, projectID, id string) (tickets.Link, error) {
	var d linkDoc
	err := r.store.col(colTicketLinks).FindOne(ctx, bson.D{{Key: "_id", Value: mustOID(id)}, {Key: "projectId", Value: mustOID(projectID)}}).Decode(&d)
	if errors.Is(err, mongo.ErrNoDocuments) {
		return tickets.Link{}, tickets.ErrNotFound
	}
	if err != nil {
		return tickets.Link{}, err
	}
	return d.toDomain(), nil
}

// TicketLinks returns the links on either side of a ticket, oldest first.
func (r *TicketsRepository) TicketLinks(ctx context.Context, ticketID string, limit int) ([]tickets.Link, error) {
	tid := mustOID(ticketID)
	docs, err := findAll[linkDoc](ctx, r.store.col(colTicketLinks), bson.D{{Key: "$or", Value: bson.A{
		bson.D{{Key: "sourceTicketId", Value: tid}}, bson.D{{Key: "targetTicketId", Value: tid}}}}},
		options.Find().SetSort(bson.D{{Key: "_id", Value: 1}}).SetLimit(int64(limit)))
	if err != nil {
		return nil, err
	}
	out := make([]tickets.Link, 0, len(docs))
	for _, d := range docs {
		out = append(out, d.toDomain())
	}
	return out, nil
}

// ---- Activity ----

// TicketActivity pages through the ticket's audit events in the timeline
// (ticket.* and message.* actions), oldest first, with actor names.
func (r *TicketsRepository) TicketActivity(ctx context.Context, projectID, ticketID string, flt tickets.ActivityFilter, after *pagination.Cursor, limit int) ([]tickets.ActivityEvent, error) {
	f := bson.D{{Key: "projectId", Value: mustOID(projectID)}, {Key: "ticketId", Value: mustOID(ticketID)},
		{Key: "action", Value: bson.Regex{Pattern: `^(ticket|message)\.`}}}
	if flt.Action != "" {
		f[2].Value = flt.Action
	}
	if after != nil {
		at, err := time.Parse(time.RFC3339Nano, after.Sort)
		id, ierr := oid(after.Id)
		if err != nil || ierr != nil {
			return nil, errInvalidCursor
		}
		cmp := "$gt"
		if flt.NewestFirst {
			cmp = "$lt"
		}
		f = append(f, bson.E{Key: "$or", Value: bson.A{
			bson.D{{Key: "createdAt", Value: bson.D{{Key: cmp, Value: at}}}},
			bson.D{{Key: "createdAt", Value: at}, {Key: "_id", Value: bson.D{{Key: cmp, Value: id}}}},
		}})
	}
	dir := 1
	if flt.NewestFirst {
		dir = -1
	}
	docs, err := findAll[auditDoc](ctx, r.store.col(colAuditEvents), f,
		options.Find().SetSort(bson.D{{Key: "createdAt", Value: dir}, {Key: "_id", Value: dir}}).SetLimit(int64(limit)))
	if err != nil {
		return nil, err
	}
	ids := map[string]map[bson.ObjectID]bool{}
	for _, d := range docs {
		col := ""
		switch d.Actor.Type {
		case audit.ActorUser:
			col = colUsers
		case audit.ActorService:
			col = colServicePrincipals
		}
		if id, err := oid(d.Actor.Id); err == nil && col != "" {
			if ids[col] == nil {
				ids[col] = map[bson.ObjectID]bool{}
			}
			ids[col][id] = true
		}
	}
	names, err := r.names(ctx, ids)
	if err != nil {
		return nil, err
	}
	out := make([]tickets.ActivityEvent, 0, len(docs))
	for _, d := range docs {
		name := d.Actor.Label
		if d.Actor.Type == audit.ActorSystem || d.Actor.Type == audit.ActorOperator {
			name = audit.SystemActorName(d.Actor.Id) // the standard actor catalog name
		} else if id, err := oid(d.Actor.Id); err == nil {
			if n, ok := names[id]; ok && n.name != "" {
				name = n.name
			}
		}
		changes := d.Changes
		if changes == nil {
			changes = []string{}
		}
		out = append(out, tickets.ActivityEvent{Id: d.Id.Hex(), Action: d.Action,
			Actor:  tickets.ActivityActor{Type: d.Actor.Type, Id: d.Actor.Id, Name: name, Scopes: d.Actor.Scopes},
			Target: d.Target, Changes: changes, Reason: d.Reason, Outcome: d.Outcome, CreatedAt: d.CreatedAt})
	}
	return out, nil
}

// ---- Redaction ----

// RedactMessage replaces the body (unless cmd.KeepBody), removes the listed
// attachments, moves the ticket version and scrubs derived copies in one
// transaction (see tickets.Service.RedactMessage).
func (r *TicketsRepository) RedactMessage(ctx context.Context, cmd tickets.RedactCommand) (tickets.Ticket, tickets.Message, error) {
	pid, tid, mid := mustOID(cmd.ProjectId), mustOID(cmd.TicketId), mustOID(cmd.MessageId)
	at := cmd.Redaction.At
	by := actorDoc{Type: cmd.Redaction.ActorType, Id: cmd.Redaction.ActorId}
	var doc ticketDoc
	var msg messageDoc
	err := r.store.transaction(ctx, func(ctx context.Context) error {
		var err error
		if cmd.KeepBody {
			// Attachments only: the body and its redaction record stay.
			err = r.store.col(colMessages).FindOne(ctx, bson.D{{Key: "_id", Value: mid}, {Key: "ticketId", Value: tid},
				{Key: "projectId", Value: pid}, {Key: "redacted", Value: bson.D{{Key: "$ne", Value: nil}}}}).Decode(&msg)
		} else {
			err = r.store.col(colMessages).FindOneAndUpdate(ctx,
				bson.D{{Key: "_id", Value: mid}, {Key: "ticketId", Value: tid}, {Key: "projectId", Value: pid}, {Key: "redacted", Value: nil}},
				bson.D{{Key: "$set", Value: bson.D{{Key: "body", Value: tickets.RedactedBody}, {Key: "redacted", Value: redactionDoc{At: at,
					By: by, Reason: cmd.Redaction.Reason}}}}},
				options.FindOneAndUpdate().SetReturnDocument(options.After)).Decode(&msg)
		}
		if errors.Is(err, mongo.ErrNoDocuments) {
			n, cerr := r.store.col(colMessages).CountDocuments(ctx, bson.D{{Key: "_id", Value: mid}, {Key: "ticketId", Value: tid}})
			if cerr != nil {
				return cerr
			}
			if n > 0 {
				return tickets.ErrAlreadyRedacted
			}
			return tickets.ErrNotFound
		}
		if err != nil {
			return err
		}
		// Removed attachments keep their record as a placeholder: the name,
		// type, size and hash are dropped (the name and the hash of a short
		// secret can be as sensitive as the file), who, when and why are
		// recorded. Every listed file must still be attached; a concurrent
		// redaction that removed one first makes this one a conflict.
		if len(cmd.RemoveAttachments) > 0 {
			res, err := r.store.col(colAttachments).UpdateMany(ctx, bson.D{{Key: "_id", Value: bson.D{{Key: "$in", Value: oidList(cmd.RemoveAttachments)}}},
				{Key: "messageId", Value: mid}, {Key: "projectId", Value: pid}, {Key: "state", Value: tickets.AttachmentAttached}},
				bson.D{{Key: "$set", Value: bson.D{{Key: "state", Value: tickets.AttachmentRemoved},
					{Key: "removed", Value: attachmentRemovalDoc{At: at, By: by, Reason: cmd.Redaction.Reason}},
					{Key: "filename", Value: ""}, {Key: "contentType", Value: ""}, {Key: "size", Value: int64(0)}, {Key: "sha256", Value: ""}}}})
			if err != nil {
				return err
			}
			if res.ModifiedCount != int64(len(cmd.RemoveAttachments)) {
				return tickets.ErrAlreadyRedacted
			}
			// The bytes go after the commit (never inside a transaction),
			// through a job enqueued here so a crash cannot lose them.
			if cmd.FilesJob != nil {
				if _, err := r.store.col(colJobs).UpdateOne(ctx, bson.D{{Key: "key", Value: cmd.FilesJob.Key}},
					bson.D{{Key: "$setOnInsert", Value: newJobDoc(*cmd.FilesJob, at)}}, options.UpdateOne().SetUpsert(true)); err != nil {
					return err
				}
			}
		}
		set := bson.D{{Key: "updatedAt", Value: at}}
		if msg.Visibility == tickets.VisibilityPublic {
			set = append(set, bson.E{Key: "publicUpdatedAt", Value: at})
		}
		err = r.store.col(colTickets).FindOneAndUpdate(ctx, bson.D{{Key: "_id", Value: tid}, {Key: "projectId", Value: pid}},
			bson.D{{Key: "$set", Value: set}, {Key: "$inc", Value: bson.D{{Key: "version", Value: int64(1)}}}},
			options.FindOneAndUpdate().SetReturnDocument(options.After)).Decode(&doc)
		if errors.Is(err, mongo.ErrNoDocuments) {
			return tickets.ErrNotFound
		}
		if err != nil {
			return err
		}
		// The scheduled message that produced this message keeps a copy of
		// the text, which its author and administrators can list.
		if msg.ScheduledId != nil {
			if _, err := r.store.col(colScheduledMessages).UpdateOne(ctx, bson.D{{Key: "_id", Value: *msg.ScheduledId}, {Key: "ticketId", Value: tid}},
				bson.D{{Key: "$set", Value: bson.D{{Key: "body", Value: tickets.RedactedBody}}}}); err != nil {
				return err
			}
		}
		// The stored response of the request that posted the message
		// (idempotent replay, REST and MCP) contains the text as well. The
		// record stays, so a late retry still cannot post a duplicate; it
		// answers idempotencyKeyUsed instead of replaying the old body.
		if _, err := r.store.col(colIdempotency).UpdateMany(ctx, bson.D{{Key: "resource", Value: "message/" + mid.Hex()}},
			bson.D{{Key: "$unset", Value: bson.D{{Key: "body", Value: ""}}}}); err != nil {
			return err
		}
		// So is the stored response of an earlier redaction request (it may
		// list attachments kept then): this request's own record is linked
		// the same way, for a later redaction to drop.
		if ref := cmd.Idempotency; ref != nil {
			if _, err := r.store.col(colIdempotency).UpdateOne(ctx, bson.D{{Key: "scope", Value: ref.Scope}, {Key: "key", Value: ref.Key}},
				bson.D{{Key: "$set", Value: bson.D{{Key: "resource", Value: "message/" + mid.Hex()}}}}); err != nil {
				return err
			}
		}
		// Stored webhook payloads of the ticket are dropped whatever their
		// status: a pending or retrying delivery rebuilds its payload from
		// the current state at the next attempt, a dead-lettered one when an
		// administrator retries it, and a sent one no longer needs the copy.
		// (Only unsent copies used to be dropped, so a manual retry of a
		// failed delivery re-sent the redacted text.)
		if _, err := r.store.col(colDeliveries).UpdateMany(ctx, append(byTicketRef(tid),
			bson.E{Key: "body", Value: bson.D{{Key: "$exists", Value: true}}}),
			bson.D{{Key: "$unset", Value: bson.D{{Key: "body", Value: ""}}}}); err != nil {
			return err
		}
		// A webhook payload being built right now (from the message read
		// before this commit) must not be stored or sent after it either
		// (review OC-ASYNC-07): moving the body generation makes the
		// worker discard it and rebuild from the redacted message.
		if err := r.store.invalidateDeliveryBodies(ctx, append(byTicketRef(tid),
			bson.E{Key: "channel", Value: notifications.ChannelWebhook})); err != nil {
			return err
		}
		if err := r.store.insertAudit(ctx, doc.WorkspaceId, cmd.Audit...); err != nil {
			return err
		}
		return r.insertOutbox(ctx, doc, cmd.Outbox, &msg, at)
	})
	if err != nil {
		return tickets.Ticket{}, tickets.Message{}, err
	}
	r.changed(cmd.ProjectId)
	out, err := r.hydrate(ctx, []ticketDoc{doc}, "")
	if err != nil {
		return tickets.Ticket{}, tickets.Message{}, err
	}
	ms, err := r.hydrateMessages(ctx, []messageDoc{msg})
	if err != nil {
		return tickets.Ticket{}, tickets.Message{}, err
	}
	return out[0], ms[0], nil
}

// ---- Automation ----

// RemindRequester counts a follow-up reminder while the ticket is still
// waiting for the requester at the job's generation and reminder count.
func (r *TicketsRepository) RemindRequester(ctx context.Context, cmd tickets.ReminderCommand) (bool, error) {
	applied := false
	err := r.store.transaction(ctx, func(ctx context.Context) error {
		applied = false
		filter := bson.D{{Key: "_id", Value: mustOID(cmd.TicketId)}, {Key: "projectId", Value: mustOID(cmd.ProjectId)},
			{Key: "statusCategory", Value: string(statuses.CategoryWaitingCustomer)},
			automationGenFilter(cmd.Generation)}
		if cmd.Reminders == 0 {
			filter = append(filter, bson.E{Key: "automation.reminders", Value: bson.D{{Key: "$in", Value: bson.A{int32(0), nil}}}})
		} else {
			filter = append(filter, bson.E{Key: "automation.reminders", Value: int32(cmd.Reminders)})
		}
		var doc ticketDoc
		set := bson.D{{Key: "automation.reminders", Value: int32(cmd.Reminders + 1)}, {Key: "automation.lastReminderAt", Value: cmd.At},
			{Key: "automation.gen", Value: cmd.Generation}}
		if cmd.Notice {
			set = append(set, bson.E{Key: "automation.notice", Value: true})
		}
		err := r.store.col(colTickets).FindOneAndUpdate(ctx, filter, bson.D{{Key: "$set", Value: set}},
			options.FindOneAndUpdate().SetReturnDocument(options.After)).Decode(&doc)
		if errors.Is(err, mongo.ErrNoDocuments) {
			return nil
		}
		if err != nil {
			return err
		}
		if err := r.store.insertAudit(ctx, doc.WorkspaceId, cmd.Audit...); err != nil {
			return err
		}
		if err := r.insertOutbox(ctx, doc, cmd.Outbox, nil, cmd.At); err != nil {
			return err
		}
		applied = true
		return nil
	})
	if err == nil && applied {
		r.changed(cmd.ProjectId)
	}
	return applied, err
}
