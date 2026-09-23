package app

import (
	"crypto/sha256"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

func branchContextFixture(t *testing.T) (*effectiveFixture, domain.Ref, []domain.HistoryEvent) {
	t.Helper()
	f := newEffectiveFixture(t)
	ctx := systemTestContext()
	// Main's later conversation intentionally does not reach either PR. Its
	// code still includes both merges. This models a same-code continuation.
	ref := domain.Ref{RepoID: f.repo, Kind: domain.RefBranch, Name: "main", BranchID: "main-id", Target: hh("continued-main")}
	if err := f.st.PutSnapshot(ctx, domain.Snapshot{ID: ref.Target, RepoID: f.repo, DocHash: ref.Target}); err != nil {
		t.Fatal(err)
	}
	for id, text := range map[domain.ContentHash]string{ref.Target: "MAIN", f.a: "MERGED A", f.b: "MERGED B"} {
		if _, err := f.svc.PutMemoryDigest(ctx, f.repo, domain.MergeDigests(domain.MemoryDigest{}, domain.MemoryDigest{SnapshotID: id, Summary: text})); err != nil {
			t.Fatal(err)
		}
	}
	base := hh("pre-merge-main")
	if err := f.st.PutSnapshot(ctx, domain.Snapshot{ID: base, RepoID: f.repo, DocHash: base}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.svc.PutMemoryDigest(ctx, f.repo, domain.MemoryDigest{SnapshotID: base, Summary: "PRE-MERGE MAIN"}); err != nil {
		t.Fatal(err)
	}
	var history []domain.HistoryEvent
	for i, id := range []domain.ContentHash{f.a, f.b} {
		e := domain.HistoryEvent{ID: fmt.Sprintf("%032x", 100+i), RepoID: string(f.repo), Branch: "main", BranchID: ref.BranchID, Kind: "pr-merge", Source: id, Target: id, SourceBranchID: fmt.Sprintf("feature-%d", i), PR: &domain.PullRequestMerge{Number: 10 + i, BaseBranch: "main", HeadBranch: fmt.Sprintf("feature-%d", i), HeadSHA: effectiveOID(8 + i), MergeSHA: effectiveOID(2 + i)}, CreatedAt: time.Unix(int64(10-i), 0)}
		history = append(history, e)
		key := sha256.Sum256([]byte(e.ID + ":completed"))
		e.SharedTarget = base
		e.ID = fmt.Sprintf("%x", key[:16])
		e.PRCompleted = true
		history = append(history, e)
	}
	return f, ref, history
}

func TestBranchContextIncludesCompletedSourcesInGitOrderAfterContinuation(t *testing.T) {
	f, ref, history := branchContextFixture(t)
	ctx := systemTestContext()
	snaps, _ := f.st.ListSnapshots(ctx, f.repo, "")
	evidence, _ := f.svc.newCodeEvidence(ctx, f.repo)
	got, err := f.svc.branchContext(ctx, ref, effectiveOID(3), snaps, history, evidence)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got.Roots, []domain.ContentHash{hh("pre-merge-main"), f.a, f.b, ref.Target}) {
		t.Fatalf("Git integration order: %+v", got)
	}
	for _, m := range got.Merges {
		if m.State != "included" {
			t.Fatalf("missing inclusion: %+v", m)
		}
	}
	projection, err := f.svc.projectBranchMemory(ctx, f.repo, got, snaps)
	if err != nil {
		t.Fatal(err)
	}
	for _, text := range []string{"PRE-MERGE MAIN", "MERGED A", "MERGED B"} {
		if strings.Count(projection.Digest.Summary, text) != 1 {
			t.Fatalf("omitted/duplicated %q: %s", text, projection.Digest.Summary)
		}
	}
	stored, _ := f.st.GetSnapshot(ctx, f.repo, ref.Target)
	if len(stored.Parents)+len(stored.GraftParents) != 0 {
		t.Fatal("query changed conversation ancestry")
	}
	// Delivery/input ordering and unrelated history do not change the projection.
	for i, j := 0, len(history)-1; i < j; i, j = i+1, j-1 {
		history[i], history[j] = history[j], history[i]
	}
	evidence, _ = f.svc.newCodeEvidence(ctx, f.repo)
	again, err := f.svc.branchContext(ctx, ref, effectiveOID(3), snaps, history, evidence)
	if err != nil || !reflect.DeepEqual(got, again) {
		t.Fatalf("arrival order changed inclusion: %+v %v", again, err)
	}
}

func TestBranchContextKeepsPastCodeUnknownEvidenceAndIdentitySeparate(t *testing.T) {
	f, ref, history := branchContextFixture(t)
	ctx := systemTestContext()
	snaps, _ := f.st.ListSnapshots(ctx, f.repo, "")
	for _, tt := range []struct {
		name, code, identity string
		roots                int
		state                string
	}{
		{"past code", effectiveOID(1), ref.BranchID, 1, "not_selected"},
		{"first merge", effectiveOID(2), ref.BranchID, 3, ""},
		{"unknown code", effectiveOID(99), ref.BranchID, 1, "review"},
		{"reused name", effectiveOID(3), "new-main-id", 1, ""},
	} {
		t.Run(tt.name, func(t *testing.T) {
			r := ref
			r.BranchID = tt.identity
			evidence, _ := f.svc.newCodeEvidence(ctx, f.repo)
			got, err := f.svc.branchContext(ctx, r, tt.code, snaps, history, evidence)
			if err != nil {
				t.Fatal(err)
			}
			if len(got.Roots) != tt.roots {
				t.Fatalf("unexpected roots: %+v", got)
			}
			if tt.state != "" {
				for _, m := range got.Merges {
					if m.State != tt.state {
						t.Fatalf("incorrect state: %+v", m)
					}
				}
			}
		})
	}
}

func TestBranchContextSharedViewAndMemoryContract(t *testing.T) {
	f, ref, history := branchContextFixture(t)
	ctx := systemTestContext()
	if err := f.st.CompareAndSwapRef(ctx, f.repo, ref, ""); err != nil {
		t.Fatal(err)
	}
	history = append(history, domain.HistoryEvent{ID: fmt.Sprintf("%032x", 200), RepoID: string(f.repo), Branch: ref.Name, BranchID: ref.BranchID, Kind: "position", Target: ref.Target, GitAfter: effectiveOID(3), CreatedAt: time.Unix(20, 0)})
	for _, e := range history {
		if err := f.st.ApplyHistoryEvent(ctx, e); err != nil {
			t.Fatal(err)
		}
	}
	full, err := f.svc.GetRepositoryView(ctx, f.repo)
	if err != nil {
		t.Fatal(err)
	}
	patch, err := f.svc.GetPendingView(ctx, f.repo)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(full.Graph, patch.Graph) {
		t.Fatal("full and live inclusion contracts diverged")
	}
	in := full.Graph.BranchContexts["main"]
	if in.CodeCommit != effectiveOID(3) || len(in.Roots) != 4 {
		t.Fatalf("auto branch inclusion: %+v", in)
	}
	if len(full.Graph.Integrations) != 1 || len(full.Graph.Integrations[0].Parents) != 2 {
		t.Fatal("merged memory has no equivalent graph integration")
	}
	list, err := f.svc.QueryContext(ctx, f.repo, domain.ContextSelection{Branch: "main", Position: "main", Scope: "current"})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(list.Inclusion, &in) || list.StateHash == "" {
		t.Fatal("MCP context list has a different inclusion contract")
	}
	var ids []domain.ContentHash
	for _, snap := range list.Snapshots {
		ids = append(ids, snap.ID)
	}
	if !reflect.DeepEqual(ids, full.Graph.BranchSnapshots["main"]) {
		t.Fatal("web/MCP timeline order differs")
	}
	got, err := f.svc.QueryBranchMemory(ctx, f.repo, "main", ref.Target, "")
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got.Inclusion, &in) {
		t.Fatal("memory and graph inclusion differ")
	}
	page, err := f.svc.QueryEffectiveMemory(ctx, f.repo, domain.EffectiveMemoryRequest{Selection: domain.EffectiveMemorySelection{Branch: "main", SnapshotID: ref.Target, CodeCommit: effectiveOID(3)}, Limit: 50})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(page.Inclusion, &in) {
		t.Fatal("effective memory uses different inclusion")
	}
	for _, text := range []string{"PRE-MERGE MAIN", "MERGED A", "MERGED B"} {
		itemByText(t, page, text)
	}
	stored, _ := f.st.GetSnapshot(ctx, f.repo, ref.Target)
	if _, err := f.svc.QueryEffectiveMemory(ctx, f.repo, domain.EffectiveMemoryRequest{Selection: domain.EffectiveMemorySelection{Branch: "main", SnapshotID: ref.Target, CodeCommit: effectiveOID(3), MemoryHash: stored.MemoryHash}}); err == nil {
		t.Fatal("ambiguous stored/integrated selection accepted")
	}
}

