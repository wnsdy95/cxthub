package inbound

import "context"

type correlationKey struct{}

// Correlation identifies a server request, never an authority or an idempotency key.
func WithCorrelation(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, correlationKey{}, id)
}
func Correlation(ctx context.Context) string {
	id, _ := ctx.Value(correlationKey{}).(string)
	return id
}
