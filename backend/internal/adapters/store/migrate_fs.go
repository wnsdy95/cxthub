package store

import "context"

// ApplyMigrations is a no-op in FS store (no schema — JSON file layout).
// Interface unification for Store. Required for DSN fallback (FSStore) in postgres build.
func (s *FSStore) ApplyMigrations(_ context.Context, _ string) (int, error) { return 0, nil }
