//go:build postgres

package app

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/wnsdy95/cxthub/backend/internal/adapters/auth"
	"github.com/wnsdy95/cxthub/backend/internal/adapters/gitengine"
	"github.com/wnsdy95/cxthub/backend/internal/adapters/store"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/inbound"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

type failedNotificationPG struct{ *store.PostgresStore }

func (s failedNotificationPG) EnqueueNotification(ctx context.Context, d outbound.NotificationDelivery) error {
	if err := s.PostgresStore.EnqueueNotification(ctx, d); err != nil {
		return err
	}
	return errors.New("injected failure after outbox insert")
}
func TestPGNotificationAtomicBusinessWritesAndConcurrentInvite(t *testing.T) {
	_, st, _ := collaborationPG(t)
	ctx := context.Background()
	user := domain.User{ID: domain.NewID("user_"), Username: fmt.Sprintf("notify%d", time.Now().UnixNano()), Email: "owner@example.test", Name: "Owner"}
	if err := st.UpsertUser(ctx, user); err != nil {
		t.Fatal(err)
	}
	wsp := domain.Workspace{ID: domain.NewID("ws_"), Name: "Notifications", OwnerID: user.ID, OwnerUsername: user.Username, Slug: "events", CreatedAt: time.Now().UTC(), WebhookURL: "https://example.test/credential"}
	if err := st.CreateWorkspace(ctx, wsp); err != nil {
		t.Fatal(err)
	}
	repo := hh(wsp.ID)
	if _, err := st.PutRepo(ctx, domain.Repo{ID: repo, WorkspaceID: wsp.ID}); err != nil {
		t.Fatal(err)
	}
	svc := NewService(st, st, auth.NewTeamTokenAuth(), gitengine.NewEngine(st), st)
	fail := failedNotificationPG{st}
	broken := NewService(fail, st, auth.NewTeamTokenAuth(), gitengine.NewEngine(st), st)
	raw := []byte(`{"version":1,"kdf":"PBKDF2-SHA256","iterations":600000,"salt_b64":"AAAAAAAAAAAAAAAAAAAAAA==","cipher":"AES-256-GCM","nonce_b64":"AAAAAAAAAAAAAAAA","ciphertext_b64":"AAAAAAAAAAAAAAAAAAAAAA==","fingerprint":"aaaaaaaaaaaa"}`)
	inputSecrets := inbound.SaveSecretsInput{RepoID: repo, ActorID: user.ID, Envelope: raw, Edit: domain.SecretsEdit{ExpectedRevision: "absent"}}
	if _, err := broken.SaveSecrets(ctx, inputSecrets); err == nil {
		t.Fatal("injected enqueue failure accepted")
	}
	if _, err := st.GetSecretsEnvelope(ctx, repo); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("ciphertext escaped rollback: %v", err)
	}
	jobs, err := st.ListNotifications(ctx, wsp.ID)
	if err != nil || len(jobs) != 0 {
		t.Fatalf("outbox escaped rollback: %+v %v", jobs, err)
	}
	target := collaborationSnapshot(t, st, repo, "notification branch")
	input := inbound.UpdateRefInput{RepoID: repo, Ref: domain.Ref{RepoID: repo, Kind: domain.RefBranch, Name: "main", Target: target}}
	if _, err := broken.UpdateRef(ctx, input); err == nil {
		t.Fatal("ref enqueue failure accepted")
	}
	if _, err := st.GetRef(ctx, repo, domain.RefBranch, "main"); !errors.Is(err, domain.ErrNotFound) {
		t.Fatal("ref escaped rollback")
	}
	if _, err := svc.UpdateRef(ctx, input); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.UpdateRef(ctx, input); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.SaveSecrets(ctx, inputSecrets); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.SaveSecrets(ctx, inputSecrets); !errors.Is(err, domain.ErrSecretsConflict) {
		t.Fatal("stale ciphertext CAS accepted")
	}
	jobs, err = st.ListNotifications(ctx, wsp.ID)
	if err != nil || len(jobs) != 2 {
		t.Fatalf("replay duplicated or lost notifications: %+v %v", jobs, err)
	}
	joiner := domain.User{ID: domain.NewID("user_"), Username: fmt.Sprintf("join%d", time.Now().UnixNano()), Email: "join@example.test", Name: "Joiner"}
	if err := st.UpsertUser(ctx, joiner); err != nil {
		t.Fatal(err)
	}
	inv := domain.Invite{Token: domain.NewID("inv_"), WorkspaceID: wsp.ID, Role: domain.RoleMember, Status: domain.InvitePending, CreatedBy: user.ID, CreatedAt: time.Now().UTC()}
	if err := st.CreateInvite(ctx, inv); err != nil {
		t.Fatal(err)
	}
	if _, err := NewIdentityService(nil, fail).AcceptInvite(ctx, joiner, inv.Token); err == nil {
		t.Fatal("member enqueue failure accepted")
	}
	if member, err := st.IsMember(ctx, wsp.ID, joiner.ID); err != nil || member {
		t.Fatalf("membership escaped rollback: %v %v", member, err)
	}
	peer, err := store.NewPostgresStore(ctx, collaborationDSN(t))
	if err != nil {
		t.Fatal(err)
	}
	defer peer.Close()
	var wg sync.WaitGroup
	start := make(chan struct{})
	errs := make(chan error, 2)
	for _, db := range []*store.PostgresStore{st, peer} {
		wg.Add(1)
		go func(db *store.PostgresStore) {
			defer wg.Done()
			<-start
			_, err := NewIdentityService(nil, db).AcceptInvite(ctx, joiner, inv.Token)
			errs <- err
		}(db)
	}
	close(start)
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	jobs, err = st.ListNotifications(ctx, wsp.ID)
	if err != nil || len(jobs) != 3 {
		t.Fatalf("concurrent joins duplicated notification: %+v %v", jobs, err)
	}
	// Competing workers claim each row once. A database restart can rediscover
	// rows because the queue is persisted, independently of either service.
	seen := map[string]bool{}
	for range jobs {
		d, err := peer.ClaimNotification(ctx, time.Now(), time.Minute)
		if err != nil {
			t.Fatal(err)
		}
		if seen[d.Job.ID] {
			t.Fatal("claimed active job twice")
		}
		seen[d.Job.ID] = true
		d.Job.State = "delivered"
		d.Job.LeaseUntil = time.Time{}
		if err := st.FinishNotification(ctx, d.Job, time.Now()); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := st.ClaimNotification(ctx, time.Now(), time.Minute); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("finished queue still claimable: %v", err)
	}
	// Two workers race for a single committed row; the active lease is exclusive.
	// Claim uses the database clock. Make these fixtures already due so a small
	// host/DB clock skew does not turn this lease test into a scheduling race.
	due := time.Unix(0, 0).UTC()
	claimJob := domain.NotificationJob{ID: domain.NewID("evt_"), WorkspaceID: wsp.ID, State: "pending", CreatedAt: time.Now().UTC(), NextAttempt: due}
	if err := st.EnqueueNotification(ctx, outbound.NotificationDelivery{Job: claimJob, Destination: wsp.WebhookURL}); err != nil {
		t.Fatal(err)
	}
	claimed := make(chan outbound.NotificationDelivery, 2)
	for _, db := range []*store.PostgresStore{st, peer} {
		wg.Add(1)
		go func(db *store.PostgresStore) {
			defer wg.Done()
			d, err := db.ClaimNotification(ctx, time.Now(), time.Minute)
			if err == nil {
				claimed <- d
			} else if !errors.Is(err, domain.ErrNotFound) {
				t.Error(err)
			}
		}(db)
	}
	wg.Wait()
	close(claimed)
	if len(claimed) != 1 {
		t.Fatalf("concurrent claims = %d", len(claimed))
	}
	winner := <-claimed
	winner.Job.State = "delivered"
	if err := st.FinishNotification(ctx, winner.Job, time.Now()); err != nil {
		t.Fatal(err)
	}
	// Reclaim an abandoned lease and fence its previous worker.
	j := domain.NotificationJob{ID: domain.NewID("evt_"), WorkspaceID: wsp.ID, Kind: "ref_updated", State: "pending", Text: "lease", CreatedAt: time.Now().UTC(), NextAttempt: due}
	if err := st.EnqueueNotification(ctx, outbound.NotificationDelivery{Job: j, Destination: wsp.WebhookURL}); err != nil {
		t.Fatal(err)
	}
	old, err := st.ClaimNotification(ctx, time.Now(), time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(5 * time.Millisecond)
	current, err := peer.ClaimNotification(ctx, time.Now(), time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	old.Job.State = "delivered"
	if err := st.FinishNotification(ctx, old.Job, time.Now()); !errors.Is(err, domain.ErrRefConflict) {
		t.Fatal("expired worker overwrote claim")
	}
	current.Job.State = "delivered"
	if err := peer.FinishNotification(ctx, current.Job, time.Now()); err != nil {
		t.Fatal(err)
	}
}
