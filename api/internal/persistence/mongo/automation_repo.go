package mongo

import (
	"context"
	"errors"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"

	"github.com/arksnorman/open-kase/api/internal/audit"
	"github.com/arksnorman/open-kase/api/internal/automation"
	"github.com/arksnorman/open-kase/api/internal/statuses"
)

type automationPolicyDoc struct {
	Id                       bson.ObjectID `bson:"_id"`
	WorkspaceId              bson.ObjectID `bson:"workspaceId"`
	ProjectId                bson.ObjectID `bson:"projectId"`
	ArchiveAfterResolvedDays int32         `bson:"archiveAfterResolvedDays"`
	FollowupAfterHours       int32         `bson:"customerFollowupAfterHours"`
	MaxReminders             int32         `bson:"maxReminders"`
	ReminderCooldownHours    int32         `bson:"reminderCooldownHours"`
	AutoResolveAfterHours    int32         `bson:"autoResolveAfterHours"`
	// Enabled is derived (any rule active) for the periodic scheduler.
	Enabled   bool      `bson:"enabled"`
	Version   int64     `bson:"version"`
	UpdatedAt time.Time `bson:"updatedAt"`
	UpdatedBy actorDoc  `bson:"updatedBy"`
}

func (d automationPolicyDoc) toDomain() automation.Policy {
	return automation.Policy{WorkspaceId: d.WorkspaceId.Hex(), ProjectId: d.ProjectId.Hex(),
		ArchiveAfterResolvedDays: int(d.ArchiveAfterResolvedDays), FollowupAfterHours: int(d.FollowupAfterHours),
		MaxReminders: int(d.MaxReminders), ReminderCooldownHours: int(d.ReminderCooldownHours), AutoResolveAfterHours: int(d.AutoResolveAfterHours),
		Version: d.Version, UpdatedAt: d.UpdatedAt, UpdatedBy: audit.Actor{Type: d.UpdatedBy.Type, Id: d.UpdatedBy.Id, Label: d.UpdatedBy.Label}}
}

// AutomationRepository implements automation.Repository.
type AutomationRepository struct{ store *Store }

var _ automation.Repository = (*AutomationRepository)(nil)

// NewAutomationRepository returns the automation repository.
func NewAutomationRepository(s *Store) *AutomationRepository { return &AutomationRepository{store: s} }

// GetPolicy returns the project's stored policy.
func (r *AutomationRepository) GetPolicy(ctx context.Context, projectID string) (automation.Policy, error) {
	var d automationPolicyDoc
	err := r.store.col(colAutomationPolicies).FindOne(ctx, bson.D{{Key: "projectId", Value: mustOID(projectID)}}).Decode(&d)
	if errors.Is(err, mongo.ErrNoDocuments) {
		return automation.Policy{}, automation.ErrNotFound
	}
	if err != nil {
		return automation.Policy{}, err
	}
	return d.toDomain(), nil
}

// SavePolicy upserts the policy under an optimistic version and audits it.
func (r *AutomationRepository) SavePolicy(ctx context.Context, p automation.Policy, expectedVersion int64, ev audit.Event) (automation.Policy, error) {
	ws, pid := mustOID(p.WorkspaceId), mustOID(p.ProjectId)
	doc := automationPolicyDoc{Id: bson.NewObjectID(), WorkspaceId: ws, ProjectId: pid,
		ArchiveAfterResolvedDays: int32(p.ArchiveAfterResolvedDays), FollowupAfterHours: int32(p.FollowupAfterHours),
		MaxReminders: int32(p.MaxReminders), ReminderCooldownHours: int32(p.ReminderCooldownHours), AutoResolveAfterHours: int32(p.AutoResolveAfterHours),
		Enabled: p.Enabled(), Version: expectedVersion + 1, UpdatedAt: p.UpdatedAt, UpdatedBy: toActorDoc(p.UpdatedBy)}
	err := r.store.transaction(ctx, func(ctx context.Context) error {
		col := r.store.col(colAutomationPolicies)
		if expectedVersion == 0 {
			if _, err := col.InsertOne(ctx, doc); err != nil {
				if mongo.IsDuplicateKeyError(err) {
					return automation.ErrVersionConflict
				}
				return err
			}
		} else {
			set := bson.D{{Key: "archiveAfterResolvedDays", Value: doc.ArchiveAfterResolvedDays},
				{Key: "customerFollowupAfterHours", Value: doc.FollowupAfterHours}, {Key: "maxReminders", Value: doc.MaxReminders},
				{Key: "reminderCooldownHours", Value: doc.ReminderCooldownHours}, {Key: "autoResolveAfterHours", Value: doc.AutoResolveAfterHours},
				{Key: "enabled", Value: doc.Enabled}, {Key: "version", Value: doc.Version}, {Key: "updatedAt", Value: doc.UpdatedAt},
				{Key: "updatedBy", Value: doc.UpdatedBy}}
			res, err := col.UpdateOne(ctx, bson.D{{Key: "projectId", Value: pid}, {Key: "version", Value: expectedVersion}}, bson.D{{Key: "$set", Value: set}})
			if err != nil {
				return err
			}
			if res.MatchedCount == 0 {
				return automation.ErrVersionConflict
			}
		}
		return r.store.insertAudit(ctx, ws, ev)
	})
	if err != nil {
		return automation.Policy{}, err
	}
	return doc.toDomain(), nil
}

