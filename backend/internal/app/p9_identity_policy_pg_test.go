//go:build postgres

package app

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/wnsdy95/cxthub/backend/internal/adapters/store"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

func TestP9PGIdentityAdminOptInSerializes(t *testing.T) {
	for _, order := range []string{"opt-in-first", "mutation-first"} {
		t.Run(order, func(t *testing.T) {
			_, st, repo := collaborationPG(t)
			ctx, cancel := context.WithTimeout(systemTestContext(), 10*time.Second)
			defer cancel()
			f := makeTeamFixture(t, st)
			if _, err := st.PutRepo(ctx, domain.Repo{ID: repo, RepositoryID: f.repository.ID}); err != nil {
				t.Fatal(err)
			}
			peer, err := store.NewPostgresStore(ctx, collaborationDSN(t))
			if err != nil {
				t.Fatal(err)
			}
			defer peer.Close()
			identity := NewIdentityService(nil, peer)
			entered, release := make(chan struct{}), make(chan struct{})
			first, second := make(chan error, 1), make(chan error, 1)
			mutation := func(ctx context.Context, s *IdentityService) error {
				v := true
				_, err := s.UpdateRepositorySettings(ctx, f.owner.ID, f.repository.ID, RepositoryPatch{Archived: &v})
				return err
			}
			if order == "opt-in-first" {
				go func() {
					first <- st.WithinRepository(ctx, repo, func(tx context.Context) error {
						if err := st.RequireDocumentIdentity(tx, repo, domain.DocumentIdentityRootV1); err != nil {
							return err
						}
						close(entered)
						<-release
						return nil
					})
				}()
				<-entered
				go func() { second <- mutation(ctx, identity) }()
			} else {
				go func() {
					first <- st.WithinIdentity(ctx, func(tx context.Context) error {
						if err := mutation(tx, f.identity); err != nil {
							return err
						}
						close(entered)
						<-release
						return nil
					})
				}()
				<-entered
				go func() { second <- peer.RequireDocumentIdentity(ctx, repo, domain.DocumentIdentityRootV1) }()
			}
			select {
			case err := <-second:
				close(release)
				t.Fatal("crossed held boundary", err)
			case <-time.After(30 * time.Millisecond):
			}
			close(release)
			if err := <-first; err != nil {
				t.Fatal(err)
			}
			err = <-second
			if order == "opt-in-first" {
				if !errors.Is(err, domain.ErrDocumentIdentityUpgradeRequired) {
					t.Fatal(err)
				}
			} else if err != nil {
				t.Fatal(err)
			}
			current, err := st.GetRepository(ctx, f.repository.ID)
			if err != nil {
				t.Fatal(err)
			}
			if current.Archived != (order == "mutation-first") {
				t.Fatal("wrong admission order", current.Archived)
			}
			if err := mutation(ctx, identity); !errors.Is(err, domain.ErrDocumentIdentityUpgradeRequired) {
				t.Fatal("late mutation", err)
			}
		})
	}
}
