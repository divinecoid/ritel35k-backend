// Package reqmeta carries per-request metadata (request id, ip, user agent, device) through context.
package reqmeta

import "context"

type Meta struct {
	RequestID string
	IP        string
	UserAgent string
	DeviceID  string
}

type key struct{}

func With(ctx context.Context, m Meta) context.Context { return context.WithValue(ctx, key{}, m) }

func From(ctx context.Context) Meta {
	m, _ := ctx.Value(key{}).(Meta)
	return m
}
