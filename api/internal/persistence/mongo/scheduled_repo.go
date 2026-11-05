package mongo

import (
	"context"
	"errors"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"

	"github.com/arksnorman/open-kase/api/internal/audit"
	"github.com/arksnorman/open-kase/api/internal/outbox"
	"github.com/arksnorman/open-kase/api/internal/tickets"
)

type scheduledDoc struct {
	Id               bson.ObjectID   `bson:"_id"`
	WorkspaceId      bson.ObjectID   `bson:"workspaceId"`
	ProjectId        bson.ObjectID   `bson:"projectId"`
	TicketId         bson.ObjectID   `bson:"ticketId"`
	AuthorId         bson.ObjectID   `bson:"authorId"`
	Visibility       string          `bson:"visibility"`
	Body             string          `bson:"body"`
	AttachmentIds    []bson.ObjectID `bson:"attachmentIds"`
	ThenStatusId     *bson.ObjectID  `bson:"thenStatusId"`
	NoSignature      bool            `bson:"noSignature"`
	SendAt           time.Time       `bson:"sendAt"`
	State            string          `bson:"state"`
	Reason           string          `bson:"reason"`
	BaseLastPublicId *bson.ObjectID  `bson:"baseLastPublicId"`
	TicketVersion    int64           `bson:"ticketVersion"`
	Revision         int64           `bson:"revision"`
	SentMessageId    *bson.ObjectID  `bson:"sentMessageId"`
	SentAt           *time.Time      `bson:"sentAt"`
	CreatedAt        time.Time       `bson:"createdAt"`
	UpdatedAt        time.Time       `bson:"updatedAt"`
	Version          int64           `bson:"version"`
}

func (d scheduledDoc) toDomain() tickets.ScheduledMessage {
	m := tickets.ScheduledMessage{Id: d.Id.Hex(), WorkspaceId: d.WorkspaceId.Hex(), ProjectId: d.ProjectId.Hex(), TicketId: d.TicketId.Hex(),
		AuthorId: d.AuthorId.Hex(), Visibility: d.Visibility, Body: d.Body, ThenStatusId: hexOrEmpty(d.ThenStatusId), NoSignature: d.NoSignature, SendAt: d.SendAt,
		State: d.State, Reason: d.Reason, BaseLastPublicId: hexOrEmpty(d.BaseLastPublicId), TicketVersion: d.TicketVersion,
		Revision: d.Revision, SentMessageId: hexOrEmpty(d.SentMessageId), SentAt: d.SentAt, CreatedAt: d.CreatedAt,
		UpdatedAt: d.UpdatedAt, Version: d.Version, AttachmentIds: make([]string, 0, len(d.AttachmentIds))}
	for _, id := range d.AttachmentIds {
		m.AttachmentIds = append(m.AttachmentIds, id.Hex())
	}
	return m
}

// Transaction implements tickets.ScheduledRepository.
func (r *TicketsRepository) Transaction(ctx context.Context, fn func(ctx context.Context) error) error {
	return r.store.transaction(ctx, fn)
}

