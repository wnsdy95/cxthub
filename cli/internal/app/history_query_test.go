package app

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/wnsdy95/cxthub/cli/internal/domain"
	"github.com/wnsdy95/cxthub/cli/internal/ports/inbound"
)

type historyGit struct{ repo string }

func (g historyGit) CurrentRepo(context.Context, string) (domain.Repo, error) {
	return domain.Repo{ID: domain.ContentHash(g.repo)}, nil
}
func (g historyGit) CurrentBranch(context.Context, string) (string, error) { return "main", nil }
func (g historyGit) CurrentCommit(context.Context, string) (string, error) {
	return strings.Repeat("a", 40), nil
}

type historyReader struct {
	snaps    []domain.Snapshot
	refs     []domain.Ref
	reads    int
	unstable bool
}

func (r *historyReader) ListSnapshots(_ context.Context, _ string, branch string) ([]domain.Snapshot, error) {
	if branch != "" {
		panic("label-based query")
	}
	r.reads++
	out := append([]domain.Snapshot(nil), r.snaps...)
	if r.unstable && len(out) > 0 {
		out[0].Message = strings.Repeat("x", r.reads)
	}
	return out, nil
}
func (r *historyReader) ListRefs(context.Context, string) ([]domain.Ref, error) {
	return append([]domain.Ref(nil), r.refs...), nil
}

type historyRemote struct {
	view  domain.ContextQueryView
	input domain.ContextSelection
	calls int
	err   error
}

func (r *historyRemote) QueryContext(_ context.Context, _ string, in domain.ContextSelection) (domain.ContextQueryView, error) {
	r.calls++
	r.input = in
	return r.view, r.err
}
func makeHistoryQuery() (*HistoryQueryService, *historyReader) {
	repo := string(domain.HashContent([]byte("repo")))
	root := domain.HashContent([]byte("r"))
	feature := domain.HashContent([]byte("f"))
	main := domain.HashContent([]byte("m"))
	unused := domain.HashContent([]byte("u"))
	local := &historyReader{snaps: []domain.Snapshot{{ID: root, RepoID: repo, Branch: "main"}, {ID: feature, RepoID: repo, Branch: "feature", Parents: []domain.ContentHash{root}}, {ID: main, RepoID: repo, Branch: "main", Parents: []domain.ContentHash{root}, GraftParents: []domain.ContentHash{feature}}, {ID: unused, RepoID: repo, Branch: "main"}}, refs: []domain.Ref{{Kind: domain.RefHEAD, Name: "HEAD", RepoID: repo, Symbolic: "main", Target: main}, {Kind: domain.RefBranch, Name: "main", RepoID: repo, Target: main}}}
	return NewHistoryQueryService(historyGit{repo}, historyGit{repo}, local, nil), local
}
func TestHistoryQueryScopesAndMutableMemoryRevision(t *testing.T) {
	s, local := makeHistoryQuery()
	ctx := context.Background()
	out, err := s.QueryHistory(ctx, inbound.HistoryQueryInput{})
	if err != nil || len(out.Snapshots) != 3 || out.ServerChecked || !out.Complete {
		t.Fatal(out, err)
	}
	first := out.StateHash
	local.snaps[2].MemoryHash = domain.HashContent([]byte("new memory"))
	out, err = s.QueryHistory(ctx, inbound.HistoryQueryInput{})
	if err != nil || out.StateHash == first {
		t.Fatal("stale memory receipt", out, err)
	}
	retained, err := s.QueryHistory(ctx, inbound.HistoryQueryInput{Retained: true})
	if err != nil || len(retained.Snapshots) != 4 {
		t.Fatal(retained, err)
	}
	local.refs[0].Target = local.snaps[1].ID
	out, err = s.QueryHistory(ctx, inbound.HistoryQueryInput{})
	if err != nil || len(out.Snapshots) != 2 {
		t.Fatal("HEAD ignored", out, err)
	}
	local.unstable = true
	if _, err = s.QueryHistory(ctx, inbound.HistoryQueryInput{}); !errors.Is(err, domain.ErrSelectionChanged) {
		t.Fatal("mixed observation accepted", err)
	}
}
func TestHistoryQueryServerUsesAuthoritativeOrderAndRevisionWithoutArchiveScan(t *testing.T) {
	s, local := makeHistoryQuery()
	ctx := context.Background()
	position := local.snaps[2].ID
	code := strings.Repeat("a", 40)
	// The server's inclusion list differs deliberately from a local ancestry walk.
	remote := &historyRemote{view: domain.ContextQueryView{Version: 1, Branch: "main", Position: position, StateHash: domain.HashContent([]byte("serverreceipt")), Revision: domain.RepositoryRevision{Evidence: 42, Graph: 9}, Snapshots: []domain.Snapshot{local.snaps[1], local.snaps[0]}, Inclusion: &domain.BranchContext{CodeCommit: code, SnapshotIDs: []domain.ContentHash{local.snaps[1].ID, local.snaps[0].ID}}}}
	s.remote = remote
	out, err := s.QueryHistory(ctx, inbound.HistoryQueryInput{Server: true})
	if err != nil || !out.ServerChecked || len(out.Snapshots) != 2 || out.StateHash != remote.view.StateHash || out.Revision.Evidence != 42 || local.reads != 0 {
		t.Fatal(out, err, local.reads)
	}
	if remote.input.Position != string(position) || remote.input.CodeCommit != code || remote.input.Branch != "main" || remote.input.Scope != "current" {
		t.Fatal(remote.input)
	}
	remote.err = errors.New("access revoked")
	if _, err = s.QueryHistory(ctx, inbound.HistoryQueryInput{Server: true}); err == nil || local.reads != 0 {
		t.Fatal("silent local fallback", err)
	}
}
func TestHistoryExplicitScopesAndAmbiguousRefs(t *testing.T) {
	s, local := makeHistoryQuery()
	ctx := context.Background()
	for _, in := range []inbound.HistoryQueryInput{{All: true, Retained: true}, {Branch: "main", Ref: "HEAD"}, {Server: true, All: true}, {All: true, Ref: "main"}} {
		if _, err := s.QueryHistory(ctx, in); err == nil {
			t.Fatal("accepted", in)
		}
	}
	local.refs = append(local.refs, domain.Ref{RepoID: local.refs[0].RepoID, Name: "main", Kind: domain.RefTag, Target: local.snaps[0].ID})
	if _, err := s.QueryHistory(ctx, inbound.HistoryQueryInput{Ref: "main"}); !errors.Is(err, domain.ErrInvalidRef) {
		t.Fatal(err)
	}
	local.refs[0].Target = domain.HashContent([]byte("unavailable"))
	out, err := s.QueryHistory(ctx, inbound.HistoryQueryInput{})
	if err != nil || out.Complete || len(out.Missing) != 1 {
		t.Fatal(out, err)
	}
}

