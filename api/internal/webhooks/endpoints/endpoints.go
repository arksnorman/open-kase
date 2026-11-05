// Package endpoints manages per-project outbound webhook endpoints and
// delivers webhook jobs.
//
// Every endpoint is represented by a scoped service principal (kind
// "webhook") bound to its project with read-only public scopes. Payloads are
// built from what that principal may read: public content only, unless an
// administrator explicitly granted includeAgentNotes, which
// adds agent-note events (with their body) and staff fields such as the
// assignee.
//
// Requests are not signed. An endpoint carries extra headers, an optional
// Authorization (Basic or Bearer token) and optional TLS settings (extra
// trusted CA certificates, a client certificate for mutual TLS). Header
// values, the password, the token and the client key are sealed with the
// master key and never returned by the API.
package endpoints

import (
	"context"
	"errors"
	"time"

	"github.com/arksnorman/open-kase/api/internal/audit"
	"github.com/arksnorman/open-kase/api/package/pagination"
)

// Authentication types.
const (
	AuthNone  = "none"
	AuthBasic = "basic"
	AuthToken = "token"
)

// Header is an extra request header; the value is sealed.
type Header struct {
	Name        string
	SealedValue string
}

// Auth is the endpoint's Authorization configuration. Secrets are sealed.
type Auth struct {
	Type           string // AuthNone, AuthBasic or AuthToken
	Username       string
	SealedPassword string
	SealedToken    string
}

// TLS holds the endpoint's certificate settings. Certificates are public
// PEM text; the client key is sealed.
type TLS struct {
	CACertificate     string
	ClientCertificate string
	SealedClientKey   string
}

// Endpoint is a project's webhook destination.
type Endpoint struct {
	Id                string
	WorkspaceId       string
	ProjectId         string
	PrincipalId       string
	Name              string
	URL               string
	EventTypes        []string
	Active            bool
	IncludeAgentNotes bool
	Headers           []Header
	Auth              Auth
	TLS               TLS
	CreatedBy         string
	CreatedAt         time.Time
	UpdatedAt         time.Time
	// PausedAt is when the endpoint was last paused (Active false); nil while active.
	PausedAt *time.Time
	Version  int64
}

// StatsWindow is the period delivery statistics cover; StripSize the number
// of recent deliveries listed for the health strip.
const (
	StatsWindow = 24 * time.Hour
	StripSize   = 10
)

// StripItem is one recent delivery of the health strip.
type StripItem struct {
	Status string // notifications delivery status
	At     time.Time
}

// Stats summarizes an endpoint's deliveries of the last StatsWindow.
type Stats struct {
	// Sent counts deliveries attempted at least once (skipped ones excluded).
	Sent int
	// FirstTry counts those delivered on the first attempt.
	FirstTry int
	// Retrying counts deliveries waiting for another attempt.
	Retrying int
	// MedianResponseMs is the median receiver response time, nil without data.
	MedianResponseMs *int
	LastSentAt       *time.Time
	// Recent lists up to StripSize deliveries, newest first.
	Recent []StripItem
}

// Repository errors.
var (
	ErrNotFound        = errors.New("endpoints: not found")
	ErrVersionConflict = errors.New("endpoints: version conflict")
)

// Repository persists endpoints and their principals.
type Repository interface {
	// NewID returns an identifier for an endpoint about to be created
	// (sealed values are bound to it).
	NewID() string
	// Create inserts the endpoint (with the ID from NewID), its service
	// principal and the audit event.
	Create(ctx context.Context, e Endpoint, principalName string, ev audit.Event) (Endpoint, error)
	List(ctx context.Context, projectID string, after *pagination.Cursor, limit int) ([]Endpoint, error)
	// Get finds an endpoint; projectID "" matches any project (workers).
	Get(ctx context.Context, projectID, id string) (Endpoint, error)
	// Update writes the endpoint with a version check and the audit event.
	// principalName, when not empty, renames the endpoint's service principal in the same transaction.
	Update(ctx context.Context, e Endpoint, principalName string, expectedVersion int64, ev audit.Event) (Endpoint, error)
	// Delete removes the endpoint and revokes its principal.
	Delete(ctx context.Context, projectID, id string, now time.Time, ev audit.Event) error
	// PrincipalActive reports whether the endpoint's principal is active.
	PrincipalActive(ctx context.Context, principalID string) (bool, error)
	// Stats summarizes the deliveries of the given endpoints since the
	// given time with one query; endpoints without deliveries are absent.
	Stats(ctx context.Context, projectID string, ids []string, since time.Time) (map[string]Stats, error)
}