// InsertScheduled implements tickets.ScheduledRepository.
func (r *TicketsRepository) InsertScheduled(ctx context.Context, m tickets.ScheduledMessage) (tickets.ScheduledMessage, error) {
	d := scheduledDoc{Id: bson.NewObjectID(), WorkspaceId: mustOID(m.WorkspaceId), ProjectId: mustOID(m.ProjectId), TicketId: mustOID(m.TicketId),
		AuthorId: mustOID(m.AuthorId), Visibility: m.Visibility, Body: m.Body, AttachmentIds: []bson.ObjectID{},
		ThenStatusId: optOID(m.ThenStatusId), NoSignature: m.NoSignature, SendAt: m.SendAt, State: m.State, BaseLastPublicId: optOID(m.BaseLastPublicId),
		TicketVersion: m.TicketVersion, Revision: m.Revision, CreatedAt: m.CreatedAt, UpdatedAt: m.UpdatedAt, Version: m.Version}
	for _, id := range m.AttachmentIds {
		d.AttachmentIds = append(d.AttachmentIds, mustOID(id))
	}
	err := r.store.transaction(ctx, func(ctx context.Context) error {
		if _, err := r.store.col(colScheduledMessages).InsertOne(ctx, d); err != nil {
			return err
		}
		if len(d.AttachmentIds) > 0 {
			// Reserve: the uploads stop expiring and cannot be attached to
			// any other message.
			ids := make(bson.A, 0, len(d.AttachmentIds))
			for _, id := range d.AttachmentIds {
				ids = append(ids, id)
			}
			res, err := r.store.col(colAttachments).UpdateMany(ctx, bson.D{
				{Key: "_id", Value: bson.D{{Key: "$in", Value: ids}}}, {Key: "projectId", Value: d.ProjectId},
				{Key: "state", Value: tickets.AttachmentStaged}, {Key: "expiresAt", Value: bson.D{{Key: "$gt", Value: m.CreatedAt}}},
				{Key: "scheduledMessageId", Value: bson.D{{Key: "$exists", Value: false}}},
				{Key: "uploader.kind", Value: "user"}, {Key: "uploader.id", Value: d.AuthorId},
			}, bson.D{{Key: "$set", Value: bson.D{{Key: "scheduledMessageId", Value: d.Id}, {Key: "ticketId", Value: d.TicketId},
				{Key: "expiresAt", Value: nil}}}})
			if err != nil {
				return err
			}
			if res.ModifiedCount != int64(len(ids)) {
				return tickets.ErrAttachmentsGone
			}
		}
		// Scheduling consumes the author's draft for the audience.
		_, err := r.store.col(colDrafts).DeleteOne(ctx, bson.D{{Key: "ticketId", Value: d.TicketId},
			{Key: "authorId", Value: d.AuthorId}, {Key: "visibility", Value: d.Visibility}})
		return err
	})
	if err != nil {
		return tickets.ScheduledMessage{}, err
	}
	out, err := r.withAuthors(ctx, []scheduledDoc{d})
	if err != nil {
		return tickets.ScheduledMessage{}, err
	}
	return out[0], nil
}

func (r *TicketsRepository) withAuthors(ctx context.Context, docs []scheduledDoc) ([]tickets.ScheduledMessage, error) {
	ids := map[bson.ObjectID]bool{}
	for _, d := range docs {
		ids[d.AuthorId] = true
	}
	names := map[bson.ObjectID]nameRec{}
	if len(ids) > 0 {
		var err error
		if names, err = r.names(ctx, map[string]map[bson.ObjectID]bool{colUsers: ids}); err != nil {
			return nil, err
		}
	}
	out := make([]tickets.ScheduledMessage, 0, len(docs))
	for _, d := range docs {
		m := d.toDomain()
		m.AuthorName = names[d.AuthorId].name
		out = append(out, m)
	}
	return out, nil
}

// GetScheduled implements tickets.ScheduledRepository.
func (r *TicketsRepository) GetScheduled(ctx context.Context, projectID, id string) (tickets.ScheduledMessage, error) {
	sid, err := oid(id)
	if err != nil {
		return tickets.ScheduledMessage{}, tickets.ErrNotFound
	}
	var d scheduledDoc
	err = r.store.col(colScheduledMessages).FindOne(ctx, bson.D{{Key: "_id", Value: sid}, {Key: "projectId", Value: mustOID(projectID)}}).Decode(&d)
	if errors.Is(err, mongo.ErrNoDocuments) {
		return tickets.ScheduledMessage{}, tickets.ErrNotFound
	}
	if err != nil {
		return tickets.ScheduledMessage{}, err
	}
	out, err := r.withAuthors(ctx, []scheduledDoc{d})
	if err != nil {
		return tickets.ScheduledMessage{}, err
	}
	return out[0], nil
}

