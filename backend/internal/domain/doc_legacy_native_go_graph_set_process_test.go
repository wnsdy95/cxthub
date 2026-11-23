package domain

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
)

type legacyGoGraphSetW85ProcessStore struct {
	test      *testing.T
	directory string
	current   context.Context
	calls     int
	bytes     int
	reads     int
	writes    int
}

func (store *legacyGoGraphSetW85ProcessStore) begin(ctx context.Context) {
	store.current, store.calls, store.bytes = ctx, 0, 0
}

func (store *legacyGoGraphSetW85ProcessStore) path(hash ContentHash) string {
	return filepath.Join(store.directory, strings.TrimPrefix(string(hash), "sha256:")+".cas")
}
