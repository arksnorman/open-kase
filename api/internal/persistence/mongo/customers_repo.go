package mongo

import (
	"context"
	"errors"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"

	"github.com/arksnorman/open-kase/api/internal/audit"
	"github.com/arksnorman/open-kase/api/internal/customers"
	"github.com/arksnorman/open-kase/api/package/pagination"
)

type contactDoc struct {
	Id              bson.ObjectID  `bson:"_id"`
	WorkspaceId     bson.ObjectID  `bson:"workspaceId"`
	ProjectId       bson.ObjectID  `bson:"projectId"`
	Name            string         `bson:"name"`
	Email           string         `bson:"email"`
	EmailNormalized string         `bson:"emailNormalized"`
	ExternalId      string         `bson:"externalId,omitempty"`
	OrganisationId  *bson.ObjectID `bson:"organisationId,omitempty"`
	CreatedAt       time.Time      `bson:"createdAt"`
	UpdatedAt       time.Time      `bson:"updatedAt"`
	Version         int64          `bson:"version"`
}

// CustomersRepository implements customers.Repository.
type CustomersRepository struct{ store *Store }

// NewCustomersRepository returns a customers repository.
func NewCustomersRepository(s *Store) *CustomersRepository { return &CustomersRepository{store: s} }

func (d contactDoc) toDomain() customers.Contact {
	return customers.Contact{Id: d.Id.Hex(), WorkspaceId: d.WorkspaceId.Hex(), ProjectId: d.ProjectId.Hex(), Name: d.Name, Email: d.Email,
		EmailNormalized: d.EmailNormalized, ExternalId: d.ExternalId, OrganisationId: hexOrEmpty(d.OrganisationId), CreatedAt: d.CreatedAt, UpdatedAt: d.UpdatedAt, Version: d.Version}
}

// FindContact matches a project contact by external ID first, then by
// normalized email. A pair identifying two contacts is a conflict.
func (r *CustomersRepository) FindContact(ctx context.Context, projectID, externalID, emailNormalized string) (customers.Contact, error) {
	pid := mustOID(projectID)
	col := r.store.col(colContacts)
	var byExt, byEmail contactDoc
	extErr := mongo.ErrNoDocuments
	if externalID != "" {
		extErr = col.FindOne(ctx, bson.D{{Key: "projectId", Value: pid}, {Key: "externalId", Value: externalID}}).Decode(&byExt)
		if extErr != nil && !errors.Is(extErr, mongo.ErrNoDocuments) {
			return customers.Contact{}, extErr
		}
	}
	emailErr := col.FindOne(ctx, bson.D{{Key: "projectId", Value: pid}, {Key: "emailNormalized", Value: emailNormalized}}).Decode(&byEmail)
	if emailErr != nil && !errors.Is(emailErr, mongo.ErrNoDocuments) {
		return customers.Contact{}, emailErr
	}
	switch {
	case extErr == nil && emailErr == nil && byExt.Id != byEmail.Id:
		return customers.Contact{}, customers.ErrContactConflict
	case extErr == nil:
		return byExt.toDomain(), nil
	case emailErr == nil:
		if externalID != "" && byEmail.ExternalId != "" && byEmail.ExternalId != externalID {
			return customers.Contact{}, customers.ErrContactConflict
		}
		return byEmail.toDomain(), nil
	}
	return customers.Contact{}, customers.ErrNotFound
}

// UpsertContact matches an existing project contact by external ID first,
// then by normalized email, and creates or updates it in one transaction.
func (r *CustomersRepository) UpsertContact(ctx context.Context, c customers.Contact, created, updated audit.Event) (customers.UpsertResult, error) {
	res, err := r.upsertContact(ctx, c, created, updated)
	if errors.Is(err, customers.ErrEmailTaken) || errors.Is(err, customers.ErrExternalIDTaken) {
		// A concurrent upsert committed the same contact first; retrying
		// matches it and updates instead of failing.
		return r.upsertContact(ctx, c, created, updated)
	}
	return res, err
}