// ListScheduled implements tickets.ScheduledRepository.
func (r *TicketsRepository) ListScheduled(ctx context.Context, ticketID, authorID string, states []string, limit int) ([]tickets.ScheduledMessage, error) {
	filter := bson.D{{Key: "ticketId", Value: mustOID(ticketID)}}
	if authorID != "" {
		filter = append(filter, bson.E{Key: "authorId", Value: mustOID(authorID)})
	}
	if len(states) > 0 {
		filter = append(filter, bson.E{Key: "state", Value: bson.D{{Key: "$in", Value: states}}})
	}
	sort := bson.D{{Key: "sendAt", Value: 1}, {Key: "_id", Value: 1}}
	if len(states) == 0 {
		sort = bson.D{{Key: "updatedAt", Value: -1}, {Key: "_id", Value: -1}}
	}
	docs, err := findAll[scheduledDoc](ctx, r.store.col(colScheduledMessages), filter, options.Find().SetSort(sort).SetLimit(int64(limit)))
	if err != nil {
		return nil, err
	}
	return r.withAuthors(ctx, docs)
}

// CountActiveScheduled implements tickets.ScheduledRepository.
func (r *TicketsRepository) CountActiveScheduled(ctx context.Context, ticketID, authorID string) (int64, error) {
	return r.store.col(colScheduledMessages).CountDocuments(ctx, bson.D{{Key: "ticketId", Value: mustOID(ticketID)},
		{Key: "authorId", Value: mustOID(authorID)},
		{Key: "state", Value: bson.D{{Key: "$in", Value: bson.A{tickets.ScheduledPending, tickets.ScheduledNeedsReview}}}}})
}

// UpdateScheduled implements tickets.ScheduledRepository.
func (r *TicketsRepository) UpdateScheduled(ctx context.Context, f tickets.ScheduledFilter, ch tickets.ScheduledChanges) (tickets.ScheduledMessage, error) {
	sid, err := oid(f.Id)
	if err != nil {
		return tickets.ScheduledMessage{}, tickets.ErrNotFound
	}
	filter := bson.D{{Key: "_id", Value: sid}, {Key: "projectId", Value: mustOID(f.ProjectId)}}
	if f.Version > 0 {
		filter = append(filter, bson.E{Key: "version", Value: f.Version})
	}
	if f.Revision > 0 {
		filter = append(filter, bson.E{Key: "revision", Value: f.Revision})
	}
	if len(f.States) > 0 {
		filter = append(filter, bson.E{Key: "state", Value: bson.D{{Key: "$in", Value: f.States}}})
	}
	set := bson.D{{Key: "updatedAt", Value: ch.UpdatedAt}}
	if ch.Body != nil {
		set = append(set, bson.E{Key: "body", Value: *ch.Body})
	}
	if ch.NoSignature != nil {
		set = append(set, bson.E{Key: "noSignature", Value: *ch.NoSignature})
	}
	if ch.ThenStatusId != nil {
		set = append(set, bson.E{Key: "thenStatusId", Value: optOID(*ch.ThenStatusId)})
	}
	if ch.SendAt != nil {
		set = append(set, bson.E{Key: "sendAt", Value: *ch.SendAt})
	}
	if ch.State != nil {
		set = append(set, bson.E{Key: "state", Value: *ch.State})
	}
	if ch.Reason != nil {
		set = append(set, bson.E{Key: "reason", Value: *ch.Reason})
	}
	if ch.BaseLastPublicId != nil {
		set = append(set, bson.E{Key: "baseLastPublicId", Value: optOID(*ch.BaseLastPublicId)})
	}
	if ch.SentMessageId != nil {
		set = append(set, bson.E{Key: "sentMessageId", Value: optOID(*ch.SentMessageId)})
	}
	if ch.SentAt != nil {
		set = append(set, bson.E{Key: "sentAt", Value: *ch.SentAt})
	}
	inc := bson.D{{Key: "version", Value: int64(1)}}
	if ch.BumpRevision {
		inc = append(inc, bson.E{Key: "revision", Value: int64(1)})
	}
	var d scheduledDoc
	err = r.store.transaction(ctx, func(ctx context.Context) error {
		err := r.store.col(colScheduledMessages).FindOneAndUpdate(ctx, filter, bson.D{{Key: "$set", Value: set}, {Key: "$inc", Value: inc}},
			options.FindOneAndUpdate().SetReturnDocument(options.After)).Decode(&d)
		if errors.Is(err, mongo.ErrNoDocuments) {
			return tickets.ErrPrecondition
		}
		if err != nil {
			return err
		}
		if ch.ReleaseUntil != nil && len(d.AttachmentIds) > 0 {
			// Back to the author's staging area; they expire as usual.
			_, err = r.store.col(colAttachments).UpdateMany(ctx, bson.D{{Key: "scheduledMessageId", Value: d.Id},
				{Key: "state", Value: tickets.AttachmentStaged}},
				bson.D{{Key: "$set", Value: bson.D{{Key: "expiresAt", Value: *ch.ReleaseUntil}, {Key: "ticketId", Value: nil}}},
					{Key: "$unset", Value: bson.D{{Key: "scheduledMessageId", Value: ""}}}})
		}
		return err
	})
	if err != nil {
		return tickets.ScheduledMessage{}, err
	}
	out, err := r.withAuthors(ctx, []scheduledDoc{d})
	if err != nil {
		return tickets.ScheduledMessage{}, err
	}
	return out[0], nil
}