func TestBranchContextDoesNotImportFutureSourceGrafts(t *testing.T) {
	f, ref, history := branchContextFixture(t)
	ctx := systemTestContext()
	// A's later placement now points at B. At A's merge revision B must stay out.
	if err := f.st.AddGraftParents(ctx, f.repo, f.a, []domain.ContentHash{f.b}); err != nil {
		t.Fatal(err)
	}
	snaps, _ := f.st.ListSnapshots(ctx, f.repo, "")
	evidence, _ := f.svc.newCodeEvidence(ctx, f.repo)
	in, err := f.svc.branchContext(ctx, ref, effectiveOID(2), snaps, history, evidence)
	if err != nil {
		t.Fatal(err)
	}
	got, err := f.svc.projectBranchMemory(ctx, f.repo, in, snaps)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(got.Digest.Summary, "MERGED B") || !strings.Contains(got.Digest.Summary, "MERGED A") {
		t.Fatalf("future graft leaked into memory: %s", got.Digest.Summary)
	}
}

func TestBranchPromptIncludesMergedHistoryWithoutArchiveSizedPrompts(t *testing.T) {
	f, ref, history := branchContextFixture(t)
	ctx := systemTestContext()
	for _, h := range history {
		if err := f.st.ApplyHistoryEvent(ctx, h); err != nil {
			t.Fatal(err)
		}
	}
	if err := f.st.CompareAndSwapRef(ctx, f.repo, ref, ""); err != nil {
		t.Fatal(err)
	}
	req := domain.EffectiveMemoryRequest{Selection: domain.EffectiveMemorySelection{Branch: "main", SnapshotID: ref.Target, CodeCommit: effectiveOID(3)}, Content: "prompt", Limit: 50}
	got, err := f.svc.QueryEffectiveMemory(ctx, f.repo, req)
	if err != nil {
		t.Fatal(err)
	}
	for _, text := range []string{"PRE-MERGE MAIN", "MERGED A", "MERGED B"} {
		if itemByText(t, got, text).State != "review" {
			t.Fatal("untyped memory was treated as verified code")
		}
	}
	d := domain.MemoryDigest{SnapshotID: f.root, Fragments: []domain.MemoryFragment{{SourceSnapshot: f.a, Summary: strings.Repeat("\u53e4\u3044memory", 200000)}, {SourceSnapshot: f.b, Summary: "FRESH UNIQUE MERGED MEMORY"}}}
	items, err := effectiveMemoryItems(d, "prompt")
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 2 || items[0].Text != "FRESH UNIQUE MERGED MEMORY" || len(items[1].Text) > 1024 {
		t.Fatal("old archive starved a new contribution", len(items))
	}
	if len(d.Fragments[0].Summary) < 1000000 {
		t.Fatal("prompt truncated saved data")
	}
}

