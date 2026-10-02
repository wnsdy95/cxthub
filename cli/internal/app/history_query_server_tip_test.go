package app

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/wnsdy95/cxthub/cli/internal/domain"
	"github.com/wnsdy95/cxthub/cli/internal/ports/inbound"
	"github.com/wnsdy95/cxthub/cli/internal/ports/outbound"
)

type historyServerTipGit struct{ historyGit }

func (historyServerTipGit) CurrentBranch(context.Context, string) (string, error) {
	panic("server tip must not read the local Git branch")
}

func (historyServerTipGit) CurrentCommit(context.Context, string) (string, error) {
	panic("server tip must not read the local Git commit")
}

type historyServerTipPanicReader struct{}

func (historyServerTipPanicReader) ListSnapshots(context.Context, string, string) ([]domain.Snapshot, error) {
	panic("server tip must not read local snapshots")
}

func (historyServerTipPanicReader) ListSnapshotCatalog(context.Context, string) ([]domain.Snapshot, error) {
	panic("server tip must not read the local catalog")
}

func (historyServerTipPanicReader) ListRefs(context.Context, string) ([]domain.Ref, error) {
	panic("server tip must not read local refs")
}

func (historyServerTipPanicReader) ResolveLocalBranch(context.Context, string, string) (domain.LocalBranchBinding, error) {
	panic("server tip must not resolve local aliases")
}

func (historyServerTipPanicReader) ReadWorkingPosition(context.Context, string) (domain.WorkingPosition, error) {
	panic("server tip must not read the local cursor")
}

type historyServerTipRemote struct {
	historyRemote
	repo string
}

func (r *historyServerTipRemote) QueryContext(ctx context.Context, repo string, in domain.ContextSelection) (domain.ContextQueryView, error) {
	r.repo = repo
	return r.historyRemote.QueryContext(ctx, repo, in)
}

func historyServerTipView(repo, branch string) domain.ContextQueryView {
	tip := domain.HashContent([]byte("server tip"))
	merged := domain.HashContent([]byte("merged source"))
	return domain.ContextQueryView{
		Version:   domain.QueryContractVersion,
		Branch:    branch,
		Position:  tip,
		StateHash: domain.HashContent([]byte("server state")),
		Revision:  domain.RepositoryRevision{Evidence: 17, Graph: 8, Pending: 2},
		Snapshots: []domain.Snapshot{
			{ID: tip, RepoID: repo, Branch: "capture-label"},
			{ID: merged, RepoID: repo, Branch: "feature"},
		},
		Inclusion: &domain.BranchContext{
			BranchID: "server-branch", SnapshotID: tip, CodeCommit: strings.Repeat("b", 40),
			SnapshotIDs: []domain.ContentHash{tip, merged},
		},
	}
}

func TestHistoryQueryServerTipIgnoresLocalSelection(t *testing.T) {
	_, stale := makeHistoryQuery()
	repo := stale.refs[0].RepoID
	git := historyServerTipGit{historyGit{repo}}
	for _, tc := range []struct {
		name  string
		local outbound.SnapshotListReader
		code  outbound.CodePosition
	}{
		{"stale refs and cursor", &pinnedHistoryReader{historyReader: stale, position: domain.WorkingPosition{Branch: "feature", Snapshot: stale.snaps[1].ID}}, git},
		{"absent local data", &historyReader{}, git},
		{"panic on any local read", historyServerTipPanicReader{}, git},
		{"no local or code readers", nil, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			remote := &historyServerTipRemote{historyRemote: historyRemote{view: historyServerTipView(repo, "main")}}
			s := NewHistoryQueryService(git, tc.code, tc.local, remote)
			out, err := s.QueryHistory(context.Background(), inbound.HistoryQueryInput{Cwd: "worktree", Branch: "main", Server: true, ServerTip: true})
			if err != nil {
				t.Fatal(err)
			}
			wantSelection := domain.ContextSelection{Branch: "main", Position: "main", Scope: "current"}
			if remote.repo != repo || remote.input != wantSelection || remote.calls != 1 {
				t.Fatalf("wrong authoritative request: repo=%q selection=%+v calls=%d", remote.repo, remote.input, remote.calls)
			}
			view := remote.view
			want := domain.HistoryQueryResult{
				Version: domain.QueryContractVersion,
				Selection: domain.HistorySelection{
					Ref: "main", Branch: "main", Scope: "current", Source: "server", CodeCommit: view.Inclusion.CodeCommit,
				},
				StateHash: view.StateHash, Revision: &view.Revision, Position: view.Position,
				Snapshots: view.Snapshots, Inclusion: view.Inclusion, Complete: true,
				Missing: []domain.ContentHash{}, ServerChecked: true,
			}
			if !reflect.DeepEqual(out, want) {
				t.Fatalf("server projection changed: got=%+v want=%+v", out, want)
			}
		})
	}
}

func TestHistoryQueryServerTipUsesExplicitBranch(t *testing.T) {
	repo := string(domain.HashContent([]byte("repo")))
	git := historyServerTipGit{historyGit{repo}}
	branch := "release/current"
	remote := &historyServerTipRemote{historyRemote: historyRemote{view: historyServerTipView(repo, branch)}}
	s := NewHistoryQueryService(git, git, historyServerTipPanicReader{}, remote)
	out, err := s.QueryHistory(context.Background(), inbound.HistoryQueryInput{Branch: branch, Server: true, ServerTip: true})
	if err != nil || out.Selection.Ref != branch || out.Selection.Branch != branch || remote.input != (domain.ContextSelection{Branch: branch, Position: branch, Scope: "current"}) {
		t.Fatalf("explicit branch lost: out=%+v selection=%+v err=%v", out, remote.input, err)
	}
}

