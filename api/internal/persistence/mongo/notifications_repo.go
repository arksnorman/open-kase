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
)

type deliveryRecipientDoc struct {
	Kind    string `bson:"kind"`
	Id      string `bson:"id,omitempty"`
	Address string `bson:"address,omitempty"`
}

type deliveryDoc struct {
	Id              bson.ObjectID        `bson:"_id"`
	WorkspaceId     bson.ObjectID        `bson:"workspaceId"`
	ProjectId       *bson.ObjectID       `bson:"projectId"`
	TicketId        *bson.ObjectID       `bson:"ticketId"`
	EventId         *bson.ObjectID       `bson:"eventId"`
	EventType       string               `bson:"eventType"`
	Channel         string               `bson:"channel"`
	Recipient       deliveryRecipientDoc `bson:"recipient"`
	Reason          string               `bson:"reason"`
	Template        string               `bson:"template"`
	EndpointId      *bson.ObjectID       `bson:"endpointId"`
	Status          string               `bson:"status"`
	Attempts        int32                `bson:"attempts"`
	NextAttemptAt   *time.Time           `bson:"nextAttemptAt"`
	LastAttemptAt   *time.Time           `bson:"lastAttemptAt"`
	ErrorClass      string               `bson:"errorClass"`
	Error           string               `bson:"error"`
	StatusCode      int32                `bson:"statusCode"`
	ResponseSnippet string               `bson:"responseSnippet"`
	ResponseMs      int32                `bson:"responseMs"`
	MessageId       string               `bson:"messageId"`
	Body            []byte               `bson:"body,omitempty"`
	BodyGen         int64                `bson:"bodyGen,omitempty"`
	CreatedAt       time.Time            `bson:"createdAt"`
	UpdatedAt       time.Time            `bson:"updatedAt"`
	CompletedAt     *time.Time           `bson:"completedAt"`
}

func (d deliveryDoc) toDomain() notifications.Delivery {
	return notifications.Delivery{Id: d.Id.Hex(), WorkspaceId: d.WorkspaceId.Hex(), ProjectId: hexOrEmpty(d.ProjectId),
		TicketId: hexOrEmpty(d.TicketId), EventId: hexOrEmpty(d.EventId), EventType: d.EventType, Channel: d.Channel,
		Recipient: notifications.Recipient{Kind: d.Recipient.Kind, Id: d.Recipient.Id, Address: d.Recipient.Address},
		Reason:    d.Reason, Template: d.Template, EndpointId: hexOrEmpty(d.EndpointId), Status: d.Status, Attempts: int(d.Attempts),
		NextAttemptAt: d.NextAttemptAt, LastAttemptAt: d.LastAttemptAt, ErrorClass: d.ErrorClass, Error: d.Error,
		StatusCode: int(d.StatusCode), ResponseSnippet: d.ResponseSnippet, ResponseMs: int(d.ResponseMs), MessageId: d.MessageId, Body: d.Body, BodyGen: d.BodyGen,
		CreatedAt: d.CreatedAt, UpdatedAt: d.UpdatedAt, CompletedAt: d.CompletedAt}
}

// invalidateDeliveryBodies drops the stored payloads matched by filter and
// moves their body generation, so a payload that a worker is building at
// this moment (from content read before the change) is not stored either.
func (s *Store) invalidateDeliveryBodies(ctx context.Context, filter bson.D) error {
	_, err := s.col(colDeliveries).UpdateMany(ctx, filter, bson.D{{Key: "$unset", Value: bson.D{{Key: "body", Value: ""}}},
		{Key: "$inc", Value: bson.D{{Key: "bodyGen", Value: int64(1)}}}})
	return err
}

func newDeliveryDoc(id bson.ObjectID, d notifications.Delivery, now time.Time) deliveryDoc {
	return deliveryDoc{Id: id, WorkspaceId: mustOID(d.WorkspaceId), ProjectId: optOID(d.ProjectId), TicketId: optOID(d.TicketId),
		EventId: optOID(d.EventId), EventType: d.EventType, Channel: d.Channel,
		Recipient: deliveryRecipientDoc{Kind: d.Recipient.Kind, Id: d.Recipient.Id, Address: d.Recipient.Address},
		Reason:    d.Reason, Template: d.Template, EndpointId: optOID(d.EndpointId), Status: notifications.StatusPending,
		CreatedAt: now, UpdatedAt: now}
}