func (r *CustomersRepository) upsertContact(ctx context.Context, c customers.Contact, created, updated audit.Event) (customers.UpsertResult, error) {
	ws, pid := mustOID(c.WorkspaceId), mustOID(c.ProjectId)
	var res customers.UpsertResult
	err := r.store.transaction(ctx, func(ctx context.Context) error {
		res = customers.UpsertResult{}
		col := r.store.col(colContacts)
		var byExt, byEmail *contactDoc
		if c.ExternalId != "" {
			var d contactDoc
			err := col.FindOne(ctx, bson.D{{Key: "projectId", Value: pid}, {Key: "externalId", Value: c.ExternalId}}).Decode(&d)
			if err == nil {
				byExt = &d
			} else if !errors.Is(err, mongo.ErrNoDocuments) {
				return err
			}
		}
		var d contactDoc
		err := col.FindOne(ctx, bson.D{{Key: "projectId", Value: pid}, {Key: "emailNormalized", Value: c.EmailNormalized}}).Decode(&d)
		if err == nil {
			byEmail = &d
		} else if !errors.Is(err, mongo.ErrNoDocuments) {
			return err
		}
		match := byExt
		if match == nil {
			match = byEmail
		} else if byEmail != nil && byEmail.Id != byExt.Id {
			return customers.ErrContactConflict
		}
		if match != nil && match.ExternalId != "" && c.ExternalId != "" && match.ExternalId != c.ExternalId {
			// Matched by email, but the stored contact has another external ID.
			return customers.ErrContactConflict
		}

		var newOrg *bson.ObjectID
		if c.OrganisationChange != nil && *c.OrganisationChange != "" {
			id, err := oid(*c.OrganisationChange)
			if err != nil {
				return customers.ErrOrganisationNotFound
			}
			ok, active, err := r.store.touchOrganisation(ctx, pid, id, c.UpdatedAt)
			if err != nil {
				return err
			}
			if !ok {
				return customers.ErrOrganisationNotFound
			}
			// An inactive organisation cannot be newly linked; keeping an existing link is fine.
			if !active && (match == nil || match.OrganisationId == nil || *match.OrganisationId != id) {
				return customers.ErrOrganisationInactive
			}
			newOrg = &id
		}

		if match == nil {
			doc := contactDoc{Id: bson.NewObjectID(), WorkspaceId: ws, ProjectId: pid, Name: c.Name, Email: c.Email,
				EmailNormalized: c.EmailNormalized, ExternalId: c.ExternalId, OrganisationId: newOrg,
				CreatedAt: c.CreatedAt, UpdatedAt: c.UpdatedAt, Version: 1}
			if _, err := col.InsertOne(ctx, doc); err != nil {
				return err
			}
			ev := created
			ev.Target, ev.Changes = "contact/"+doc.Id.Hex(), []string{"name", "email", "externalId"}
			if newOrg != nil {
				ev.Changes = append(ev.Changes, "organisationId")
			}
			res = customers.UpsertResult{Contact: doc.toDomain(), Created: true}
			return r.store.insertAudit(ctx, ws, ev)
		}

		var changes []string
		set := bson.D{}
		if c.Name != match.Name {
			set, changes = append(set, bson.E{Key: "name", Value: c.Name}), append(changes, "name")
		}
		if c.Email != match.Email {
			set = append(set, bson.E{Key: "email", Value: c.Email}, bson.E{Key: "emailNormalized", Value: c.EmailNormalized})
			changes = append(changes, "email")
		}
		if c.ExternalId != "" && c.ExternalId != match.ExternalId {
			set, changes = append(set, bson.E{Key: "externalId", Value: c.ExternalId}), append(changes, "externalId")
		}
		update := bson.D{{Key: "$inc", Value: bson.D{{Key: "version", Value: int64(1)}}}}
		if c.OrganisationChange != nil {
			switch {
			case newOrg != nil && (match.OrganisationId == nil || *match.OrganisationId != *newOrg):
				set, changes = append(set, bson.E{Key: "organisationId", Value: *newOrg}), append(changes, "organisationId")
			case newOrg == nil && match.OrganisationId != nil:
				update = append(update, bson.E{Key: "$unset", Value: bson.D{{Key: "organisationId", Value: ""}}})
				changes = append(changes, "organisationId")
			}
		}
		if len(changes) == 0 {
			res = customers.UpsertResult{Contact: (*match).toDomain()}
			return nil
		}
		set = append(set, bson.E{Key: "updatedAt", Value: c.UpdatedAt})
		update = append(update, bson.E{Key: "$set", Value: set})
		if _, err := col.UpdateOne(ctx, bson.D{{Key: "_id", Value: match.Id}}, update); err != nil {
			if isDuplicate(err, "projectEmailUnique") {
				return customers.ErrEmailTaken
			}
			return err
		}
		var doc contactDoc
		if err := col.FindOne(ctx, bson.D{{Key: "_id", Value: match.Id}}).Decode(&doc); err != nil {
			return err
		}
		ev := updated
		ev.Target, ev.Changes = "contact/"+doc.Id.Hex(), changes
		res = customers.UpsertResult{Contact: doc.toDomain()}
		return r.store.insertAudit(ctx, ws, ev)
	})
	if err != nil {
		if isDuplicate(err, "projectEmailUnique") {
			return customers.UpsertResult{}, customers.ErrEmailTaken
		}
		if isDuplicate(err, "projectExternalIdUnique") {
			return customers.UpsertResult{}, customers.ErrExternalIDTaken
		}
		return customers.UpsertResult{}, err
	}
	return res, nil
}

