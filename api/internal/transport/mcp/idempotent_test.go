package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/arksnorman/open-kase/api/internal/access"
	"github.com/arksnorman/open-kase/api/internal/idempotency"
	"github.com/arksnorman/open-kase/api/package/clock"
)

// ctxStore is an in-memory idempotency.Store whose writes fail once their
// context is cancelled, as MongoDB operations do.
type ctxStore struct {
	mu      sync.Mutex
	done    map[string]idempotency.Record
	pending map[string]bool
}

func (s *ctxStore) Begin(ctx context.Context, scope, key, _ string, _ time.Time) (*idempotency.Record, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if r, ok := s.done[scope+"\x00"+key]; ok {
		return &r, nil
	}
	if s.pending[scope+"\x00"+key] {
		return nil, idempotency.ErrInProgress
	}
	s.pending[scope+"\x00"+key] = true
	return nil, nil
}

func (s *ctxStore) Complete(ctx context.Context, scope, key string, rec idempotency.Record, _ time.Time) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.pending, scope+"\x00"+key)
	s.done[scope+"\x00"+key] = rec
	return nil
}

func (s *ctxStore) Abandon(ctx context.Context, scope, key string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.pending, scope+"\x00"+key)
	return nil
}

// OC-OPS-09 (MCP side): a client that disconnects right after a tool call
// ran must not leave the reservation pending; the retry replays the result
// (and a failed call's reservation is released) instead of answering
// idempotencyInProgress and later running the command again.
func TestIdempotentOutcomeOutlivesTheRequest(t *testing.T) {
	st := &ctxStore{done: map[string]idempotency.Record{}, pending: map[string]bool{}}
	h := &Handler{d: Deps{Idempotency: st, Clock: clock.System{}}, log: slog.New(slog.DiscardHandler)}
	c := &call{p: access.Principal{Kind: access.KindUser, Id: "u1"}}
	recheck := func(context.Context) error { return nil }
	args := map[string]string{"subject": "Mail bounces"}

	runs := 0
	ctx, disconnect := context.WithCancel(context.Background())
	run := func(context.Context) (any, error) {
		runs++
		disconnect() // the command committed; the client is gone
		return map[string]string{"id": "t1"}, nil
	}
	if _, err := h.idempotent(ctx, c, ToolTicketsCreate, "intake-1", args, recheck, run); err != nil {
		t.Fatal(err)
	}
	out, err := h.idempotent(context.Background(), c, ToolTicketsCreate, "intake-1", args, recheck, run)
	if err != nil {
		t.Fatalf("retry after the disconnect: %v", err)
	}
	if raw, _ := out.(json.RawMessage); string(raw) != `{"id":"t1"}` || runs != 1 {
		t.Fatalf("retry: %s, runs %d; want the replayed result and one run", out, runs)
	}

	ctx, disconnect = context.WithCancel(context.Background())
	failing := func(context.Context) (any, error) {
		disconnect()
		return nil, errors.New("validation failed")
	}
	if _, err := h.idempotent(ctx, c, ToolTicketsCreate, "intake-2", args, recheck, failing); err == nil {
		t.Fatal("failing call succeeded")
	}
	if _, err := h.idempotent(context.Background(), c, ToolTicketsCreate, "intake-2", args, recheck, run); err != nil {
		t.Fatalf("retry of a failed call: %v (want it to run again)", err)
	}
}
