// Package mongo implements repositories, the database schema and database
// health checks on MongoDB. MongoDB must run as a replica set (a single-node
// replica set is fine) because business mutations use transactions.
package mongo

import (
	"context"
	"errors"
	"fmt"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
	"go.mongodb.org/mongo-driver/v2/mongo/readconcern"
	"go.mongodb.org/mongo-driver/v2/mongo/readpref"
	"go.mongodb.org/mongo-driver/v2/mongo/writeconcern"
	"go.mongodb.org/mongo-driver/v2/x/mongo/driver/connstring"

	"github.com/arksnorman/open-kase/api/package/product"
)

// Collection names. schema.go creates them with their validators and
// indexes.
const (
	colWorkspaces         = "workspaces"
	colSettings           = "settings"
	colUsers              = "users"
	colUserMFA            = "userMfa"
	colMFAChallenges      = "mfaChallenges"
	colAccountTokens      = "accountTokens"
	colSessions           = "sessions"
	colRefreshTokens      = "refreshTokens"
	colSigningKeys        = "signingKeys"
	colLoginThrottle      = "loginThrottle"
	colServicePrincipals  = "servicePrincipals"
	colAPICredentials     = "apiCredentials"
	colProjects           = "projects"
	colGroups             = "groups"
	colGroupMemberships   = "groupMemberships"
	colStatuses           = "statuses"
	colRequestTypes       = "requestTypes"
	colContacts           = "contacts"
	colIdempotency        = "idempotencyRecords"
	colTickets            = "tickets"
	colMessages           = "messages"
	colDrafts             = "drafts"
	colScheduledMessages  = "scheduledMessages"
	colWatchers           = "ticketWatchers"
	colTicketLinks        = "ticketLinks"
	colTags               = "tags"
	colAttachments        = "attachments"
	colCounters           = "counters"
	colOutbox             = "outbox"
	colJobs               = "jobs"
	colDeliveries         = "notificationDeliveries"
	colWebhookEndpoints   = "webhookEndpoints"
	colSLAPolicies        = "slaPolicies"
	colSLAInstances       = "slaInstances"
	colAutomationPolicies = "automationPolicies"
	colMacros             = "macros"
	colHoldReasons        = "holdReasons"
	colOrganisations      = "organisations"
	colAuditEvents        = "auditEvents"
	// attachmentBucket is the GridFS bucket for attachment bytes
	// (collections attachmentBytes.files / attachmentBytes.chunks).
	attachmentBucket = "attachmentBytes"
)

// ErrNotReplicaSet means the server cannot run multi-document transactions.
var ErrNotReplicaSet = errors.New("mongodb is not running as a replica set; transactions require one (a single-node replica set is supported)")

// Store owns the MongoDB client and database handle.
type Store struct {
	client *mongo.Client
	db     *mongo.Database
}

// Connect creates a client for uri. The database name comes from the URI
// path, defaulting to product.DefaultDatabase. Connect does not block on
// server availability; use Ping or ReplicaSet to check.
func Connect(uri string) (*Store, error) {
	cs, err := connstring.Parse(uri)
	if err != nil {
		return nil, fmt.Errorf("invalid MongoDB URI: %w", redactURIError(err))
	}
	name := cs.Database
	if name == "" {
		name = product.DefaultDatabase
	}
	opts := options.Client().ApplyURI(uri).
		SetAppName(product.Slug).
		SetWriteConcern(writeconcern.Majority()).
		SetReadConcern(readconcern.Majority()).
		SetServerSelectionTimeout(5 * time.Second)
	client, err := mongo.Connect(opts)
	if err != nil {
		return nil, fmt.Errorf("mongodb client: %w", err)
	}
	return &Store{client: client, db: client.Database(name)}, nil
}

// redactURIError avoids echoing a URI that may contain credentials.
func redactURIError(error) error { return errors.New("the URI could not be parsed") }

// Database returns the database name in use.
func (s *Store) Database() string { return s.db.Name() }

// Close disconnects the client.
func (s *Store) Close(ctx context.Context) error { return s.client.Disconnect(ctx) }

// Ping checks that a primary is reachable.
func (s *Store) Ping(ctx context.Context) error { return s.client.Ping(ctx, readpref.Primary()) }

// ReplicaSetInfo summarises the connected deployment.
type ReplicaSetInfo struct {
	Name            string
	Members         int
	WritablePrimary bool
	ServerVersion   string
}

// ReplicaSet reports replica-set membership. It returns ErrNotReplicaSet for
// a standalone server.
func (s *Store) ReplicaSet(ctx context.Context) (ReplicaSetInfo, error) {
	var hello struct {
		SetName           string   `bson:"setName"`
		Hosts             []string `bson:"hosts"`
		Passives          []string `bson:"passives"`
		Arbiters          []string `bson:"arbiters"`
		IsWritablePrimary bool     `bson:"isWritablePrimary"`
	}
	if err := s.client.Database("admin").RunCommand(ctx, bson.D{{Key: "hello", Value: 1}}).Decode(&hello); err != nil {
		return ReplicaSetInfo{}, fmt.Errorf("mongodb hello: %w", err)
	}
	info := ReplicaSetInfo{
		Name:            hello.SetName,
		Members:         len(hello.Hosts) + len(hello.Passives) + len(hello.Arbiters),
		WritablePrimary: hello.IsWritablePrimary,
	}
	var build struct {
		Version string `bson:"version"`
	}
	if err := s.client.Database("admin").RunCommand(ctx, bson.D{{Key: "buildInfo", Value: 1}}).Decode(&build); err == nil {
		info.ServerVersion = build.Version
	}
	if info.Name == "" {
		return info, ErrNotReplicaSet
	}
	if !info.WritablePrimary {
		return info, fmt.Errorf("mongodb replica set %q has no writable primary reachable", info.Name)
	}
	return info, nil
}

// transaction runs fn in a majority-committed transaction, retrying
// transient errors as the driver allows. fn must not perform external side
// effects because it may run more than once.
//
// Called inside another transaction (ctx carries its session), fn joins
// that transaction instead of starting an independent one, so composed
// operations — e.g. an automation action inside a job's fenced completion —
// commit or abort together; the outer transaction handles retries.
func (s *Store) transaction(ctx context.Context, fn func(ctx context.Context) error) error {
	if sess := mongo.SessionFromContext(ctx); sess != nil {
		return fn(ctx)
	}
	sess, err := s.client.StartSession()
	if err != nil {
		return err
	}
	defer sess.EndSession(context.WithoutCancel(ctx))
	txOpts := options.Transaction().
		SetReadConcern(readconcern.Snapshot()).
		SetWriteConcern(writeconcern.Majority())
	_, err = sess.WithTransaction(ctx, func(ctx context.Context) (any, error) {
		return nil, fn(ctx)
	}, txOpts)
	return err
}

func (s *Store) col(name string) *mongo.Collection { return s.db.Collection(name) }