// NotificationsRepository implements the notifications Events, Deliveries,
// Directory and People interfaces.
type NotificationsRepository struct {
	store *Store
	jobs  *JobsRepository
}

var (
	_ notifications.Events     = (*NotificationsRepository)(nil)
	_ notifications.Deliveries = (*NotificationsRepository)(nil)
	_ notifications.Directory  = (*NotificationsRepository)(nil)
	_ notifications.People     = (*NotificationsRepository)(nil)
)

// NewNotificationsRepository returns the repository.
func NewNotificationsRepository(s *Store, js *JobsRepository) *NotificationsRepository {
	return &NotificationsRepository{store: s, jobs: js}
}

// ---- Events ----

func (d outboxDoc) toRecord() outbox.Record {
	r := outbox.Record{Id: d.Id.Hex(), WorkspaceId: d.WorkspaceId.Hex(), ProjectId: hexOrEmpty(d.ProjectId), TicketId: hexOrEmpty(d.TicketId),
		MessageId: hexOrEmpty(d.MessageId), Type: d.Type, Visibility: d.Visibility, TicketVersion: d.TicketVersion,
		Actor: audit.Actor{Type: d.Actor.Type, Id: d.Actor.Id, Label: d.Actor.Label, Scopes: d.Actor.Scopes}, Transition: d.Transition, Changes: d.Changes,
		UserId: hexOrEmpty(d.UserId), GroupId: hexOrEmpty(d.GroupId), State: d.State, OccurredAt: d.OccurredAt, CreatedAt: d.CreatedAt,
		LeaseGeneration: d.LeaseGen}
	if d.MessageAuthor != nil {
		r.MessageAuthorKind, r.MessageAuthorId = d.MessageAuthor.Kind, d.MessageAuthor.Id.Hex()
	}
	if d.SLA != nil {
		due := d.SLA.DueAt
		r.SLAKind, r.SLADueAt = d.SLA.Kind, &due
	}
	if s := d.Snapshot; s != nil {
		r.Snapshot = outbox.Snapshot{Subject: s.Subject, Priority: int(s.Priority), StatusId: s.StatusId.Hex(), StatusCategory: s.StatusCategory,
			GroupId: s.GroupId.Hex(), AssigneeId: hexOrEmpty(s.AssigneeId), RequesterId: s.RequesterId.Hex(),
			RequestTypeId: s.RequestTypeId.Hex(), OrganisationId: hexOrEmpty(s.OrganisationId)}
	}
	return r
}

// ClaimEvent leases the oldest pending event whose lease is free.
func (r *NotificationsRepository) ClaimEvent(ctx context.Context, owner string, now time.Time, ttl time.Duration) (outbox.Record, bool, error) {
	var doc outboxDoc
	err := r.store.col(colOutbox).FindOneAndUpdate(ctx,
		bson.D{{Key: "state", Value: outbox.StatePending}, {Key: "$or", Value: bson.A{
			bson.D{{Key: "leaseUntil", Value: nil}},
			bson.D{{Key: "leaseUntil", Value: bson.D{{Key: "$lt", Value: now}}}},
		}}},
		bson.D{{Key: "$set", Value: bson.D{{Key: "leaseOwner", Value: owner}, {Key: "leaseUntil", Value: now.Add(ttl)}}},
			{Key: "$inc", Value: bson.D{{Key: "leaseGeneration", Value: int64(1)}}}},
		options.FindOneAndUpdate().SetSort(bson.D{{Key: "createdAt", Value: 1}, {Key: "_id", Value: 1}}).SetReturnDocument(options.After),
	).Decode(&doc)
	if errors.Is(err, mongo.ErrNoDocuments) {
		return outbox.Record{}, false, nil
	}
	if err != nil {
		return outbox.Record{}, false, err
	}
	return doc.toRecord(), true, nil
}

