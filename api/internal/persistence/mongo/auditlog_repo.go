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
	"github.com/arksnorman/open-kase/api/internal/auditlog"
	"github.com/arksnorman/open-kase/api/internal/integrations"
	"github.com/arksnorman/open-kase/api/internal/users"
)

// AuditLogRepository lists audit events for the admin audit log.
type AuditLogRepository struct{ store *Store }

// NewAuditLogRepository returns an audit log repository.
func NewAuditLogRepository(s *Store) *AuditLogRepository { return &AuditLogRepository{store: s} }

// TicketID resolves a ticket number.
func (r *AuditLogRepository) TicketID(ctx context.Context, workspaceID string, number int64) (string, bool, error) {
	var d struct {
		Id bson.ObjectID `bson:"_id"`
	}
	err := r.store.col(colTickets).FindOne(ctx, bson.D{{Key: "workspaceId", Value: mustOID(workspaceID)}, {Key: "number", Value: number}},
		options.FindOne().SetProjection(bson.D{{Key: "_id", Value: 1}})).Decode(&d)
	if errors.Is(err, mongo.ErrNoDocuments) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	return d.Id.Hex(), true, nil
}

// List returns audit events newest first with actor and target names
// looked up for the page (one query per referenced collection).
func (r *AuditLogRepository) List(ctx context.Context, f auditlog.Filter) ([]auditlog.Entry, error) {
	filter := bson.D{{Key: "workspaceId", Value: mustOID(f.WorkspaceId)}}
	if len(f.ActorTypes) > 0 {
		filter = append(filter, bson.E{Key: "actor.type", Value: bson.D{{Key: "$in", Value: f.ActorTypes}}})
	}
	if f.ActorId != "" {
		filter = append(filter, bson.E{Key: "actor.id", Value: f.ActorId})
	}
	if f.Action != "" {
		filter = append(filter, bson.E{Key: "action", Value: f.Action})
	}
	if f.ActionPrefix != "" {
		filter = append(filter, bson.E{Key: "action", Value: prefixRegex(f.ActionPrefix)})
	}
	if f.TicketId != "" {
		filter = append(filter, bson.E{Key: "ticketId", Value: mustOID(f.TicketId)})
	}
	created := bson.D{}
	if f.From != nil {
		created = append(created, bson.E{Key: "$gte", Value: *f.From})
	}
	if f.To != nil {
		created = append(created, bson.E{Key: "$lt", Value: *f.To})
	}
	if len(created) > 0 {
		filter = append(filter, bson.E{Key: "createdAt", Value: created})
	}
	if f.After != nil {
		at, err := time.Parse(time.RFC3339Nano, f.After.Sort)
		id, ierr := oid(f.After.Id)
		if err != nil || ierr != nil {
			return nil, errInvalidCursor
		}
		filter = append(filter, bson.E{Key: "$or", Value: bson.A{
			bson.D{{Key: "createdAt", Value: bson.D{{Key: "$lt", Value: at}}}},
			bson.D{{Key: "createdAt", Value: at}, {Key: "_id", Value: bson.D{{Key: "$lt", Value: id}}}},
		}})
	}
	docs, err := findAll[auditDoc](ctx, r.store.col(colAuditEvents), filter,
		options.Find().SetSort(bson.D{{Key: "createdAt", Value: -1}, {Key: "_id", Value: -1}}).SetLimit(int64(f.Limit)))
	if err != nil {
		return nil, err
	}

	// Collect the referenced IDs per collection.
	refs := map[string]map[bson.ObjectID]bool{}
	add := func(col, hex string) {
		if id, err := oid(hex); err == nil {
			if refs[col] == nil {
				refs[col] = map[bson.ObjectID]bool{}
			}
			refs[col][id] = true
		}
	}
	targetCols := map[string]string{"user": colUsers, "contact": colContacts, "group": colGroups, "integration": colServicePrincipals,
		"macro": colMacros, "webhook": colWebhookEndpoints}
	for _, d := range docs {
		switch d.Actor.Type {
		case audit.ActorUser:
			add(colUsers, d.Actor.Id)
		case audit.ActorService:
			add(colServicePrincipals, d.Actor.Id)
		}
		if d.TicketId != nil {
			add(colTickets, d.TicketId.Hex())
		} else if typ, id, ok := strings.Cut(d.Target, "/"); ok && targetCols[typ] != "" {
			add(targetCols[typ], id)
		}
	}
	names := map[string]map[bson.ObjectID]auditName{}
	for col, set := range refs {
		m, err := r.lookup(ctx, col, set)
		if err != nil {
			return nil, err
		}
		names[col] = m
	}
	nameOf := func(col, hex string) (auditName, bool) {
		id, err := oid(hex)
		if err != nil {
			return auditName{}, false
		}
		n, ok := names[col][id]
		return n, ok
	}

	out := make([]auditlog.Entry, 0, len(docs))
	for _, d := range docs {
		e := auditlog.Entry{Id: d.Id.Hex(), CreatedAt: d.CreatedAt, Action: d.Action, RequestId: d.RequestId,
			Changes: d.Changes, Reason: d.Reason, Outcome: d.Outcome}
		a := auditlog.Actor{Type: audit.ListedType(d.Actor.Type), Id: d.Actor.Id, Label: d.Actor.Label, Scopes: d.Actor.Scopes}
		switch d.Actor.Type {
		case audit.ActorUser:
			a.Name = "Unknown user"
			if n, ok := nameOf(colUsers, d.Actor.Id); ok {
				a.Name = n.name
			}
		case audit.ActorService:
			a.Name = "Deleted token"
			if n, ok := nameOf(colServicePrincipals, d.Actor.Id); ok {
				a.Name = n.name
			}
		default:
			a.Name = audit.SystemActorName(d.Actor.Id)
		}
		e.Actor = a
		typ, id, _ := strings.Cut(d.Target, "/")
		switch {
		case d.TicketId != nil:
			t := &auditlog.Target{Type: "ticket", Id: d.TicketId.Hex()}
			if n, ok := names[colTickets][*d.TicketId]; ok {
				t.Name = n.name
				t.Ticket = &auditlog.TicketRef{Id: d.TicketId.Hex(), Number: n.number, Subject: n.name}
			}
			e.Target = t
		case d.Target != "":
			t := &auditlog.Target{Type: typ, Id: id}
			if typ == "tag" {
				t.Name = id
			} else if n, ok := nameOf(targetCols[typ], id); ok {
				t.Name = n.name
			}
			e.Target = t
		}
		out = append(out, e)
	}
	return out, nil
}