type automationCandidateDoc struct {
	Id         bson.ObjectID  `bson:"_id"`
	Automation *automationDoc `bson:"automation"`
}

// Candidates finds live tickets for which an action may be due.
func (r *AutomationRepository) Candidates(ctx context.Context, p automation.Policy, now time.Time, limit int) ([]automation.Candidate, error) {
	pid := mustOID(p.ProjectId)
	base := func(cat statuses.Category) bson.D {
		return bson.D{{Key: "projectId", Value: pid}, {Key: "statusCategory", Value: string(cat)}}
	}
	hours := func(n int) time.Time { return now.Add(-time.Duration(n) * time.Hour) }
	type query struct {
		action string
		filter bson.D
	}
	var qs []query
	if p.ArchiveAfterResolvedDays > 0 {
		qs = append(qs, query{automation.ActionArchive, append(base(statuses.CategoryResolved),
			bson.E{Key: "resolvedAt", Value: bson.D{{Key: "$lte", Value: now.Add(-time.Duration(p.ArchiveAfterResolvedDays) * 24 * time.Hour)}}})})
	}
	if p.FollowupAfterHours > 0 {
		qs = append(qs, query{automation.ActionRemind, append(base(statuses.CategoryWaitingCustomer),
			bson.E{Key: "automation.waitingSince", Value: bson.D{{Key: "$lte", Value: hours(p.FollowupAfterHours)}}},
			bson.E{Key: "automation.reminders", Value: bson.D{{Key: "$in", Value: bson.A{int32(0), nil}}}})})
		if p.MaxReminders > 1 {
			qs = append(qs, query{automation.ActionRemind, append(base(statuses.CategoryWaitingCustomer),
				bson.E{Key: "automation.reminders", Value: bson.D{{Key: "$gte", Value: 1}, {Key: "$lt", Value: p.MaxReminders}}},
				bson.E{Key: "automation.lastReminderAt", Value: bson.D{{Key: "$lte", Value: hours(p.ReminderCooldownHours)}}})})
		}
		if p.AutoResolves() {
			// Reminders exhausted without the auto-resolve notice: one final
			// reminder carries it (automation.Due decides again).
			qs = append(qs, query{automation.ActionRemind, append(base(statuses.CategoryWaitingCustomer),
				bson.E{Key: "automation.reminders", Value: bson.D{{Key: "$gte", Value: p.MaxReminders}}},
				bson.E{Key: "automation.notice", Value: bson.D{{Key: "$ne", Value: true}}},
				bson.E{Key: "automation.lastReminderAt", Value: bson.D{{Key: "$lte", Value: hours(p.ReminderCooldownHours)}}})})
			qs = append(qs, query{automation.ActionResolve, append(base(statuses.CategoryWaitingCustomer),
				bson.E{Key: "automation.reminders", Value: bson.D{{Key: "$gte", Value: p.MaxReminders}}},
				bson.E{Key: "automation.notice", Value: true},
				bson.E{Key: "automation.lastReminderAt", Value: bson.D{{Key: "$lte", Value: hours(p.AutoResolveAfterHours)}}})})
		}
	}
	var out []automation.Candidate
	for _, q := range qs {
		docs, err := findAll[automationCandidateDoc](ctx, r.store.col(colTickets), q.filter,
			options.Find().SetProjection(bson.D{{Key: "automation", Value: 1}}).SetSort(bson.D{{Key: "_id", Value: 1}}).SetLimit(int64(limit)))
		if err != nil {
			return nil, err
		}
		for _, d := range docs {
			st := d.Automation.toDomain()
			out = append(out, automation.Candidate{TicketId: d.Id.Hex(), Generation: st.Generation, Reminders: st.Reminders, Action: q.action})
		}
	}
	return out, nil
}

// EnabledProjects lists projects whose policy has a rule enabled.
func (r *AutomationRepository) EnabledProjects(ctx context.Context) ([]automation.ProjectRef, error) {
	docs, err := findAll[automationPolicyDoc](ctx, r.store.col(colAutomationPolicies), bson.D{{Key: "enabled", Value: true}}, options.Find().SetLimit(10_000))
	if err != nil {
		return nil, err
	}
	out := make([]automation.ProjectRef, 0, len(docs))
	for _, d := range docs {
		out = append(out, automation.ProjectRef{WorkspaceId: d.WorkspaceId.Hex(), ProjectId: d.ProjectId.Hex()})
	}
	return out, nil
}
