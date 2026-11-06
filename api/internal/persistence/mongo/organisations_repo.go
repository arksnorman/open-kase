package mongo

import (
	"context"
	"errors"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"

	"github.com/arksnorman/open-kase/api/internal/audit"
	"github.com/arksnorman/open-kase/api/internal/organisations"
	"github.com/arksnorman/open-kase/api/package/pagination"
)

type organisationDoc struct {
	Id             bson.ObjectID `bson:"_id"`
	WorkspaceId    bson.ObjectID `bson:"workspaceId"`
	ProjectId      bson.ObjectID `bson:"projectId"`
	Name           string        `bson:"name"`
	NameNormalized string        `bson:"nameNormalized"`
	ExternalRef    string        `bson:"externalRef,omitempty"`
	Active         bool          `bson:"active"`
	CreatedAt      time.Time     `bson:"createdAt"`
	UpdatedAt      time.Time     `bson:"updatedAt"`
	// LastUsedAt is touched whenever a contact or ticket is linked, so a
	// concurrent delete conflicts with the link and re-checks.
	LastUsedAt *time.Time `bson:"lastUsedAt,omitempty"`
	Version    int64      `bson:"version"`
}

func (d organisationDoc) toDomain() organisations.Organisation {
	return organisations.Organisation{Id: d.Id.Hex(), WorkspaceId: d.WorkspaceId.Hex(), ProjectId: d.ProjectId.Hex(), Name: d.Name,
		ExternalRef: d.ExternalRef, Active: d.Active, CreatedAt: d.CreatedAt, UpdatedAt: d.UpdatedAt, Version: d.Version}
}

// OrganisationsRepository implements organisations.Repository.
type OrganisationsRepository struct{ store *Store }

var _ organisations.Repository = (*OrganisationsRepository)(nil)

// NewOrganisationsRepository returns the organisations repository.
func NewOrganisationsRepository(s *Store) *OrganisationsRepository {
	return &OrganisationsRepository{store: s}
}

func dupErr(err error) error {
	switch {
	case isDuplicate(err, "projectExternalRefUnique"):
		return organisations.ErrRefTaken
	case isDuplicate(err, ""):
		return organisations.ErrNameTaken
	}
	return err
}

// touchOrganisation proves, inside the caller's transaction, that the
// organisation still exists in the project and writes it, so the link
// conflicts with a concurrent delete. It returns false when it is gone.
func (s *Store) touchOrganisation(ctx context.Context, projectID bson.ObjectID, id bson.ObjectID, now time.Time) (found, active bool, err error) {
	var d organisationDoc
	err = s.col(colOrganisations).FindOneAndUpdate(ctx, bson.D{{Key: "_id", Value: id}, {Key: "projectId", Value: projectID}},
		bson.D{{Key: "$set", Value: bson.D{{Key: "lastUsedAt", Value: now}}}}).Decode(&d)
	if errors.Is(err, mongo.ErrNoDocuments) {
		return false, false, nil
	}
	return err == nil, d.Active, err
}

// List pages through the project's organisations by normalized name.
func (r *OrganisationsRepository) List(ctx context.Context, projectID, q, active, externalRef string, after *pagination.Cursor, limit int, withUsage bool) ([]organisations.Organisation, error) {
	pid := mustOID(projectID)
	filter := bson.D{{Key: "projectId", Value: pid}}
	if active != "all" {
		filter = append(filter, bson.E{Key: "active", Value: active != "false"})
	}
	if q != "" {
		re := bson.Regex{Pattern: regexpQuote(q), Options: "i"}
		filter = append(filter, bson.E{Key: "$or", Value: bson.A{
			bson.D{{Key: "name", Value: re}}, bson.D{{Key: "externalRef", Value: re}},
		}})
	}
	if externalRef != "" {
		filter = append(filter, bson.E{Key: "externalRef", Value: externalRef})
	}
	filter, err := pageFilter(filter, "nameNormalized", after)
	if err != nil {
		return nil, err
	}
	docs, err := findAll[organisationDoc](ctx, r.store.col(colOrganisations), filter, pageOptions("nameNormalized", limit))
	if err != nil {
		return nil, err
	}
	counts := map[string][2]int64{}
	if withUsage && len(docs) > 0 {
		ids := make([]string, 0, len(docs))
		for _, d := range docs {
			ids = append(ids, d.Id.Hex())
		}
		if counts, err = r.Usage(ctx, projectID, ids); err != nil {
			return nil, err
		}
	}
	out := make([]organisations.Organisation, 0, len(docs))
	for _, d := range docs {
		o := d.toDomain()
		o.ContactCount, o.TicketCount = counts[o.Id][0], counts[o.Id][1]
		out = append(out, o)
	}
	return out, nil
}

