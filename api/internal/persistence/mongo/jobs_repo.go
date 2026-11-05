package mongo

import (
	"context"
	"errors"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"

	"github.com/arksnorman/open-kase/api/internal/jobs"
)

type jobAttemptDoc struct {
	Number     int32     `bson:"n"`
	Owner      string    `bson:"owner"`
	Generation int64     `bson:"generation"`
	StartedAt  time.Time `bson:"startedAt"`
	EndedAt    time.Time `bson:"endedAt"`
	Outcome    string    `bson:"outcome"`
	ErrorClass string    `bson:"errorClass,omitempty"`
	Error      string    `bson:"error,omitempty"`
}

type jobDoc struct {
	Id          bson.ObjectID     `bson:"_id"`
	WorkspaceId *bson.ObjectID    `bson:"workspaceId"`
	ProjectId   *bson.ObjectID    `bson:"projectId"`
	Kind        string            `bson:"kind"`
	Key         string            `bson:"key"`
	State       string            `bson:"state"`
	Payload     map[string]string `bson:"payload"`
	Attempts    int32             `bson:"attempts"`
	MaxAttempts int32             `bson:"maxAttempts"`
	RunAt       time.Time         `bson:"runAt"`
	LeaseOwner  string            `bson:"leaseOwner"`
	LeaseUntil  *time.Time        `bson:"leaseUntil"`
	Generation  int64             `bson:"generation"`
	LastError   string            `bson:"lastError"`
	LastClass   string            `bson:"lastErrorClass"`
	History     []jobAttemptDoc   `bson:"history"`
	CreatedAt   time.Time         `bson:"createdAt"`
	UpdatedAt   time.Time         `bson:"updatedAt"`
	CompletedAt *time.Time        `bson:"completedAt"`
}

func (d jobDoc) toDomain() jobs.Job {
	j := jobs.Job{Id: d.Id.Hex(), WorkspaceId: hexOrEmpty(d.WorkspaceId), ProjectId: hexOrEmpty(d.ProjectId), Kind: d.Kind, Key: d.Key,
		State: jobs.State(d.State), Payload: d.Payload, Attempts: int(d.Attempts), MaxAttempts: int(d.MaxAttempts), RunAt: d.RunAt,
		LeaseOwner: d.LeaseOwner, LeaseUntil: d.LeaseUntil, Generation: d.Generation, LastError: d.LastError, LastClass: d.LastClass,
		CreatedAt: d.CreatedAt, UpdatedAt: d.UpdatedAt, CompletedAt: d.CompletedAt}
	if j.Payload == nil {
		j.Payload = map[string]string{}
	}
	for _, a := range d.History {
		j.History = append(j.History, jobs.Attempt{Number: int(a.Number), Owner: a.Owner, Generation: a.Generation, StartedAt: a.StartedAt,
			EndedAt: a.EndedAt, Outcome: a.Outcome, ErrorClass: a.ErrorClass, Error: a.Error})
	}
	return j
}

// JobsRepository implements jobs.Store.
type JobsRepository struct {
	store *Store
	now   func() time.Time
}

var _ jobs.Store = (*JobsRepository)(nil)

// NewJobsRepository returns the durable job store.
func NewJobsRepository(s *Store) *JobsRepository {
	return &JobsRepository{store: s, now: func() time.Time { return time.Now().UTC().Truncate(time.Millisecond) }}
}

// SetNow replaces the clock that stamps jobs enqueued without a run time.
// It must be the clock the runner acquires with (Container.BizClock), or a
// job stamped by one clock is not due for the other.
func (r *JobsRepository) SetNow(now func() time.Time) { r.now = now }

func newJobDoc(j jobs.NewJob, now time.Time) jobDoc {
	id := bson.NewObjectID()
	if j.Id != "" {
		id = mustOID(j.Id)
	}
	maxAttempts := j.MaxAttempts
	if maxAttempts <= 0 {
		maxAttempts = jobs.DefaultMaxAttempts
	}
	runAt := j.RunAt
	if runAt.IsZero() {
		runAt = now
	}
	payload := j.Payload
	if payload == nil {
		payload = map[string]string{}
	}
	return jobDoc{Id: id, WorkspaceId: optOID(j.WorkspaceId), ProjectId: optOID(j.ProjectId), Kind: j.Kind, Key: j.Key,
		State: string(jobs.StateQueued), Payload: payload, MaxAttempts: int32(maxAttempts), RunAt: runAt,
		History: []jobAttemptDoc{}, CreatedAt: now, UpdatedAt: now}
}

