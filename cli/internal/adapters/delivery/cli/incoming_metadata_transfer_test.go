package cli

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"sort"
	"testing"
	"time"

	"github.com/wnsdy95/cxthub/cli/internal/adapters/storage"
	"github.com/wnsdy95/cxthub/cli/internal/app"
	"github.com/wnsdy95/cxthub/cli/internal/domain"
	"github.com/wnsdy95/cxthub/cli/internal/ports/outbound"
)

// Only transport and append effects are fake. Discovery, selected fetch,
// validation, FileStore and the hook's candidate/pruning code are real.
type incomingTransferRemote struct {
	outbound.RemoteSync
	ref     domain.Ref
	snaps   []domain.Snapshot
	docs    map[domain.ContentHash]domain.SessionDoc
	bodies  []domain.ContentHash
	broad   int
	planErr error
}

func (r *incomingTransferRemote) PullCapabilities(context.Context, string) (outbound.PullCapabilities, error) {
	return outbound.PullCapabilities{BranchPlanVersion: 1}, nil
}
func (r *incomingTransferRemote) ReadSnapshotCatalog(context.Context, string) ([]domain.Snapshot, error) {
	return r.snaps, nil
}
func (r *incomingTransferRemote) Pull(context.Context, string, map[domain.ContentHash]domain.ContentHash, []domain.ContentHash) ([]domain.Snapshot, []domain.SessionDoc, []domain.Ref, error) {
	r.broad++
	return nil, nil, nil, errors.New("unrelated full transfer")
}
func (r *incomingTransferRemote) PullSelectedBranchTo(ctx context.Context, repo string, request domain.BranchPullRequest, states map[domain.ContentHash]domain.ContentHash, haves []domain.ContentHash, receiver outbound.PullDocumentReceiver) (domain.BranchPullPlan, []domain.Snapshot, error) {
	if r.planErr != nil {
		return domain.BranchPullPlan{}, nil, r.planErr
	}
	if err := request.Validate(); err != nil {
		return domain.BranchPullPlan{}, nil, err
	}
	if request.Branch != r.ref.Name {
		return domain.BranchPullPlan{}, nil, domain.ErrNotFound
	}
	plan := domain.BranchPullPlan{Version: 1, RepoID: repo, Branch: request.Branch, SelectedRef: r.ref, Refs: []domain.Ref{r.ref}, SnapshotStates: map[domain.ContentHash]domain.ContentHash{}}
	byID := map[domain.ContentHash]domain.Snapshot{}
	for _, s := range r.snaps {
		byID[s.ID] = s
	}
	wanted := map[domain.ContentHash]bool{}
	var visit func(domain.ContentHash) error
	visit = func(id domain.ContentHash) error {
		if wanted[id] {
			return nil
		}
		s, ok := byID[id]
		if !ok {
			return domain.ErrNotFound
		}
		wanted[id] = true
		for _, p := range s.ReachabilityParents() {
			if err := visit(p); err != nil {
				return err
			}
		}
		return nil
	}
	if err := visit(r.ref.Target); err != nil {
		return plan, nil, err
	}
	for _, id := range request.ObservationRoots {
		if _, ok := byID[id]; !ok {
			plan.AbsentRoots = append(plan.AbsentRoots, id)
		} else if err := visit(id); err != nil {
			return plan, nil, err
		}
	}
	for id := range wanted {
		plan.SnapshotIndex = append(plan.SnapshotIndex, id)
	}
	sort.Slice(plan.SnapshotIndex, func(i, j int) bool { return plan.SnapshotIndex[i] < plan.SnapshotIndex[j] })
	var changed []domain.Snapshot
	for _, id := range plan.SnapshotIndex {
		s := byID[id]
		plan.SnapshotStates[id], _ = domain.SnapshotStateHash(s)
		if states[id] != plan.SnapshotStates[id] {
			changed = append(changed, s)
		}
		have, err := receiver.HasVerifiedDoc(ctx, id)
		if err != nil {
			return plan, nil, err
		}
		if !have {
			doc, ok := r.docs[id]
			if !ok {
				return plan, nil, fmt.Errorf("unrelated body requested: %s", id)
			}
			r.bodies = append(r.bodies, id)
			if err := receiver.ReceiveDoc(ctx, doc); err != nil {
				return plan, nil, err
			}
		}
	}
	return plan, changed, nil
}

