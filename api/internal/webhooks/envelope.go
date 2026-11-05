package webhooks

import (
	"encoding/json"
	"errors"
	"time"
)

// SchemaVersion is the version of the payload envelope.
const SchemaVersion = 1

// Envelope is the JSON body of every delivery. Data contains only the
// minimized fields the endpoint's principal is authorized to receive; it is
// built per endpoint from the current authorized projection, never by
// trimming a full ticket document.
type Envelope struct {
	SchemaVersion int             `json:"schemaVersion"`
	EventId       string          `json:"eventId"`
	Type          string          `json:"type"`
	OccurredAt    time.Time       `json:"occurredAt"`
	Ticket        *TicketRef      `json:"ticket,omitempty"`
	Data          json.RawMessage `json:"data"`
}

// TicketRef identifies the ticket. Revision is the ticket version after the
// event, so receivers can discard events older than the state they hold.
type TicketRef struct {
	Id       string `json:"id"`
	Number   int64  `json:"number,omitempty"`
	Revision int64  `json:"revision"`
}

// Marshal validates the envelope and returns the exact bytes to sign and
// send. Store these bytes with the delivery so retries send identical
// content.
func (e Envelope) Marshal() ([]byte, error) {
	if e.SchemaVersion == 0 {
		e.SchemaVersion = SchemaVersion
	}
	if e.EventId == "" || e.Type == "" || e.OccurredAt.IsZero() {
		return nil, errors.New("webhook envelope requires eventId, type and occurredAt")
	}
	if e.Ticket != nil && e.Ticket.Id == "" {
		return nil, errors.New("webhook envelope ticket requires an id")
	}
	if len(e.Data) == 0 {
		e.Data = json.RawMessage(`{}`)
	} else if !json.Valid(e.Data) {
		return nil, errors.New("webhook envelope data is not valid JSON")
	}
	e.OccurredAt = e.OccurredAt.UTC().Truncate(time.Millisecond)
	return json.Marshal(e)
}
