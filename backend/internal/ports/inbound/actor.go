package inbound

import "context"

type repositoryActorKey struct{}
type repositoryActor struct {
	user   string
	system bool
}

// WithRepositoryActor carries authenticated server identity, never a body field.
// Every repository command rechecks current authority inside its transaction.
func WithRepositoryActor(ctx context.Context, user string) context.Context {
	return context.WithValue(ctx, repositoryActorKey{}, repositoryActor{user: user})
}

// WithSystemActor is for trusted workers and verified host events. It is not a
// transport option: clients cannot request system authority. Jobs must validate
// their durable identity, lease and current repository policy before publication.
func WithSystemActor(ctx context.Context) context.Context {
	return context.WithValue(ctx, repositoryActorKey{}, repositoryActor{system: true})
}
func RepositoryActor(ctx context.Context) (string, bool) {
	actor, _ := ctx.Value(repositoryActorKey{}).(repositoryActor)
	return actor.user, actor.system
}
