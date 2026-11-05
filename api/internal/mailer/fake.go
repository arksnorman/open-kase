package mailer

import (
	"context"
	"sync"
)

// Fake is an in-memory Mailer for tests. It validates every message like the
// SMTP adapter. FailWith, when set, decides the error for a message; failed
// messages are not recorded as sent.
type Fake struct {
	mu       sync.Mutex
	sent     []Message
	FailWith func(Message) error
}

// Send records msg or returns the configured failure.
func (f *Fake) Send(ctx context.Context, msg Message) error {
	if err := ctx.Err(); err != nil {
		return &Error{Class: ClassTemporary, Phase: PhaseConnect, Err: err}
	}
	if err := msg.Validate(); err != nil {
		return &Error{Class: ClassPermanent, Phase: PhaseValidate, Err: err}
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.FailWith != nil {
		if err := f.FailWith(msg); err != nil {
			return err
		}
	}
	f.sent = append(f.sent, cloneMessage(msg))
	return nil
}

// Sent returns a copy of the delivered messages in order.
func (f *Fake) Sent() []Message {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]Message, len(f.sent))
	for i, m := range f.sent {
		out[i] = cloneMessage(m)
	}
	return out
}

// Reset forgets delivered messages.
func (f *Fake) Reset() {
	f.mu.Lock()
	f.sent = nil
	f.mu.Unlock()
}

func cloneMessage(m Message) Message {
	if m.ReplyTo != nil {
		r := *m.ReplyTo
		m.ReplyTo = &r
	}
	if m.Headers != nil {
		h := make(map[string]string, len(m.Headers))
		for k, v := range m.Headers {
			h[k] = v
		}
		m.Headers = h
	}
	return m
}