// GetEvent returns one stored event.
func (r *NotificationsRepository) GetEvent(ctx context.Context, id string) (outbox.Record, error) {
	eid, err := oid(id)
	if err != nil {
		return outbox.Record{}, notifications.ErrNotFound
	}
	var doc outboxDoc
	err = r.store.col(colOutbox).FindOne(ctx, bson.D{{Key: "_id", Value: eid}}).Decode(&doc)
	if errors.Is(err, mongo.ErrNoDocuments) {
		return outbox.Record{}, notifications.ErrNotFound
	}
	if err != nil {
		return outbox.Record{}, err
	}
	return doc.toRecord(), nil
}

// fanoutChunk bounds the jobs created per transaction.
const fanoutChunk = 200

// FanOut creates missing jobs and deliveries in chunks and marks the event
// dispatched with the last chunk. Each chunk re-checks that the event is
// still pending under the same lease generation (when the record carries
// one), so a dispatcher whose lease was taken over stops; job keys make
// any overlap harmless.
func (r *NotificationsRepository) FanOut(ctx context.Context, ev outbox.Record, planned []notifications.Planned, now time.Time) (int, error) {
	eid := mustOID(ev.Id)
	created := 0
	for start := 0; start == 0 || start < len(planned); start += fanoutChunk {
		end := min(start+fanoutChunk, len(planned))
		final := end == len(planned)
		chunk := planned[start:end]
		n := 0
		stop := false
		err := r.store.transaction(ctx, func(ctx context.Context) error {
			n, stop = 0, false
			fence := bson.D{{Key: "_id", Value: eid}, {Key: "state", Value: outbox.StatePending}}
			if ev.LeaseGeneration > 0 {
				fence = append(fence, bson.E{Key: "leaseGeneration", Value: ev.LeaseGeneration})
			}
			set := bson.D{{Key: "leaseUntil", Value: now.Add(time.Minute)}}
			if final {
				set = bson.D{{Key: "state", Value: outbox.StateDispatched}, {Key: "dispatchedAt", Value: now},
					{Key: "leaseUntil", Value: nil}, {Key: "leaseOwner", Value: ""}}
			}
			res, err := r.store.col(colOutbox).UpdateOne(ctx, fence, bson.D{{Key: "$set", Value: set}})
			if err != nil {
				return err
			}
			if res.MatchedCount == 0 {
				stop = true // already dispatched, or the lease moved on
				return nil
			}
			for _, p := range chunk {
				id, ok, err := r.jobs.enqueueOne(ctx, p.Job)
				if err != nil {
					return err
				}
				if !ok {
					continue
				}
				n++
				if p.Delivery != nil {
					if _, err := r.store.col(colDeliveries).InsertOne(ctx, newDeliveryDoc(mustOID(id), *p.Delivery, now)); err != nil {
						return err
					}
				}
			}
			if n > 0 {
				_, err = r.store.col(colOutbox).UpdateOne(ctx, bson.D{{Key: "_id", Value: eid}},
					bson.D{{Key: "$inc", Value: bson.D{{Key: "fanoutCount", Value: int32(n)}}}})
			}
			return err
		})
		if err != nil {
			return created, err
		}
		created += n
		if stop || len(planned) == 0 {
			break
		}
	}
	return created, nil
}

// FailEvent releases the lease until retryAt and counts the attempt; after
// maxAttempts the event is marked failed (visible to operators).
func (r *NotificationsRepository) FailEvent(ctx context.Context, ev outbox.Record, msg string, retryAt time.Time, maxAttempts int) error {
	var doc outboxDoc
	err := r.store.col(colOutbox).FindOneAndUpdate(ctx,
		bson.D{{Key: "_id", Value: mustOID(ev.Id)}, {Key: "state", Value: outbox.StatePending}, {Key: "leaseGeneration", Value: ev.LeaseGeneration}},
		bson.D{{Key: "$set", Value: bson.D{{Key: "leaseUntil", Value: retryAt}, {Key: "leaseOwner", Value: ""}, {Key: "lastError", Value: truncateString(msg, 1000)}}},
			{Key: "$inc", Value: bson.D{{Key: "attempts", Value: int32(1)}}}},
		options.FindOneAndUpdate().SetReturnDocument(options.After)).Decode(&doc)
	if errors.Is(err, mongo.ErrNoDocuments) {
		return nil
	}
	if err != nil {
		return err
	}
	if int(doc.Attempts) >= maxAttempts {
		_, err = r.store.col(colOutbox).UpdateOne(ctx, bson.D{{Key: "_id", Value: doc.Id}, {Key: "state", Value: outbox.StatePending}},
			bson.D{{Key: "$set", Value: bson.D{{Key: "state", Value: outbox.StateFailed}}}})
	}
	return err
}

