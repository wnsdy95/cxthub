package app

import (
	"context"
	"errors"
	"github.com/wnsdy95/cxthub/backend/internal/adapters/store"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/inbound"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
	"testing"
	"time"
)

type historyCompatibilityReadKey struct{}

var historyCompatibilityStop = errors.New("review capture stop; never mutate history")

type historyCompatibilityHistoryBoundary struct {
	*store.FSStore
	captureCalls         int
	captureCompatibility error
}

func (p *historyCompatibilityHistoryBoundary) WithinRepository(ctx context.Context, _ domain.ContentHash, fn func(context.Context) error) error {
	return fn(ctx)
}
func (p *historyCompatibilityHistoryBoundary) WithinReadSnapshot(ctx context.Context, fn func(context.Context) error) error {
	return fn(context.WithValue(ctx, historyCompatibilityReadKey{}, true))
}
func (p *historyCompatibilityHistoryBoundary) InReadOnlyTransaction(ctx context.Context) bool {
	return ctx.Value(historyCompatibilityReadKey{}) == true
}
func (p *historyCompatibilityHistoryBoundary) WithinHistoryDocumentProof(ctx context.Context, _ domain.ContentHash, fn func(context.Context, outbound.HistoryDocumentProof) error) error {
	return fn(ctx, p)
}
func (p *historyCompatibilityHistoryBoundary) Capture(ctx context.Context, snaps []domain.Snapshot) error {
	p.captureCalls++
	if len(snaps) != 1 || snaps[0].DocIdentity != domain.DocumentIdentityRootV1 {
		return domain.ErrIntegrity
	}
	p.captureCompatibility = outbound.CheckDocumentIdentityCompatibility(ctx, domain.DocumentIdentityRootV1)
	if p.captureCompatibility != nil {
		return p.captureCompatibility
	}
	return historyCompatibilityStop
}
func (p *historyCompatibilityHistoryBoundary) Pin(context.Context) error {
	panic("review capture stop must prevent pin")
}
func TestRootHistoryForwardsCompatibility(t *testing.T) {
	for _, peer := range []bool{false, true} {
		for _, inject := range []bool{false, true} {
			name := "inbound-only"
			if inject {
				name = "explicit-outbound-control"
			}
			if !peer {
				name += "/old-peer"
			}
			t.Run(name, func(t *testing.T) {
				dir := t.TempDir()
				fs := store.NewFSStore(dir)
				repo := hh(t.Name())
				bindCommitTestRepo(t, fs, repo)
				doc, _ := seedRootReadDoc(t, dir, fs, repo, historyMessage(domain.RoleUser, "Synthetic reviewer history root"))
				if err := fs.RequireDocumentIdentity(context.Background(), repo, domain.DocumentIdentityRootV1); err != nil {
					t.Fatal(err)
				}
				probe := &historyCompatibilityHistoryBoundary{FSStore: fs}
				svc := NewService(probe, probe, nil, nil, nil)
				ids := []domain.DocumentIdentity{domain.DocumentIdentityRootV1}
				ctx := systemTestContext()
				if peer {
					ctx = inbound.WithDocumentIdentities(ctx, ids)
				}
				if inject {
					ctx = outbound.WithDocumentIdentityCompatibility(ctx, ids, ids)
				}
				event := domain.HistoryEvent{ID: "00000000000000000000000000000001", RepoID: string(repo), Branch: "main", BranchID: "main", Kind: "position", Source: doc.Hash, Target: doc.Hash, SharedTarget: doc.Hash, CreatedAt: time.Now().UTC()}
				err := svc.RecordHistory(ctx, event)
				if !conversationRootReleaseReady || !peer {
					if !errors.Is(err, domain.ErrDocumentIdentityUpgradeRequired) || probe.captureCalls != 0 {
						t.Fatalf("incompatible source reached Capture: calls=%d err=%v", probe.captureCalls, err)
					}
					return
				}
				if probe.captureCalls != 1 {
					t.Fatalf("did not reach exact root capture after current-byte proof: calls=%d err=%v", probe.captureCalls, err)
				}
				if probe.captureCompatibility != nil {
					t.Fatalf("public inbound declaration lost before root Capture: %v (RecordHistory=%v)", probe.captureCompatibility, err)
				}
				if !errors.Is(err, historyCompatibilityStop) {
					t.Fatalf("capture sentinel changed: %v", err)
				}
			})
		}
	}

}