// enqueueOne inserts the job unless its key exists. Inside a transaction a
// concurrent insert of the same key surfaces as a write conflict that the
// driver retries, after which the key is found and nothing is inserted.
func (r *JobsRepository) enqueueOne(ctx context.Context, j jobs.NewJob) (string, bool, error) {
	doc := newJobDoc(j, r.now())
	res, err := r.store.col(colJobs).UpdateOne(ctx, bson.D{{Key: "key", Value: j.Key}},
		bson.D{{Key: "$setOnInsert", Value: doc}}, options.UpdateOne().SetUpsert(true))
	if mongo.IsDuplicateKeyError(err) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	if res.UpsertedCount == 1 {
		return doc.Id.Hex(), true, nil
	}
	return "", false, nil
}

// Enqueue inserts jobs whose keys are new.
func (r *JobsRepository) Enqueue(ctx context.Context, js ...jobs.NewJob) (int, error) {
	n := 0
	for _, j := range js {
		_, created, err := r.enqueueOne(ctx, j)
		if err != nil {
			return n, err
		}
		if created {
			n++
		}
	}
	return n, nil
}

// Acquire leases one due job atomically and increments its fencing
// generation and attempt count.
func (r *JobsRepository) Acquire(ctx context.Context, owner string, kinds []string, now time.Time, ttl time.Duration) (jobs.Job, bool, error) {
	if len(kinds) == 0 {
		return jobs.Job{}, false, nil
	}
	ks := bson.A{}
	for _, k := range kinds {
		ks = append(ks, k)
	}
	filter := bson.D{
		{Key: "kind", Value: bson.D{{Key: "$in", Value: ks}}},
		{Key: "$or", Value: bson.A{
			bson.D{{Key: "state", Value: string(jobs.StateQueued)}, {Key: "runAt", Value: bson.D{{Key: "$lte", Value: now}}}},
			bson.D{{Key: "state", Value: string(jobs.StateRunning)}, {Key: "leaseUntil", Value: bson.D{{Key: "$lt", Value: now}}}},
		}},
	}
	until := now.Add(ttl)
	update := bson.D{
		{Key: "$set", Value: bson.D{{Key: "state", Value: string(jobs.StateRunning)}, {Key: "leaseOwner", Value: owner},
			{Key: "leaseUntil", Value: until}, {Key: "updatedAt", Value: now}}},
		{Key: "$inc", Value: bson.D{{Key: "attempts", Value: int32(1)}, {Key: "generation", Value: int64(1)}}},
	}
	var doc jobDoc
	err := r.store.col(colJobs).FindOneAndUpdate(ctx, filter, update,
		options.FindOneAndUpdate().SetSort(bson.D{{Key: "runAt", Value: 1}}).SetReturnDocument(options.After)).Decode(&doc)
	if errors.Is(err, mongo.ErrNoDocuments) {
		return jobs.Job{}, false, nil
	}
	if err != nil {
		return jobs.Job{}, false, err
	}
	return doc.toDomain(), true, nil
}

func fence(l jobs.Lease) bson.D {
	return bson.D{{Key: "_id", Value: mustOID(l.JobId)}, {Key: "state", Value: string(jobs.StateRunning)},
		{Key: "leaseOwner", Value: l.Owner}, {Key: "generation", Value: l.Generation}}
}

// Extend renews a lease.
func (r *JobsRepository) Extend(ctx context.Context, l jobs.Lease, until time.Time) error {
	res, err := r.store.col(colJobs).UpdateOne(ctx, fence(l), bson.D{{Key: "$set", Value: bson.D{{Key: "leaseUntil", Value: until}}}})
	if err != nil {
		return err
	}
	if res.MatchedCount == 0 {
		return jobs.ErrLeaseLost
	}
	return nil
}

// Fenced runs fn after proving the lease, in one transaction. Writing the
// job document makes a concurrent takeover conflict with fn's writes.
func (r *JobsRepository) Fenced(ctx context.Context, l jobs.Lease, fn func(ctx context.Context) error) error {
	return r.store.transaction(ctx, func(ctx context.Context) error {
		res, err := r.store.col(colJobs).UpdateOne(ctx, fence(l), bson.D{{Key: "$set", Value: bson.D{{Key: "updatedAt", Value: r.now()}}}})
		if err != nil {
			return err
		}
		if res.MatchedCount == 0 {
			return jobs.ErrLeaseLost
		}
		return fn(ctx)
	})
}

