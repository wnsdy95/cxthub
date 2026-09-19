package app

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/wnsdy95/cxthub/backend/internal/adapters/store"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/inbound"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

func secretsTestEnvelope(fp string) []byte {
	return []byte(`{"version":1,"kdf":"PBKDF2-SHA256","iterations":600000,"salt_b64":"AAAAAAAAAAAAAAAAAAAAAA==","cipher":"AES-256-GCM","nonce_b64":"AAAAAAAAAAAAAAAA","ciphertext_b64":"AAAAAAAAAAAAAAAAAAAAAA==","fingerprint":"` + fp + `"}`)
}

func seedSecretsRepo(t *testing.T, meta outbound.MetadataStore, ws outbound.WorkspaceStore) (domain.Workspace, inbound.SaveSecretsInput) {
	t.Helper()
	ctx := context.Background()
	owner := domain.User{ID: domain.NewID("user_"), Username: fmt.Sprintf("secrets%d", time.Now().UnixNano()), Email: "secrets@example.test", Name: "Owner"}
	if err := ws.UpsertUser(ctx, owner); err != nil {
		t.Fatal(err)
	}
	wsp := domain.Workspace{ID: domain.NewID("ws_"), Name: "Secrets", Slug: "secrets", OwnerID: owner.ID, OwnerUsername: owner.Username, CreatedAt: time.Now().UTC()}
	if err := ws.CreateWorkspace(ctx, wsp); err != nil {
		t.Fatal(err)
	}
	repo := domain.HashContent([]byte(wsp.ID))
	if _, err := meta.PutRepo(ctx, domain.Repo{ID: repo, WorkspaceID: wsp.ID}); err != nil {
		t.Fatal(err)
	}
	return wsp, inbound.SaveSecretsInput{RepoID: repo, ActorID: owner.ID, Envelope: secretsTestEnvelope("aaaaaaaaaaaa"), Edit: domain.SecretsEdit{ExpectedRevision: "absent"}}
}

func TestSecretsCommandRejectsChangedPassphrase(t *testing.T) {
	ctx := context.Background()
	st := store.NewFSStore(t.TempDir())
	_, in := seedSecretsRepo(t, st, st)
	svc := NewService(st, st, nil, nil, st)
	first, err := svc.SaveSecrets(ctx, in)
	if err != nil {
		t.Fatal(err)
	}
	original, _ := st.GetSecretsEnvelope(ctx, in.RepoID)
	in.Edit.ExpectedRevision = first.Revision
	in.Envelope = secretsTestEnvelope("bbbbbbbbbbbb")
	if _, err := svc.SaveSecrets(ctx, in); !errors.Is(err, domain.ErrSecretsPassphraseMismatch) {
		t.Fatalf("direct application write: %v", err)
	}
	after, _ := st.GetSecretsEnvelope(ctx, in.RepoID)
	if !bytes.Equal(original, after) {
		t.Fatal("rejected write modified ciphertext")
	}
	in.Edit.Rotate = true
	in.Edit.ExpectedFingerprint = "aaaaaaaaaaaa"
	second, err := svc.SaveSecrets(ctx, in)
	if err != nil {
		t.Fatal(err)
	}
	if second.Revision == first.Revision {
		t.Fatal("rotation reused generation")
	}
	if _, err := svc.SaveSecrets(ctx, in); !errors.Is(err, domain.ErrSecretsConflict) {
		t.Fatalf("old editing baseline accepted: %v", err)
	}
}

func TestSecretsCommandAuthorizesInsideApplication(t *testing.T) {
	for _, tc := range []struct {
		name                       string
		role                       domain.MemberRole
		policy                     string
		archived, creator, allowed bool
	}{
		{name: "creator", creator: true, allowed: true},
		{name: "co-owner", role: domain.RoleOwner, policy: "owner", allowed: true},
		{name: "maintainer", role: domain.RoleMaintainer, allowed: true},
		{name: "maintainer owner-only", role: domain.RoleMaintainer, policy: "owner"},
		{name: "member", role: domain.RoleMember}, {name: "public puller", role: domain.RolePuller},
		{name: "outsider"}, {name: "archived owner", creator: true, archived: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			st := store.NewFSStore(t.TempDir())
			wsp, in := seedSecretsRepo(t, st, st)
			wsp.SecretsPolicy = tc.policy
			wsp.Archived = tc.archived
			wsp.Visibility = domain.VisibilityPublic
			wsp.PublicRole = "puller"
			if err := st.CreateWorkspace(ctx, wsp); err != nil {
				t.Fatal(err)
			}
			if !tc.creator {
				in.ActorID = domain.NewID("user_")
				if tc.role != "" {
					if err := st.AddMember(ctx, domain.Membership{WorkspaceID: wsp.ID, UserID: in.ActorID, Role: tc.role}); err != nil {
						t.Fatal(err)
					}
				}
			}
			_, err := NewService(st, st, nil, nil, st).SaveSecrets(ctx, in)
			if tc.allowed {
				if err != nil {
					t.Fatal(err)
				}
			} else {
				if !errors.Is(err, domain.ErrForbidden) {
					t.Fatalf("authorization: %v", err)
				}
				if _, err := st.GetSecretsEnvelope(ctx, in.RepoID); !errors.Is(err, domain.ErrNotFound) {
					t.Fatal("forbidden command stored envelope")
				}
			}
		})
	}
}

type secretsReadFailure struct{ *store.FSStore }

func (s secretsReadFailure) GetSecretsEnvelope(context.Context, domain.ContentHash) ([]byte, error) {
	return nil, context.DeadlineExceeded
}

func TestSecretsCommandFailsClosed(t *testing.T) {
	ctx := context.Background()
	st := store.NewFSStore(t.TempDir())
	_, in := seedSecretsRepo(t, st, st)
	broken := secretsReadFailure{st}
	svc := NewService(broken, st, nil, nil, st)
	if _, err := svc.SaveSecrets(ctx, in); !errors.Is(err, domain.ErrSecretsConsistency) || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("read failure: %v", err)
	}
	if _, err := st.GetSecretsEnvelope(ctx, in.RepoID); !errors.Is(err, domain.ErrNotFound) {
		t.Fatal("wrote without baseline read")
	}
	in.ActorID = ""
	if _, err := svc.SaveSecrets(ctx, in); !errors.Is(err, domain.ErrUnauthorized) {
		t.Fatalf("anonymous command: %v", err)
	}
}

func TestSecretsConcurrentSameBaseline(t *testing.T) {
	ctx := context.Background()
	st := store.NewFSStore(t.TempDir())
	_, in := seedSecretsRepo(t, st, st)
	svc := NewService(st, st, nil, nil, st)
	first, err := svc.SaveSecrets(ctx, in)
	if err != nil {
		t.Fatal(err)
	}
	in.Edit.ExpectedRevision = first.Revision
	var wg sync.WaitGroup
	start := make(chan struct{})
	results := make(chan error, 8)
	for range 8 {
		wg.Add(1)
		go func() { defer wg.Done(); <-start; _, err := svc.SaveSecrets(ctx, in); results <- err }()
	}
	close(start)
	wg.Wait()
	close(results)
	successes := 0
	for err := range results {
		if err == nil {
			successes++
		} else if !errors.Is(err, domain.ErrSecretsConflict) {
			t.Fatal(err)
		}
	}
	if successes != 1 {
		t.Fatalf("accepted %d simultaneous edits", successes)
	}
	stored, _ := st.GetSecretsEnvelope(ctx, in.RepoID)
	if domain.SecretsRevision(stored) == first.Revision {
		t.Fatal("same-key edit did not change generation")
	}
}