func TestHistoryQueryServerTipRejectsIncompatibleArguments(t *testing.T) {
	for _, tc := range []struct {
		name   string
		change func(*inbound.HistoryQueryInput)
	}{
		{"without server", func(in *inbound.HistoryQueryInput) { in.Server = false }},
		{"implicit branch", func(in *inbound.HistoryQueryInput) { in.Branch = "" }},
		{"HEAD", func(in *inbound.HistoryQueryInput) { in.Branch = "HEAD" }},
		{"ref", func(in *inbound.HistoryQueryInput) { in.Ref = "main" }},
		{"position", func(in *inbound.HistoryQueryInput) { in.Position = domain.HashContent([]byte("pinned")) }},
		{"all", func(in *inbound.HistoryQueryInput) { in.All = true }},
		{"retained", func(in *inbound.HistoryQueryInput) { in.Retained = true }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			in := inbound.HistoryQueryInput{Branch: "main", Server: true, ServerTip: true}
			tc.change(&in)
			// Invalid selection must fail before any repository or remote lookup.
			s := NewHistoryQueryService(nil, nil, historyServerTipPanicReader{}, nil)
			if _, err := s.QueryHistory(context.Background(), in); err == nil || !strings.Contains(err.Error(), "invalid_arguments") {
				t.Fatalf("incompatible selection accepted: %+v err=%v", in, err)
			}
		})
	}
	for _, branch := range []string{"../main", " main", "main\n", "-main", "main.lock", "main//child", string(domain.HashContent([]byte("not a branch")))} {
		t.Run(branch, func(t *testing.T) {
			s := NewHistoryQueryService(nil, nil, historyServerTipPanicReader{}, nil)
			_, err := s.QueryHistory(context.Background(), inbound.HistoryQueryInput{Branch: branch, Server: true, ServerTip: true})
			if !errors.Is(err, domain.ErrInvalidRef) {
				t.Fatalf("invalid branch accepted: %q err=%v", branch, err)
			}
		})
	}
}

func TestHistoryQueryServerTipRejectsRemoteFailuresWithoutFallback(t *testing.T) {
	repo := string(domain.HashContent([]byte("repo")))
	git := historyServerTipGit{historyGit{repo}}
	denied := errors.New("server access denied")
	for _, tc := range []struct {
		name   string
		change func(*historyRemote)
		want   error
	}{
		{"denied", func(r *historyRemote) { r.err = denied }, denied},
		{"missing branch", func(r *historyRemote) { r.err = domain.ErrNotFound }, domain.ErrNotFound},
		{"wrong branch", func(r *historyRemote) { r.view.Branch = "other" }, domain.ErrSelectionChanged},
		{"invalid state hash", func(r *historyRemote) { r.view.StateHash = "invalid" }, domain.ErrHashMismatch},
		{"wrong repository", func(r *historyRemote) { r.view.Snapshots[0].RepoID = "other" }, domain.ErrHashMismatch},
		{"wrong inclusion order", func(r *historyRemote) {
			r.view.Inclusion.SnapshotIDs[0], r.view.Inclusion.SnapshotIDs[1] = r.view.Inclusion.SnapshotIDs[1], r.view.Inclusion.SnapshotIDs[0]
		}, domain.ErrHashMismatch},
		{"unsupported version", func(r *historyRemote) { r.view.Version++ }, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			remote := &historyRemote{view: historyServerTipView(repo, "main")}
			tc.change(remote)
			s := NewHistoryQueryService(git, git, historyServerTipPanicReader{}, remote)
			out, err := s.QueryHistory(context.Background(), inbound.HistoryQueryInput{Branch: "main", Server: true, ServerTip: true})
			if err == nil || (tc.want != nil && !errors.Is(err, tc.want)) {
				t.Fatalf("remote failure not preserved: got=%v want=%v", err, tc.want)
			}
			if remote.calls != 1 || out.ServerChecked || len(out.Snapshots) != 0 || out.Position != "" || out.Revision != nil {
				t.Fatalf("failed remote query yielded history: calls=%d out=%+v", remote.calls, out)
			}
		})
	}
	t.Run("unavailable server", func(t *testing.T) {
		s := NewHistoryQueryService(git, git, historyServerTipPanicReader{}, nil)
		if _, err := s.QueryHistory(context.Background(), inbound.HistoryQueryInput{Branch: "main", Server: true, ServerTip: true}); err == nil || !strings.Contains(err.Error(), "server context query unavailable") {
			t.Fatalf("unavailable server accepted: %v", err)
		}
	})
}

func TestHistoryQueryServerTipPreservesIncompleteProjection(t *testing.T) {
	repo := string(domain.HashContent([]byte("repo")))
	git := historyServerTipGit{historyGit{repo}}
	for _, reason := range []string{"no inclusion", "merge review"} {
		t.Run(reason, func(t *testing.T) {
			remote := &historyRemote{view: historyServerTipView(repo, "main")}
			if reason == "no inclusion" {
				remote.view.Inclusion = nil
			} else {
				remote.view.Inclusion.Merges = []domain.BranchContextMerge{{State: "review"}}
			}
			s := NewHistoryQueryService(git, git, historyServerTipPanicReader{}, remote)
			out, err := s.QueryHistory(context.Background(), inbound.HistoryQueryInput{Branch: "main", Server: true, ServerTip: true})
			if err != nil || !out.ServerChecked || out.Complete || !reflect.DeepEqual(out.Inclusion, remote.view.Inclusion) {
				t.Fatalf("incomplete evidence changed: out=%+v err=%v", out, err)
			}
			if remote.view.Inclusion == nil && out.Selection.CodeCommit != "" {
				t.Fatalf("local code position substituted: %q", out.Selection.CodeCommit)
			}
		})
	}
}
