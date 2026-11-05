package http

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"net/http"
	"regexp"

	"github.com/gin-gonic/gin"

	"github.com/arksnorman/open-kase/api/internal/idempotency"
	"github.com/arksnorman/open-kase/api/package/fault"
	"github.com/arksnorman/open-kase/api/package/validation"
)

const (
	headerIdempotencyKey = "Idempotency-Key"
	// ctxNoReplayStore marks a response (e.g. a one-time secret) whose body
	// must not be stored for replay.
	ctxNoReplayStore = "idempotencyNoStore"
)

var validIdempotencyKey = regexp.MustCompile(`^[\x21-\x7e]{1,200}$`)

// captureWriter tees the response body so it can be stored for replay.
type captureWriter struct {
	gin.ResponseWriter
	buf bytes.Buffer
}

func (w *captureWriter) Write(b []byte) (int, error) {
	w.buf.Write(b)
	return w.ResponseWriter.Write(b)
}

func (w *captureWriter) WriteString(s string) (int, error) {
	w.buf.WriteString(s)
	return w.ResponseWriter.WriteString(s)
}

// idempotent wraps a create handler. Without an Idempotency-Key header the
// handler runs normally. With one, the key is scoped to the principal and
// route; a retry with the same body replays the stored response, and a
// different body is rejected with 409 idempotencyKeyReused. Replays still
// require the principal to be authenticated now and, for project routes,
// to still see the project.
func (a *api) idempotent(next gin.HandlerFunc) gin.HandlerFunc {
	return func(c *gin.Context) {
		key := c.GetHeader(headerIdempotencyKey)
		if key == "" || a.Idempotency == nil {
			next(c)
			return
		}
		if !validIdempotencyKey.MatchString(key) {
			writeError(c, fault.Field(headerIdempotencyKey, validation.CodeInvalidFormat, "Use 1 to 200 printable ASCII characters."))
			return
		}
		raw, err := io.ReadAll(http.MaxBytesReader(c.Writer, c.Request.Body, validation.DefaultMaxBodyBytes))
		if err != nil {
			writeError(c, fault.New(fault.KindTooLarge, "bodyTooLarge", "The request body is too large."))
			return
		}
		c.Request.Body = io.NopCloser(bytes.NewReader(raw))
		sum := sha256.Sum256(append([]byte(c.Request.Method+" "+c.Request.URL.Path+"\n"), raw...))
		p := principal(c)
		scope := string(p.Kind) + ":" + p.Id + ":" + c.Request.Method + " " + c.FullPath()
		ctx := c.Request.Context()

		rec, err := a.Idempotency.Begin(ctx, scope, key, hex.EncodeToString(sum[:]), a.Clock.Now())
		switch {
		case errors.Is(err, idempotency.ErrMismatch):
			writeError(c, fault.Conflict("idempotencyKeyReused", "This Idempotency-Key was used with a different request."))
			return
		case errors.Is(err, idempotency.ErrInProgress):
			writeError(c, fault.Conflict("idempotencyInProgress", "A request with this Idempotency-Key is still being processed."))
			return
		case err != nil:
			writeError(c, err)
			return
		}
		if rec != nil {
			// Possession of a key never outlives the permission to run the
			// command: user creation is administrator-only, every other
			// create re-checks the caller's access.
			if c.FullPath() != "/api/v1/users" {
				s, ok := a.scope(c)
				if !ok {
					return
				}
				// Ticket sub-resources (messages) also re-check that the
				// caller can still read the ticket.
				if t := c.Param("t"); t != "" && a.Tickets != nil {
					if _, err := a.Tickets.Get(c.Request.Context(), s, t); err != nil {
						writeError(c, err)
						return
					}
				}
			} else if err := p.RequireAdmin(); err != nil {
				writeError(c, err)
				return
			}
			if len(rec.Body) == 0 {
				writeError(c, fault.Conflict("idempotencyKeyUsed", "This request already completed; its one-time response cannot be shown again."))
				return
			}
			c.Header("Idempotent-Replayed", "true")
			c.Data(rec.Status, "application/json; charset=utf-8", rec.Body)
			return
		}

		w := &captureWriter{ResponseWriter: c.Writer}
		c.Writer = w
		// Commands that can record the key inside their own transaction
		// (ticket creation) find the reservation in the context.
		c.Request = c.Request.WithContext(idempotency.WithRef(ctx, idempotency.Ref{Scope: scope, Key: key}))
		next(c)
		c.Writer = w.ResponseWriter
		// The outcome is recorded even if the client has gone meanwhile.
		octx, cancel := idempotency.OutcomeContext(ctx)
		defer cancel()
		status := w.Status()
		if status < 200 || status > 299 {
			if err := a.Idempotency.Abandon(octx, scope, key); err != nil {
				a.log.Warn("idempotency abandon failed", "error", err.Error())
			}
			return
		}
		out := idempotency.Record{Status: status, Body: w.buf.Bytes()}
		if c.GetBool(ctxNoReplayStore) {
			out.Body = nil
		}
		if err := a.Idempotency.Complete(octx, scope, key, out, a.Clock.Now()); err != nil {
			a.log.Warn("idempotency completion failed", "error", err.Error())
		}
	}
}
