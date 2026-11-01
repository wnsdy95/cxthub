package inbound

import "context"

type documentResponseRetainerKey struct{}

type DocumentResponseRetainer func(func()) bool

func WithDocumentResponseRetainer(ctx context.Context, retain DocumentResponseRetainer) context.Context {
	return context.WithValue(ctx, documentResponseRetainerKey{}, retain)
}

func RetainDocumentResponse(ctx context.Context, release func()) bool {
	retain, _ := ctx.Value(documentResponseRetainerKey{}).(DocumentResponseRetainer)
	return retain != nil && release != nil && retain(release)
}
