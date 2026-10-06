package app

import (
	"context"
	"errors"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/wnsdy95/cxthub/cli/internal/adapters/storage"
	"github.com/wnsdy95/cxthub/cli/internal/domain"
	"github.com/wnsdy95/cxthub/cli/internal/ports/inbound"
	"github.com/wnsdy95/cxthub/cli/internal/ports/outbound"
)

type initializingPublicationRemote struct {
	*strictPublicationRemote
	repo                    domain.Repo
	receipt                 *domain.RepositoryInitializationReceipt
	anchors                 map[string]domain.RepositoryInitializationAnchor
	exists                  bool
	beginErr, finalizeErr   error
	lostBegin, lostFinalize bool
	begins, finalizes       int
}

func (r *initializingPublicationRemote) RepositoryInitializationState(_ context.Context, _ string, branch string) (outbound.RepositoryInitializationState, error) {
	if !r.exists {
		return outbound.RepositoryInitializationState{}, domain.ErrNotFound
	}
	repo := r.repo
	repo.ContextProtocol = r.protocol
	available := r.receipt != nil && r.protocol == 1 && branch != ""
	if _, used := r.anchors[branch]; used {
		available = false
	}
	for _, ref := range r.refs {
		if ref.Name == branch {
			available = false
		}
	}
	for _, e := range r.sent {
		if (e.Branch == branch || e.PreviousBranch == branch) && (e.BranchID != domain.LegacyContextBranchID(repo.ID, branch) || e.Kind != "position" && e.Kind != "publish") {
			available = false
		}
	}
	return outbound.RepositoryInitializationState{Repo: repo, InitialAnchorAvailable: available}, nil
}

func (r *initializingPublicationRemote) RegisterRepo(context.Context, domain.Repo) (domain.Repo, error) {
	if !r.exists {
		return domain.Repo{}, errors.New("unproven ordinary registration during initialization")
	}
	repo := r.repo
	repo.ContextProtocol = r.protocol
	return repo, nil
}

func (r *initializingPublicationRemote) BeginRepositoryInitialization(context.Context, domain.Repo) (domain.RepositoryInitializationReceipt, error) {
	r.begins++
	if r.beginErr != nil {
		return domain.RepositoryInitializationReceipt{}, r.beginErr
	}
	if r.receipt == nil {
		if r.exists {
			return domain.RepositoryInitializationReceipt{}, domain.ErrRepositoryInitializationConflict
		}
		r.exists = true
		r.protocol, r.repo.ContextProtocol = 1, 1
		r.receipt = &domain.RepositoryInitializationReceipt{Version: 1, CreationID: "init_" + strings.Repeat("a", 32), Repo: r.repo}
	}
	if r.lostBegin {
		r.lostBegin = false
		return domain.RepositoryInitializationReceipt{}, errors.New("creation acknowledgement lost")
	}
	receipt := *r.receipt
	receipt.Anchor = nil
	return receipt, nil
}

func (r *initializingPublicationRemote) ReadRepositoryInitialization(context.Context, string) (domain.RepositoryInitializationReceipt, error) {
	if r.receipt == nil {
		return domain.RepositoryInitializationReceipt{}, domain.ErrNotFound
	}
	if r.beginErr != nil {
		return domain.RepositoryInitializationReceipt{}, r.beginErr
	}
	receipt := *r.receipt
	receipt.Anchor = nil
	return receipt, nil
}

func (r *initializingPublicationRemote) FinalizeRepositoryInitialization(_ context.Context, repo string, in domain.RepositoryInitializationFinalize) (domain.RepositoryInitializationReceipt, error) {
	r.finalizes++
	if r.finalizeErr != nil {
		return domain.RepositoryInitializationReceipt{}, r.finalizeErr
	}
	if r.receipt == nil || in.CreationID != r.receipt.CreationID || in.Validate(repo) != nil {
		return domain.RepositoryInitializationReceipt{}, errors.New("initialization did not precede publication")
	}
	for id := range in.Anchor.SnapshotStates {
		if !slices.Contains(r.snaps, id) || !slices.Contains(r.docs, id) {
			return domain.RepositoryInitializationReceipt{}, errors.New("initialization preceded object staging")
		}
	}
	anchor := in.Anchor
	if r.anchors == nil {
		r.anchors = map[string]domain.RepositoryInitializationAnchor{}
	}
	if prior, exists := r.anchors[anchor.Ref.Name]; exists {
		if !reflect.DeepEqual(prior, anchor) {
			return domain.RepositoryInitializationReceipt{}, domain.ErrRepositoryInitializationConflict
		}
		receipt := *r.receipt
		receipt.Anchor = &prior
		return receipt, nil
	} else {
		state, _ := r.RepositoryInitializationState(context.Background(), repo, anchor.Ref.Name)
		if !state.InitialAnchorAvailable {
			return domain.RepositoryInitializationReceipt{}, domain.ErrRepositoryInitializationConflict
		}
		r.anchors[anchor.Ref.Name] = anchor
	}
	r.receipt.Anchor = &anchor
	r.protocol = 1
	r.setRef(anchor.Ref)
	r.calls = append(r.calls, "initialization")
	if r.lostFinalize {
		r.lostFinalize = false
		return domain.RepositoryInitializationReceipt{}, errors.New("completion acknowledgement lost")
	}
	return *r.receipt, nil
}

