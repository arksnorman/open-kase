// Package reports computes basic support reports for one project: backlog
// by status, priority and group; created versus resolved tickets per day;
// first-response and resolution medians; SLA breach counts; reopen rate.
//
// Every report is a bounded live traversal of the current data, computed
// on request: it is not a stored point-in-time snapshot. Ranges are limited
// to MaxRangeDays days, duration samples to MaxSamples tickets (the result
// says when a sample was truncated), and deleted tickets are excluded.
// Reports contain counts and durations only, never message content.
package reports

import (
	"context"
	"slices"
	"time"

	"github.com/arksnorman/open-kase/api/internal/access"
	"github.com/arksnorman/open-kase/api/internal/audit"
	"github.com/arksnorman/open-kase/api/package/clock"
	"github.com/arksnorman/open-kase/api/package/fault"
	"github.com/arksnorman/open-kase/api/package/validation"
)

// Bounds.
const (
	MaxRangeDays     = 92
	DefaultRangeDays = 30
	MaxSamples       = 10_000
)

// Kind tells clients how the numbers were produced.
const Kind = "boundedLiveTraversal"

// Count is a labelled count.
type Count struct {
	Id       string
	Name     string
	Category string
	Priority int
	Count    int64
}

// Backlog is the current open work (live tickets that are not resolved or
// archived).
type Backlog struct {
	Total      int64
	ByStatus   []Count
	ByPriority []Count
	ByGroup    []Count
}

// Day is one day of the range.
type Day struct {
	Date     string
	Created  int64
	Resolved int64
}

// Durations summarises a sample of durations in seconds.
type Durations struct {
	MedianSeconds *int64
	SampleSize    int
	Truncated     bool
}

// Report is the summary report.
type Report struct {
	Kind          string
	GeneratedAt   time.Time
	From, To      string // inclusive UTC dates
	Backlog       Backlog
	Days          []Day
	FirstResponse Durations
	Resolution    Durations
	SLABreaches   int64 // first-response breaches recorded in the range
	Resolved      int64
	Reopened      int64
	ReopenRate    *float64
}

// Repository runs the bounded aggregations.
type Repository interface {
	Backlog(ctx context.Context, workspaceID, projectID string) (Backlog, error)
	CreatedPerDay(ctx context.Context, projectID string, from, to time.Time) (map[string]int64, error)
	EventsPerDay(ctx context.Context, projectID string, action audit.Action, from, to time.Time) (map[string]int64, error)
	// FirstResponseSeconds samples tickets created in the range;
	// ResolutionSeconds tickets resolved in the range.
	FirstResponseSeconds(ctx context.Context, projectID string, from, to time.Time, limit int) ([]int64, bool, error)
	ResolutionSeconds(ctx context.Context, projectID string, from, to time.Time, limit int) ([]int64, bool, error)
	// SLABreaches counts first-response breaches recorded in the range.
	SLABreaches(ctx context.Context, projectID string, from, to time.Time) (int64, error)
}

// Service builds reports.
type Service struct {
	repo  Repository
	clock clock.Clock
}

// NewService returns the reports service.
func NewService(repo Repository, clk clock.Clock) *Service { return &Service{repo: repo, clock: clk} }

const day = 24 * time.Hour

// Summary returns the report for [from, to] (UTC dates; empty values
// default to the last 30 days). admins
// only: the report covers every group of the project.
func (s *Service) Summary(ctx context.Context, scope *access.ProjectScope, fromS, toS string) (Report, error) {
	if err := scope.Principal.RequireUser(); err != nil {
		return Report{}, err
	}
	if err := scope.Require(access.CapGroupReadAll); err != nil {
		return Report{}, err
	}
	now := s.clock.Now()
	to := now.UTC().Truncate(day)
	from := to.Add(-(DefaultRangeDays - 1) * day)
	var v validation.Collector
	if toS != "" {
		v.String(validation.ReportDate, "to", toS)
	}
	if fromS != "" {
		v.String(validation.ReportDate, "from", fromS)
	}
	if err := v.Err(); err != nil {
		return Report{}, err
	}
	var err error
	if toS != "" {
		if to, err = time.Parse("2006-01-02", toS); err != nil {
			return Report{}, fault.Field("to", validation.CodeInvalidFormat, "Use a valid date.")
		}
		if fromS == "" {
			from = to.Add(-(DefaultRangeDays - 1) * day)
		}
	}
	if fromS != "" {
		if from, err = time.Parse("2006-01-02", fromS); err != nil {
			return Report{}, fault.Field("from", validation.CodeInvalidFormat, "Use a valid date.")
		}
	}
	if to.Before(from) {
		return Report{}, fault.Field("to", validation.CodeOutOfRange, "The end date must not be before the start date.")
	}
	if to.Sub(from) >= MaxRangeDays*day {
		return Report{}, fault.Field("from", validation.CodeOutOfRange, "Use a range of at most 92 days.")
	}
	end := to.Add(day) // exclusive
	pid := scope.Project.Id

	r := Report{Kind: Kind, GeneratedAt: now, From: from.Format("2006-01-02"), To: to.Format("2006-01-02")}
	if r.Backlog, err = s.repo.Backlog(ctx, scope.Principal.WorkspaceId, pid); err != nil {
		return Report{}, err
	}
	created, err := s.repo.CreatedPerDay(ctx, pid, from, end)
	if err != nil {
		return Report{}, err
	}
	resolved, err := s.repo.EventsPerDay(ctx, pid, audit.TicketResolved, from, end)
	if err != nil {
		return Report{}, err
	}
	reopened, err := s.repo.EventsPerDay(ctx, pid, audit.TicketReopened, from, end)
	if err != nil {
		return Report{}, err
	}
	for d := from; d.Before(end); d = d.Add(day) {
		key := d.Format("2006-01-02")
		r.Days = append(r.Days, Day{Date: key, Created: created[key], Resolved: resolved[key]})
		r.Resolved += resolved[key]
		r.Reopened += reopened[key]
	}
	if r.Resolved > 0 {
		rate := float64(r.Reopened) / float64(r.Resolved)
		r.ReopenRate = &rate
	}
	fr, trunc, err := s.repo.FirstResponseSeconds(ctx, pid, from, end, MaxSamples)
	if err != nil {
		return Report{}, err
	}
	r.FirstResponse = summarize(fr, trunc)
	res, trunc, err := s.repo.ResolutionSeconds(ctx, pid, from, end, MaxSamples)
	if err != nil {
		return Report{}, err
	}
	r.Resolution = summarize(res, trunc)
	if r.SLABreaches, err = s.repo.SLABreaches(ctx, pid, from, end); err != nil {
		return Report{}, err
	}
	return r, nil
}

func summarize(xs []int64, truncated bool) Durations {
	d := Durations{SampleSize: len(xs), Truncated: truncated}
	if len(xs) == 0 {
		return d
	}
	s := slices.Clone(xs)
	slices.Sort(s)
	var m int64
	if n := len(s); n%2 == 1 {
		m = s[n/2]
	} else {
		m = (s[n/2-1] + s[n/2]) / 2
	}
	d.MedianSeconds = &m
	return d
}
