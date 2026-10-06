package cli

import (
	"context"
	"testing"
	"time"

	"github.com/wnsdy95/cxthub/cli/internal/adapters/storage"
	"github.com/wnsdy95/cxthub/cli/internal/app"
	"github.com/wnsdy95/cxthub/cli/internal/domain"
	"github.com/wnsdy95/cxthub/cli/internal/ports/inbound"
	"github.com/wnsdy95/cxthub/cli/internal/ports/outbound"
)

type incomingPromotionDowngradeRemote struct {
	*incomingTransferRemote
	capabilities int
}

// Expose only the public sync contract to exercise the briefing fallback when
// the optional pointer-only reader is absent.
type incomingBriefingFallback struct{ inbound.SyncRepo }

func (r *incomingPromotionDowngradeRemote) PullCapabilities(context.Context, string) (outbound.PullCapabilities, error) {
	r.capabilities++
	version := 1
	if r.capabilities >= 3 {
		version = 0
	}
	return outbound.PullCapabilities{BranchPlanVersion: version}, nil
}

// Real discovery, selected pull, FileStore, generic append entrypoint and
// resolver. Synthetic transport only; no native Git, sockets, or providers.
func TestIncomingPromotionObservationCannotBroadenAfterScopedDiscovery(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	ctx := context.Background()
	dir := t.TempDir()
	repo := string(domain.HashContent([]byte("review423 repo")))
	st := storage.NewFileStore(dir)
	remote := &incomingPromotionDowngradeRemote{incomingTransferRemote: &incomingTransferRemote{
		docs: map[domain.ContentHash]domain.SessionDoc{},
	}}
	for i, label := range []string{"base", "candidate [git aaaa]"} {
		doc := domain.SessionDoc{CIR: domain.CIRDocument{
			Envelope: domain.Envelope{CIRVersion: "1", SourceProvider: domain.ProviderClaude, Fidelity: domain.FidelityFull},
			Events: []domain.Event{{Kind: domain.EventMessage, Seq: 0, Role: "user",
				Blocks: []domain.ContentBlock{{Type: "text", Text: "synthetic " + label}}}},
		}}
		raw, err := domain.CanonicalBytes(doc.CIR)
		if err != nil {
			t.Fatal(err)
		}
		id := domain.HashContent(raw)
		doc.Hash = id
		snap := domain.Snapshot{ID: id, DocHash: id, RepoID: repo, Branch: "feature", Message: label, CreatedAt: time.Unix(int64(i+1), 0)}
		remote.docs[id] = doc
		remote.snaps = append(remote.snaps, snap)
		if i == 0 {
			if _, err := st.PutDoc(ctx, doc); err != nil {
				t.Fatal(err)
			}
			if err := st.PutSnapshot(ctx, snap); err != nil {
				t.Fatal(err)
			}
			remote.ref = domain.Ref{RepoID: repo, Kind: domain.RefBranch, Name: "main", Target: id}
			if err := st.PutRef(ctx, remote.ref); err != nil {
				t.Fatal(err)
			}
		}
	}
	svc := app.NewSyncRepoService(st, remote, mergeObservationGit{domain.Repo{ID: repo, LocalPath: dir}}, storage.NewSyncOutbox())
	syncer := &mergeObservationSync{SyncRepoService: svc}
	c := &Container{Sync: syncer, List: app.NewListSessionsService(st)}
	shas := []string{"aaaa1111"}
	out, err := fetchIncomingContexts(ctx, c, dir, "main", shas)
	if err != nil {
		t.Fatal(err)
	}
	if remote.capabilities != 2 || remote.broad != 0 {
		t.Fatalf("fixture did not establish scoped discovery/hydration: reads=%d broad=%d", remote.capabilities, remote.broad)
	}
	if appendMergedContexts(ctx, c, dir, "main", shas, out.requireBranchPlan) {
		t.Fatal("capability loss unexpectedly promoted")
	}
	if remote.broad != 0 {
		t.Fatalf("fresh promotion observation silently broadened after scoped discovery: capability_reads=%d broad_pulls=%d", remote.capabilities, remote.broad)
	}
	c.Sync = incomingBriefingFallback{SyncRepo: svc}
	writePullBriefingFromBaseline(ctx, c, dir, "main", remote.ref.Target, out.requireBranchPlan)
	if remote.capabilities != 4 || remote.broad != 0 {
		t.Fatalf("briefing fallback broadened or skipped validation: capability_reads=%d broad_pulls=%d", remote.capabilities, remote.broad)
	}
}