// GetContact returns a contact in a project.
func (r *CustomersRepository) GetContact(ctx context.Context, projectID, id string) (customers.Contact, error) {
	cid, err := oid(id)
	if err != nil {
		return customers.Contact{}, customers.ErrNotFound
	}
	var doc contactDoc
	err = r.store.col(colContacts).FindOne(ctx, bson.D{{Key: "_id", Value: cid}, {Key: "projectId", Value: mustOID(projectID)}}).Decode(&doc)
	if errors.Is(err, mongo.ErrNoDocuments) {
		return customers.Contact{}, customers.ErrNotFound
	}
	if err != nil {
		return customers.Contact{}, err
	}
	return doc.toDomain(), nil
}

// ListContacts pages through a project's contacts by normalized email.
func (r *CustomersRepository) ListContacts(ctx context.Context, projectID, prefix, externalID string, after *pagination.Cursor, limit int) ([]customers.Contact, error) {
	filter := bson.D{{Key: "projectId", Value: mustOID(projectID)}}
	if prefix != "" {
		filter = append(filter, bson.E{Key: "emailNormalized", Value: prefixRegex(prefix)})
	}
	if externalID != "" {
		filter = append(filter, bson.E{Key: "externalId", Value: externalID})
	}
	filter, err := pageFilter(filter, "emailNormalized", after)
	if err != nil {
		return nil, err
	}
	docs, err := findAll[contactDoc](ctx, r.store.col(colContacts), filter, pageOptions("emailNormalized", limit))
	if err != nil {
		return nil, err
	}
	out := make([]customers.Contact, 0, len(docs))
	for _, d := range docs {
		out = append(out, d.toDomain())
	}
	return out, nil
}

// OrganisationExists reports whether the organisation exists in the project.
func (r *CustomersRepository) OrganisationExists(ctx context.Context, projectID, id string) (bool, error) {
	oidv, err := oid(id)
	if err != nil {
		return false, nil
	}
	n, err := r.store.col(colOrganisations).CountDocuments(ctx, bson.D{{Key: "_id", Value: oidv}, {Key: "projectId", Value: mustOID(projectID)}})
	return n > 0, err
}
