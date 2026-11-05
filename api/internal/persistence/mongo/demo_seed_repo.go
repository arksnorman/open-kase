package mongo

import (
	"context"
	"errors"
	"strconv"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
)

// DemoSeedRepository holds the few lookups the `seed-demo` command needs
// beyond the application services. Every business write still goes through
// services (validation, audit, outbox).
type DemoSeedRepository struct{ store *Store }

// NewDemoSeedRepository returns the demo seed helper.
func NewDemoSeedRepository(s *Store) *DemoSeedRepository { return &DemoSeedRepository{store: s} }

// ErrNoBootstrapAdmin means `init` has not created the first administrator.
var ErrNoBootstrapAdmin = errors.New("no bootstrap administrator; run init first")

// BootstrapAdminID returns the first administrator created by `init`.
func (r *DemoSeedRepository) BootstrapAdminID(ctx context.Context) (string, error) {
	var ws workspaceDoc
	if err := r.store.col(colWorkspaces).FindOne(ctx, bson.D{{Key: "installKey", Value: "primary"}}).Decode(&ws); err != nil {
		return "", err
	}
	if ws.BootstrapAdminId == nil {
		return "", ErrNoBootstrapAdmin
	}
	return ws.BootstrapAdminId.Hex(), nil
}

// TicketRefBySubject returns the reference ("#1042") of a ticket with this
// exact subject in the project, or "".
func (r *DemoSeedRepository) TicketRefBySubject(ctx context.Context, projectID, subject string) (string, error) {
	var d ticketDoc
	err := r.store.col(colTickets).FindOne(ctx, bson.D{{Key: "projectId", Value: mustOID(projectID)}, {Key: "subject", Value: subject}}).Decode(&d)
	if errors.Is(err, mongo.ErrNoDocuments) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	return "#" + strconv.FormatInt(d.Number, 10), nil
}
