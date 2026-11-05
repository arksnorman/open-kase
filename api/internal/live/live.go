// Package live fans committed ticket changes out to authenticated
// live-update streams (Server-Sent Events, architecture §11.2).
//
// The source of truth is the transactional outbox in MongoDB: a Hub polls
// recent outbox records of the projects that have subscribers and hands
// each new change to them. Redis pub/sub (and the in-process change hook)
// only shorten the delay by triggering a poll early; when Redis is absent or
// a hint is lost, the next poll delivers the change. Changes carry
// identifiers only — {type, ticket ID, ticket version} — and every stream
// authorizes each change for its own principal before sending it. Streams
// are invalidation hints, not history: after a reconnect clients refetch.
package live

import (
	"context"
	"log/slog"
	"sync"
	"time"
)

// Change is one committed ticket change read from the outbox.
type Change struct {
	Id         string // outbox event ID (also the SSE event id)
	ProjectId  string
	TicketId   string
	Type       string // outbox event type, e.g. message.created
	Visibility string // audience of the change: public or agent
	Version    int64  // ticket version after the change
	At         time.Time
}

// Source reads changes recorded at or after since for the given projects,
// oldest first.
type Source interface {
	ChangesSince(ctx context.Context, projectIDs []string, since time.Time, limit int) ([]Change, error)
}

// Config tunes a Hub.
type Config struct {
	Source Source
	// Poll is the fallback polling interval (default 2 s).
	Poll time.Duration
	// Lookback re-reads this much history on every poll so changes whose
	// transaction committed late are not missed (default 30 s).
	Lookback time.Duration
	Log      *slog.Logger
	Now      func() time.Time
}

// Subscription receives the changes of one project.
type Subscription struct {
	hub       *Hub
	projectID string
	joined    time.Time
	ch        chan Change
	mu        sync.Mutex
	overflow  bool
	closed    bool
}

// C delivers changes. It is closed when the hub shuts down.
func (s *Subscription) C() <-chan Change { return s.ch }

// TakeOverflow reports (and clears) whether changes were dropped because
// the subscriber was too slow; the client must then refetch.
func (s *Subscription) TakeOverflow() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	o := s.overflow
	s.overflow = false
	return o
}

// Close unsubscribes.
func (s *Subscription) Close() { s.hub.unsubscribe(s) }

// Hub polls the outbox for subscribed projects.
type Hub struct {
	cfg     Config
	mu      sync.Mutex
	subs    map[string]map[*Subscription]bool
	seen    map[string]time.Time
	wake    chan struct{}
	started bool
	cancel  context.CancelFunc
	done    chan struct{}
	closed  bool
}

// NewHub returns a hub; its polling loop starts with the first subscriber.
func NewHub(cfg Config) *Hub {
	if cfg.Poll <= 0 {
		cfg.Poll = 2 * time.Second
	}
	if cfg.Lookback <= 0 {
		cfg.Lookback = 30 * time.Second
	}
	if cfg.Log == nil {
		cfg.Log = slog.New(slog.DiscardHandler)
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	return &Hub{cfg: cfg, subs: map[string]map[*Subscription]bool{}, seen: map[string]time.Time{}, wake: make(chan struct{}, 1)}
}

// subscriberBuffer bounds queued changes per stream.
const subscriberBuffer = 256

// Subscribe registers a subscriber for a project's changes. It returns nil
// after Close.
func (h *Hub) Subscribe(projectID string) *Subscription {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.closed {
		return nil
	}
	s := &Subscription{hub: h, projectID: projectID, joined: h.cfg.Now(), ch: make(chan Change, subscriberBuffer)}
	if h.subs[projectID] == nil {
		h.subs[projectID] = map[*Subscription]bool{}
	}
	h.subs[projectID][s] = true
	if !h.started {
		h.started = true
		ctx, cancel := context.WithCancel(context.Background())
		h.cancel, h.done = cancel, make(chan struct{})
		go h.run(ctx)
	}
	h.Notify(projectID)
	return s
}

func (h *Hub) unsubscribe(s *Subscription) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if set := h.subs[s.projectID]; set != nil {
		delete(set, s)
		if len(set) == 0 {
			delete(h.subs, s.projectID)
		}
	}
	s.mu.Lock()
	if !s.closed {
		s.closed = true
		close(s.ch)
	}
	s.mu.Unlock()
}

// Notify triggers an early poll (a hint that projectID changed).
func (h *Hub) Notify(string) {
	select {
	case h.wake <- struct{}{}:
	default:
	}
}

// Close stops polling and closes every subscription (server shutdown).
func (h *Hub) Close() {
	h.mu.Lock()
	if h.closed {
		h.mu.Unlock()
		return
	}
	h.closed = true
	cancel, done := h.cancel, h.done
	var all []*Subscription
	for _, set := range h.subs {
		for s := range set {
			all = append(all, s)
		}
	}
	h.mu.Unlock()
	if cancel != nil {
		cancel()
		<-done
	}
	for _, s := range all {
		h.unsubscribe(s)
	}
}

func (h *Hub) projects() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := make([]string, 0, len(h.subs))
	for p := range h.subs {
		out = append(out, p)
	}
	return out
}

func (h *Hub) run(ctx context.Context) {
	defer close(h.done)
	t := time.NewTimer(0)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		case <-h.wake:
			// Debounce bursts of hints.
			select {
			case <-ctx.Done():
				return
			case <-time.After(50 * time.Millisecond):
			}
		}
		if err := h.PollOnce(ctx); err != nil && ctx.Err() == nil {
			h.cfg.Log.Warn("live update poll failed", "error", err.Error())
		}
		if !t.Stop() {
			select {
			case <-t.C:
			default:
			}
		}
		t.Reset(h.cfg.Poll)
	}
}

// PollOnce reads recent changes and delivers the unseen ones.
func (h *Hub) PollOnce(ctx context.Context) error {
	projects := h.projects()
	if len(projects) == 0 {
		return nil
	}
	now := h.cfg.Now()
	changes, err := h.cfg.Source.ChangesSince(ctx, projects, now.Add(-h.cfg.Lookback), 1000)
	if err != nil {
		return err
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	for id, at := range h.seen {
		if now.Sub(at) > 2*h.cfg.Lookback {
			delete(h.seen, id)
		}
	}
	for _, c := range changes {
		if _, dup := h.seen[c.Id]; dup {
			continue
		}
		h.seen[c.Id] = now
		for s := range h.subs[c.ProjectId] {
			// Changes recorded before the stream opened are already part
			// of what the client fetched (ObjectID times have 1 s
			// resolution, hence the margin).
			if c.At.Before(s.joined.Add(-2 * time.Second)) {
				continue
			}
			s.mu.Lock()
			if !s.closed {
				select {
				case s.ch <- c:
				default:
					s.overflow = true
				}
			}
			s.mu.Unlock()
		}
	}
	return nil
}