// Usage returns, per organisation id, the contact and ticket counts: one
// aggregation per collection.
func (r *OrganisationsRepository) Usage(ctx context.Context, projectID string, ids []string) (map[string][2]int64, error) {
	counts := map[string][2]int64{}
	if len(ids) == 0 {
		return counts, nil
	}
	oids := make(bson.A, 0, len(ids))
	for _, id := range ids {
		oids = append(oids, mustOID(id))
	}
	for i, col := range []string{colContacts, colTickets} {
		cur, err := r.store.col(col).Aggregate(ctx, bson.A{
			bson.D{{Key: "$match", Value: bson.D{{Key: "projectId", Value: mustOID(projectID)}, {Key: "organisationId", Value: bson.D{{Key: "$in", Value: oids}}}}}},
			bson.D{{Key: "$group", Value: bson.D{{Key: "_id", Value: "$organisationId"}, {Key: "n", Value: bson.D{{Key: "$sum", Value: 1}}}}}},
		})
		if err != nil {
			return nil, err
		}
		var rows []struct {
			Id bson.ObjectID `bson:"_id"`
			N  int64         `bson:"n"`
		}
		if err := cur.All(ctx, &rows); err != nil {
			return nil, err
		}
		for _, row := range rows {
			c := counts[row.Id.Hex()]
			c[i] = row.N
			counts[row.Id.Hex()] = c
		}
	}
	return counts, nil
}

// Get returns one organisation.
func (r *OrganisationsRepository) Get(ctx context.Context, projectID, id string) (organisations.Organisation, error) {
	var d organisationDoc
	err := r.store.col(colOrganisations).FindOne(ctx, bson.D{{Key: "_id", Value: mustOID(id)}, {Key: "projectId", Value: mustOID(projectID)}}).Decode(&d)
	if errors.Is(err, mongo.ErrNoDocuments) {
		return organisations.Organisation{}, organisations.ErrNotFound
	}
	if err != nil {
		return organisations.Organisation{}, err
	}
	return d.toDomain(), nil
}

// Create inserts an organisation.
func (r *OrganisationsRepository) Create(ctx context.Context, o organisations.Organisation, ev audit.Event) (organisations.Organisation, error) {
	doc := organisationDoc{Id: bson.NewObjectID(), WorkspaceId: mustOID(o.WorkspaceId), ProjectId: mustOID(o.ProjectId), Name: o.Name,
		NameNormalized: organisations.NormalizeName(o.Name), ExternalRef: o.ExternalRef, Active: true, CreatedAt: o.CreatedAt, UpdatedAt: o.UpdatedAt, Version: 1}
	err := r.store.transaction(ctx, func(ctx context.Context) error {
		if _, err := r.store.col(colOrganisations).InsertOne(ctx, doc); err != nil {
			return dupErr(err)
		}
		ev.Target = "organisation/" + doc.Id.Hex()
		return r.store.insertAudit(ctx, doc.WorkspaceId, ev)
	})
	if err != nil {
		return organisations.Organisation{}, err
	}
	return doc.toDomain(), nil
}

// Update changes name and external reference at the expected version.
func (r *OrganisationsRepository) Update(ctx context.Context, o organisations.Organisation, expectedVersion int64, ev audit.Event) (organisations.Organisation, error) {
	var out organisationDoc
	set := bson.D{{Key: "active", Value: o.Active}, {Key: "name", Value: o.Name}, {Key: "nameNormalized", Value: organisations.NormalizeName(o.Name)}, {Key: "updatedAt", Value: o.UpdatedAt}}
	update := bson.D{{Key: "$inc", Value: bson.D{{Key: "version", Value: int64(1)}}}}
	if o.ExternalRef != "" {
		set = append(set, bson.E{Key: "externalRef", Value: o.ExternalRef})
	} else {
		update = append(update, bson.E{Key: "$unset", Value: bson.D{{Key: "externalRef", Value: ""}}})
	}
	update = append(update, bson.E{Key: "$set", Value: set})
	err := r.store.transaction(ctx, func(ctx context.Context) error {
		err := r.store.col(colOrganisations).FindOneAndUpdate(ctx,
			bson.D{{Key: "_id", Value: mustOID(o.Id)}, {Key: "projectId", Value: mustOID(o.ProjectId)}, {Key: "version", Value: expectedVersion}},
			update, options.FindOneAndUpdate().SetReturnDocument(options.After)).Decode(&out)
		if errors.Is(err, mongo.ErrNoDocuments) {
			return organisations.ErrConflict
		}
		if err != nil {
			return dupErr(err)
		}
		return r.store.insertAudit(ctx, out.WorkspaceId, ev)
	})
	if err != nil {
		return organisations.Organisation{}, err
	}
	return out.toDomain(), nil
}

// Delete removes an organisation unless a contact or ticket references it.
// Linking writes the organisation document in its own transaction, so a
// concurrent link and delete conflict and the loser re-checks.
func (r *OrganisationsRepository) Delete(ctx context.Context, o organisations.Organisation, expectedVersion int64, ev audit.Event) error {
	return r.store.transaction(ctx, func(ctx context.Context) error {
		res, err := r.store.col(colOrganisations).DeleteOne(ctx,
			bson.D{{Key: "_id", Value: mustOID(o.Id)}, {Key: "projectId", Value: mustOID(o.ProjectId)}, {Key: "version", Value: expectedVersion}})
		if err != nil {
			return err
		}
		if res.DeletedCount == 0 {
			return organisations.ErrConflict
		}
		ref := bson.D{{Key: "projectId", Value: mustOID(o.ProjectId)}, {Key: "organisationId", Value: mustOID(o.Id)}}
		for _, col := range []string{colContacts, colTickets} {
			n, err := r.store.col(col).CountDocuments(ctx, ref, options.Count().SetLimit(1))
			if err != nil {
				return err
			}
			if n > 0 {
				return organisations.ErrInUse
			}
		}
		return r.store.insertAudit(ctx, mustOID(o.WorkspaceId), ev)
	})
}
