package mongo

import (
	"context"
	"slices"
	"strings"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo/options"

	"github.com/arksnorman/open-kase/api/internal/tickets"
)

// subjectWeight ranks a subject match above an equally scored message match.
const subjectWeight = 2.0

type searchCand struct {
	score    float64
	subject  bool
	msgID    bson.ObjectID
	vis      string
	msgScore float64
}

// Search runs the full-text query against the project's partition of the
// ticket subject index and, separately per allowed audience, the message
// body index (whose equality prefix is project + visibility, so an audience
// that is not listed is never read). The caller's ticket access filter
// (live tickets only) applies before the candidate bounds: it is part of
// the subject query, and when the cheap message pass reaches its bound for
// a caller restricted to some groups, the message matches are grouped by
// ticket and joined with the filter before the bound. Tickets the caller
// cannot read therefore neither push its own tickets out of the bounded
// candidates nor show in total or total_capped. The candidates are finally
// intersected with the same filter in one query.
func (r *TicketsRepository) Search(ctx context.Context, q tickets.SearchQuery) ([]tickets.SearchCandidate, bool, error) {
	pid := mustOID(q.Base.ProjectId)
	limit := int64(q.MaxCandidates)
	capped := false
	cands := map[bson.ObjectID]*searchCand{}
	text := bson.D{{Key: "$search", Value: q.Text}}
	base := q.Base
	base.Search, base.After = tickets.Search{}, nil
	access, err := r.filter(ctx, base)
	if err != nil {
		return nil, false, err
	}

	var subj []struct {
		Id    bson.ObjectID `bson:"_id"`
		Score float64       `bson:"score"`
	}
	cur, err := r.store.col(colTickets).Find(ctx, append(bson.D{{Key: "$text", Value: text}}, access...),
		options.Find().SetProjection(bson.D{{Key: "_id", Value: 1}, {Key: "score", Value: bson.D{{Key: "$meta", Value: "textScore"}}}}).
			SetSort(bson.D{{Key: "score", Value: bson.D{{Key: "$meta", Value: "textScore"}}}}).SetLimit(limit))
	if err != nil {
		return nil, false, err
	}
	if err := cur.All(ctx, &subj); err != nil {
		return nil, false, err
	}
	if int64(len(subj)) == limit {
		capped = true
	}
	for _, s := range subj {
		cands[s.Id] = &searchCand{score: s.Score * subjectWeight, subject: true}
	}

	for _, aud := range q.Audiences {
		rows, hit, err := r.messageMatches(ctx, pid, aud, text, limit, nil)
		if err != nil {
			return nil, false, err
		}
		if hit && !q.Base.Access.All {
			// The cheap pass was cut by its bound and may have kept only
			// tickets the caller cannot read: bound readable tickets instead.
			if rows, hit, err = r.messageMatches(ctx, pid, aud, text, limit, access); err != nil {
				return nil, false, err
			}
		}
		capped = capped || hit
		for _, row := range rows {
			c := cands[row.TicketId]
			if c == nil {
				c = &searchCand{}
				cands[row.TicketId] = c
			}
			if row.Score > c.msgScore {
				c.msgScore, c.msgID, c.vis = row.Score, row.Best, aud
			}
			c.score = max(c.score, row.Score)
		}
	}
	if len(cands) == 0 {
		return []tickets.SearchCandidate{}, capped, nil
	}

	ids := make(bson.A, 0, len(cands))
	for id := range cands {
		ids = append(ids, id)
	}
	f := append(slices.Clone(access), bson.E{Key: "_id", Value: bson.D{{Key: "$in", Value: ids}}})
	allowed, err := findAll[struct {
		Id bson.ObjectID `bson:"_id"`
	}](ctx, r.store.col(colTickets), f, options.Find().SetProjection(bson.D{{Key: "_id", Value: 1}}))
	if err != nil {
		return nil, false, err
	}
	out := make([]tickets.SearchCandidate, 0, len(allowed))
	for _, a := range allowed {
		c := cands[a.Id]
		sc := tickets.SearchCandidate{TicketId: a.Id.Hex(), Score: c.score, SubjectMatched: c.subject}
		if !c.msgID.IsZero() {
			sc.MessageId, sc.Visibility = c.msgID.Hex(), c.vis
		}
		out = append(out, sc)
	}
	slices.SortFunc(out, func(a, b tickets.SearchCandidate) int {
		switch {
		case a.Score > b.Score:
			return -1
		case a.Score < b.Score:
			return 1
		}
		return -strings.Compare(a.TicketId, b.TicketId)
	})
	return out, capped, nil
}

// messageMatch is the best matching message of one ticket in one audience.
type messageMatch struct {
	TicketId bson.ObjectID `bson:"_id"`
	Score    float64       `bson:"score"`
	Best     bson.ObjectID `bson:"best"`
	N        int64         `bson:"n"`
}

