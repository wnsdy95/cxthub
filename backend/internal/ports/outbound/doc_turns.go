package outbound

import "context"

type docReadOnlyKey struct{}

// WithDocReadOnly forbids lazy index creation and manifest repacking, including
// stores without transactions. Missing projections must be built by maintenance.
func WithDocReadOnly(ctx context.Context) context.Context {
	return context.WithValue(ctx, docReadOnlyKey{}, true)
}

func DocReadOnly(ctx context.Context) bool {
	v, _ := ctx.Value(docReadOnlyKey{}).(bool)
	return v
}
