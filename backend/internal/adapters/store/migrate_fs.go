package store

import "context"

func (s *FSStore) VerifyMigrations(ctx context.Context, _ string) error { return ctx.Err() }