func TestBranchInheritsCompletedMainKnowledgeAtItsRecordedBirth(t *testing.T) {
	f, main, history := branchContextFixture(t)
	ctx := systemTestContext()
	publication := domain.HistoryEvent{ID: fmt.Sprintf("%032x", 300), RepoID: string(f.repo), Branch: "main", BranchID: main.BranchID, Kind: "publish", Source: main.Target, Target: main.Target, GitAfter: effectiveOID(3)}
	birth := domain.HistoryEvent{ID: fmt.Sprintf("%032x", 301), RepoID: string(f.repo), Branch: "new-feature", BranchID: "new-feature-id", Kind: "birth", Source: main.Target, Target: main.Target, GitAfter: effectiveOID(3)}
	history = append(history, publication, birth)
	snaps, _ := f.st.ListSnapshots(ctx, f.repo, "")
	ref := main
	ref.Name = birth.Branch
	ref.BranchID = birth.BranchID
	for _, code := range []string{effectiveOID(3), effectiveOID(1)} {
		evidence, _ := f.svc.newCodeEvidence(ctx, f.repo)
		in, err := f.svc.branchContext(ctx, ref, code, snaps, history, evidence)
		if err != nil {
			t.Fatal(err)
		}
		got, err := f.svc.projectBranchMemory(ctx, f.repo, in, snaps)
		if err != nil {
			t.Fatal(err)
		}
		included := code == effectiveOID(3)
		for _, text := range []string{"MERGED A", "MERGED B"} {
			if strings.Contains(got.Digest.Summary, text) != included {
				t.Fatal("birth dropped inherited PR memory or imported future code", code, text)
			}
		}
		for _, m := range in.Merges {
			if m.DestinationBranchID != main.BranchID {
				t.Fatal("inherited PR was attributed to new branch")
			}
		}
	}
	birth.Kind = "orphan"
	birth.Source = ""
	birth.Target = ""
	history[len(history)-1] = birth
	evidence, _ := f.svc.newCodeEvidence(ctx, f.repo)
	in, err := f.svc.branchContext(ctx, ref, effectiveOID(3), snaps, history, evidence)
	if err != nil {
		t.Fatal(err)
	}
	if len(in.Merges) != 0 {
		t.Fatal("orphan acquired inherited conversation history")
	}
}