func truncateString(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}

// ---- Deliveries ----

// GetDelivery returns one delivery including its stored webhook body.
func (r *NotificationsRepository) GetDelivery(ctx context.Context, id string) (notifications.Delivery, error) {
	did, err := oid(id)
	if err != nil {
		return notifications.Delivery{}, notifications.ErrNotFound
	}
	var doc deliveryDoc
	err = r.store.col(colDeliveries).FindOne(ctx, bson.D{{Key: "_id", Value: did}}).Decode(&doc)
	if errors.Is(err, mongo.ErrNoDocuments) {
		return notifications.Delivery{}, notifications.ErrNotFound
	}
	if err != nil {
		return notifications.Delivery{}, err
	}
	return doc.toDomain(), nil
}

// UpdateDelivery records an attempt result (called in a fenced transaction).
func (r *NotificationsRepository) UpdateDelivery(ctx context.Context, id string, u notifications.DeliveryUpdate) error {
	set := bson.D{{Key: "status", Value: u.Status}, {Key: "attempts", Value: int32(u.Attempts)}, {Key: "lastAttemptAt", Value: u.LastAttemptAt},
		{Key: "nextAttemptAt", Value: u.NextAttemptAt}, {Key: "errorClass", Value: u.ErrorClass}, {Key: "error", Value: truncateString(u.Error, 1000)},
		{Key: "statusCode", Value: int32(u.StatusCode)}, {Key: "responseSnippet", Value: truncateString(u.ResponseSnippet, 1024)},
		{Key: "updatedAt", Value: u.LastAttemptAt}}
	if u.ResponseMs > 0 {
		set = append(set, bson.E{Key: "responseMs", Value: int32(u.ResponseMs)})
	}
	if u.MessageId != "" {
		set = append(set, bson.E{Key: "messageId", Value: u.MessageId})
	}
	if u.Address != "" {
		set = append(set, bson.E{Key: "recipient.address", Value: u.Address})
	}
	if notifications.Terminal(u.Status) {
		set = append(set, bson.E{Key: "completedAt", Value: u.LastAttemptAt})
	}
	update := bson.D{{Key: "$set", Value: set}}
	if u.ResponseMs <= 0 {
		// No response this attempt: drop a value left by an earlier one.
		update = append(update, bson.E{Key: "$unset", Value: bson.D{{Key: "responseMs", Value: ""}}})
	}
	_, err := r.store.col(colDeliveries).UpdateOne(ctx, bson.D{{Key: "_id", Value: mustOID(id)}}, update)
	return err
}

// SetDeliveryBody stores the exact webhook payload once (called in a
// fenced transaction before the first send), and only while the body
// generation is still gen, the one read before the payload was built: a
// redaction or a withdrawn grant in between means the payload may hold
// removed content, so it is not stored (the caller rebuilds it later).
func (r *NotificationsRepository) SetDeliveryBody(ctx context.Context, id string, body []byte, gen int64, now time.Time) error {
	filter := bson.D{{Key: "_id", Value: mustOID(id)}, {Key: "body", Value: bson.D{{Key: "$exists", Value: false}}}}
	if gen == 0 {
		filter = append(filter, bson.E{Key: "bodyGen", Value: bson.D{{Key: "$in", Value: bson.A{int64(0), nil}}}})
	} else {
		filter = append(filter, bson.E{Key: "bodyGen", Value: gen})
	}
	_, err := r.store.col(colDeliveries).UpdateOne(ctx, filter,
		bson.D{{Key: "$set", Value: bson.D{{Key: "body", Value: body}, {Key: "updatedAt", Value: now}}}})
	return err
}

// MarkRequeued resets a delivery for a manual retry.
func (r *NotificationsRepository) MarkRequeued(ctx context.Context, id string, now time.Time) error {
	_, err := r.store.col(colDeliveries).UpdateOne(ctx, bson.D{{Key: "_id", Value: mustOID(id)}},
		bson.D{{Key: "$set", Value: bson.D{{Key: "status", Value: notifications.StatusPending}, {Key: "nextAttemptAt", Value: now},
			{Key: "completedAt", Value: nil}, {Key: "updatedAt", Value: now}}}})
	return err
}

