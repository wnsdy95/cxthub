//go:build postgres

package app

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/wnsdy95/cxthub/backend/internal/adapters/store"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

func TestPGSecretsConcurrentEditingBaseline(t *testing.T) {
	_, st, _ := collaborationPG(t)
	ctx := systemTestContext()
	repositoryRecord, in := seedSecretsRepo(t, st, st)
	// This fixture checks enqueue only. Remove its own undelivered test jobs so
	// repeated suites do not feed them to another worker test's global queue.
	t.Cleanup(func() {
		cleanup, cancel := context.WithTimeout(systemTestContext(), 5*time.Second)
		defer cancel()
		conn, err := pgxpool.New(cleanup, collaborationDSN(t))
		if err != nil {
			t.Error(err)
			return
		}
		defer conn.Close()
		if _, err := conn.Exec(cleanup, "DELETE FROM notification_outbox WHERE repository_id=$1", repositoryRecord.ID); err != nil {
			t.Error(err)
		}
	})

	repositoryRecord.WebhookURL = "https://example.test/secrets-test"
	if err := st.CreateRepository(ctx, repositoryRecord); err != nil {
		t.Fatal(err)
	}
	peer, err := store.NewPostgresStore(ctx, collaborationDSN(t))
	if err != nil {
		t.Fatal(err)
	}
	defer peer.Close()
	services := []*Service{NewService(st, st, nil, nil, st), NewService(peer, peer, nil, nil, peer)}
	var wg sync.WaitGroup
	start := make(chan struct{})
	results := make(chan error, 8)
	for i := range 8 {
		wg.Add(1)
		go func(i int) { defer wg.Done(); <-start; _, err := services[i%2].SaveSecrets(ctx, in); results <- err }(i)
	}
	close(start)
	wg.Wait()
	close(results)
	wins := 0
	for err := range results {
		if err == nil {
			wins++
		} else if !errors.Is(err, domain.ErrSecretsConflict) {
			t.Fatal(err)
		}
	}
	if wins != 1 {
		t.Fatalf("accepted %d concurrent initial writes", wins)
	}
	jobs, err := st.ListNotifications(ctx, repositoryRecord.ID)
	if err != nil || len(jobs) != 1 {
		t.Fatalf("notifications %d: %v", len(jobs), err)
	}
	raw, err := st.GetSecretsEnvelope(ctx, in.RepoID)
	if err != nil {
		t.Fatal(err)
	}
	baseline := domain.SecretsRevision(raw)
	in.Edit.ExpectedRevision = baseline
	ack, err := services[1].SaveSecrets(ctx, in)
	if err != nil {
		t.Fatal(err)
	}
	if ack.Revision == baseline {
		t.Fatal("identical ciphertext reused editing generation")
	}
	in.Edit.Rotate = true
	in.Edit.ExpectedFingerprint = "aaaaaaaaaaaa"
	if _, err := services[0].SaveSecrets(ctx, in); !errors.Is(err, domain.ErrSecretsConflict) {
		t.Fatalf("stale rotation accepted: %v", err)
	}
}

type pausedSecretsPG struct {
	*store.PostgresStore
	ready, release chan struct{}
}

func (s pausedSecretsPG) CompareAndSwapSecrets(ctx context.Context, repo domain.ContentHash, expected, next []byte) error {
	close(s.ready)
	select {
	case <-s.release:
	case <-ctx.Done():
		return ctx.Err()
	}
	return s.PostgresStore.CompareAndSwapSecrets(ctx, repo, expected, next)
}

func TestPGSecretsAuthorizationPinnedUntilCommit(t *testing.T) {
	_, st, _ := collaborationPG(t)
	ctx, cancel := context.WithTimeout(systemTestContext(), 15*time.Second)
	defer cancel()
	repositoryRecord, in := seedSecretsRepo(t, st, st)
	member := domain.User{ID: domain.NewID("user_"), Username: "m" + domain.NewID("")[:12], Email: "maintainer@example.test", Name: "Maintainer"}
	if err := st.UpsertUser(ctx, member); err != nil {
		t.Fatal(err)
	}
	if err := st.AddMember(ctx, domain.Membership{RepositoryID: repositoryRecord.ID, UserID: member.ID, Role: domain.RoleMaintainer}); err != nil {
		t.Fatal(err)
	}
	in.ActorID = member.ID
	peer, err := store.NewPostgresStore(ctx, collaborationDSN(t))
	if err != nil {
		t.Fatal(err)
	}
	defer peer.Close()
	pause := pausedSecretsPG{st, make(chan struct{}), make(chan struct{})}
	var release sync.Once
	defer release.Do(func() { close(pause.release) })
	svc := NewService(pause, st, nil, nil, st)
	done := make(chan error, 1)
	go func() { _, err := svc.SaveSecrets(ctx, in); done <- err }()
	select {
	case <-pause.ready:
	case err := <-done:
		t.Fatalf("write ended before CAS: %v", err)
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	// Both operations would invalidate the authorization that has already passed.
	// Their row updates must wait for the command transaction to finish.
	for _, change := range []func(context.Context) error{
		func(ctx context.Context) error { return peer.RemoveMember(ctx, repositoryRecord.ID, member.ID) },
		func(ctx context.Context) error {
			changed := repositoryRecord
			changed.SecretsPolicy = "owner"
			return peer.CreateRepository(ctx, changed)
		},
	} {
		blocked, stop := context.WithTimeout(ctx, 250*time.Millisecond)
		err := change(blocked)
		stop()
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("authorization changed before commit: %v", err)
		}
	}
	release.Do(func() { close(pause.release) })
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if err := peer.RemoveMember(ctx, repositoryRecord.ID, member.ID); err != nil {
		t.Fatal(err)
	}
	raw, err := st.GetSecretsEnvelope(ctx, in.RepoID)
	if err != nil {
		t.Fatal(err)
	}
	in.Edit.ExpectedRevision = domain.SecretsRevision(raw)
	if _, err := NewService(st, st, nil, nil, st).SaveSecrets(ctx, in); !errors.Is(err, domain.ErrForbidden) {
		t.Fatalf("revoked member saved: %v", err)
	}
}
