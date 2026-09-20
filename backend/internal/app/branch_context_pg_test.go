//go:build postgres

package app

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/wnsdy95/cxthub/backend/internal/adapters/auth"
	"github.com/wnsdy95/cxthub/backend/internal/adapters/gitengine"
	"github.com/wnsdy95/cxthub/backend/internal/adapters/store"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

func TestPGMemoryProjectionBranchIntegrationReplicaSnapshot(t *testing.T) {
	dsn := os.Getenv("CXT_TEST_DSN")
	if dsn == "" {
		t.Skip("CXT_TEST_DSN unset")
	}
	ctx := context.Background()
	st, err := store.NewPostgresStore(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if _, err = st.ApplyMigrations(ctx, "../../../schemas/db/migrations"); err != nil {
		t.Fatal(err)
	}
	peer, err := store.NewPostgresStore(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer peer.Close()
	writer := NewService(st, st, auth.NewTeamTokenAuth(), gitengine.NewEngine(st), st)
	reader := NewService(peer, peer, auth.NewTeamTokenAuth(), gitengine.NewEngine(peer), peer)
	repo := hh(t.Name() + time.Now().String())
	origin := "https://github.com/example/branch-inclusion"
	if _, err = st.PutRepo(ctx, domain.Repo{ID: repo, GitRemoteURL: origin}); err != nil {
		t.Fatal(err)
	}
	var ids []domain.ContentHash
	for _, text := range []string{"BASE", "PR SOURCE MEMORY", "CONTINUED MAIN"} {
		doc := domain.SessionDoc{CIR: domain.CIRDocument{Envelope: domain.CIREnvelope{CIRVersion: "1", SourceProvider: domain.ProviderCodex, SessionOriginID: string(repo) + text}}}
		raw, _ := domain.CanonicalBytes(doc.CIR)
		doc.Hash = domain.HashContent(raw)
		if _, err = st.PutDoc(ctx, repo, doc); err != nil {
			t.Fatal(err)
		}
		if err = st.PutSnapshot(ctx, domain.Snapshot{ID: doc.Hash, RepoID: repo, DocHash: doc.Hash, Provider: domain.ProviderCodex, Fidelity: domain.FidelityFull, CreatedAt: time.Now().UTC()}); err != nil {
			t.Fatal(err)
		}
		if _, err = writer.PutMemoryDigest(ctx, repo, domain.MergeDigests(domain.MemoryDigest{}, domain.MemoryDigest{SnapshotID: doc.Hash, Summary: text})); err != nil {
			t.Fatal(err)
		}
		ids = append(ids, doc.Hash)
	}
	ref := domain.Ref{RepoID: repo, Kind: domain.RefBranch, Name: "main", BranchID: "main-id", Target: ids[2]}
	if err = st.CompareAndSwapRef(ctx, repo, ref, ""); err != nil {
		t.Fatal(err)
	}
	binding := domain.HistoryEvent{ID: fmt.Sprintf("%032x", 91), RepoID: string(repo), Kind: "pr-merge", Branch: "main", BranchID: ref.BranchID, SourceBranchID: "feature-id", Source: ids[1], Target: ids[1], PR: &domain.PullRequestMerge{Number: 1, BaseBranch: "main", HeadBranch: "feature", HeadSHA: effectiveOID(8), MergeSHA: effectiveOID(2)}, CreatedAt: time.Now().UTC()}
	completion := binding
	key := sha256.Sum256([]byte(binding.ID + ":completed"))
	completion.ID = fmt.Sprintf("%x", key[:16])
	completion.PRCompleted = true
	completion.SharedTarget = ids[0]
	position := domain.HistoryEvent{ID: fmt.Sprintf("%032x", 92), RepoID: string(repo), Kind: "position", Branch: "main", BranchID: ref.BranchID, Target: ids[2], GitAfter: effectiveOID(3), CreatedAt: time.Now().UTC()}
	for _, h := range []domain.HistoryEvent{binding, completion, position} {
		if err = st.ApplyHistoryEvent(ctx, h); err != nil {
			t.Fatal(err)
		}
	}
	provider := &scanReader{deltas: map[string]domain.GitCommitDelta{}}
	for n := 1; n <= 3; n++ {
		d := domain.GitCommitDelta{Commit: effectiveOID(n), Parents: []string{}, Complete: true, Changes: []domain.GitPathChange{}}
		if n > 1 {
			d.Parent = effectiveOID(n - 1)
			d.Parents = []string{d.Parent}
		}
		provider.deltas[d.Commit] = d
	}
	scans, err := NewGitScans(writer, provider)
	if err != nil {
		t.Fatal(err)
	}
	req := domain.EffectiveMemoryRequest{Selection: domain.EffectiveMemorySelection{Branch: "main", SnapshotID: ref.Target, CodeCommit: effectiveOID(3)}, Limit: 1}
	err = peer.WithinReadSnapshot(ctx, func(bound context.Context) error {
		before, e := reader.QueryBranchMemory(bound, repo, "main", ref.Target, "")
		if e != nil {
			return e
		}
		if strings.Contains(before.Digest.Summary, "PR SOURCE MEMORY") {
			t.Fatal("unverified source included")
		}
		if _, e = scans.ObservePush(ctx, origin, "refs/heads/main", "", effectiveOID(3), false, "push"); e != nil {
			return e
		}
		for n := 0; n < 20; n++ {
			job, e := st.ClaimGitScan(ctx, repo, time.Now().UTC(), time.Minute)
			if errors.Is(e, domain.ErrNotFound) {
				break
			}
			if e != nil {
				return e
			}
			if e = scans.run(ctx, job); e != nil {
				return e
			}
		}
		held, e := reader.QueryBranchMemory(bound, repo, "main", ref.Target, "")
		if e != nil {
			return e
		}
		if held.StateHash != before.StateHash || held.Inclusion.Merges[0].State != "review" {
			t.Fatal("mixed evidence generations")
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	provider.offline = true
	current, err := reader.QueryBranchMemory(ctx, repo, "main", ref.Target, "")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(current.Digest.Summary, "PR SOURCE MEMORY") || !strings.Contains(current.Digest.Summary, "CONTINUED MAIN") || current.Inclusion.Merges[0].State != "included" {
		t.Fatal("replica omitted integrated memory", current)
	}
	full, err := reader.GetRepositoryView(ctx, repo)
	if err != nil {
		t.Fatal(err)
	}
	if len(full.Graph.Integrations) != 1 {
		t.Fatal("graph disagrees with branch memory")
	}
	page, err := reader.QueryEffectiveMemory(ctx, repo, req)
	if err != nil || page.NextCursor == "" {
		t.Fatal(page, err)
	}
	previous, err := st.GetSnapshot(ctx, repo, ids[1])
	if err != nil {
		t.Fatal(err)
	}
	if _, err = writer.PutMemoryDigestCAS(ctx, repo, domain.MergeDigests(domain.MemoryDigest{}, domain.MemoryDigest{SnapshotID: ids[1], PreviousMemoryHash: previous.MemoryHash, Summary: "UPDATED SOURCE"})); err != nil {
		t.Fatal(err)
	}
	req.Cursor = page.NextCursor
	if _, err = reader.QueryEffectiveMemory(ctx, repo, req); !errors.Is(err, domain.ErrConflict) {
		t.Fatal("mixed memory page generations", err)
	}
	saved, err := reader.GetMemoryDigest(ctx, repo, ref.Target)
	if err != nil || saved.Summary != "CONTINUED MAIN" {
		t.Fatal("read overwrote saved original", saved, err)
	}
	snap, err := st.GetSnapshot(ctx, repo, ref.Target)
	if err != nil || len(snap.ReachabilityParents()) != 0 {
		t.Fatal("read rewrote ancestry", snap, err)
	}
}
