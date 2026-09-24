package app

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

type GitSyncAudit struct {
	core   *Service
	reader outbound.GitSyncReader
}

func NewGitSyncAudit(core *Service, reader outbound.GitSyncReader) *GitSyncAudit {
	return &GitSyncAudit{core, reader}
}

type auditView struct {
	view     domain.RepositoryView
	origin   string
	graphErr error
}

func (a *GitSyncAudit) load(ctx context.Context, repo domain.ContentHash, project bool) (auditView, error) {
	return repositoryRead(ctx, a.core, func(ctx context.Context) (auditView, error) {
		var out auditView
		r, err := a.core.meta.GetRepo(ctx, repo)
		if err != nil {
			return out, err
		}
		out.origin = r.GitRemoteURL
		out.view, err = a.core.loadRepositoryView(ctx, repo)
		if err != nil {
			return out, err
		}
		if !project {
			return out, nil
		}
		g, err := a.core.projectGraphState(ctx, out.view, "")
		out.graphErr = err
		if err == nil {
			out.view.Graph = &g
		}
		return out, nil
	})
}

// The audit generation excludes live pending captures. They may change during
// the scan without changing branch birth/PR evidence. Every referenced snapshot
// is included, so memory/parent changes invalidate the continuation cursor.
func auditRevision(v auditView) string {
	relevant := map[domain.ContentHash]bool{}
	byID := map[domain.ContentHash]domain.Snapshot{}
	for _, s := range v.view.Snapshots {
		byID[s.ID] = s
	}
	stack := []domain.ContentHash{}
	for _, e := range v.view.History {
		stack = append(stack, e.Source, e.Target, e.SharedTarget, e.MemorySource)
	}
	refs := append([]domain.Ref{}, v.view.Refs...)
	sort.Slice(refs, func(i, j int) bool {
		if refs[i].Kind != refs[j].Kind {
			return refs[i].Kind < refs[j].Kind
		}
		return refs[i].Name < refs[j].Name
	})
	for _, ref := range refs {
		stack = append(stack, ref.Target)
	}
	for len(stack) > 0 {
		id := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if relevant[id] {
			continue
		}
		relevant[id] = true
		if sn, ok := byID[id]; ok {
			stack = append(stack, sn.ReachabilityParents()...)
		}
	}
	snapshots := []domain.Snapshot{}
	for _, s := range v.view.Snapshots {
		if relevant[s.ID] {
			snapshots = append(snapshots, s)
		}
	}
	sort.Slice(snapshots, func(i, j int) bool { return snapshots[i].ID < snapshots[j].ID })
	history := append([]domain.HistoryEvent{}, v.view.History...)
	sort.Slice(history, func(i, j int) bool { return history[i].ID < history[j].ID })
	raw, _ := json.Marshal(struct {
		Origin        string
		History       []domain.HistoryEvent
		Snapshots     []domain.Snapshot
		Refs          []domain.Ref
		Evidence      uint64
		DefaultBranch string
		Reflog        []domain.RefLogEntry
	}{v.origin, history, snapshots, refs, v.view.Revision.Evidence, v.view.DefaultBranch, v.view.Reflog})
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

func (a *GitSyncAudit) CheckGitHubSync(ctx context.Context, repo domain.ContentHash, cursor string) (domain.SyncAuditPage, error) {
	ctx = outbound.WithGitRepository(ctx, repo)
	out := domain.SyncAuditPage{Version: 1, Checks: []domain.SyncAuditCheck{}, CheckedAt: time.Now().UTC()}
	if err := domain.ValidateContentHash(repo); err != nil {
		return out, err
	}
	v, err := a.load(ctx, repo, cursor == "")
	if err != nil {
		return out, err
	}
	out.Revision = auditRevision(v)
	if strings.Contains(cursor, ":github:") {
		return a.checkRemotePRPage(ctx, repo, v, cursor, out)
	}
	offset := 0
	if cursor != "" {
		parts := strings.Split(cursor, ":")
		if len(parts) != 2 || len(parts[0]) != 64 {
			return out, domain.ErrValidation
		}
		offset, err = strconv.Atoi(parts[1])
		if err != nil || offset < 0 {
			return out, domain.ErrValidation
		}
		if parts[0] != out.Revision {
			return out, fmt.Errorf("%w: repository evidence changed; restart the sync check", domain.ErrConflict)
		}
	}
	// Stable identity ordering, not event arrival time. A page makes at most five
	// provider requests; no network call holds a repository transaction or lock.
	events := []domain.HistoryEvent{}
	for _, e := range v.view.History {
		if e.Kind == "birth" || e.Kind == "orphan" || e.Kind == "attach" || (e.Kind == "pr-merge" && e.PRCompleted) {
			events = append(events, e)
		}
	}
	sort.Slice(events, func(i, j int) bool { return events[i].ID < events[j].ID })
	out.Total = len(events)
	if offset > len(events) {
		return out, domain.ErrValidation
	}
	if offset == 0 {
		out.Checks = domain.AuditGraphContracts(v.view, v.view.Graph)
		if v.view.Graph != nil {
			out.Checks = append(out.Checks, domain.AuditIntegrationContracts(*v.view.Graph)...)
		}
		if v.graphErr != nil {
			state := "unavailable"
			if errors.Is(v.graphErr, domain.ErrIntegrity) || errors.Is(v.graphErr, domain.ErrValidation) {
				state = "mismatch"
			}
			out.Checks = append(out.Checks, domain.SyncAuditCheck{ID: "graph", State: state, Code: "graph_invalid"})
		} else {
			out.Checks = append(out.Checks, domain.SyncAuditCheck{ID: "graph", State: "verified", Code: "graph_structure_valid"})
		}
	}
	end := min(offset+5, len(events))
	for _, e := range events[offset:end] {
		if err := ctx.Err(); err != nil {
			return out, err
		}
		check := domain.SyncAuditCheck{ID: e.ID + ":github", EventID: e.ID, Snapshot: e.Source, Branch: e.Branch, Creation: e.Creation}
		var err error
		if a.reader == nil || v.origin == "" {
			check.State = "unavailable"
			check.Code = "github_unavailable"
		} else if e.PR != nil && e.PRCompleted {
			var got domain.PullRequestMerge
			var merged bool
			got, merged, err = a.reader.ReadAuditPR(ctx, v.origin, e.PR.Number)
			check.Expected = fmt.Sprintf("PR #%d %s → %s · %s · %s", e.PR.Number, e.PR.HeadBranch, e.PR.BaseBranch, e.PR.HeadSHA, e.PR.MergeSHA)
			check.Actual = fmt.Sprintf("PR #%d %s → %s · %s · %s", got.Number, got.HeadBranch, got.BaseBranch, got.HeadSHA, got.MergeSHA) + fmt.Sprintf(" · merged=%t", merged)
			if err == nil {
				if !merged || got.Number != e.PR.Number || got.HeadBranch != e.PR.HeadBranch || got.BaseBranch != e.PR.BaseBranch || got.HeadSHA != e.PR.HeadSHA || got.MergeSHA != e.PR.MergeSHA {
					check.State = "mismatch"
					check.Code = "github_pr_mismatch"
				} else {
					check.State = "verified"
					check.Code = "github_pr_matches"
				}
			}
		} else {
			sha := e.GitAfter
			if e.Kind == "orphan" {
				sha = e.GitBefore
				if e.Creation != nil && e.Creation.StartCommit != "" {
					sha = e.Creation.StartCommit
				}
			}
			check.Expected = sha
			if sha == "" {
				check.State = "incomplete"
				check.Code = "code_position_missing"
			} else {
				check.Actual, err = a.reader.ReadAuditCommit(ctx, v.origin, sha)
				if err == nil {
					if check.Actual != sha {
						check.State = "mismatch"
						check.Code = "github_commit_mismatch"
					} else {
						check.State = "verified"
						check.Code = "github_commit_matches"
					}
				}
			}
		}
		if err != nil {
			check.State = "unavailable"
			check.Code = "github_lookup_failed"
			check.Actual = ""
		}
		out.Checks = append(out.Checks, check)
	}
	out.Processed = end
	if end < len(events) {
		out.NextCursor = out.Revision + ":" + strconv.Itoa(end)
	} else {
		out.NextCursor = out.Revision + ":github:1:-"
	}
	// Recheck after provider I/O. Do not return success for a generation that was
	// replaced during the request; retries are reads and cannot mutate history.
	fresh, err := a.load(ctx, repo, false)
	if err != nil {
		return out, err
	}
	if auditRevision(fresh) != out.Revision {
		return out, fmt.Errorf("%w: repository evidence changed; restart the sync check", domain.ErrConflict)
	}
	return out, nil
}

// Also search the reverse direction: a merged GitHub PR may have no CXTHub
// receipt at all. Deleted branch refs do not affect this PR-based check.
func (a *GitSyncAudit) checkRemotePRPage(ctx context.Context, repo domain.ContentHash, v auditView, cursor string, out domain.SyncAuditPage) (domain.SyncAuditPage, error) {
	parts := strings.Split(cursor, ":")
	if len(parts) != 4 || parts[1] != "github" {
		return out, domain.ErrValidation
	}
	page, err := strconv.Atoi(parts[2])
	if err != nil || page < 1 || page > 10000 {
		return out, domain.ErrValidation
	}
	if parts[0] != out.Revision {
		return out, fmt.Errorf("%w: repository evidence changed; restart the sync check", domain.ErrConflict)
	}
	if a.reader == nil || v.origin == "" {
		out.Checks = append(out.Checks, domain.SyncAuditCheck{ID: "github-inventory", State: "unavailable", Code: "github_unavailable"})
		return out, nil
	}
	first, err := a.reader.ListAuditPRs(ctx, v.origin, 1)
	if err != nil {
		out.Checks = append(out.Checks, domain.SyncAuditCheck{ID: "github-inventory", State: "unavailable", Code: "github_lookup_failed"})
		return out, nil
	}
	anchor := first.Anchor
	if page > 1 && parts[3] != anchor {
		return out, fmt.Errorf("%w: GitHub PR list changed; restart the sync check", domain.ErrConflict)
	}
	result, err := a.reader.ListAuditPRs(ctx, v.origin, page)
	if err != nil {
		out.Checks = append(out.Checks, domain.SyncAuditCheck{ID: "github-inventory:" + strconv.Itoa(page), State: "unavailable", Code: "github_lookup_failed"})
		return out, nil
	}
	// Protect the first page against a change between anchor and content reads.
	if page == 1 {
		if result.Anchor != anchor {
			return out, domain.ErrConflict
		}
	}
	for _, pr := range result.PRs {
		found := false
		for _, e := range v.view.History {
			if e.PRCompleted && e.PR != nil && *e.PR == pr {
				found = true
				break
			}
		}
		c := domain.SyncAuditCheck{ID: "github-pr:" + strconv.Itoa(pr.Number), Branch: pr.HeadBranch, State: "verified", Code: "github_receipt_present", Expected: fmt.Sprintf("PR #%d %s → %s · %s", pr.Number, pr.HeadBranch, pr.BaseBranch, pr.MergeSHA)}
		if !found {
			c.State = "incomplete"
			c.Code = "github_pr_not_recorded"
		}
		out.Checks = append(out.Checks, c)
	}
	out.Processed = (page-1)*20 + result.Count
	out.Total = out.Processed
	out.Phase = "github"
	if result.More {
		out.NextCursor = out.Revision + ":github:" + strconv.Itoa(page+1) + ":" + anchor
	}
	fresh, err := a.load(ctx, repo, false)
	if err != nil {
		return out, err
	}
	if auditRevision(fresh) != out.Revision {
		return out, domain.ErrConflict
	}
	return out, nil
}