func TestIncomingMetadataSelectedTransferPreservesCandidatesAndLocalState(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	ctx := context.Background()
	root := t.TempDir()
	repo := string(domain.HashContent([]byte("incoming metadata repo")))
	st := storage.NewFileStore(root)
	r := &incomingTransferRemote{docs: map[domain.ContentHash]domain.SessionDoc{}}
	newSnap := func(label, branch, message string, when int64) domain.Snapshot {
		cir := domain.CIRDocument{Envelope: domain.Envelope{CIRVersion: "1", SourceProvider: domain.ProviderClaude, Fidelity: domain.FidelityFull}, Events: []domain.Event{{Kind: domain.EventMessage, Seq: 0, Role: "user", Blocks: []domain.ContentBlock{{Type: "text", Text: "synthetic " + label}}}}}
		cb, err := domain.CanonicalBytes(cir)
		if err != nil {
			t.Fatal(err)
		}
		id := domain.HashContent(cb)
		r.docs[id] = domain.SessionDoc{Hash: id, CIR: cir}
		return domain.Snapshot{ID: id, DocHash: id, RepoID: repo, Branch: branch, Message: message, CreatedAt: time.Unix(when, 0)}
	}
	base := newSnap("base", "main", "base", 1)
	ancestor := newSnap("ancestor", "other", "ancestor", 2)
	old := newSnap("old candidate", "other", "old [git aaaa]", 3)
	remote := newSnap("new candidate", "feature/other", "remote [git dddd]", 4)
	remote.Parents = []domain.ContentHash{ancestor.ID}
	local := newSnap("local unpublished", "local/topic", "local [git bbbb]", 5)
	local.GraftParents = []domain.ContentHash{remote.ID}
	local.GraftSeq = 1
	unrelated := newSnap("unrelated", "unrelated", "unrelated [git cccc]", 6)
	// If full transfer or incorrect selection hydrates either body, fail.
	delete(r.docs, unrelated.ID)
	delete(r.docs, old.ID)
	r.snaps = []domain.Snapshot{base, ancestor, old, remote, unrelated}
	r.ref = domain.Ref{RepoID: repo, Kind: domain.RefBranch, Name: "main", Target: base.ID}
	for _, s := range []domain.Snapshot{base, local} {
		if _, err := st.PutDoc(ctx, r.docs[s.ID]); err != nil {
			t.Fatal(err)
		}
		if err := st.PutSnapshot(ctx, s); err != nil {
			t.Fatal(err)
		}
	}
	if err := st.PutRef(ctx, r.ref); err != nil {
		t.Fatal(err)
	}
	if err := saveRewrites(root, map[string]string{"dddd": "aaaa1111"}); err != nil {
		t.Fatal(err)
	}
	svc := app.NewSyncRepoService(st, r, mergeObservationGit{domain.Repo{ID: repo, LocalPath: root}}, storage.NewSyncOutbox())
	syncer := &mergeObservationSync{SyncRepoService: svc}
	c := &Container{Sync: syncer, List: app.NewListSessionsService(st)}
	refsBefore, _ := st.ListRefs(ctx, repo)
	fullBefore, _ := st.ReadRemoteObservation(ctx, repo, "configured")
	// Compare persisted representations on both sides. JSON timestamps are UTC;
	// time.Unix uses time.Local even when the host's local zone is UTC.
	localBefore, err := st.GetSnapshot(ctx, local.ID)
	if err != nil {
		t.Fatal(err)
	}
	for attempt := 0; attempt < 2; attempt++ {
		out, err := fetchIncomingContexts(ctx, c, root, "main", []string{"aaaa1111", "bbbb2222"})
		if err != nil || len(out.Conflicts) != 0 {
			t.Fatalf("selected transfer %+v %v", out, err)
		}
		syncer.appends = nil
		if !appendMergedContexts(ctx, c, root, "main", []string{"aaaa1111", "bbbb2222"}, out.requireBranchPlan) {
			t.Fatal("candidates not reflected")
		}
		if !reflect.DeepEqual(syncer.appends, []domain.ContentHash{remote.ID, local.ID}) {
			t.Fatalf("lost newest/aliased remote candidate or local unpublished work: %v", syncer.appends)
		}
		for _, id := range []domain.ContentHash{old.ID, unrelated.ID} {
			if has, _ := st.HasDoc(ctx, id); has {
				t.Fatal("unrelated body hydrated")
			}
			if _, err := st.GetSnapshot(ctx, id); !errors.Is(err, domain.ErrNotFound) {
				t.Fatal("discovery persisted unrelated metadata")
			}
		}
		refsAfter, _ := st.ListRefs(ctx, repo)
		fullAfter, _ := st.ReadRemoteObservation(ctx, repo, "configured")
		localAfter, err := st.GetSnapshot(ctx, local.ID)
		if err != nil {
			t.Fatal(err)
		}
		history, err := st.ListHistoryEvents(ctx, repo)
		if err != nil || len(history) != 0 {
			t.Fatalf("fetch changed history: %v %v", history, err)
		}
		if !reflect.DeepEqual(refsBefore, refsAfter) {
			t.Fatalf("fetch changed refs: before=%+v after=%+v", refsBefore, refsAfter)
		}
		if !reflect.DeepEqual(fullBefore, fullAfter) {
			t.Fatal("fetch changed full repair observation")
		}
		if !reflect.DeepEqual(localBefore, localAfter) {
			t.Fatalf("fetch changed unpublished snapshot: before=%+v after=%+v", localBefore, localAfter)
		}
	}
	want := []domain.ContentHash{remote.ID, ancestor.ID}
	sort.Slice(want, func(i, j int) bool { return want[i] < want[j] })
	if !reflect.DeepEqual(r.bodies, want) || r.broad != 0 {
		t.Fatalf("cold/warm bodies=%v want=%v broad=%d", r.bodies, want, r.broad)
	}
}
