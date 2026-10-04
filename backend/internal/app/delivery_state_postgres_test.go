//go:build postgres

package app

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/wnsdy95/cxthub/backend/internal/adapters/auth"
	"github.com/wnsdy95/cxthub/backend/internal/adapters/gitengine"
	"github.com/wnsdy95/cxthub/backend/internal/adapters/store"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

func TestPGEffectiveMemoryDeliveryCacheRawWriteRollback(t *testing.T) {
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
	if _, err := st.ApplyMigrations(ctx, "../../../schemas/db/migrations"); err != nil {
		t.Fatal(err)
	}
	svc := NewService(st, st, auth.NewTeamTokenAuth(), gitengine.NewEngine(st), st)
	repo, origin, code := hh(t.Name()+time.Now().String()), "https://github.com/example/delivery-cache", effectiveOID(1)
	if _, err := st.PutRepo(ctx, domain.Repo{ID: repo, GitRemoteURL: origin}); err != nil {
		t.Fatal(err)
	}
	doc := domain.SessionDoc{CIR: domain.CIRDocument{Envelope: domain.CIREnvelope{CIRVersion: "1", SourceProvider: domain.ProviderCodex, SessionOriginID: string(repo)}}}
	raw, err := domain.CanonicalBytes(doc.CIR)
	if err != nil {
		t.Fatal(err)
	}
	doc.Hash = domain.HashContent(raw)
	if _, err := st.PutDoc(ctx, repo, doc); err != nil {
		t.Fatal(err)
	}
	if err := st.PutSnapshot(ctx, domain.Snapshot{ID: doc.Hash, RepoID: repo, DocHash: doc.Hash, Provider: domain.ProviderCodex, Fidelity: domain.FidelityFull}); err != nil {
		t.Fatal(err)
	}
	digest := domain.MemoryDigest{SnapshotID: doc.Hash, ClaimsVersion: 1, Fragments: []domain.MemoryFragment{{SourceSnapshot: doc.Hash, Claims: []domain.MemoryClaim{{Kind: "code", Text: "synthetic code claim", Code: &domain.MemoryCodeScope{Commit: code, Paths: []string{"file"}}}}}}}
	if _, err := svc.PutMemoryDigestCAS(ctx, repo, digest); err != nil {
		t.Fatal(err)
	}
	event := domain.HistoryEvent{ID: strings.Repeat("d", 32), RepoID: string(repo), Branch: "main", BranchID: "main-id", Kind: "position", Target: doc.Hash, GitAfter: code, CreatedAt: time.Now().UTC()}
	if err := st.ApplyHistoryEvent(ctx, event); err != nil {
		t.Fatal(err)
	}
	if err := st.ApplyHistoryEvent(ctx, prPublication(event)); err != nil {
		t.Fatal(err)
	}
	if err := st.AdvanceRepositoryRevision(ctx, repo, false); err != nil {
		t.Fatal(err)
	}
	job := domain.NewGitScan(repo, origin, code, time.Now().UTC())
	if err := st.EnqueueGitScan(ctx, job); err != nil {
		t.Fatal(err)
	}
	job, err = st.ClaimGitScan(ctx, repo, time.Now().UTC(), time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	node, err := domain.NewGitTreeNode(map[string]domain.GitEntry{}, 40)
	if err != nil {
		t.Fatal(err)
	}
	finish := domain.GitScanFinish{Job: job, Tree: &domain.GitTreeEvidence{Commit: domain.GitCommitTree{Commit: code, Tree: node.OID, Parents: []string{}}, Nodes: []domain.GitTreeNode{node}}}
	finish.Job.State, finish.Job.TreeIndexed = "waiting", true
	req := domain.EffectiveMemoryRequest{Selection: domain.EffectiveMemorySelection{SnapshotID: doc.Hash, CodeCommit: code}, Limit: 1}
	baseline, err := svc.QueryEffectiveMemory(ctx, repo, req)
	if err != nil || baseline.DeliveryStateHash == "" {
		t.Fatal("initial proof", err)
	}
	if _, ok := svc.deliveryCache.get(baseline.StateHash); !ok {
		t.Fatal("read-only snapshot did not cache")
	}
	rollback := errors.New("synthetic rollback")
	for _, warm := range []bool{true, false} {
		if !warm {
			svc.deliveryCache.entries = nil
		}
		entries := len(svc.deliveryCache.entries)
		err = st.WithinRepository(ctx, repo, func(bound context.Context) error {
			if _, marked := bound.Value(afterCommitKey{}).(*afterCommitActions); marked {
				t.Fatal("fixture unexpectedly uses application write marker")
			}
			if st.InReadOnlyTransaction(bound) {
				t.Fatal("write advertised as read-only")
			}
			// Raw storage has inserted evidence but has not advanced its clock.
			if e := st.FinishGitScan(bound, finish); e != nil {
				return e
			}
			uncommitted, e := svc.QueryEffectiveMemory(bound, repo, req)
			if e != nil {
				return e
			}
			if uncommitted.StateHash != baseline.StateHash || uncommitted.Revision != baseline.Revision {
				t.Fatal("fixture did not preserve the cache key")
			}
			if uncommitted.DeliveryStateHash == baseline.DeliveryStateHash {
				t.Fatal("write reused committed cache despite changed evidence")
			}
			if len(svc.deliveryCache.entries) != entries {
				t.Fatal("raw write published uncommitted proof")
			}
			return rollback
		})
		if !errors.Is(err, rollback) {
			t.Fatal("rollback failed", err)
		}
		restored, err := svc.QueryEffectiveMemory(ctx, repo, req)
		if err != nil || restored.StateHash != baseline.StateHash || restored.DeliveryStateHash != baseline.DeliveryStateHash {
			t.Fatal("rolled-back evidence poisoned committed proof", err)
		}
		if _, err := st.GetGitCommitTree(ctx, repo, origin, code); !errors.Is(err, domain.ErrNotFound) {
			t.Fatal("evidence survived rollback", err)
		}
	}
}
