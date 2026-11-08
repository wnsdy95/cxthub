package outbound

import (
	"context"
	"time"
)

// UploadPolicy describes how patiently a bulk upload treats server
// backpressure. A context without a policy keeps the bounded legacy contract
// (one short absolute budget per batch). Policies are set by delivery
// adapters, never inferred by transport.
type UploadPolicy struct {
	// StallBudget bounds how long one batch may wait out consecutive
	// resource_overloaded rejections. Every accepted batch restarts it, so a
	// slow but progressing upload continues while a stuck one stops.
	StallBudget time.Duration
	// Gate serializes bulk batches across local processes for one repository.
	// It runs exactly one request; it is never held across a backoff sleep.
	Gate func(ctx context.Context, request func() error) error
	// Progress observes accepted chunk counts for one document.
	Progress func(sent, total int)
}

type uploadPolicyKey struct{}

func WithUploadPolicy(ctx context.Context, policy UploadPolicy) context.Context {
	return context.WithValue(ctx, uploadPolicyKey{}, policy)
}

func UploadPolicyFromContext(ctx context.Context) (UploadPolicy, bool) {
	policy, ok := ctx.Value(uploadPolicyKey{}).(UploadPolicy)
	return policy, ok && policy.StallBudget > 0
}
