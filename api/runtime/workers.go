package runtime

import (
	"context"
	"os"
	"strconv"
	"sync"
	"time"

	"github.com/redis/go-redis/v9"
	"go.mongodb.org/mongo-driver/v2/bson"

	"github.com/arksnorman/open-kase/api/internal/automation"
	"github.com/arksnorman/open-kase/api/internal/jobs"
	"github.com/arksnorman/open-kase/api/internal/mailer"
	"github.com/arksnorman/open-kase/api/internal/notifications"
	"github.com/arksnorman/open-kase/api/internal/sla"
	"github.com/arksnorman/open-kase/api/internal/tickets"
	"github.com/arksnorman/open-kase/api/internal/webhooks"
	"github.com/arksnorman/open-kase/api/internal/webhooks/endpoints"
	"github.com/arksnorman/open-kase/api/package/product"
)

// workerOwner identifies this process in job leases.
func workerOwner(role string) string {
	host, _ := os.Hostname()
	return role + "/" + host + "/" + strconv.Itoa(os.Getpid()) + "/" + bson.NewObjectID().Hex()[16:]
}

// webhookPolicy is the destination policy from APP_ENV: plain HTTP and
// loopback receivers only in development.
func (c *Container) webhookPolicy() webhooks.Policy {
	dev := c.Boot.Env == EnvDevelopment
	return webhooks.Policy{AllowHTTP: dev, AllowLoopback: dev}
}

// newRunner builds a job runner with every handler registered.
func (c *Container) newRunner(owner string, concurrency int) *jobs.Runner {
	r := jobs.NewRunner(c.Jobs, jobs.Config{Owner: owner, Concurrency: concurrency, Log: c.Log, Waker: c.Waker,
		Backoff: c.JobBackoff, LeaseTTL: c.JobLeaseTTL, Grace: c.JobGrace, Now: c.BizClock.Now})
	r.Handle(notifications.KindEmail, c.Email.HandleNotification)
	r.Handle(notifications.KindTestEmail, c.Email.HandleTest)
	r.Handle(notifications.KindAccountEmail, c.Email.HandleAccount)
	r.Handle(notifications.KindWebhook, c.Deliverer.Handle)
	r.Handle(notifications.KindReleaseAssignment, notifications.ReleaseHandler(c.Tickets))
	r.Handle(sla.KindCheck, sla.CheckHandler(c.Tickets))
	r.Handle(sla.KindStop, sla.StopHandler(c.Tickets))
	r.Handle(automation.KindScan, c.Automation.HandleScan)
	r.Handle(automation.KindAct, c.Automation.HandleAct)
	r.Handle(tickets.KindScheduledSend, c.Tickets.HandleScheduledSend)
	r.Handle(tickets.KindHoldExpire, c.Tickets.HandleHoldExpire)
	r.Handle(tickets.KindDeleteRemovedFiles, c.Tickets.HandleDeleteRemovedFiles)
	// A delivery job the runner dead-letters on its own (attempt budget
	// spent by expired leases or panics) marks its delivery failed.
	for _, kind := range []string{notifications.KindEmail, notifications.KindTestEmail, notifications.KindAccountEmail, notifications.KindWebhook} {
		r.OnDead(kind, notifications.DeadHook(c.NotifyRepo))
	}
	return r
}

func (c *Container) newDispatcher(owner string) *notifications.Dispatcher {
	return notifications.NewDispatcher(notifications.DispatcherConfig{Events: c.NotifyRepo, Dir: c.NotifyRepo, Owner: owner,
		Log: c.Log, Waker: c.Waker, Now: c.BizClock.Now})
}

// runWorkers runs the outbox dispatcher and the job worker pool until ctx
// ends. No job is taken after that; attempts in progress may finish within
// the runner's grace period (15 s) before they are cancelled and recorded
// as failed attempts (retried after the backoff); see jobs.Runner.Run.
func (c *Container) runWorkers(ctx context.Context, role string, concurrency int) {
	owner := workerOwner(role)
	c.Log.Info("background workers started", "owner", owner, "concurrency", concurrency)
	var wg sync.WaitGroup
	wg.Go(func() { c.newDispatcher(owner).Run(ctx) })
	wg.Go(func() { c.newRunner(owner, concurrency).Run(ctx) })
	if rw, ok := c.Waker.(*redisWaker); ok {
		wg.Go(func() { rw.listen(ctx) })
	}
	wg.Wait()
	c.Log.Info("background workers stopped")
}

