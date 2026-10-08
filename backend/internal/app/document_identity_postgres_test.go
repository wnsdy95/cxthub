//go:build postgres

package app

import (
	"context"
	"errors"
	"github.com/wnsdy95/cxthub/backend/internal/adapters/store"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/inbound"
	"os"
	"testing"
	"time"
)

func TestP6PGQueuedOldWriteSeesCommittedRequirement(t *testing.T) {
	dsn := os.Getenv("CXT_TEST_DSN")
	if dsn == "" {
		t.Skip("requires disposable CXT_TEST_DSN")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	st, err := store.NewPostgresStore(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if _, err = st.ApplyMigrations(ctx, "../../../schemas/db/migrations"); err != nil {
		t.Fatal(err)
	}
	id := domain.HashContent([]byte(t.Name() + time.Now().String()))
	if _, err = st.PutRepo(ctx, domain.Repo{ID: id}); err != nil {
		t.Fatal(err)
	}
	svc := NewService(st, st, nil, nil, nil)
	locked := make(chan struct{})
	release := make(chan struct{})
	ownerDone := make(chan error, 1)
	go func() {
		ownerDone <- st.WithinRepository(ctx, id, func(tx context.Context) error {
			close(locked)
			<-release
			return st.RequireDocumentIdentity(tx, id, domain.DocumentIdentityRootV1)
		})
	}()
	<-locked
	oldDone := make(chan error, 1)
	go func() { oldDone <- svc.UpdateAbout(inbound.WithSystemActor(ctx), id, "forbidden-effect", "", nil) }()
	select {
	case err = <-oldDone:
		close(release)
		<-ownerDone
		t.Fatalf("old write escaped held transaction: %v", err)
	case <-time.After(40 * time.Millisecond):
	}
	close(release)
	if err = <-ownerDone; err != nil {
		t.Fatal(err)
	}
	if err = <-oldDone; !errors.Is(err, domain.ErrDocumentIdentityUpgradeRequired) {
		t.Fatal("old write accepted", err)
	}
	r, err := st.GetRepo(ctx, id)
	if err != nil || r.Description != "" || r.RequiredDocIdentity != domain.DocumentIdentityRootV1 {
		t.Fatal("write effects", r, err)
	}
	rev, err := st.RepositoryRevision(ctx, id)
	if err != nil || rev.Graph != 0 || rev.Pending != 0 || rev.Evidence != 0 {
		t.Fatal("revision effects", rev, err)
	}
}
