//go:build postgres

package app

import (
	"context"
	"errors"
	"github.com/wnsdy95/cxthub/backend/internal/adapters/auth"
	"github.com/wnsdy95/cxthub/backend/internal/adapters/gitengine"
	"github.com/wnsdy95/cxthub/backend/internal/adapters/store"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"os"
	"strings"
	"testing"
	"time"
)

func TestPGEffectiveMemoryCoherentReplicaRead(t *testing.T) {
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
	origin := "https://github.com/example/effective-replicas"
	if _, err = st.PutRepo(ctx, domain.Repo{ID: repo, GitRemoteURL: origin}); err != nil {
		t.Fatal(err)
	}
	doc := domain.SessionDoc{CIR: domain.CIRDocument{Envelope: domain.CIREnvelope{CIRVersion: "1", SourceProvider: domain.ProviderCodex, SessionOriginID: string(repo)}}}
	raw, _ := domain.CanonicalBytes(doc.CIR)
	doc.Hash = domain.HashContent(raw)
	if _, err = st.PutDoc(ctx, repo, doc); err != nil {
		t.Fatal(err)
	}
	id := doc.Hash
	if err = st.PutSnapshot(ctx, domain.Snapshot{ID: id, RepoID: repo, DocHash: id, Provider: domain.ProviderCodex, Fidelity: domain.FidelityFull, CreatedAt: time.Now().UTC()}); err != nil {
		t.Fatal(err)
	}
	oid := strings.Repeat("a", 40)
	d := domain.MemoryDigest{SnapshotID: id, ClaimsVersion: 1, Fragments: []domain.MemoryFragment{{SourceSnapshot: id, Claims: []domain.MemoryClaim{{Kind: "code", Text: "Scoped feature", Code: &domain.MemoryCodeScope{Commit: oid, Paths: []string{"file"}}}, {Kind: "rationale", Text: "Before decision"}}}}}
	root, err := writer.PutMemoryDigestCAS(ctx, repo, d)
	if err != nil {
		t.Fatal(err)
	}
	observation := domain.HistoryEvent{ID: strings.Repeat("1", 32), RepoID: string(repo), BranchID: "feature", Branch: "feature", Kind: "position", Target: id, GitAfter: oid, CreatedAt: time.Now().UTC()}
	if err = writer.RecordHistory(ctx, observation); err != nil {
		t.Fatal(err)
	}
	publishPRSource(t, writer, observation)
	req := domain.EffectiveMemoryRequest{Selection: domain.EffectiveMemorySelection{SnapshotID: id, CodeCommit: oid}, Content: "claims", Limit: 1}
	before, err := reader.QueryEffectiveMemory(ctx, repo, req)
	if err != nil || before.Items[0].State != "review" || before.Content != "claims" {
		t.Fatal(before, err)
	}
	provider := &scanReader{deltas: map[string]domain.GitCommitDelta{oid: {Commit: oid, Parents: []string{}, Complete: true, Changes: []domain.GitPathChange{{Path: "file", After: domain.GitEntry{OID: strings.Repeat("b", 40), Mode: "100644"}}}}}}
	provider.deltas[strings.Repeat("c", 40)] = domain.GitCommitDelta{Commit: strings.Repeat("c", 40), Parent: oid, Parents: []string{oid}, Complete: true, Changes: []domain.GitPathChange{}}
	scans, err := NewGitScans(writer, provider)
	if err != nil {
		t.Fatal(err)
	}
	// Keep the reader in one database generation while a separate replica commits
	// new memory and Git evidence. Nested query calls must reuse the bound read.
	err = peer.WithinReadSnapshot(ctx, func(bound context.Context) error {

		positions, e := reader.QueryMemoryPositions(bound, repo, id, "")
		if e != nil || positions.CodeCommit != oid || positions.Reason != "unique" {
			t.Fatalf("initial positions: %+v %v", positions, e)
		}
		another := observation
		another.ID, another.GitAfter = strings.Repeat("2", 32), strings.Repeat("c", 40)
		another.CreatedAt = time.Now().UTC()
		if e = writer.RecordHistory(ctx, another); e != nil {
			return e
		}
		againPositions, e := reader.QueryMemoryPositions(bound, repo, id, "")
		if e != nil || againPositions.CodeCommit != oid || againPositions.Reason != "unique" {
			t.Fatalf("mixed position generations: %+v %v", againPositions, e)
		}
		held, e := reader.QueryEffectiveMemory(bound, repo, req)
		if e != nil {
			return e
		}
		d.PreviousMemoryHash = root
		d.Fragments[0].Claims[1].Text = "After decision"
		if _, e = writer.PutMemoryDigestCAS(ctx, repo, d); e != nil {
			return e
		}
		if _, e = scans.ObservePush(ctx, origin, "refs/heads/main", "", oid, false, "delivery"); e != nil {
			return e
		}
		for i := 0; i < 10; i++ {
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
		again, e := reader.QueryEffectiveMemory(bound, repo, req)
		if e != nil {
			return e
		}
		if again.StateHash != held.StateHash || again.Items[0].State != "review" {
			t.Fatal("mixed committed generations", held, again)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}

	positions, err := reader.QueryMemoryPositions(ctx, repo, id, "")
	if err != nil || positions.Reason != "ambiguous" || positions.CodeCommit != "" {
		t.Fatalf("missing committed position: %+v %v", positions, err)
	}
	after, err := reader.QueryEffectiveMemory(ctx, repo, req)
	if err != nil || after.Items[0].State != "applied" || after.StateHash == before.StateHash {
		t.Fatal("replica missed committed evidence", after, err)
	}
	req.Cursor = before.NextCursor
	if _, err = reader.QueryEffectiveMemory(ctx, repo, req); !errors.Is(err, domain.ErrConflict) {
		t.Fatal("mixed page generations", err)
	}
	req.Cursor = ""
	req.Selection.MemoryHash = root
	req.Limit = 20
	old, err := reader.QueryEffectiveMemory(ctx, repo, req)
	if err != nil || itemByText(t, old, "Before decision").State != "retained" {
		t.Fatal("old memory was overwritten", err)
	}
	provider.offline = true
	if _, err = reader.QueryEffectiveMemory(ctx, repo, req); err != nil {
		t.Fatal("query needs provider", err)
	}
}