// wireNotifications creates the Milestone 5 services.
func (c *Container) wireNotifications() {
	if c.Redis != nil {
		c.Waker = &redisWaker{local: jobs.NewLocalWaker(), rdb: c.Redis, log: c}
	} else {
		c.Waker = jobs.NewLocalWaker()
	}
	c.Notifications = notifications.NewService(c.NotifyRepo, c.Jobs, c.Waker, c.Audit, c.Clock.Now)
	c.Email = notifications.NewEmail(notifications.EmailDeps{
		Jobs: c.Jobs, Deliveries: c.NotifyRepo, Events: c.NotifyRepo, Dir: c.NotifyRepo, People: c.NotifyRepo,
		Tickets: c.Tickets, Access: c.Access, Settings: c.Settings.Current, Keys: c.Keys,
		Mailers: func(cfg mailer.Config) (mailer.Mailer, error) { return c.Mailers(cfg) }, Renderer: c.Renderer, Now: c.Clock.Now,
		Accounts: c.IdentityRepo,
	})
	c.Webhooks = endpoints.NewService(endpoints.Deps{Repo: c.WebhooksRepo, Keys: c.Keys, Deliveries: c.Notifications,
		Policy: c.webhookPolicy, Waker: c.Waker, Clock: c.Clock})
	c.Deliverer = endpoints.NewDeliverer(endpoints.DelivererDeps{Jobs: c.Jobs, Deliveries: c.NotifyRepo, Events: c.NotifyRepo,
		Endpoints: c.WebhooksRepo, Access: c.Access, Tickets: c.Tickets, Keys: c.Keys,
		Policy: c.webhookPolicy, Now: c.Clock.Now, Jitter: func(d time.Duration) time.Duration {
			if c.WebhookJitter != nil {
				return c.WebhookJitter(d)
			}
			return webhooks.DefaultJitter(d)
		}})
}

// liveChannel is the Redis pub/sub channel for live update hints (payload:
// project ID). Hints only trigger an early outbox poll.
const liveChannel = product.Slug + ":live:changed"

// liveHint is the tickets repository's change hook: wake this process's
// live hub and, best effort, other processes' hubs through Redis.
func (c *Container) liveHint(projectID string) {
	if c.Live != nil {
		c.Live.Notify(projectID)
	}
	if c.Redis != nil {
		go func() {
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			_ = c.Redis.Publish(ctx, liveChannel, projectID).Err()
		}()
	}
}

// listenLiveHints forwards other processes' change hints to the live hub
// until ctx ends. Without Redis the hub simply polls.
func (c *Container) listenLiveHints(ctx context.Context) {
	if c.Redis == nil || c.Live == nil {
		return
	}
	for ctx.Err() == nil {
		sub := c.Redis.Subscribe(ctx, liveChannel)
		ch := sub.Channel()
	loop:
		for {
			select {
			case <-ctx.Done():
				break loop
			case m, ok := <-ch:
				if !ok {
					break loop
				}
				c.Live.Notify(m.Payload)
			}
		}
		_ = sub.Close()
		select {
		case <-ctx.Done():
		case <-time.After(5 * time.Second):
		}
	}
}

// wakeChannel is the Redis pub/sub channel for job wake-up hints.
const wakeChannel = product.Slug + ":jobs:wake"

// redisWaker publishes wake-up hints to every process. Redis is only a
// latency optimisation: workers poll MongoDB regardless, so a lost message
// or a Redis outage delays work by at most the poll interval.
type redisWaker struct {
	local *jobs.LocalWaker
	rdb   *redis.Client
	log   *Container
}

func (w *redisWaker) Notify(ctx context.Context) {
	w.local.Notify(ctx)
	pctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), time.Second)
	defer cancel()
	_ = w.rdb.Publish(pctx, wakeChannel, "1").Err()
}

func (w *redisWaker) C() <-chan struct{} { return w.local.C() }

func (w *redisWaker) listen(ctx context.Context) {
	for ctx.Err() == nil {
		sub := w.rdb.Subscribe(ctx, wakeChannel)
		ch := sub.Channel()
	loop:
		for {
			select {
			case <-ctx.Done():
				break loop
			case _, ok := <-ch:
				if !ok {
					break loop
				}
				w.local.Notify(ctx)
			}
		}
		_ = sub.Close()
		select {
		case <-ctx.Done():
		case <-time.After(5 * time.Second):
		}
	}
}
