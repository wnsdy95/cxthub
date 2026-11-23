package store

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

func TestWorkerFSMetadataPin(t *testing.T) {
	st := NewFSStore(t.TempDir())
	ctx := context.Background()
	repo := domain.HashContent([]byte(t.Name()))
	if _, e := st.PutRepo(ctx, domain.Repo{ID: repo}); e != nil {
		t.Fatal(e)
	}
	release, e := st.pinWorkerRepoPolicy(ctx, repo)
	if e != nil {
		t.Fatal(e)
	}
	done := make(chan error, 1)
	go func() { done <- st.RequireDocumentIdentity(ctx, repo, domain.DocumentIdentityRootV1) }()
	select {
	case e := <-done:
		release()
		t.Fatal("opt-in crossed retained policy", e)
	case <-time.After(30 * time.Millisecond):
	}
	release()
	if e := <-done; e != nil {
		t.Fatal(e)
	}
	if release, e = st.pinWorkerRepoPolicy(ctx, repo); !errors.Is(e, domain.ErrDocumentIdentityUpgradeRequired) {
		if release != nil {
			release()
		}
		t.Fatal(e)
	}
}