// NewerRequesterReply implements tickets.ScheduledRepository: a public
// message by a contact committed after afterMessageID (by sequence).
func (r *TicketsRepository) NewerRequesterReply(ctx context.Context, ticketID, afterMessageID string) (bool, error) {
	tid := mustOID(ticketID)
	var after int64
	if afterMessageID != "" {
		var base messageDoc
		err := r.store.col(colMessages).FindOne(ctx, bson.D{{Key: "_id", Value: mustOID(afterMessageID)}, {Key: "ticketId", Value: tid}},
			options.FindOne().SetProjection(bson.D{{Key: "seq", Value: 1}})).Decode(&base)
		if err != nil && !errors.Is(err, mongo.ErrNoDocuments) {
			return false, err
		}
		after = base.Seq
	}
	n, err := r.store.col(colMessages).CountDocuments(ctx, bson.D{{Key: "ticketId", Value: tid}, {Key: "visibility", Value: "public"},
		{Key: "author.kind", Value: tickets.AuthorContact}, {Key: "seq", Value: bson.D{{Key: "$gt", Value: after}}}},
		options.Count().SetLimit(1))
	return n > 0, err
}

// TicketEvents implements tickets.ScheduledRepository.
func (r *TicketsRepository) TicketEvents(ctx context.Context, projectID, ticketID string, events []audit.Event, ob []outbox.Event) error {
	err := r.store.transaction(ctx, func(ctx context.Context) error {
		var doc ticketDoc
		if err := r.store.col(colTickets).FindOne(ctx, bson.D{{Key: "_id", Value: mustOID(ticketID)},
			{Key: "projectId", Value: mustOID(projectID)}}).Decode(&doc); err != nil {
			if errors.Is(err, mongo.ErrNoDocuments) {
				return tickets.ErrNotFound
			}
			return err
		}
		if err := r.store.insertAudit(ctx, doc.WorkspaceId, events...); err != nil {
			return err
		}
		now := time.Now().UTC()
		if len(ob) > 0 {
			now = ob[0].OccurredAt
		}
		return r.insertOutbox(ctx, doc, ob, nil, now)
	})
	if err == nil && len(ob) > 0 {
		r.changed(projectID)
	}
	return err
}
