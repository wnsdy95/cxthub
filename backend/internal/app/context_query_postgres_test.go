//go:build postgres

package app

import (
	"context"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/wnsdy95/cxthub/backend/internal/adapters/auth"
	"github.com/wnsdy95/cxthub/backend/internal/adapters/gitengine"
	"github.com/wnsdy95/cxthub/backend/internal/adapters/store"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

type pausedQueryStore struct {
	*store.PostgresStore
	started, release chan struct{}
	once             sync.Once
}

func (s *pausedQueryStore) GetRepo(ctx context.Context, id domain.ContentHash) (domain.Repo, error) {
	r, err := s.PostgresStore.GetRepo(ctx, id)
	s.once.Do(func() {
		close(s.started)
		select {
		case <-s.release:
		case <-ctx.Done():
		}
	})
	return r, err
}

func TestPGContextQueryPinsPositionAndRevisionAcrossConcurrentWriter(t *testing.T) {
	dsn := os.Getenv("CXT_TEST_DSN")
	if dsn == "" {
		t.Skip("CXT_TEST_DSN unset")
	}
	ctx, cancel := context.WithTimeout(systemTestContext(), 20*time.Second)
	defer cancel()
	st, err := store.NewPostgresStore(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if _, err = st.ApplyMigrations(ctx, "../../../schemas/db/migrations"); err != nil {
		t.Fatal(err)
	}
	repo := domain.HashContent([]byte(t.Name() + time.Now().String()))
	if _, err = st.PutRepo(ctx, domain.Repo{ID: repo, DefaultBranch: "trunk"}); err != nil {
		t.Fatal(err)
	}
	var ids []domain.ContentHash
	for _, text := range []string{"old", "new"} {
		doc := domain.SessionDoc{CIR: domain.CIRDocument{Envelope: domain.CIREnvelope{CIRVersion: "1", SourceProvider: domain.ProviderCodex}, Events: []domain.CIREvent{{Kind: domain.EventMessage, Role: "user", Blocks: []domain.ContentBlock{{Type: "text", Text: string(repo) + text}}}}}}
		raw, _ := domain.CanonicalBytes(doc.CIR)
		doc.Hash = domain.HashContent(raw)
		if _, err = st.PutDoc(ctx, repo, doc); err != nil {
			t.Fatal(err)
		}
		if err = st.PutSnapshot(ctx, domain.Snapshot{ID: doc.Hash, RepoID: repo, DocHash: doc.Hash, Provider: domain.ProviderCodex, Fidelity: domain.FidelityFull}); err != nil {
			t.Fatal(err)
		}
		ids = append(ids, doc.Hash)
	}
	ref := domain.Ref{Kind: domain.RefBranch, Name: "trunk", RepoID: repo, Target: ids[0]}
	if err = st.CompareAndSwapRef(ctx, repo, ref, ""); err != nil {
		t.Fatal(err)
	}
	before, err := st.RepositoryRevision(ctx, repo)
	if err != nil {
		t.Fatal(err)
	}
	paused := &pausedQueryStore{PostgresStore: st, started: make(chan struct{}), release: make(chan struct{})}
	svc := NewService(paused, st, auth.NewTeamTokenAuth(), gitengine.NewEngine(st), st)
	type result struct {
		view domain.ContextQueryView
		err  error
	}
	done := make(chan result, 1)
	go func() {
		v, e := svc.QueryContext(ctx, repo, domain.ContextSelection{Position: "HEAD", Scope: "current"})
		done <- result{v, e}
	}()
	select {
	case <-paused.started:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	ref.Target = ids[1]
	err = st.WithinRepository(ctx, repo, func(tx context.Context) error {
		if err := st.CompareAndSwapRef(tx, repo, ref, ids[0]); err != nil {
			return err
		}
		return st.AdvanceRepositoryRevision(tx, repo, false)
	})
	close(paused.release)
	if err != nil {
		t.Fatal(err)
	}
	select {
	case got := <-done:
		if got.err != nil || got.view.Position != ids[0] || got.view.Revision != before || len(got.view.Snapshots) != 1 {
			t.Fatalf("torn generation: %+v %v", got.view, got.err)
		}
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	after, err := svc.QueryContext(ctx, repo, domain.ContextSelection{Position: "HEAD", Scope: "current"})
	if err != nil || after.Position != ids[1] || after.Revision.Graph <= before.Graph {
		t.Fatalf("next generation missing: %+v %v", after, err)
	}
}

func TestPGGraphQueriesPinFactsAndRevisionAcrossConcurrentWriter(t *testing.T) {
	for _, mode := range []string{"view", "pending", "position"} {
		t.Run(mode, func(t *testing.T) {
			dsn := os.Getenv("CXT_TEST_DSN")
			if dsn == "" {
				t.Skip("CXT_TEST_DSN unset")
			}
			ctx, cancel := context.WithTimeout(systemTestContext(), 20*time.Second)
			defer cancel()
			st, err := store.NewPostgresStore(ctx, dsn)
			if err != nil {
				t.Fatal(err)
			}
			defer st.Close()
			if _, err = st.ApplyMigrations(ctx, "../../../schemas/db/migrations"); err != nil {
				t.Fatal(err)
			}
			repo := domain.HashContent([]byte(t.Name() + time.Now().String()))
			if _, err = st.PutRepo(ctx, domain.Repo{ID: repo, DefaultBranch: "trunk"}); err != nil {
				t.Fatal(err)
			}
			var ids []domain.ContentHash
			for _, text := range []string{"old", "new"} {
				doc := domain.SessionDoc{CIR: domain.CIRDocument{Envelope: domain.CIREnvelope{CIRVersion: "1", SourceProvider: domain.ProviderCodex}, Events: []domain.CIREvent{{Kind: domain.EventMessage, Role: "user", Blocks: []domain.ContentBlock{{Type: "text", Text: string(repo) + text}}}}}}
				raw, _ := domain.CanonicalBytes(doc.CIR)
				doc.Hash = domain.HashContent(raw)
				if _, err = st.PutDoc(ctx, repo, doc); err != nil {
					t.Fatal(err)
				}
				if err = st.PutSnapshot(ctx, domain.Snapshot{ID: doc.Hash, RepoID: repo, DocHash: doc.Hash, Provider: domain.ProviderCodex, Fidelity: domain.FidelityFull}); err != nil {
					t.Fatal(err)
				}
				ids = append(ids, doc.Hash)
			}
			ref := domain.Ref{Kind: domain.RefBranch, Name: "trunk", RepoID: repo, Target: ids[0]}
			if err = st.CompareAndSwapRef(ctx, repo, ref, ""); err != nil {
				t.Fatal(err)
			}
			before, err := st.RepositoryRevision(ctx, repo)
			if err != nil {
				t.Fatal(err)
			}
			paused := &pausedQueryStore{PostgresStore: st, started: make(chan struct{}), release: make(chan struct{})}
			svc := NewService(paused, st, auth.NewTeamTokenAuth(), gitengine.NewEngine(st), st)
			type result struct {
				view domain.GraphState
				err  error
			}
			done := make(chan result, 1)
			read := func() (domain.GraphState, error) {
				switch mode {
				case "view":
					v, e := svc.GetRepositoryView(ctx, repo)
					if e != nil {
						return domain.GraphState{}, e
					}
					return *v.Graph, nil
				case "pending":
					v, e := svc.GetPendingView(ctx, repo)
					if e != nil {
						return domain.GraphState{}, e
					}
					return *v.Graph, nil
				default:
					return svc.QueryGraphState(ctx, repo, "")
				}
			}
			go func() {
				v, e := read()
				done <- result{v, e}
			}()
			select {
			case <-paused.started:
			case <-ctx.Done():
				t.Fatal(ctx.Err())
			}
			ref.Target = ids[1]
			err = st.WithinRepository(ctx, repo, func(tx context.Context) error {
				if err := st.CompareAndSwapRef(tx, repo, ref, ids[0]); err != nil {
					return err
				}
				return st.AdvanceRepositoryRevision(tx, repo, false)
			})
			close(paused.release)
			if err != nil {
				t.Fatal(err)
			}
			select {
			case got := <-done:
				if got.err != nil || len(got.view.PushedIDs) != 1 || got.view.PushedIDs[0] != ids[0] || got.view.Revision != before || len(got.view.BranchSnapshots["trunk"]) != 1 {
					t.Fatalf("torn generation: %+v %v", got.view, got.err)
				}
			case <-ctx.Done():
				t.Fatal(ctx.Err())
			}
			after, err := read()
			if err != nil || len(after.BranchSnapshots["trunk"]) != 1 || after.BranchSnapshots["trunk"][0] != ids[1] || len(after.PushedIDs) != 2 || after.Revision.Graph <= before.Graph {
				t.Fatalf("next generation missing: %+v %v", after, err)
			}

		})
	}
}
