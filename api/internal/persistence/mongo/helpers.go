package mongo

import (
	"context"
	"errors"
	"strings"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"

	"github.com/arksnorman/open-kase/api/internal/audit"
	"github.com/arksnorman/open-kase/api/internal/outbox"
	"github.com/arksnorman/open-kase/api/package/fault"
	"github.com/arksnorman/open-kase/api/package/pagination"
)

// byTicketRef matches one ticket's records in a collection whose ticket_id
// is optional (outbox events, notification deliveries). Their ticket index
// (ticket_purge) is partial on {ticket_id: {$type: "objectId"}}, and MongoDB
// uses a partial index only when the query states that predicate itself.
func byTicketRef(tid bson.ObjectID) bson.D {
	return bson.D{{Key: "ticketId", Value: bson.D{{Key: "$eq", Value: tid}, {Key: "$type", Value: "objectId"}}}}
}

// errBadID is returned for identifiers that are not ObjectIDs; repositories
// translate it to their own not-found error.
var errBadID = errors.New("mongo: invalid object id")

func oid(hex string) (bson.ObjectID, error) {
	id, err := bson.ObjectIDFromHex(hex)
	if err != nil {
		return bson.ObjectID{}, errBadID
	}
	return id, nil
}

// mustOID parses an ID that services have already validated or that came
// from storage. An invalid value maps to the zero ID, which matches nothing.
func mustOID(hex string) bson.ObjectID {
	id, _ := bson.ObjectIDFromHex(hex)
	return id
}

func optOID(hex string) *bson.ObjectID {
	if hex == "" {
		return nil
	}
	id := mustOID(hex)
	return &id
}

func hexOrEmpty(id *bson.ObjectID) string {
	if id == nil {
		return ""
	}
	return id.Hex()
}

// isDuplicate reports whether err is a duplicate-key error on the named index.
func isDuplicate(err error, index string) bool {
	if !mongo.IsDuplicateKeyError(err) {
		return false
	}
	return index == "" || strings.Contains(err.Error(), index)
}

var errInvalidCursor = fault.Validation(fault.FieldError{Field: "cursor", Code: "invalidCursor", Message: "The page cursor is invalid or expired."})

// pageFilter adds keyset-pagination conditions to filter for an ascending
// sort on (field, _id). An empty field sorts by _id only.
func pageFilter(filter bson.D, field string, after *pagination.Cursor) (bson.D, error) {
	if after == nil {
		return filter, nil
	}
	id, err := oid(after.Id)
	if err != nil {
		return nil, errInvalidCursor
	}
	if field == "" {
		return append(filter, bson.E{Key: "_id", Value: bson.D{{Key: "$gt", Value: id}}}), nil
	}
	return append(filter, bson.E{Key: "$or", Value: bson.A{
		bson.D{{Key: field, Value: bson.D{{Key: "$gt", Value: after.Sort}}}},
		bson.D{{Key: field, Value: after.Sort}, {Key: "_id", Value: bson.D{{Key: "$gt", Value: id}}}},
	}}), nil
}

func pageOptions(field string, limit int) *options.FindOptionsBuilder {
	sort := bson.D{{Key: "_id", Value: 1}}
	if field != "" {
		sort = bson.D{{Key: field, Value: 1}, {Key: "_id", Value: 1}}
	}
	return options.Find().SetSort(sort).SetLimit(int64(limit))
}

// findAll runs a paged find and decodes every document into T.
func findAll[T any](ctx context.Context, col *mongo.Collection, filter bson.D, opts *options.FindOptionsBuilder) ([]T, error) {
	cur, err := col.Find(ctx, filter, opts)
	if err != nil {
		return nil, err
	}
	var out []T
	if err := cur.All(ctx, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// prefixRegex returns an anchored, escaped case-sensitive prefix match
// usable with an index on a normalized field.
func prefixRegex(prefix string) bson.Regex {
	return bson.Regex{Pattern: "^" + regexpQuote(prefix)}
}

func regexpQuote(s string) string {
	var b strings.Builder
	for _, r := range s {
		if strings.ContainsRune(`\.+*?()|[]{}^$`, r) {
			b.WriteByte('\\')
		}
		b.WriteRune(r)
	}
	return b.String()
}

// insertAudit appends events in the current transaction.
func (s *Store) insertAudit(ctx context.Context, workspaceID bson.ObjectID, events ...audit.Event) error {
	if len(events) == 0 {
		return nil
	}
	docs := make([]any, 0, len(events))
	for _, e := range events {
		docs = append(docs, toAuditDoc(workspaceID, e))
	}
	_, err := s.col(colAuditEvents).InsertMany(ctx, docs)
	return err
}

// insertAccessEvent records, in the current transaction, an outbox event
// for an access change (user disabled, membership removed) so the
// dispatcher can release the user's assignments durably.
func (s *Store) insertAccessEvent(ctx context.Context, ws bson.ObjectID, typ string, projectID *bson.ObjectID, userID bson.ObjectID, groupID *bson.ObjectID, ev *audit.Event, now time.Time) error {
	actor := actorDoc{Type: audit.ActorSystem}
	if ev != nil {
		actor = actorDoc{Type: ev.Actor.Type, Id: ev.Actor.Id, Label: ev.Actor.Label}
	}
	uid := userID
	state := outbox.StatePending
	if outbox.Suppressed(ctx) {
		state = outbox.StateSuppressed
	}
	_, err := s.col(colOutbox).InsertOne(ctx, outboxDoc{
		Id: bson.NewObjectID(), WorkspaceId: ws, ProjectId: projectID, Type: typ, Actor: actor,
		UserId: &uid, GroupId: groupID, State: state, SchemaVersion: outbox.SchemaVersion,
		OccurredAt: now, CreatedAt: now,
	})
	return err
}

// AuditRepository appends standalone audit events.
type AuditRepository struct{ store *Store }

// NewAuditRepository returns an audit repository.
func NewAuditRepository(s *Store) *AuditRepository { return &AuditRepository{store: s} }

// Append stores one event outside any business mutation.
func (r *AuditRepository) Append(ctx context.Context, ev audit.Event) error {
	ws, err := oid(ev.WorkspaceId)
	if err != nil {
		return err
	}
	_, err = r.store.col(colAuditEvents).InsertOne(ctx, toAuditDoc(ws, ev))
	return err
}

// AuditEvent is a read model used by tests and future audit views.
type AuditEvent struct {
	Action, ActorType, ActorId, ActorLabel, Target, Outcome, ProjectId, TicketId string
	ActorScopes, Changes                                                         []string
}

// RecentAudit returns the newest events for a workspace (diagnostics/tests).
func (r *AuditRepository) RecentAudit(ctx context.Context, limit int) ([]AuditEvent, error) {
	docs, err := findAll[auditDoc](ctx, r.store.col(colAuditEvents), bson.D{},
		options.Find().SetSort(bson.D{{Key: "createdAt", Value: -1}, {Key: "_id", Value: -1}}).SetLimit(int64(limit)))
	if err != nil {
		return nil, err
	}
	out := make([]AuditEvent, 0, len(docs))
	for _, d := range docs {
		out = append(out, AuditEvent{Action: d.Action, ActorType: d.Actor.Type, ActorId: d.Actor.Id, ActorLabel: d.Actor.Label,
			ActorScopes: d.Actor.Scopes, Target: d.Target, Outcome: d.Outcome, ProjectId: hexOrEmpty(d.ProjectId),
			TicketId: hexOrEmpty(d.TicketId), Changes: d.Changes})
	}
	return out, nil
}
