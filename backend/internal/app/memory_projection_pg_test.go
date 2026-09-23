//go:build postgres

package app

import (
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

func TestPGMemoryProjectionAcrossServerInstances(t *testing.T) {
	dsn := os.Getenv("CXT_TEST_DSN")
	if dsn == "" {
		t.Skip("CXT_TEST_DSN unset")
	}
	ctx := systemTestContext()
	first, err := store.NewPostgresStore(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer first.Close()
	if _, err := first.ApplyMigrations(ctx, "../../../schemas/db/migrations"); err != nil {
		t.Fatal(err)
	}
	second, err := store.NewPostgresStore(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer second.Close()
	writer := NewService(first, first, auth.NewTeamTokenAuth(), gitengine.NewEngine(first), first)
	reader := NewService(second, second, auth.NewTeamTokenAuth(), gitengine.NewEngine(second), second)
	repo := hh(fmt.Sprint("projection-replicas-", time.Now().UnixNano()))
	if _, err := first.PutRepo(ctx, domain.Repo{ID: repo}); err != nil {
		t.Fatal(err)
	}
	var ids []domain.ContentHash
	for _, summary := range []string{"BASE", "FEATURE"} {
		doc := domain.SessionDoc{CIR: domain.CIRDocument{Envelope: domain.CIREnvelope{CIRVersion: "1", SourceProvider: domain.ProviderCodex}, Events: []domain.CIREvent{{Kind: domain.EventMessage, Role: "user", Blocks: []domain.ContentBlock{{Type: "text", Text: summary}}}}}}
		raw, err := domain.CanonicalBytes(doc.CIR)
		if err != nil {
			t.Fatal(err)
		}
		doc.Hash = domain.HashContent(raw)
		if _, err := first.PutDoc(ctx, repo, doc); err != nil {
			t.Fatal(err)
		}
		if err := first.PutSnapshot(ctx, domain.Snapshot{ID: doc.Hash, RepoID: repo, DocHash: doc.Hash, Provider: domain.ProviderCodex, Fidelity: domain.FidelityFull, CreatedAt: time.Now().UTC()}); err != nil {
			t.Fatal(err)
		}
		if _, err := writer.PutMemoryDigest(ctx, repo, domain.MergeDigests(domain.MemoryDigest{}, domain.MemoryDigest{SnapshotID: doc.Hash, Summary: summary})); err != nil {
			t.Fatal(err)
		}
		ids = append(ids, doc.Hash)
	}
	base, tip := ids[0], ids[1]
	before, err := reader.GetMemoryProjection(ctx, repo, tip)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(before.Digest.Summary, "BASE") {
		t.Fatal("unmerged memory imported")
	}
	if err := first.AddGraftParents(ctx, repo, tip, []domain.ContentHash{base}); err != nil {
		t.Fatal(err)
	}
	after, err := reader.GetMemoryProjection(ctx, repo, tip)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(after.Digest.Summary, "BASE") || !strings.Contains(after.Digest.Summary, "FEATURE") || before.StateHash == after.StateHash {
		t.Fatalf("second server missed merge: %+v", after)
	}
	exact, err := reader.GetMemoryDigest(ctx, repo, tip)
	if err != nil {
		t.Fatal(err)
	}
	if exact.Summary != "FEATURE" {
		t.Fatal("projection modified immutable attachment")
	}
}
