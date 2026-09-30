//go:build postgres

package store

import (
	"context"
	"errors"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

type contextWritePGCounts struct {
	protocol, history, refs atomic.Int32
}
type contextWritePGCountKey struct{}
type contextWritePGTracer struct{}

func (contextWritePGTracer) TraceQueryStart(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryStartData) context.Context {
	counts, ok := ctx.Value(contextWritePGCountKey{}).(*contextWritePGCounts)
	if !ok {
		return ctx
	}
	sql := strings.ToLower(strings.Join(strings.Fields(data.SQL), " "))
	if strings.HasPrefix(sql, "select ") {
		switch {
		case strings.Contains(sql, " from repos "):
			counts.protocol.Add(1)
		case strings.Contains(sql, " from context_history "):
			counts.history.Add(1)
		case strings.Contains(sql, " from refs "):
			counts.refs.Add(1)
		}
	}
	return ctx
}

func (contextWritePGTracer) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {}

func assertContextWritePGCounts(t *testing.T, counts *contextWritePGCounts, protocol, history, refs int32) {
	t.Helper()
	p, h, r := counts.protocol.Load(), counts.history.Load(), counts.refs.Load()
	t.Logf("protocol=%d history=%d current-ref=%d", p, h, r)
	if p != protocol || h != history || r != refs {
		t.Fatalf("want protocol=%d history=%d current-ref=%d", protocol, history, refs)
	}
}

func TestPGContextWritePreflight(t *testing.T) {
	dsn := os.Getenv("CXT_TEST_DSN")
	if dsn == "" {
		t.Skip("requires a disposable CXT_TEST_DSN")
	}
	ctx := context.Background()
	base, err := NewPostgresStore(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer base.Close()
	if _, err := base.ApplyMigrations(ctx, "../../../../schemas/db/migrations"); err != nil {
		t.Fatal(err)
	}
	config := base.pool.Config()
	config.ConnConfig.Tracer = contextWritePGTracer{}
	pool, err := pgxpool.NewWithConfig(ctx, config)
	if err != nil {
		t.Fatal(err)
	}
	s := &PostgresStore{pool: pool}
	defer s.Close()
	repo := domain.HashContent([]byte(t.Name() + time.Now().String()))
	if _, err := s.PutRepo(ctx, domain.Repo{ID: repo}); err != nil {
		t.Fatal(err)
	}
	if err := s.EnableContextProtocol(ctx, repo); err != nil {
		t.Fatal(err)
	}
	doc := domain.SessionDoc{CIR: domain.CIRDocument{Envelope: domain.CIREnvelope{CIRVersion: "1", SourceProvider: domain.ProviderClaude, Fidelity: domain.FidelityFull}}}
	raw, err := domain.CanonicalBytes(doc.CIR)
	if err != nil {
		t.Fatal(err)
	}
	doc.Hash = domain.HashContent(raw)
	if _, err := s.PutDoc(ctx, repo, doc); err != nil {
		t.Fatal(err)
	}
	target := doc.Hash
	if err := s.PutSnapshot(ctx, domain.Snapshot{RepoID: repo, ID: target, DocHash: target, Provider: domain.ProviderClaude, Fidelity: domain.FidelityFull}); err != nil {
		t.Fatal(err)
	}
	for _, kind := range []domain.RefKind{domain.RefTag, domain.RefSession, domain.RefHead} {
		t.Run(string(kind), func(t *testing.T) {
			ref := domain.Ref{RepoID: repo, Kind: kind, Name: "pointer", Target: target}
			if kind == domain.RefHead {
				ref.Name = domain.HeadRefName
			}
			counts := &contextWritePGCounts{}
			counted := context.WithValue(ctx, contextWritePGCountKey{}, counts)
			if err := s.CompareAndSwapRef(counted, repo, ref, ""); err != nil {
				t.Fatal(err)
			}
			// Includes the second protocol read in CompareAndSwapRef; the
			// conditional INSERT/UPDATE still owns the actual CAS decision.
			assertContextWritePGCounts(t, counts, 2, 0, 0)
			if err := s.CompareAndSwapRef(ctx, repo, ref, ""); !errors.Is(err, domain.ErrRefConflict) {
				t.Fatal("CAS conflict lost", err)
			}
			stop := errors.New("rollback nonbranch write")
			next := ref
			next.Symbolic = ""
			err := s.WithinRepository(ctx, repo, func(bound context.Context) error {
				next.Name = "rolled-back"
				if kind == domain.RefHead {
					next.Name, next.Target, next.Symbolic = domain.HeadRefName, "", "main"
				}
				expected := domain.ContentHash("")
				if kind == domain.RefHead {
					expected = ref.Target
				}
				if err := s.CompareAndSwapRef(bound, repo, next, expected); err != nil {
					return err
				}
				return stop
			})
			if !errors.Is(err, stop) {
				t.Fatal(err)
			}
			got, err := s.GetRef(ctx, repo, next.Kind, next.Name)
			if kind == domain.RefHead {
				if err != nil || got.Target != ref.Target || got.Symbolic != "" {
					t.Fatalf("rollback changed HEAD: %+v %v", got, err)
				}
			} else if !errors.Is(err, domain.ErrNotFound) {
				t.Fatal("rollback leaked pointer", err)
			}
		})
	}

	t.Run("branch state and injected read errors", func(t *testing.T) {
		birth := domain.HistoryEvent{ID: strings.Repeat("a", 32), RepoID: string(repo), Kind: "orphan", Branch: "branch", BranchID: "identity", CreatedAt: time.Now().UTC()}
		if err := s.ApplyHistoryEvent(ctx, birth); err != nil {
			t.Fatal(err)
		}
		ref := domain.Ref{RepoID: repo, Kind: domain.RefBranch, Name: birth.Branch, BranchID: birth.BranchID, Target: target}
		counts := &contextWritePGCounts{}
		counted := context.WithValue(ctx, contextWritePGCountKey{}, counts)
		if err := s.CompareAndSwapRef(counted, repo, ref, ""); err != nil {
			t.Fatal(err)
		}
		assertContextWritePGCounts(t, counts, 2, 1, 1)
		tx, err := s.pool.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer tx.Rollback(ctx)
		// Transaction-local corruption guarantees history decoding would fail.
		if _, err := tx.Exec(ctx, `UPDATE context_history SET event='"unreadable history"'::jsonb WHERE repo_id=$1`, string(repo)); err != nil {
			t.Fatal(err)
		}
		for _, kind := range []domain.RefKind{domain.RefTag, domain.RefSession, domain.RefHead} {
			next := ref
			next.Kind = kind
			if err := validateContextWritePG(ctx, tx, repo, next); err != nil {
				t.Fatalf("%s read corrupt history: %v", kind, err)
			}
		}
		missingID := ref
		missingID.BranchID = ""
		if err := validateContextWritePG(ctx, tx, repo, missingID); !errors.Is(err, domain.ErrConflict) {
			t.Fatal("identity preflight lost", err)
		}
		if err := validateContextWritePG(ctx, tx, repo, ref); err == nil {
			t.Fatal("branch did not read history")
		}
	})

	t.Run("repository and unsupported protocol", func(t *testing.T) {
		tx, err := s.pool.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer tx.Rollback(ctx)
		ref := domain.Ref{RepoID: repo, Kind: domain.RefTag, Name: "tag", Target: target}
		if err := validateContextWritePG(ctx, tx, domain.HashContent([]byte("absent repo")), ref); !errors.Is(err, domain.ErrNotFound) {
			t.Fatal("missing repository accepted", err)
		}
		// Shadow repos only in this transaction to exercise future versions
		// without relaxing the durable schema's current (0,1) constraint.
		if _, err := tx.Exec(ctx, `CREATE TEMP TABLE repos (id text, context_protocol integer) ON COMMIT DROP`); err != nil {
			t.Fatal(err)
		}
		if _, err := tx.Exec(ctx, `INSERT INTO repos VALUES ($1,2)`, string(repo)); err != nil {
			t.Fatal(err)
		}
		counts := &contextWritePGCounts{}
		counted := context.WithValue(ctx, contextWritePGCountKey{}, counts)
		if err := validateContextWritePG(counted, tx, repo, ref); !errors.Is(err, domain.ErrConflict) {
			t.Fatal("unsupported protocol accepted", err)
		}
		assertContextWritePGCounts(t, counts, 1, 0, 0)
	})
}
