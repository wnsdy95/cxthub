package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"

	"github.com/wnsdy95/cxthub/cli/internal/domain"
	"github.com/wnsdy95/cxthub/cli/internal/ports/outbound"
)

// SelectedPullService applies only the server projection for the selected code.
// It does not move shared refs, change raw memory attachments or launch agents.
type SelectedPullService struct {
	store  outbound.SelectedPullStore
	query  outbound.ContextQueryReader
	memory outbound.EffectiveMemoryReader
	code   outbound.CodePosition
	git    outbound.GitContext
	remote string
}

func NewSelectedPullService(store outbound.SelectedPullStore, query outbound.ContextQueryReader, memory outbound.EffectiveMemoryReader, code outbound.CodePosition, git outbound.GitContext, remote string) *SelectedPullService {
	return &SelectedPullService{store: store, query: query, memory: memory, code: code, git: git, remote: remote}
}

type SelectedPullInput struct{ RepoID, Cwd string }

// Preview freezes an exact, complete context/memory read without applying it.
// Named remote/ref routing belongs to the adapter and must be validated before
// constructing this service; it cannot be silently substituted here.
func (s *SelectedPullService) Preview(ctx context.Context, in SelectedPullInput) (outbound.SelectedPullPlan, error) {
	plan, err := s.prepareSelection(ctx, in)
	if err != nil {
		return outbound.SelectedPullPlan{}, err
	}
	plan.Context, plan.Memory, err = s.readProjection(ctx, plan.RepoID, plan.Selection, &pullReadAnchor{})
	if err != nil {
		return outbound.SelectedPullPlan{}, err
	}
	if err := s.checkLocal(ctx, in.Cwd, plan); err != nil {
		return outbound.SelectedPullPlan{}, err
	}
	plan.ID = outbound.SelectedPullPlanID(plan)
	return plan, nil
}

// ApplyCurrent is the one-command pull path. It may refresh only server read
// revisions, at most three times, while retaining the original local selection,
// index, prior receipt and every observed context/memory page. Explicit Preview
// and Apply remain exact-revision operations and never silently replace a plan.
func (s *SelectedPullService) ApplyCurrent(ctx context.Context, in SelectedPullInput) (outbound.SelectedPullReceipt, error) {
	var zero outbound.SelectedPullReceipt
	plan, err := s.prepareSelection(ctx, in)
	if err != nil {
		return zero, err
	}
	anchor := &pullReadAnchor{}
	for attempt := 0; attempt < 3; attempt++ {
		if err := ctx.Err(); err != nil {
			return zero, err
		}
		if err := s.checkLocal(ctx, in.Cwd, plan); err != nil {
			return zero, err
		}
		plan.Context, plan.Memory, err = s.readProjection(ctx, plan.RepoID, plan.Selection, anchor)
		if err == nil {
			plan.ID = outbound.SelectedPullPlanID(plan)
			var receipt outbound.SelectedPullReceipt
			receipt, err = s.apply(ctx, in.Cwd, plan, anchor)
			if err == nil {
				return receipt, nil
			}
		}
		// A concurrent local move is terminal even if a server revision also changed.
		if localErr := s.checkLocal(ctx, in.Cwd, plan); localErr != nil {
			return zero, localErr
		}
		var contention *pullRevisionContention
		if !errors.As(err, &contention) {
			return zero, err
		}
	}
	return zero, err
}

