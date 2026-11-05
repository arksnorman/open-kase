package mongo

import (
	"context"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo/options"

	"github.com/arksnorman/open-kase/api/internal/statuses"
)

// CatalogRepository implements statuses.CatalogRepository.
type CatalogRepository struct{ store *Store }

// NewCatalogRepository returns a workflow catalog repository.
func NewCatalogRepository(s *Store) *CatalogRepository { return &CatalogRepository{store: s} }

func workflowFilter(workspaceID, projectID string) bson.D {
	return bson.D{{Key: "workspaceId", Value: mustOID(workspaceID)}, {Key: "active", Value: true},
		{Key: "projectId", Value: bson.D{{Key: "$in", Value: bson.A{nil, mustOID(projectID)}}}}}
}

var workflowSort = options.Find().SetSort(bson.D{{Key: "order", Value: 1}, {Key: "_id", Value: 1}}).SetLimit(500)

// ProjectStatuses returns built-in and project statuses in display order.
func (r *CatalogRepository) ProjectStatuses(ctx context.Context, workspaceID, projectID string) ([]statuses.Status, error) {
	type doc struct {
		Id        bson.ObjectID `bson:"_id"`
		Code      string        `bson:"code"`
		Label     string        `bson:"label"`
		Category  string        `bson:"category"`
		Builtin   bool          `bson:"builtin"`
		Order     int           `bson:"order"`
		CreatedAt time.Time     `bson:"createdAt"`
	}
	docs, err := findAll[doc](ctx, r.store.col(colStatuses), workflowFilter(workspaceID, projectID), workflowSort)
	if err != nil {
		return nil, err
	}
	out := make([]statuses.Status, 0, len(docs))
	for _, d := range docs {
		out = append(out, statuses.Status{Id: d.Id.Hex(), Code: d.Code, Name: d.Label, Category: statuses.Category(d.Category), Builtin: d.Builtin, Order: d.Order})
	}
	return out, nil
}

// ProjectRequestTypes returns installation and project request types.
func (r *CatalogRepository) ProjectRequestTypes(ctx context.Context, workspaceID, projectID string) ([]statuses.RequestType, error) {
	type doc struct {
		Id    bson.ObjectID `bson:"_id"`
		Code  string        `bson:"code"`
		Name  string        `bson:"name"`
		Order int           `bson:"order"`
	}
	docs, err := findAll[doc](ctx, r.store.col(colRequestTypes), workflowFilter(workspaceID, projectID), workflowSort)
	if err != nil {
		return nil, err
	}
	out := make([]statuses.RequestType, 0, len(docs))
	for _, d := range docs {
		out = append(out, statuses.RequestType{Id: d.Id.Hex(), Code: d.Code, Name: d.Name, Order: d.Order})
	}
	return out, nil
}