func (r *initializingPublicationRemote) Push(ctx context.Context, repo string, snaps []domain.Snapshot, docs []domain.SessionDoc, refs []domain.Ref, force, appendMode bool) error {
	for _, ref := range refs {
		if r.protocol != 1 {
			return errors.New("ref before initialization")
		}
		found := false
		for _, current := range r.refs {
			if current.Name != ref.Name {
				continue
			}
			found = true
			if current.Target != ref.Target || current.BranchID != ref.BranchID {
				return domain.ErrSyncConflict
			}
		}
		if !found {
			for _, e := range r.accepted {
				if e.Kind == "birth" && e.Branch == ref.Name && e.BranchID == ref.BranchID {
					found = true
				}
			}
			if !found {
				return domain.ErrSyncConflict
			}
		}
	}
	return r.strictPublicationRemote.Push(ctx, repo, snaps, docs, refs, force, appendMode)
}

func initialPublicationFixture(t *testing.T) (*SyncRepoService, *storage.FileStore, *initializingPublicationRemote, inbound.SyncInput, domain.ContentHash) {
	t.Helper()
	root := t.TempDir()
	repo := domain.Repo{ID: domain.HashContent([]byte("new repository")), DefaultBranch: "main", LocalPath: root, RemoteURL: "https://synthetic.invalid/owner/repo", GitRemoteURL: "https://code.invalid/owner/repo", RepositoryID: "repository_fixture"}
	st := storage.NewFileStore(root)
	add := func(label, branch string) domain.ContentHash {
		doc := domain.SessionDoc{CIR: domain.CIRDocument{Envelope: domain.Envelope{CIRVersion: "1", SourceProvider: domain.ProviderCodex}, Events: []domain.Event{{Kind: domain.EventMessage, Role: "user", Blocks: []domain.ContentBlock{{Type: "text", Text: label}}}}}}
		id, err := st.PutDoc(context.Background(), doc)
		if err != nil {
			t.Fatal(err)
		}
		if err := st.PutSnapshot(context.Background(), domain.Snapshot{RepoID: repo.ID, ID: id, DocHash: id, Branch: branch}); err != nil {
			t.Fatal(err)
		}
		if err := st.PutRef(context.Background(), domain.Ref{RepoID: repo.ID, Kind: domain.RefBranch, Name: branch, BranchID: domain.LegacyContextBranchID(repo.ID, branch), Target: id}); err != nil {
			t.Fatal(err)
		}
		return id
	}
	id := add("first captured request", "main")
	foreign := add("separate unpublished work", "other")
	for i, kind := range []string{"position", "publish"} {
		e := domain.HistoryEvent{ID: strings.Repeat(string(rune('a'+i)), 32), RepoID: repo.ID, Kind: kind, Branch: "main", BranchID: domain.LegacyContextBranchID(repo.ID, "main"), Source: id, Target: id, GitAfter: strings.Repeat("c", 40), CreatedAt: time.Unix(int64(i+1), 0).UTC()}
		if err := st.PutHistoryEvent(context.Background(), e); err != nil {
			t.Fatal(err)
		}
	}
	r := &initializingPublicationRemote{strictPublicationRemote: &strictPublicationRemote{memoryObjects: map[domain.ContentHash]domain.MemoryDigest{}, attachments: map[domain.ContentHash]domain.ContentHash{}}, repo: repo}
	r.repo.LocalPath = ""
	svc := newTestSyncService(st, r, pushOrderGit{repo: repo})
	return svc, st, r, inbound.SyncInput{RepoID: repo.ID, Cwd: root, Ref: "main"}, foreign
}