func (s *SelectedPullService) prepareSelection(ctx context.Context, in SelectedPullInput) (outbound.SelectedPullPlan, error) {
	var plan outbound.SelectedPullPlan
	if s.store == nil || s.query == nil || s.memory == nil || s.code == nil || s.git == nil || s.remote == "" || s.remote == "configured" {
		return plan, fmt.Errorf("selected pull dependencies and explicit remote identity are required")
	}
	repo, err := s.git.CurrentRepo(ctx, in.Cwd)
	if err != nil {
		return plan, err
	}
	if in.RepoID == "" {
		in.RepoID = string(repo.ID)
	}
	if in.RepoID != string(repo.ID) || domain.ValidateContentHash(repo.ID) != nil {
		return plan, domain.ErrHashMismatch
	}
	state, err := s.store.ReadCheckoutState(ctx, in.RepoID)
	if err != nil {
		return plan, err
	}
	p := state.Position
	if p == nil || p.RepoID != in.RepoID || p.WorktreeID == "" || domain.ValidateContentHash(p.Snapshot) != nil {
		return plan, fmt.Errorf("%w: select a verified worktree context before pull", domain.ErrSelectionChanged)
	}
	code, err := s.code.CurrentCommit(ctx, in.Cwd)
	if err != nil {
		return plan, err
	}
	if !domain.ValidGitOID(code) || p.GitCommit != code {
		return plan, fmt.Errorf("%w: selected context does not match the current Git commit", domain.ErrCodePositionMismatch)
	}
	previous, err := s.store.ReadAppliedPull(ctx, in.RepoID, s.remote)
	if err != nil && !errors.Is(err, domain.ErrNotFound) {
		return plan, err
	}
	plan = outbound.SelectedPullPlan{Version: 1, RepoID: in.RepoID, Remote: s.remote, Expected: state, Previous: previous.Plan.ID,
		Selection: domain.ContextSelection{Branch: p.Branch, Position: string(p.Snapshot), CodeCommit: code, Scope: "current"}}
	return plan, s.checkLocal(ctx, in.Cwd, plan)
}

func (s *SelectedPullService) Apply(ctx context.Context, cwd string, plan outbound.SelectedPullPlan) (outbound.SelectedPullReceipt, error) {
	return s.apply(ctx, cwd, plan, &pullReadAnchor{})
}

func (s *SelectedPullService) apply(ctx context.Context, cwd string, plan outbound.SelectedPullPlan, anchor *pullReadAnchor) (outbound.SelectedPullReceipt, error) {
	var zero outbound.SelectedPullReceipt
	if plan.Version != 1 || plan.Remote != s.remote || plan.ID != outbound.SelectedPullPlanID(plan) {
		return zero, domain.ErrHashMismatch
	}
	if err := s.checkLocal(ctx, cwd, plan); err != nil {
		return zero, err
	}
	current, err := s.store.ReadAppliedPull(ctx, plan.RepoID, s.remote)
	if err != nil && !errors.Is(err, domain.ErrNotFound) {
		return zero, err
	}
	if current.Plan.ID != plan.ID && current.Plan.ID != plan.Previous {
		return zero, fmt.Errorf("%w: applied-pull receipt changed", domain.ErrSelectionChanged)
	}
	if err := anchor.context(plan.Context); err != nil {
		return zero, err
	}
	for i, page := range plan.Memory {
		if err := anchor.memory(i, page); err != nil {
			return zero, err
		}
	}
	view, memory, err := s.readProjection(ctx, plan.RepoID, plan.Selection, anchor)
	if err != nil {
		return zero, err
	}
	// The hash and read revision are server-owned. Pending activity does not
	// invalidate the selected committed projection, so compare only its inputs.
	if !samePullProjection(plan.Context, plan.Memory, view, memory) {
		if !pullProjectionRevisionChanged(plan.Context, plan.Memory, view, memory) {
			return zero, domain.ErrHashMismatch
		}
		return zero, retryPullRevision("prepared pull revision changed before apply")
	}
	if err := s.checkLocal(ctx, cwd, plan); err != nil {
		return zero, err
	}
	if current.Plan.ID == plan.ID {
		return current, nil
	}
	return s.store.ApplySelectedPull(ctx, plan)
}

