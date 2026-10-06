package store

import (
	"context"
	"errors"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"testing"
)

func TestFSRepositoryInitializationUnsupportedPreservesRegistration(t *testing.T) {
	ctx := context.Background()
	st := NewFSStore(t.TempDir())
	r := domain.Repo{ID: domain.HashContent([]byte("fs init")), DefaultBranch: "main"}
	if _, err := st.BeginRepositoryInitialization(ctx, r); !errors.Is(err, domain.ErrRepositoryInitializationUnsupported) {
		t.Fatal(err)
	}
	if _, err := st.GetRepo(ctx, r.ID); !errors.Is(err, domain.ErrNotFound) {
		t.Fatal("unsupported created repo", err)
	}
	registered, err := st.PutRepo(ctx, r)
	if err != nil {
		t.Fatal(err)
	}
	if registered.ContextProtocol != 0 {
		t.Fatal("legacy registration upgraded")
	}
	if _, err := st.FinalizeRepositoryInitialization(ctx, r.ID, domain.RepositoryInitializationFinalize{}, nil); !errors.Is(err, domain.ErrRepositoryInitializationUnsupported) {
		t.Fatal(err)
	}
	got, err := st.GetRepo(ctx, r.ID)
	if err != nil || got.ContextProtocol != 0 {
		t.Fatal("unsupported altered registration", err)
	}
}