type auditName struct {
	name   string
	number int64
}

// lookup returns display names for the IDs of one collection.
func (r *AuditLogRepository) lookup(ctx context.Context, col string, set map[bson.ObjectID]bool) (map[bson.ObjectID]auditName, error) {
	in := make(bson.A, 0, len(set))
	for id := range set {
		in = append(in, id)
	}
	rows, err := findAll[struct {
		Id          bson.ObjectID `bson:"_id"`
		Name        string        `bson:"name"`
		DisplayName string        `bson:"displayName"`
		Username    string        `bson:"username"`
		Subject     string        `bson:"subject"`
		Number      int64         `bson:"number"`
		URL         string        `bson:"url"`
		Email       string        `bson:"email"`
	}](ctx, r.store.col(col), bson.D{{Key: "_id", Value: bson.D{{Key: "$in", Value: in}}}},
		options.Find().SetProjection(bson.D{{Key: "name", Value: 1}, {Key: "displayName", Value: 1}, {Key: "username", Value: 1},
			{Key: "subject", Value: 1}, {Key: "number", Value: 1}, {Key: "url", Value: 1}, {Key: "email", Value: 1}}))
	if err != nil {
		return nil, err
	}
	out := make(map[bson.ObjectID]auditName, len(rows))
	for _, row := range rows {
		n := auditName{name: row.Name}
		switch col {
		case colUsers:
			n.name = row.DisplayName
			if n.name == "" {
				n.name = row.Username
			}
		case colTickets:
			n = auditName{name: row.Subject, number: row.Number}
		case colWebhookEndpoints:
			if n.name == "" {
				n.name = row.URL
			}
		case colContacts:
			if n.name == "" {
				n.name = row.Email
			}
		}
		out[row.Id] = n
	}
	return out, nil
}

// Actors lists users and integration tokens for the audit actor catalog:
// up to limit of each whose name (users: also username or email) contains
// q, ignoring case, sorted by name.
func (r *AuditLogRepository) Actors(ctx context.Context, workspaceID, q string, limit int) ([]auditlog.ActorOption, bool, error) {
	ws := mustOID(workspaceID)
	match := func(fields ...string) bson.E {
		or := bson.A{}
		for _, f := range fields {
			or = append(or, bson.D{{Key: f, Value: bson.Regex{Pattern: regexpQuote(q), Options: "i"}}})
		}
		return bson.E{Key: "$or", Value: or}
	}
	userFilter := bson.D{{Key: "workspaceId", Value: ws}}
	tokenFilter := bson.D{{Key: "workspaceId", Value: ws}, {Key: "kind", Value: "integration"}}
	if q != "" {
		userFilter = append(userFilter, match("displayName", "username", "email"))
		tokenFilter = append(tokenFilter, match("name"))
	}
	userDocs, err := findAll[userDoc](ctx, r.store.col(colUsers), userFilter, options.Find().
		SetProjection(bson.D{{Key: "displayName", Value: 1}, {Key: "username", Value: 1}, {Key: "email", Value: 1}, {Key: "status", Value: 1}}).
		SetSort(bson.D{{Key: "displayName", Value: 1}, {Key: "_id", Value: 1}}).SetLimit(int64(limit+1)))
	if err != nil {
		return nil, false, err
	}
	tokenDocs, err := findAll[servicePrincipalDoc](ctx, r.store.col(colServicePrincipals), tokenFilter, options.Find().
		SetProjection(bson.D{{Key: "name", Value: 1}, {Key: "status", Value: 1}}).
		SetSort(bson.D{{Key: "name", Value: 1}, {Key: "_id", Value: 1}}).SetLimit(int64(limit+1)))
	if err != nil {
		return nil, false, err
	}
	truncated := len(userDocs) > limit || len(tokenDocs) > limit
	userDocs, tokenDocs = userDocs[:min(len(userDocs), limit)], tokenDocs[:min(len(tokenDocs), limit)]
	out := make([]auditlog.ActorOption, 0, len(userDocs)+len(tokenDocs))
	for _, d := range userDocs {
		name := d.DisplayName
		if name == "" {
			name = d.Username
		}
		var detail []string
		if d.Username != "" {
			detail = append(detail, "@"+d.Username)
		}
		detail = append(detail, d.Email)
		switch d.Status {
		case users.StatusDisabled:
			detail = append(detail, "deactivated")
		case users.StatusInvited:
			detail = append(detail, "invited")
		}
		out = append(out, auditlog.ActorOption{Type: audit.ListedUser, Id: d.Id.Hex(), Name: name, Detail: strings.Join(detail, " · ")})
	}
	for _, d := range tokenDocs {
		detail := "API token"
		if d.Status == integrations.StatusRevoked {
			detail += " · revoked"
		}
		out = append(out, auditlog.ActorOption{Type: audit.ListedToken, Id: d.Id.Hex(), Name: d.Name, Detail: detail})
	}
	return out, truncated, nil
}