// MarkDead marks a delivery whose job was dead-lettered by the runner as
// failed, unless it already has a final status.
func (r *NotificationsRepository) MarkDead(ctx context.Context, id string, u notifications.DeliveryUpdate) error {
	_, err := r.store.col(colDeliveries).UpdateOne(ctx, bson.D{{Key: "_id", Value: mustOID(id)},
		{Key: "status", Value: bson.D{{Key: "$in", Value: bson.A{notifications.StatusPending, notifications.StatusRetrying}}}}},
		bson.D{{Key: "$set", Value: bson.D{{Key: "status", Value: notifications.StatusFailed}, {Key: "attempts", Value: int32(u.Attempts)},
			{Key: "lastAttemptAt", Value: u.LastAttemptAt}, {Key: "nextAttemptAt", Value: nil}, {Key: "errorClass", Value: u.ErrorClass},
			{Key: "error", Value: truncateString(u.Error, 1000)}, {Key: "updatedAt", Value: u.LastAttemptAt}, {Key: "completedAt", Value: u.LastAttemptAt}}}})
	return err
}

// ListDeliveries returns deliveries newest first, without bodies.
func (r *NotificationsRepository) ListDeliveries(ctx context.Context, f notifications.DeliveryFilter) ([]notifications.Delivery, error) {
	filter := bson.D{{Key: "workspaceId", Value: mustOID(f.WorkspaceId)}}
	if f.ProjectId != "" {
		filter = append(filter, bson.E{Key: "projectId", Value: mustOID(f.ProjectId)})
	}
	if f.EndpointId != "" {
		filter = append(filter, bson.E{Key: "endpointId", Value: mustOID(f.EndpointId)})
	}
	if f.Channel != "" {
		filter = append(filter, bson.E{Key: "channel", Value: f.Channel})
	}
	if f.Status != "" {
		filter = append(filter, bson.E{Key: "status", Value: f.Status})
	}
	if f.EventId != "" {
		filter = append(filter, bson.E{Key: "eventId", Value: mustOID(f.EventId)})
	}
	if f.BeforeId != "" {
		filter = append(filter, bson.E{Key: "_id", Value: bson.D{{Key: "$lt", Value: mustOID(f.BeforeId)}}})
	}
	docs, err := findAll[deliveryDoc](ctx, r.store.col(colDeliveries), filter, options.Find().
		SetSort(bson.D{{Key: "_id", Value: -1}}).SetLimit(int64(f.Limit)).SetProjection(bson.D{{Key: "body", Value: 0}}))
	if err != nil {
		return nil, err
	}
	out := make([]notifications.Delivery, 0, len(docs))
	for _, d := range docs {
		out = append(out, d.toDomain())
	}
	return out, nil
}

// CreateDirect stores one job and its delivery in a transaction.
func (r *NotificationsRepository) CreateDirect(ctx context.Context, p notifications.Planned) (notifications.Delivery, error) {
	var out deliveryDoc
	err := r.store.transaction(ctx, func(ctx context.Context) error {
		id, ok, err := r.jobs.enqueueOne(ctx, p.Job)
		if err != nil {
			return err
		}
		if !ok {
			return errors.New("notifications: job key already exists")
		}
		out = newDeliveryDoc(mustOID(id), *p.Delivery, r.jobs.now())
		_, err = r.store.col(colDeliveries).InsertOne(ctx, out)
		return err
	})
	if err != nil {
		return notifications.Delivery{}, err
	}
	return out.toDomain(), nil
}

// ---- Directory and People ----

// ProjectInfo returns a project's notification data.
func (r *NotificationsRepository) ProjectInfo(ctx context.Context, projectID string) (notifications.ProjectInfo, error) {
	var d projectDoc
	err := r.store.col(colProjects).FindOne(ctx, bson.D{{Key: "_id", Value: mustOID(projectID)}}).Decode(&d)
	if errors.Is(err, mongo.ErrNoDocuments) {
		return notifications.ProjectInfo{Id: projectID}, nil
	}
	if err != nil {
		return notifications.ProjectInfo{}, err
	}
	return notifications.ProjectInfo{Id: d.Id.Hex(),
		RequesterAck: !d.RequesterAckDisabled}, nil
}