type pinnedHistoryReader struct {
	*historyReader
	position domain.WorkingPosition
}

func (r *pinnedHistoryReader) ReadWorkingPosition(context.Context, string) (domain.WorkingPosition, error) {
	return r.position, nil
}
func TestHistoryServerPinnedHeadKeepsLogicalBranch(t *testing.T) {
	s, reader := makeHistoryQuery()
	reader.refs[0].Symbolic = ""
	pinned := &pinnedHistoryReader{historyReader: reader, position: domain.WorkingPosition{Snapshot: reader.refs[0].Target, Branch: "main"}}
	s.local = pinned
	remote := &historyRemote{err: errors.New("stop after request")}
	s.remote = remote
	_, _ = s.QueryHistory(context.Background(), inbound.HistoryQueryInput{Server: true})
	if remote.input.Branch != "main" || remote.input.Position != string(pinned.position.Snapshot) {
		t.Fatal("lost named source", remote.input)
	}
	pinned.position.Snapshot = reader.snaps[0].ID
	if _, err := s.QueryHistory(context.Background(), inbound.HistoryQueryInput{Server: true}); !errors.Is(err, domain.ErrSelectionChanged) {
		t.Fatal("racing cursor accepted", err)
	}
}
func TestHistoryExplicitPackagePositionDoesNotFollowBranchTip(t *testing.T) {
	s, local := makeHistoryQuery()
	remote := &historyRemote{err: errors.New("stop after request")}
	s.remote = remote
	_, _ = s.QueryHistory(context.Background(), inbound.HistoryQueryInput{Server: true, Branch: "main", Position: local.snaps[0].ID})
	if remote.input.Branch != "main" || remote.input.Position != string(local.snaps[0].ID) {
		t.Fatal("package moved to current branch tip", remote.input)
	}
}

func TestHistoryExplicitBranchDoesNotUseCurrentWorktreeCode(t *testing.T) {
	s, local := makeHistoryQuery()
	local.refs = append(local.refs, domain.Ref{Kind: domain.RefBranch, Name: "feature", RepoID: local.refs[0].RepoID, Target: local.snaps[1].ID})
	remote := &historyRemote{err: errors.New("request captured")}
	s.remote = remote
	_, _ = s.QueryHistory(context.Background(), inbound.HistoryQueryInput{Ref: "feature", Server: true})
	if remote.input.Branch != "feature" || remote.input.CodeCommit != "" {
		t.Fatalf("current branch code leaked into reference read: %+v", remote.input)
	}
}