func TestInitializationPrecedesSelectedHistoryAndRefs(t *testing.T) {
	svc, _, remote, in, foreign := initialPublicationFixture(t)
	if _, err := svc.Connect(context.Background(), in); err != nil {
		t.Fatal(err)
	}
	if remote.receipt == nil || remote.receipt.Anchor != nil || remote.protocol != 1 {
		t.Fatal("connect fabricated an initial conversation")
	}
	if _, err := svc.Push(context.Background(), in); err != nil {
		t.Fatal(err)
	}
	if remote.protocol != 1 || remote.finalizes != 1 || slices.Contains(remote.snaps, foreign) || len(remote.refs) != 1 {
		t.Fatal("initialization widened the selection")
	}
	init := slices.Index(remote.calls, "initialization")
	if init < 0 || slices.Index(remote.calls, "history") < init || slices.Index(remote.calls, "ref") < init || len(remote.unsync) != 0 {
		t.Fatal(remote.calls)
	}
	for _, event := range remote.sent {
		if event.Kind == "birth" {
			t.Fatal("initialization fabricated Git birth")
		}
	}
}

func TestInitializationFailuresPreservePublication(t *testing.T) {
	for _, kind := range []string{"unsupported", "creation-conflict", "creation-denied", "finalize-denied", "finalize-conflict", "existing-empty", "existing-nonempty", "history-only"} {
		t.Run(kind, func(t *testing.T) {
			svc, _, r, in, _ := initialPublicationFixture(t)
			switch kind {
			case "unsupported":
				r.beginErr = domain.ErrRepositoryInitializationUnsupported
			case "creation-conflict":
				r.beginErr = domain.ErrRepositoryInitializationConflict
			case "creation-denied":
				r.beginErr = errors.New("manage permission required")
			case "finalize-denied":
				r.finalizeErr = errors.New("manage permission revoked")
			case "finalize-conflict":
				r.finalizeErr = domain.ErrRepositoryInitializationConflict
			case "existing-empty", "existing-nonempty":
				r.exists = true
				if kind == "existing-nonempty" {
					r.refs = []domain.Ref{{Kind: domain.RefBranch, Name: "old", Target: domain.HashContent([]byte("old"))}}
				}
			case "history-only":
				in.Ref = ""
				in.Publication = &domain.PublicationScope{HistoryOnly: true, Branches: []domain.PublicationBranch{{Branch: "main", BranchID: domain.LegacyContextBranchID(in.RepoID, "main")}}}
			}
			_, err := svc.Push(context.Background(), in)
			if err == nil || len(r.sent)+len(r.writes) != 0 {
				t.Fatalf("failed initialization published: %v", err)
			}
			if strings.HasPrefix(kind, "existing-") && (r.protocol != 0 || r.begins != 0 || !errors.Is(err, domain.ErrContextProtocolRequired)) {
				t.Fatal("existing empty state inferred as new")
			}
		})
	}
}

func TestInitializationLostAcknowledgements(t *testing.T) {
	for _, phase := range []string{"begin", "finalize"} {
		t.Run(phase, func(t *testing.T) {
			svc, _, r, in, _ := initialPublicationFixture(t)
			r.lostBegin, r.lostFinalize = phase == "begin", phase == "finalize"
			if _, err := svc.Push(context.Background(), in); err == nil {
				t.Fatal("lost response reported success")
			}
			if len(r.sent)+len(r.writes) != 0 {
				t.Fatal("continued without acknowledgement")
			}
			id := r.receipt.CreationID
			if _, err := svc.Push(context.Background(), in); err != nil {
				t.Fatal(err)
			}
			if r.receipt.CreationID != id || r.finalizes != 1 || r.protocol != 1 {
				t.Fatal("retry created a new initialization")
			}
		})
	}
}

func TestInitializationRetryDoesNotRestoreOldReceiptTip(t *testing.T) {
	svc, _, r, in, newer := initialPublicationFixture(t)
	r.lostFinalize = true
	if _, err := svc.Push(context.Background(), in); err == nil {
		t.Fatal("expected lost response")
	}
	original := *r.receipt.Anchor
	advanced := original.Ref
	advanced.Target = newer
	r.setRef(advanced)
	if _, err := svc.Push(context.Background(), in); !errors.Is(err, domain.ErrSyncConflict) {
		t.Fatal(err)
	}
	if r.refs[0].Target != newer || !reflect.DeepEqual(original, *r.receipt.Anchor) || r.finalizes != 1 {
		t.Fatal("old receipt rewound current branch")
	}
}

