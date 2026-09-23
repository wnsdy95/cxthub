package app

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

type auditReader struct {
	err    error
	calls  int
	during func()
	prs    []domain.PullRequestMerge
}

func (r *auditReader) ReadAuditCommit(_ context.Context, _ string, sha string) (string, error) {
	r.calls++
	if r.during != nil {
		r.during()
		r.during = nil
	}
	return sha, r.err
}
func (r *auditReader) ReadAuditPR(_ context.Context, _ string, _ int) (domain.PullRequestMerge, bool, error) {
	r.calls++
	return domain.PullRequestMerge{}, false, r.err
}
func (r *auditReader) ListAuditPRs(_ context.Context, _ string, _ int) (outbound.GitAuditPRPage, error) {
	return outbound.GitAuditPRPage{PRs: r.prs, Count: len(r.prs), Anchor: "stable"}, r.err
}
func TestSyncAuditReadOnlyPagedAndRevisionFenced(t *testing.T) {
	svc, st := newFsckSvc(t)
	ctx := systemTestContext()
	repo := hh("audit-repo")
	if _, err := st.PutRepo(ctx, domain.Repo{ID: repo, DefaultBranch: "main", GitRemoteURL: "https://github.com/org/project"}); err != nil {
		t.Fatal(err)
	}
	source := prSnapshot(t, st, repo, "base")
	for i := 1; i <= 6; i++ {
		e := domain.HistoryEvent{ID: fmt.Sprintf("%032x", i), RepoID: string(repo), BranchID: fmt.Sprintf("topic%d", i), Branch: fmt.Sprintf("topic%d", i), Kind: "birth", Source: source, Target: source, GitAfter: strings.Repeat("a", 40), CreatedAt: time.Now().UTC()}
		if err := svc.RecordHistory(ctx, e); err != nil {
			t.Fatal(err)
		}
	}
	before, _ := svc.ListHistory(ctx, repo)
	refs, _ := svc.ListRefs(ctx, repo)
	reader := &auditReader{}
	audit := NewGitSyncAudit(svc, reader)
	first, err := audit.CheckGitHubSync(ctx, repo, "")
	if err != nil {
		t.Fatal(err)
	}
	if first.Processed != 5 || first.Total != 6 || first.NextCursor == "" || reader.calls != 5 {
		t.Fatalf("unbounded first page %+v calls=%d", first, reader.calls)
	}
	second, err := audit.CheckGitHubSync(ctx, repo, first.NextCursor)
	if err != nil {
		t.Fatal(err)
	}
	if second.Processed != 6 || !strings.Contains(second.NextCursor, ":github:") {
		t.Fatal(second)
	}
	reader.prs = []domain.PullRequestMerge{{Number: 9, BaseBranch: "main", HeadBranch: "missed", HeadSHA: strings.Repeat("a", 40), MergeSHA: strings.Repeat("b", 40)}}
	remote, err := audit.CheckGitHubSync(ctx, repo, second.NextCursor)
	if err != nil || len(remote.Checks) != 1 || remote.Checks[0].Code != "github_pr_not_recorded" {
		t.Fatal(remote, err)
	}
	after, _ := svc.ListHistory(ctx, repo)
	afterRefs, _ := svc.ListRefs(ctx, repo)
	if !reflect.DeepEqual(before, after) || !reflect.DeepEqual(refs, afterRefs) {
		t.Fatal("audit mutated repository")
	}
	reader.err = errors.New("provider down")
	failed, err := audit.CheckGitHubSync(ctx, repo, first.NextCursor)
	if err != nil {
		t.Fatal(err)
	}
	if failed.Checks[0].State != "unavailable" {
		t.Fatal(failed)
	}
	reader.err = nil
	reader.during = func() {
		e := before[0]
		e.ID = strings.Repeat("f", 32)
		e.Kind = "position"
		if err := svc.RecordHistory(ctx, e); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := audit.CheckGitHubSync(ctx, repo, first.NextCursor); !errors.Is(err, domain.ErrConflict) {
		t.Fatal("mixed generation accepted", err)
	}
}

func TestAuditRevisionIncludesOverlayEvidenceButIgnoresUnrelatedLiveCaptures(t *testing.T) {
	root, ancestor, live := hh("root"), hh("ancestor"), hh("live")
	v := auditView{origin: "https://github.com/org/project", view: domain.RepositoryView{Refs: []domain.Ref{{Kind: domain.RefBranch, Name: "main", Target: root}}, Snapshots: []domain.Snapshot{{ID: root, Parents: []domain.ContentHash{ancestor}}, {ID: ancestor}, {ID: live, Message: "hook: old"}}}}
	old := auditRevision(v)
	v.view.Snapshots[2].Message = "hook: new"
	if old != auditRevision(v) {
		t.Fatal("unrelated capture invalidated audit")
	}
	v.view.Snapshots[1].GraftParents = []domain.ContentHash{hh("other")}
	if old == auditRevision(v) {
		t.Fatal("ancestor overlay change missed")
	}
	old = auditRevision(v)
	v.view.Revision.Evidence++
	if old == auditRevision(v) {
		t.Fatal("code evidence change missed")
	}
}
