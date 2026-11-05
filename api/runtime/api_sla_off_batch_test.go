package runtime

import (
	"context"
	"strconv"
	"testing"

	"go.mongodb.org/mongo-driver/v2/bson"

	"github.com/arksnorman/open-kase/api/internal/jobs"
	"github.com/arksnorman/open-kase/api/internal/sla"
	transporthttp "github.com/arksnorman/open-kase/api/internal/transport/http"
	"github.com/arksnorman/open-kase/api/package/logger"
)

// The stop job works in bounded batches: each part stops at most
// sla.StopBatch cycles in one transaction and enqueues the next part.
func TestSLAOffStopsCyclesInBatches(t *testing.T) {
	f := newSLAFixture(t)
	// More tickets than the per-minute create limit; rate limits are
	// covered by TestRateLimits.
	off := map[string]transporthttp.RateLimit{}
	for class := range transporthttp.DefaultRateLimits {
		off[class] = transporthttp.RateLimit{PerMinute: -1}
	}
	r, err := transporthttp.NewRouter(transporthttp.Deps{Logger: logger.Discard(), Readiness: transporthttp.NewReadiness(logger.Discard()),
		Services: f.c.httpServices(), RateLimits: off})
	if err != nil {
		t.Fatal(err)
	}
	f.h = r
	f.putPolicy()
	total := sla.StopBatch + 1
	for range total {
		f.createTicket(f.integration, map[string]any{})
	}
	f.setPolicyActive(false, 1)
	f.runDue()
	ctx := context.Background()
	if n, _ := f.coll("slaInstances").CountDocuments(ctx, bson.D{{Key: "current", Value: true}}); n != 0 {
		t.Fatalf("%d cycles still current", n)
	}
	if n, _ := f.coll("slaInstances").CountDocuments(ctx, bson.D{{Key: "closedReason", Value: "slaOff"}}); n != int64(total) {
		t.Fatalf("%d cycles stopped, want %d", n, total)
	}
	if n, _ := f.coll("tickets").CountDocuments(ctx, bson.D{{Key: "$or", Value: bson.A{
		bson.D{{Key: "slaDueAt", Value: bson.D{{Key: "$ne", Value: nil}}}}, bson.D{{Key: "slaWarnAt", Value: bson.D{{Key: "$ne", Value: nil}}}}}}}); n != 0 {
		t.Fatalf("%d tickets keep a due-soon projection", n)
	}
	parts, err := f.c.Jobs.List(ctx, jobs.Filter{Kind: sla.KindStop, Limit: 10})
	if err != nil || len(parts) != 2 {
		t.Fatalf("stop jobs %d (%v), want 2 parts", len(parts), err)
	}
	for _, j := range parts {
		if j.State != jobs.StateSucceeded {
			t.Fatalf("stop job %s is %s", j.Key, j.State)
		}
	}
	n, _ := f.coll("auditEvents").CountDocuments(ctx, bson.D{{Key: "action", Value: "sla.cyclesStopped"}})
	m, _ := f.coll("auditEvents").CountDocuments(ctx, bson.D{{Key: "action", Value: "sla.cyclesStopped"},
		{Key: "changes", Value: "stopped:" + strconv.Itoa(total)}})
	if n != 1 || m != 1 {
		t.Fatalf("audit: %d sla.cyclesStopped events, %d with the total", n, m)
	}
}
