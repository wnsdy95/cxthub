package storage

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/wnsdy95/cxthub/cli/internal/domain"
)

func repairFixture(t *testing.T, st *FileStore, repo, text string, parents ...domain.ContentHash) domain.ContentHash {
	t.Helper()
	ctx := context.Background()
	doc := domain.SessionDoc{CIR: domain.CIRDocument{Events: []domain.Event{{Kind: domain.EventMessage, Role: "user", Blocks: []domain.ContentBlock{{Type: "text", Text: text}}}}}}
	id, err := st.PutDoc(ctx, doc)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.PutSnapshot(ctx, domain.Snapshot{ID: id, DocHash: id, RepoID: repo, Branch: "main", Parents: parents}); err != nil {
		t.Fatal(err)
	}
	return id
}

func TestServerRepairQuarantinesDamageAndPreservesLocalAhead(t *testing.T) {
	ctx := context.Background()
	repo := domain.HashContent([]byte("repo"))
	local, cloud := NewFileStore(t.TempDir()), NewFileStore(t.TempDir())
	base := repairFixture(t, cloud, repo, "server baseline")
	repairFixture(t, local, repo, "server baseline")
	later := repairFixture(t, local, repo, "local-only conversation", base)
	digest := domain.MemoryDigest{SnapshotID: base, Summary: strings.Repeat("project memory ", 1000)}
	mh, err := cloud.PutMemory(ctx, digest)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := local.PutMemory(ctx, digest); err != nil {
		t.Fatal(err)
	}
	for _, st := range []*FileStore{local, cloud} {
		if err := st.CompareAndSwapSnapshotMemory(ctx, base, "", mh); err != nil {
			t.Fatal(err)
		}
	}
	localRef := domain.Ref{Kind: domain.RefBranch, Name: "main", RepoID: repo, Target: later}
	if err := local.PutRef(ctx, localRef); err != nil {
		t.Fatal(err)
	}
	before, _ := os.ReadFile(local.objectPath("snapshots", later))
	badDoc, badMem := []byte("damaged document"), []byte("damaged memory")
	os.WriteFile(local.objectPath("docs", base), badDoc, 0600)
	os.WriteFile(local.objectPath("memories", mh), badMem, 0600)
	refs := []domain.Ref{{Kind: domain.RefBranch, Name: "main", RepoID: repo, Target: base}, {Kind: domain.RefHEAD, Name: "HEAD", RepoID: repo, Symbolic: "main"}}
	backup := t.TempDir()
	report, err := local.RepairFromReplica(ctx, cloud, repo, refs, backup)
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Repaired) != 3 {
		t.Fatalf("repairs=%v", report.Repaired)
	}
	if _, err := local.GetDoc(ctx, base); err != nil {
		t.Fatal(err)
	}
	got, err := local.GetMemory(ctx, mh)
	if err != nil || got.Summary != digest.Summary {
		t.Fatalf("memory: %v", err)
	}
	ref, err := local.GetRef(ctx, repo, domain.RefBranch, "main")
	if err != nil || ref.Target != later {
		t.Fatalf("local head changed: %+v %v", ref, err)
	}
	after, _ := os.ReadFile(local.objectPath("snapshots", later))
	if !bytes.Equal(before, after) {
		t.Fatal("local-only metadata changed")
	}
	for kind, item := range map[string]struct {
		hash string
		raw  []byte
	}{"docs": {base, badDoc}, "memories": {mh, badMem}} {
		saved := filepath.Join(backup, ".cxt", "objects", kind, hexOf(item.hash)+"."+hexOf(domain.HashContent(item.raw)))
		actual, err := os.ReadFile(saved)
		if err != nil || !bytes.Equal(actual, item.raw) {
			t.Fatalf("quarantine incomplete: %s %v", kind, err)
		}
	}
	again, err := local.RepairFromReplica(ctx, cloud, repo, refs, backup)
	if err != nil || len(again.Repaired) != 0 {
		t.Fatalf("retry: %+v %v", again, err)
	}
}

func TestServerRepairRejectsBadSourceAndBackupFailureBeforeReplacement(t *testing.T) {
	ctx := context.Background()
	repo := domain.HashContent([]byte("repo"))
	local, cloud := NewFileStore(t.TempDir()), NewFileStore(t.TempDir())
	id := repairFixture(t, cloud, repo, "baseline")
	repairFixture(t, local, repo, "baseline")
	damaged := []byte("damaged local")
	os.WriteFile(local.objectPath("docs", id), damaged, 0600)
	original, _ := os.ReadFile(cloud.objectPath("docs", id))
	os.WriteFile(cloud.objectPath("docs", id), []byte("damaged cloud"), 0600)
	if _, err := local.RepairFromReplica(ctx, cloud, repo, nil, t.TempDir()); err == nil {
		t.Fatal("bad source accepted")
	}
	got, _ := os.ReadFile(local.objectPath("docs", id))
	if !bytes.Equal(got, damaged) {
		t.Fatal("bad source mutated local data")
	}
	os.WriteFile(cloud.objectPath("docs", id), original, 0600)
	backup := t.TempDir()
	os.WriteFile(filepath.Join(backup, ".cxt"), []byte("blocked"), 0600)
	if _, err := local.RepairFromReplica(ctx, cloud, repo, nil, backup); err == nil {
		t.Fatal("backup failure ignored")
	}
	got, _ = os.ReadFile(local.objectPath("docs", id))
	if !bytes.Equal(got, damaged) {
		t.Fatal("replacement preceded backup")
	}
}

func TestServerRepairReportsUnrecoverableLocalOnlyObject(t *testing.T) {
	ctx := context.Background()
	repo := domain.HashContent([]byte("repo"))
	local, cloud := NewFileStore(t.TempDir()), NewFileStore(t.TempDir())
	base := repairFixture(t, cloud, repo, "shared")
	repairFixture(t, local, repo, "shared")
	only := repairFixture(t, local, repo, "only here", base)
	local.PutRef(ctx, domain.Ref{RepoID: repo, Kind: domain.RefBranch, Name: "main", Target: only})
	os.WriteFile(local.objectPath("docs", only), []byte("unrecoverable"), 0600)
	report, err := local.RepairFromReplica(ctx, cloud, repo, nil, t.TempDir())
	if err == nil || len(report.Issues) == 0 {
		t.Fatal("local-only loss was hidden")
	}
	if _, err := local.GetSnapshot(ctx, only); err != nil {
		t.Fatal("local-only snapshot deleted")
	}
}