// Watchers returns the IDs of users watching a ticket.
func (r *NotificationsRepository) Watchers(ctx context.Context, ticketID string) ([]string, error) {
	docs, err := findAll[watcherDoc](ctx, r.store.col(colWatchers), bson.D{{Key: "ticketId", Value: mustOID(ticketID)}},
		options.Find().SetSort(bson.D{{Key: "createdAt", Value: 1}}).SetLimit(1000))
	if err != nil {
		return nil, err
	}
	out := make([]string, 0, len(docs))
	for _, d := range docs {
		out = append(out, d.UserId.Hex())
	}
	return out, nil
}

// TicketCC returns a ticket's CC addresses (none when it is gone).
func (r *NotificationsRepository) TicketCC(ctx context.Context, ticketID string) ([]string, error) {
	var d struct {
		CCEmails []string `bson:"ccEmails"`
	}
	err := r.store.col(colTickets).FindOne(ctx, bson.D{{Key: "_id", Value: mustOID(ticketID)}},
		options.FindOne().SetProjection(bson.D{{Key: "ccEmails", Value: 1}})).Decode(&d)
	if errors.Is(err, mongo.ErrNoDocuments) {
		return nil, nil
	}
	return d.CCEmails, err
}

// GroupMembers returns the IDs of a group's members.
func (r *NotificationsRepository) GroupMembers(ctx context.Context, groupID string) ([]string, error) {
	docs, err := findAll[groupMembershipDoc](ctx, r.store.col(colGroupMemberships), bson.D{{Key: "groupId", Value: mustOID(groupID)}},
		options.Find().SetSort(bson.D{{Key: "userId", Value: 1}}).SetLimit(5000))
	if err != nil {
		return nil, err
	}
	out := make([]string, 0, len(docs))
	for _, d := range docs {
		out = append(out, d.UserId.Hex())
	}
	return out, nil
}

// ActiveEndpoints returns a project's active webhook subscriptions.
func (r *NotificationsRepository) ActiveEndpoints(ctx context.Context, projectID string) ([]notifications.EndpointRef, error) {
	docs, err := findAll[webhookEndpointDoc](ctx, r.store.col(colWebhookEndpoints),
		bson.D{{Key: "projectId", Value: mustOID(projectID)}, {Key: "active", Value: true}}, options.Find().SetSort(bson.D{{Key: "_id", Value: 1}}))
	if err != nil {
		return nil, err
	}
	out := make([]notifications.EndpointRef, 0, len(docs))
	for _, d := range docs {
		out = append(out, notifications.EndpointRef{Id: d.Id.Hex(), EventTypes: d.EventTypes, IncludeAgentNotes: d.IncludeAgentNotes})
	}
	return out, nil
}

// User returns a staff user's notification data.
func (r *NotificationsRepository) User(ctx context.Context, workspaceID, id string) (notifications.UserInfo, error) {
	uid, err := oid(id)
	if err != nil {
		return notifications.UserInfo{}, notifications.ErrNotFound
	}
	var d userDoc
	err = r.store.col(colUsers).FindOne(ctx, bson.D{{Key: "_id", Value: uid}, {Key: "workspaceId", Value: mustOID(workspaceID)}}).Decode(&d)
	if errors.Is(err, mongo.ErrNoDocuments) {
		return notifications.UserInfo{}, notifications.ErrNotFound
	}
	if err != nil {
		return notifications.UserInfo{}, err
	}
	u := d.toDomain()
	return notifications.UserInfo{Id: u.Id, WorkspaceId: u.WorkspaceId, Name: u.Name, Email: u.Email, Active: u.Active(), Admin: u.IsAdmin(),
		MuteWatcher: u.EmailPrefs.MuteWatcherUpdates, MuteQueue: u.EmailPrefs.MuteQueueUpdates}, nil
}