func (s *SelectedPullService) checkLocal(ctx context.Context, cwd string, plan outbound.SelectedPullPlan) error {
	repo, err := s.git.CurrentRepo(ctx, cwd)
	if err != nil {
		return err
	}
	if string(repo.ID) != plan.RepoID {
		return domain.ErrHashMismatch
	}
	branch, err := s.git.CurrentBranch(ctx, cwd)
	if err != nil {
		return err
	}
	if plan.Expected.Position == nil || branch != plan.Expected.Position.GitBranch() {
		return fmt.Errorf("%w: actual Git branch changed", domain.ErrSelectionChanged)
	}
	actual, err := s.code.CurrentCommit(ctx, cwd)
	if err != nil {
		return err
	}
	if actual != plan.Selection.CodeCommit {
		return domain.ErrCodePositionMismatch
	}
	state, err := s.store.ReadCheckoutState(ctx, plan.RepoID)
	if err != nil {
		return err
	}
	if !reflect.DeepEqual(state, plan.Expected) {
		return fmt.Errorf("%w: local checkout state changed", domain.ErrSelectionChanged)
	}
	return nil
}

func samePullProjection(a domain.ContextQueryView, am []domain.EffectiveMemoryPage, b domain.ContextQueryView, bm []domain.EffectiveMemoryPage) bool {
	if a.StateHash != b.StateHash || a.Position != b.Position || a.Revision.Graph != b.Revision.Graph || a.Revision.Evidence != b.Revision.Evidence || len(am) != len(bm) {
		return false
	}
	// Keep item/page equality too: a broken peer must not reuse a state hash for
	// different content and have that response treated as the previewed result.
	a.Revision.Pending, b.Revision.Pending = 0, 0
	if !reflect.DeepEqual(a, b) {
		return false
	}
	for i := range am {
		left, right := am[i], bm[i]
		left.Revision.Pending, right.Revision.Pending = 0, 0
		if !reflect.DeepEqual(left, right) {
			return false
		}
	}
	return true
}

func (s *SelectedPullService) readProjection(ctx context.Context, repo string, selection domain.ContextSelection, anchor *pullReadAnchor) (domain.ContextQueryView, []domain.EffectiveMemoryPage, error) {
	view, err := s.query.QueryContext(ctx, repo, selection)
	if err != nil {
		return view, nil, err
	}
	if err := domain.ValidateContextQuery(repo, selection, view); err != nil {
		return view, nil, err
	}
	if view.Position != domain.ContentHash(selection.Position) || view.Branch != selection.Branch {
		return view, nil, fmt.Errorf("%w: server selected another position or branch", domain.ErrSelectionChanged)
	}
	if err := anchor.context(view); err != nil {
		return view, nil, err
	}
	request := domain.EffectiveMemoryRequest{Selection: domain.EffectiveMemorySelection{Branch: selection.Branch, SnapshotID: view.Position, CodeCommit: selection.CodeCommit}, Content: "prompt", Limit: 50}
	if err := request.Selection.Validate(); err != nil {
		return view, nil, err
	}
	var pages []domain.EffectiveMemoryPage
	seen, cursors := map[domain.ContentHash]bool{}, map[string]bool{}
	var first domain.EffectiveMemoryPage
	count, size := 0, 0
	for {
		page, err := s.memory.QueryEffectiveMemory(ctx, repo, request)
		if err != nil {
			if request.Cursor != "" && errors.Is(err, domain.ErrEffectiveMemoryCursorStale) {
				return view, nil, retryPullRevision("memory pagination cursor became stale")
			}
			return view, nil, err
		}
		if !validEffectivePromptPage(page, request) {
			return view, nil, domain.ErrHashMismatch
		}
		if len(pages) == 0 {
			first = page
		} else if first.LineageHash != page.LineageHash || first.Total != page.Total || (first.Revision.Graph == page.Revision.Graph && first.Revision.Evidence == page.Revision.Evidence && first.StateHash != page.StateHash) {
			return view, nil, fmt.Errorf("%w: memory pagination content changed", domain.ErrSelectionChanged)
		}
		for _, item := range page.Items {
			if seen[item.ID] {
				return view, nil, domain.ErrHashMismatch
			}
			seen[item.ID] = true
			count++
			size += len(item.Text)
		}
		if size > 16<<20 || count > page.Total || len(pages) >= 328 {
			return view, nil, fmt.Errorf("selected memory exceeds the bounded complete projection contract")
		}
		if count > page.Total || (page.NextCursor == "" && count != page.Total) || (page.NextCursor != "" && count >= page.Total) || (page.NextCursor != "" && cursors[page.NextCursor]) {
			return view, nil, domain.ErrHashMismatch
		}
		if err := anchor.memory(len(pages), page); err != nil {
			return view, nil, err
		}
		if page.Revision.Graph != view.Revision.Graph || page.Revision.Evidence != view.Revision.Evidence {
			return view, nil, retryPullRevision("memory and context read revisions differ")
		}
		pages = append(pages, page)
		if page.NextCursor == "" {
			if count != page.Total {
				return view, nil, domain.ErrHashMismatch
			}
			break
		}
		if cursors[page.NextCursor] {
			return view, nil, domain.ErrHashMismatch
		}
		cursors[page.NextCursor] = true
		request.Cursor = page.NextCursor
	}
	// Detect revision changes and permission revocation during pagination.
	after, err := s.query.QueryContext(ctx, repo, selection)
	if err != nil {
		return view, nil, err
	}
	if err := domain.ValidateContextQuery(repo, selection, after); err != nil {
		return view, nil, err
	}
	if err := anchor.context(after); err != nil {
		return view, nil, err
	}
	if !samePullProjection(view, nil, after, nil) {
		return view, nil, retryPullRevision("context read revision changed during pull preparation")
	}
	request.Cursor = ""
	check, err := s.memory.QueryEffectiveMemory(ctx, repo, request)
	if err != nil {
		return view, nil, err
	}
	if !validEffectivePromptPage(check, request) {
		return view, nil, domain.ErrHashMismatch
	}
	if err := anchor.memory(0, check); err != nil {
		return view, nil, err
	}
	if !samePullProjection(view, []domain.EffectiveMemoryPage{first}, view, []domain.EffectiveMemoryPage{check}) {
		if first.Revision.Graph == check.Revision.Graph && first.Revision.Evidence == check.Revision.Evidence {
			return view, nil, domain.ErrHashMismatch
		}
		return view, nil, retryPullRevision("memory read revision changed during pull preparation")
	}
	return view, pages, nil
}