// messageMatches groups the audience's matching messages by ticket. With a
// nil access filter it considers the 5 × limit best messages (cheap; hit
// reports that the bound was reached). With an access filter it groups
// every match, keeps the tickets the filter admits and returns the limit
// best of them (hit: more readable tickets matched).
func (r *TicketsRepository) messageMatches(ctx context.Context, pid bson.ObjectID, aud string, text bson.D, limit int64, access bson.D) ([]messageMatch, bool, error) {
	pipeline := bson.A{
		bson.D{{Key: "$match", Value: bson.D{{Key: "projectId", Value: pid}, {Key: "visibility", Value: aud}, {Key: "$text", Value: text}}}},
		bson.D{{Key: "$addFields", Value: bson.D{{Key: "score", Value: bson.D{{Key: "$meta", Value: "textScore"}}}}}},
	}
	if access == nil {
		pipeline = append(pipeline,
			bson.D{{Key: "$sort", Value: bson.D{{Key: "score", Value: -1}, {Key: "_id", Value: -1}}}},
			bson.D{{Key: "$limit", Value: limit * 5}},
			bson.D{{Key: "$group", Value: bson.D{{Key: "_id", Value: "$ticketId"}, {Key: "score", Value: bson.D{{Key: "$max", Value: "$score"}}},
				{Key: "best", Value: bson.D{{Key: "$first", Value: "$_id"}}}, {Key: "n", Value: bson.D{{Key: "$sum", Value: 1}}}}}})
	} else {
		pipeline = append(pipeline,
			bson.D{{Key: "$group", Value: bson.D{{Key: "_id", Value: "$ticketId"}, {Key: "score", Value: bson.D{{Key: "$max", Value: "$score"}}},
				{Key: "best", Value: bson.D{{Key: "$top", Value: bson.D{{Key: "sortBy", Value: bson.D{{Key: "score", Value: -1}, {Key: "_id", Value: -1}}},
					{Key: "output", Value: "$_id"}}}}}, {Key: "n", Value: bson.D{{Key: "$sum", Value: 1}}}}}},
			bson.D{{Key: "$lookup", Value: bson.D{{Key: "from", Value: colTickets}, {Key: "localField", Value: "_id"}, {Key: "foreignField", Value: "_id"},
				{Key: "pipeline", Value: bson.A{bson.D{{Key: "$match", Value: access}}, bson.D{{Key: "$project", Value: bson.D{{Key: "_id", Value: 1}}}}}},
				{Key: "as", Value: "readable"}}}},
			bson.D{{Key: "$match", Value: bson.D{{Key: "readable.0", Value: bson.D{{Key: "$exists", Value: true}}}}}},
			bson.D{{Key: "$sort", Value: bson.D{{Key: "score", Value: -1}, {Key: "_id", Value: -1}}}},
			bson.D{{Key: "$limit", Value: limit + 1}},
			bson.D{{Key: "$project", Value: bson.D{{Key: "readable", Value: 0}}}})
	}
	cur, err := r.store.col(colMessages).Aggregate(ctx, pipeline)
	if err != nil {
		return nil, false, err
	}
	var rows []messageMatch
	if err := cur.All(ctx, &rows); err != nil {
		return nil, false, err
	}
	if access != nil {
		if int64(len(rows)) > limit {
			return rows[:limit], true, nil
		}
		return rows, false, nil
	}
	var total int64
	for _, row := range rows {
		total += row.N
	}
	return rows, total >= limit*5, nil
}

// ListByIDs returns the project's tickets with the given IDs, in the order
// of ids (missing IDs are skipped).
func (r *TicketsRepository) ListByIDs(ctx context.Context, projectID string, ids []string, viewerID string) ([]tickets.Ticket, error) {
	if len(ids) == 0 {
		return []tickets.Ticket{}, nil
	}
	docs, err := findAll[ticketDoc](ctx, r.store.col(colTickets),
		bson.D{{Key: "projectId", Value: mustOID(projectID)}, {Key: "_id", Value: bson.D{{Key: "$in", Value: oidList(ids)}}}}, options.Find())
	if err != nil {
		return nil, err
	}
	rows, err := r.hydrate(ctx, docs, viewerID)
	if err != nil {
		return nil, err
	}
	pos := make(map[string]int, len(ids))
	for i, id := range ids {
		pos[id] = i
	}
	slices.SortFunc(rows, func(a, b tickets.Ticket) int { return pos[a.Id] - pos[b.Id] })
	return rows, nil
}

// MessageBodies returns messages of the project by ID, without attachments
// or author names.
func (r *TicketsRepository) MessageBodies(ctx context.Context, projectID string, ids []string) (map[string]tickets.Message, error) {
	docs, err := findAll[messageDoc](ctx, r.store.col(colMessages),
		bson.D{{Key: "projectId", Value: mustOID(projectID)}, {Key: "_id", Value: bson.D{{Key: "$in", Value: oidList(ids)}}}}, options.Find())
	if err != nil {
		return nil, err
	}
	out := make(map[string]tickets.Message, len(docs))
	for _, d := range docs {
		out[d.Id.Hex()] = tickets.Message{Id: d.Id.Hex(), TicketId: d.TicketId.Hex(), ProjectId: d.ProjectId.Hex(),
			Visibility: d.Visibility, Body: d.Body, Seq: d.Seq, CreatedAt: d.CreatedAt}
	}
	return out, nil
}