// Contact returns a requester contact's address.
func (r *NotificationsRepository) Contact(ctx context.Context, projectID, id string) (notifications.ContactInfo, error) {
	cid, err := oid(id)
	if err != nil {
		return notifications.ContactInfo{}, notifications.ErrNotFound
	}
	var d contactDoc
	err = r.store.col(colContacts).FindOne(ctx, bson.D{{Key: "_id", Value: cid}, {Key: "projectId", Value: mustOID(projectID)}}).Decode(&d)
	if errors.Is(err, mongo.ErrNoDocuments) {
		return notifications.ContactInfo{}, notifications.ErrNotFound
	}
	if err != nil {
		return notifications.ContactInfo{}, err
	}
	return notifications.ContactInfo{Id: d.Id.Hex(), Name: d.Name, Email: d.Email}, nil
}

// DescribeRecipients fills the display details (name, email, endpoint URL)
// of the deliveries' recipients with one query per recipient kind. Missing
// records (deleted users, contacts or endpoints) leave the details empty.
func (r *NotificationsRepository) DescribeRecipients(ctx context.Context, ds []notifications.Delivery) error {
	ids := map[string]map[bson.ObjectID]bool{}
	add := func(col, hex string) {
		if id, err := oid(hex); err == nil {
			if ids[col] == nil {
				ids[col] = map[bson.ObjectID]bool{}
			}
			ids[col][id] = true
		}
	}
	for _, d := range ds {
		switch d.Recipient.Kind {
		case notifications.RecipientUser:
			add(colUsers, d.Recipient.Id)
		case notifications.RecipientContact:
			add(colContacts, d.Recipient.Id)
		}
		if d.EndpointId != "" {
			add(colWebhookEndpoints, d.EndpointId)
		}
	}
	type rec struct {
		Id          bson.ObjectID `bson:"_id"`
		Name        string        `bson:"name"`
		DisplayName string        `bson:"displayName"`
		Email       string        `bson:"email"`
		URL         string        `bson:"url"`
	}
	found := map[bson.ObjectID]rec{}
	for col, set := range ids {
		in := make(bson.A, 0, len(set))
		for id := range set {
			in = append(in, id)
		}
		rows, err := findAll[rec](ctx, r.store.col(col), bson.D{{Key: "_id", Value: bson.D{{Key: "$in", Value: in}}}},
			options.Find().SetProjection(bson.D{{Key: "name", Value: 1}, {Key: "displayName", Value: 1}, {Key: "email", Value: 1}, {Key: "url", Value: 1}}))
		if err != nil {
			return err
		}
		for _, row := range rows {
			if col == colUsers {
				row.Name = row.DisplayName
			}
			found[row.Id] = row
		}
	}
	if err := r.describeTickets(ctx, ds); err != nil {
		return err
	}
	for i := range ds {
		d := &ds[i]
		switch d.Recipient.Kind {
		case notifications.RecipientUser, notifications.RecipientContact:
			if id, err := oid(d.Recipient.Id); err == nil {
				if row, ok := found[id]; ok {
					d.Recipient.Name, d.Recipient.Email = row.Name, row.Email
				}
			}
		}
		if id, err := oid(d.EndpointId); err == nil {
			if row, ok := found[id]; ok {
				d.Recipient.URL = row.URL
				if d.Recipient.Kind == notifications.RecipientEndpoint {
					d.Recipient.Name = row.Name
				}
			}
		}
	}
	return nil
}

// describeTickets fills the ticket numbers of the deliveries with one query.
func (r *NotificationsRepository) describeTickets(ctx context.Context, ds []notifications.Delivery) error {
	set := map[bson.ObjectID]bool{}
	for _, d := range ds {
		if id, err := oid(d.TicketId); err == nil {
			set[id] = true
		}
	}
	if len(set) == 0 {
		return nil
	}
	in := make(bson.A, 0, len(set))
	for id := range set {
		in = append(in, id)
	}
	type row struct {
		Id     bson.ObjectID `bson:"_id"`
		Number int64         `bson:"number"`
	}
	rows, err := findAll[row](ctx, r.store.col(colTickets), bson.D{{Key: "_id", Value: bson.D{{Key: "$in", Value: in}}}},
		options.Find().SetProjection(bson.D{{Key: "number", Value: 1}}))
	if err != nil {
		return err
	}
	numbers := make(map[string]int64, len(rows))
	for _, rw := range rows {
		numbers[rw.Id.Hex()] = rw.Number
	}
	for i := range ds {
		ds[i].TicketNumber = numbers[ds[i].TicketId]
	}
	return nil
}