func TestInitializationBeforeBarePublicationObservesAllAuthorizedLegacyBranches(t *testing.T) {
	svc, _, r, in, foreign := initialPublicationFixture(t)
	in.Ref = ""
	in.Force, in.Append = true, true
	if err := svc.initializeBeforeBroadPublication(context.Background(), in, in.RepoID); err != nil {
		t.Fatal(err)
	}
	if r.receipt == nil || r.receipt.Anchor == nil || len(r.anchors) != 2 || r.protocol != 1 || !slices.Contains(r.snaps, foreign) {
		t.Fatal("bare publication did not initialize its selected legacy branches")
	}
}

func TestInitializationUsesOrdinaryObservedBirth(t *testing.T) {
	svc, st, r, in, _ := initialPublicationFixture(t)
	ref, err := st.GetRef(context.Background(), in.RepoID, domain.RefBranch, "main")
	if err != nil {
		t.Fatal(err)
	}
	id := strings.Repeat("d", 32)
	e := domain.HistoryEvent{ID: id, RepoID: in.RepoID, Kind: "birth", Branch: "new-branch", BranchID: id, Source: ref.Target, Target: ref.Target, CreatedAt: time.Unix(3, 0).UTC()}
	if err := st.PutHistoryEvent(context.Background(), e); err != nil {
		t.Fatal(err)
	}
	if err := st.PutRef(context.Background(), domain.Ref{RepoID: in.RepoID, Kind: domain.RefBranch, Name: e.Branch, BranchID: id, Target: ref.Target}); err != nil {
		t.Fatal(err)
	}
	in.Ref = e.Branch
	if _, err := svc.Connect(context.Background(), in); err != nil {
		t.Fatal(err)
	}
	begins := r.begins
	r.beginErr = errors.New("member cannot manage initialization")
	if _, err := svc.Push(context.Background(), in); err != nil {
		t.Fatal(err)
	}
	if r.protocol != 1 || r.finalizes != 0 || r.begins != begins || len(r.refs) != 1 || r.refs[0].Name != e.Branch {
		t.Fatal("real birth was routed through privileged legacy anchoring")
	}
	found := false
	for _, sent := range r.sent {
		if sent.Kind == "birth" {
			if !reflect.DeepEqual(sent, e) {
				t.Fatal("invented birth")
			}
			found = true
		}
	}
	if !found {
		t.Fatal("actual birth was not published")
	}
	state, err := r.RepositoryInitializationState(context.Background(), in.RepoID, e.Branch)
	if err != nil || state.InitialAnchorAvailable {
		t.Fatal("ordinary publication left false anchor eligibility")
	}
}

func TestInitializationAllowsLaterIndependentLegacyBranches(t *testing.T) {
	for _, first := range []string{"main", "feature"} {
		t.Run(first, func(t *testing.T) {
			svc, st, remote, in, foreign := initialPublicationFixture(t)
			if first == "feature" {
				ref, err := st.GetRef(context.Background(), in.RepoID, domain.RefBranch, "main")
				if err != nil {
					t.Fatal(err)
				}
				id := strings.Repeat("e", 32)
				event := domain.HistoryEvent{ID: id, RepoID: in.RepoID, Kind: "birth", Branch: first, BranchID: id, Source: ref.Target, Target: ref.Target, CreatedAt: time.Unix(3, 0).UTC()}
				if err := st.PutHistoryEvent(context.Background(), event); err != nil {
					t.Fatal(err)
				}
				if err := st.PutRef(context.Background(), domain.Ref{RepoID: in.RepoID, Kind: domain.RefBranch, Name: first, BranchID: id, Target: ref.Target}); err != nil {
					t.Fatal(err)
				}
			}
			in.Ref = first
			if _, err := svc.Push(context.Background(), in); err != nil {
				t.Fatal(err)
			}
			if slices.Contains(remote.snaps, foreign) {
				t.Fatal("first selection uploaded the later branch")
			}
			firstRefs := slices.Clone(remote.refs)
			for _, next := range []string{"main", "other"} {
				in.Ref = next
				if _, err := svc.Push(context.Background(), in); err != nil {
					t.Fatal(err)
				}
			}
			if remote.finalizes != 2 {
				t.Fatalf("legacy branches observed %d times, want two", remote.finalizes)
			}
			for _, original := range firstRefs {
				if !slices.Contains(remote.refs, original) {
					t.Fatal("later initialization changed the first branch")
				}
			}
		})
	}
}