// Complete records the attempt and the next state if the lease is current.
func (r *JobsRepository) Complete(ctx context.Context, l jobs.Lease, f jobs.Finish, also func(ctx context.Context) error) error {
	now := r.now()
	a := f.Attempt
	set := bson.D{{Key: "state", Value: string(f.State)}, {Key: "leaseOwner", Value: ""}, {Key: "leaseUntil", Value: nil},
		{Key: "updatedAt", Value: now}, {Key: "lastError", Value: a.Error}, {Key: "lastErrorClass", Value: a.ErrorClass}}
	switch f.State {
	case jobs.StateQueued:
		set = append(set, bson.E{Key: "runAt", Value: f.RunAt})
	case jobs.StateSucceeded, jobs.StateDead:
		set = append(set, bson.E{Key: "completedAt", Value: now})
	default:
		return errors.New("jobs: invalid finish state " + string(f.State))
	}
	update := bson.D{{Key: "$set", Value: set}, {Key: "$push", Value: bson.D{{Key: "history", Value: bson.D{
		{Key: "$each", Value: bson.A{jobAttemptDoc{Number: int32(a.Number), Owner: a.Owner, Generation: a.Generation,
			StartedAt: a.StartedAt, EndedAt: a.EndedAt, Outcome: a.Outcome, ErrorClass: a.ErrorClass, Error: a.Error}}},
		{Key: "$slice", Value: -jobs.MaxHistory},
	}}}}}
	return r.store.transaction(ctx, func(ctx context.Context) error {
		res, err := r.store.col(colJobs).UpdateOne(ctx, fence(l), update)
		if err != nil {
			return err
		}
		if res.MatchedCount == 0 {
			return jobs.ErrLeaseLost
		}
		if also != nil {
			return also(ctx)
		}
		return nil
	})
}

// Get returns one job.
func (r *JobsRepository) Get(ctx context.Context, id string) (jobs.Job, error) {
	jid, err := oid(id)
	if err != nil {
		return jobs.Job{}, jobs.ErrNotFound
	}
	var doc jobDoc
	err = r.store.col(colJobs).FindOne(ctx, bson.D{{Key: "_id", Value: jid}}).Decode(&doc)
	if errors.Is(err, mongo.ErrNoDocuments) {
		return jobs.Job{}, jobs.ErrNotFound
	}
	if err != nil {
		return jobs.Job{}, err
	}
	return doc.toDomain(), nil
}

// List returns the newest jobs matching f.
func (r *JobsRepository) List(ctx context.Context, f jobs.Filter) ([]jobs.Job, error) {
	filter := bson.D{}
	if f.State != "" {
		filter = append(filter, bson.E{Key: "state", Value: string(f.State)})
	}
	if f.Kind != "" {
		filter = append(filter, bson.E{Key: "kind", Value: f.Kind})
	}
	limit := f.Limit
	if limit <= 0 || limit > 500 {
		limit = 50
	}
	docs, err := findAll[jobDoc](ctx, r.store.col(colJobs), filter, options.Find().SetSort(bson.D{{Key: "_id", Value: -1}}).SetLimit(int64(limit)))
	if err != nil {
		return nil, err
	}
	out := make([]jobs.Job, 0, len(docs))
	for _, d := range docs {
		out = append(out, d.toDomain())
	}
	return out, nil
}

// Requeue makes a dead-lettered or completed job runnable again with a
// fresh attempt budget.
func (r *JobsRepository) Requeue(ctx context.Context, id string, now time.Time, also func(ctx context.Context, j jobs.Job) error) (jobs.Job, error) {
	jid, err := oid(id)
	if err != nil {
		return jobs.Job{}, jobs.ErrNotFound
	}
	var out jobs.Job
	err = r.store.transaction(ctx, func(ctx context.Context) error {
		var cur jobDoc
		err := r.store.col(colJobs).FindOne(ctx, bson.D{{Key: "_id", Value: jid}}).Decode(&cur)
		if errors.Is(err, mongo.ErrNoDocuments) {
			return jobs.ErrNotFound
		}
		if err != nil {
			return err
		}
		if cur.State != string(jobs.StateDead) && cur.State != string(jobs.StateSucceeded) {
			return jobs.ErrNotRetryable
		}
		var doc jobDoc
		err = r.store.col(colJobs).FindOneAndUpdate(ctx, bson.D{{Key: "_id", Value: jid}, {Key: "state", Value: cur.State}},
			bson.D{{Key: "$set", Value: bson.D{{Key: "state", Value: string(jobs.StateQueued)}, {Key: "runAt", Value: now},
				{Key: "maxAttempts", Value: cur.Attempts + int32(jobs.DefaultMaxAttempts)}, {Key: "completedAt", Value: nil},
				{Key: "updatedAt", Value: now}}}},
			options.FindOneAndUpdate().SetReturnDocument(options.After)).Decode(&doc)
		if err != nil {
			return err
		}
		out = doc.toDomain()
		if also != nil {
			return also(ctx, out)
		}
		return nil
	})
	return out, err
}
