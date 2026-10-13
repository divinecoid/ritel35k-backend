// Package eventbus is an in-process registry of domain-event handlers, fed by the outbox relay.
package eventbus

import (
	"context"
	"encoding/json"
	"time"

	"github.com/google/uuid"
)

type Event struct {
	ID            int64
	CompanyID     uuid.UUID
	Type          string
	AggregateType string
	AggregateID   uuid.UUID
	Payload       json.RawMessage
	CreatedAt     time.Time
}

// Handler must be idempotent: the relay delivers at-least-once.
type Handler func(ctx context.Context, e Event) error

type registration struct {
	name string
	fn   Handler
}

type Bus struct {
	handlers map[string][]registration
}

func New() *Bus { return &Bus{handlers: map[string][]registration{}} }

// Subscribe registers a named consumer for an event type. The name keys processed_events dedup.
func (b *Bus) Subscribe(eventType, consumer string, h Handler) {
	b.handlers[eventType] = append(b.handlers[eventType], registration{name: consumer, fn: h})
}

type Delivery struct {
	Consumer string
	Fn       Handler
}

func (b *Bus) For(eventType string) []Delivery {
	regs := b.handlers[eventType]
	out := make([]Delivery, len(regs))
	for i, r := range regs {
		out[i] = Delivery{Consumer: r.name, Fn: r.fn}
	}
	return out
}
