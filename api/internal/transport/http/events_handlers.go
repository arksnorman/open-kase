package http

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/arksnorman/open-kase/api/internal/access"
	"github.com/arksnorman/open-kase/api/internal/integrations"
	"github.com/arksnorman/open-kase/api/package/fault"
)

// DefaultLiveHeartbeat is the SSE heartbeat (and access re-check) interval.
const DefaultLiveHeartbeat = 15 * time.Second

// liveEventDTO is the data of an SSE `invalidate` event: which ticket
// changed and how, never its content. Clients refetch what they show.
type liveEventDTO struct {
	Type     string `json:"type" doc:"The committed change, e.g. ticket.created, ticket.updated, ticket.claimed, ticket.statusChanged, message.created, message.redacted, ticket.linkAdded."`
	TicketId string `json:"ticketId"`
	Version  int64  `json:"version" doc:"Ticket version after the change (always 0 for integrations)."`
}

// liveControlDTO is the data of the ready/resync/reconnect/error events.
type liveControlDTO struct {
	Type             string `json:"type" enum:"ready,resync,reconnect,error"`
	Code             string `json:"code,omitempty"`
	HeartbeatSeconds int    `json:"heartbeatSeconds,omitempty"`
}

func bearerToken(c *gin.Context) string {
	_, raw, _ := strings.Cut(c.GetHeader("Authorization"), " ")
	return strings.TrimSpace(raw)
}

// principalFor re-authenticates a bearer credential (expiry, session and
// account status are checked again).
func (a *api) principalFor(ctx context.Context, raw string) (access.Principal, error) {
	if integrations.LooksLikeToken(raw) {
		return a.Integrations.Authenticate(ctx, raw)
	}
	return a.Auth.Authenticate(ctx, raw)
}

// streamEvents is an authenticated Server-Sent Events stream of minimal
// invalidation events for tickets of the project the caller can read. The
// credential comes from the Authorization header (fetch streaming); tokens
// are never accepted in the URL. Every change is authorized for the caller
// when it is delivered, and the credential and project access are checked
// again on every heartbeat: an expired token, revoked session, disabled
// account or lost membership ends the stream with an `error` event. The
// stream carries no history: after any reconnect clients refetch, and
// polling the list endpoints is an equally valid client strategy.
func (a *api) streamEvents(c *gin.Context) {
	s, ok := a.scope(c)
	if !ok {
		return
	}
	if err := s.Require(access.CapTicketRead); err != nil {
		writeError(c, err)
		return
	}
	key := string(s.Principal.Kind) + ":" + s.Principal.Id
	if !a.liveStreams.acquire(key) {
		writeError(c, fault.RateLimited("tooManyStreams",
			"Too many live update streams are open for this account. Close other tabs or poll instead.", 30*time.Second))
		return
	}
	defer a.liveStreams.release(key)
	raw := bearerToken(c)
	sub := a.Live.Subscribe(s.Project.Id)
	if sub == nil {
		writeError(c, fault.New(fault.KindUnavailable, "shuttingDown", "The server is shutting down. Reconnect shortly."))
		return
	}
	defer sub.Close()

	heartbeat := a.LiveHeartbeat
	if heartbeat <= 0 {
		heartbeat = DefaultLiveHeartbeat
	}
	w := c.Writer
	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-store")
	h.Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	rc := http.NewResponseController(w)
	write := func(chunk []byte) bool {
		_ = rc.SetWriteDeadline(time.Now().Add(heartbeat + 30*time.Second))
		if _, err := w.Write(chunk); err != nil {
			return false
		}
		return rc.Flush() == nil
	}
	send := func(event, id string, data any) bool {
		var b bytes.Buffer
		if id != "" {
			b.WriteString("id: " + id + "\n")
		}
		raw, _ := json.Marshal(data)
		b.WriteString("event: " + event + "\ndata: ")
		b.Write(raw)
		b.WriteString("\n\n")
		return write(b.Bytes())
	}
	if !write([]byte("retry: 5000\n\n")) ||
		!send("ready", "", liveControlDTO{Type: "ready", HeartbeatSeconds: int(heartbeat / time.Second)}) {
		return
	}

	ctx := c.Request.Context()
	recheck := func() bool {
		p, err := a.principalFor(ctx, raw)
		if err == nil {
			var ns *access.ProjectScope
			if ns, err = a.Access.Scope(ctx, p); err == nil {
				err = ns.Require(access.CapTicketRead)
				if err == nil {
					s = ns
					return true
				}
			}
		}
		send("error", "", liveControlDTO{Type: "error", Code: fault.As(err).Code})
		return false
	}
	lastCheck := time.Now()
	t := time.NewTicker(heartbeat)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if !recheck() {
				return
			}
			lastCheck = time.Now()
			if sub.TakeOverflow() && !send("resync", "", liveControlDTO{Type: "resync"}) {
				return
			}
			if !write([]byte(": ping\n\n")) {
				return
			}
		case ch, open := <-sub.C():
			if !open {
				send("reconnect", "", liveControlDTO{Type: "reconnect"})
				return
			}
			if time.Since(lastCheck) > time.Second {
				if !recheck() {
					return
				}
				lastCheck = time.Now()
			}
			version, visible, err := a.Tickets.LiveVisible(ctx, s, ch.TicketId, ch.Type, ch.Visibility, ch.Version)
			if err != nil {
				a.log.Warn("live update authorization failed", "error", err.Error())
				continue
			}
			if visible && !send("invalidate", ch.Id, liveEventDTO{Type: ch.Type, TicketId: ch.TicketId, Version: version}) {
				return
			}
			if sub.TakeOverflow() && !send("resync", "", liveControlDTO{Type: "resync"}) {
				return
			}
		}
	}
}