func pullProjectionRevisionChanged(a domain.ContextQueryView, am []domain.EffectiveMemoryPage, b domain.ContextQueryView, bm []domain.EffectiveMemoryPage) bool {
	if a.Revision.Graph != b.Revision.Graph || a.Revision.Evidence != b.Revision.Evidence {
		return true
	}
	for i := range am {
		if i < len(bm) && (am[i].Revision.Graph != bm[i].Revision.Graph || am[i].Revision.Evidence != bm[i].Revision.Evidence) {
			return true
		}
	}
	return false
}

// These fingerprints pin response bodies, not only peer-provided hashes. Cursor
// bytes may change with a revision; cursor presence and page contents may not.
// Hashing also prevents a mutable test/client buffer from moving the anchor.
type pullReadAnchor struct {
	contextHash domain.ContentHash
	memoryPages memoryReadAnchor
}

func (a *pullReadAnchor) context(v domain.ContextQueryView) error {
	v.Revision = domain.RepositoryRevision{}
	raw, err := json.Marshal(v)
	if err != nil {
		return err
	}
	hash := domain.HashContent(raw)
	if a.contextHash != "" && a.contextHash != hash {
		return fmt.Errorf("%w: selected context content changed", domain.ErrSelectionChanged)
	}
	a.contextHash = hash
	return nil
}

func (a *pullReadAnchor) memory(index int, p domain.EffectiveMemoryPage) error {
	return a.memoryPages.check(index, p)
}

type pullRevisionContention struct{ error }

func (e *pullRevisionContention) Unwrap() error { return e.error }
func retryPullRevision(message string) error {
	return &pullRevisionContention{fmt.Errorf("%w: %s", domain.ErrSelectionChanged, message)}
}
