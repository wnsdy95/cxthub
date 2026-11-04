package app

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/inbound"
)

type ContextBranchProfile string

const (
	ContextBranchCompatibility ContextBranchProfile = "compatibility"
	ContextBranchComplete      ContextBranchProfile = "complete"
)

type ContextBranchStore interface {
	GetRepo(context.Context, domain.ContentHash) (domain.Repo, error)
	GetSnapshot(context.Context, domain.ContentHash, domain.ContentHash) (domain.Snapshot, error)
	GetRef(context.Context, domain.ContentHash, domain.RefKind, string) (domain.Ref, error)
	CompareAndSwapRef(context.Context, domain.ContentHash, domain.Ref, domain.ContentHash) error
	ListSnapshots(context.Context, domain.ContentHash, string) ([]domain.Snapshot, error)
	ListRefs(context.Context, domain.ContentHash) ([]domain.Ref, error)
	ListPendings(context.Context, domain.ContentHash) ([]domain.Pending, error)
	ReadReflog(context.Context, domain.ContentHash) ([]domain.RefLogEntry, error)
	ApplyJoin(context.Context, domain.JoinMutation) error
}

type ContextBranchScope interface {
	WithinFork(context.Context, domain.ContentHash, func(context.Context) (inbound.ForkOutput, error)) (inbound.ForkOutput, error)
	WithinJoin(context.Context, domain.ContentHash, func(context.Context) (inbound.JoinOutput, error)) (inbound.JoinOutput, error)
	WithinProtocol(context.Context, domain.ContentHash, func(context.Context) error) error
	WithinPreview(context.Context, domain.ContentHash, func(context.Context) (inbound.JoinPreviewOutput, error)) (inbound.JoinPreviewOutput, error)
	WithinHistory(context.Context, domain.ContentHash, func(context.Context) ([]domain.HistoryEvent, error)) ([]domain.HistoryEvent, error)
}

type ContextBranchHistory interface {
	ListHistoryEvents(context.Context, domain.ContentHash) ([]domain.HistoryEvent, error)
}

type ContextBranchProtocol interface {
	EnableContextProtocol(context.Context, domain.ContentHash) error
}

type ContextBranchAccess interface {
	AuthorizeJoin(context.Context, domain.ContentHash, string, bool) error
}

type ContextBranchPending interface {
	ReconcileSharedPending(context.Context, domain.ContentHash) error
}

type ContextBranchOperationsConfig struct {
	Profile  ContextBranchProfile
	Store    ContextBranchStore
	Scope    ContextBranchScope
	History  ContextBranchHistory
	Recorder inbound.HistoryCommands
	Protocol ContextBranchProtocol
	Access   ContextBranchAccess
	Pending  ContextBranchPending
}

type contextBranchOperations struct {
	store              ContextBranchStore
	scope              ContextBranchScope
	history            ContextBranchHistory
	recorder           inbound.HistoryCommands
	protocol           ContextBranchProtocol
	access             ContextBranchAccess
	pending            ContextBranchPending
	profile            ContextBranchProfile
	configurationError error
}

type ContextBranchOperations = contextBranchOperations

var _ inbound.ContextBranchOperations = (*ContextBranchOperations)(nil)

func NewContextBranchOperations(config ContextBranchOperationsConfig) (*ContextBranchOperations, error) {
	if config.Profile != ContextBranchCompatibility && config.Profile != ContextBranchComplete {
		return nil, domain.ErrValidation
	}
	if !rootAdapterPresent(config.Store) || !rootAdapterPresent(config.Scope) {
		return nil, domain.ErrValidation
	}
	for _, dependency := range []any{config.History, config.Recorder, config.Protocol, config.Access, config.Pending} {
		if dependency != nil && !rootAdapterPresent(dependency) {
			return nil, domain.ErrValidation
		}
	}
	if config.Profile == ContextBranchComplete {
		for _, dependency := range []any{config.History, config.Recorder, config.Protocol, config.Access, config.Pending} {
			if dependency == nil {
				return nil, domain.ErrCatalogUnsupported
			}
		}
	}
	return &contextBranchOperations{store: config.Store, scope: config.Scope, history: config.History,
		recorder: config.Recorder, protocol: config.Protocol, access: config.Access, pending: config.Pending, profile: config.Profile}, nil
}

func (commands *contextBranchOperations) ready() error {
	if commands == nil {
		return domain.ErrValidation
	}
	if commands.configurationError != nil {
		return commands.configurationError
	}
	if commands.scope == nil || commands.store == nil {
		return domain.ErrValidation
	}
	return nil
}

func (commands *contextBranchOperations) Fork(ctx context.Context, input inbound.ForkInput) (inbound.ForkOutput, error) {
	if err := commands.ready(); err != nil {
		return inbound.ForkOutput{}, err
	}
	output, err := commands.scope.WithinFork(ctx, input.RepoID, func(bound context.Context) (inbound.ForkOutput, error) {
		return commands.fork(bound, input)
	})
	if err != nil && commands.profile == ContextBranchComplete {
		return inbound.ForkOutput{}, err
	}
	return output, err
}

func (commands *contextBranchOperations) PreviewJoin(ctx context.Context, input inbound.JoinPreviewInput) (inbound.JoinPreviewOutput, error) {
	if err := commands.ready(); err != nil {
		return inbound.JoinPreviewOutput{}, err
	}
	if err := domain.ValidateContentHash(input.Snapshot); err != nil {
		return inbound.JoinPreviewOutput{}, err
	}
	output, err := commands.scope.WithinPreview(ctx, input.RepoID, func(bound context.Context) (inbound.JoinPreviewOutput, error) {
		return commands.previewJoin(bound, input)
	})
	if err != nil && commands.profile == ContextBranchComplete {
		return inbound.JoinPreviewOutput{}, err
	}
	return output, err
}

func (commands *contextBranchOperations) ConfirmJoin(ctx context.Context, input inbound.ConfirmJoinInput) (inbound.JoinOutput, error) {
	if err := commands.ready(); err != nil {
		return inbound.JoinOutput{}, err
	}
	ctx = inbound.WithRepositoryActor(ctx, input.ActorID)
	if input.PlanRevision == "" || input.ExpectedHead == "" {
		return inbound.JoinOutput{}, domain.ErrJoinPreviewChanged
	}
	output, err := commands.scope.WithinJoin(ctx, input.RepoID, func(bound context.Context) (inbound.JoinOutput, error) {
		if commands.access == nil {
			return inbound.JoinOutput{}, domain.ErrForbidden
		}
		if err := commands.access.AuthorizeJoin(bound, input.RepoID, input.ActorID, true); err != nil {
			return inbound.JoinOutput{}, err
		}
		return commands.join(bound, input.JoinInput)
	})
	if err != nil && commands.profile == ContextBranchComplete {
		return inbound.JoinOutput{}, err
	}
	return output, err
}

func (commands *contextBranchOperations) EnableContextProtocol(ctx context.Context, repo domain.ContentHash) error {
	if err := commands.ready(); err != nil {
		return err
	}
	return commands.scope.WithinProtocol(ctx, repo, func(bound context.Context) error {
		if err := domain.ValidateContentHash(repo); err != nil {
			return err
		}
		if _, err := commands.store.GetRepo(bound, repo); err != nil {
			return err
		}
		if commands.protocol == nil {
			return fmt.Errorf("context protocol storage unavailable")
		}
		return commands.protocol.EnableContextProtocol(bound, repo)
	})
}

func (commands *contextBranchOperations) ListHistory(ctx context.Context, repo domain.ContentHash) ([]domain.HistoryEvent, error) {
	if err := commands.ready(); err != nil {
		return nil, err
	}
	events, err := commands.scope.WithinHistory(ctx, repo, func(bound context.Context) ([]domain.HistoryEvent, error) {
		if commands.history == nil {
			return nil, fmt.Errorf("context history storage unavailable")
		}
		return commands.history.ListHistoryEvents(bound, repo)
	})
	if err != nil && commands.profile == ContextBranchComplete {
		return nil, err
	}
	return events, err
}

func (commands *contextBranchOperations) fork(ctx context.Context, in inbound.ForkInput) (inbound.ForkOutput, error) {
	if err := validateHashes(in.RepoID, in.FromSnapshot); err != nil {
		return inbound.ForkOutput{}, err
	}
	if err := domain.ValidateBranchName(in.NewBranch); err != nil {
		return inbound.ForkOutput{}, err
	}
	if _, err := commands.store.GetSnapshot(ctx, in.RepoID, in.FromSnapshot); err != nil {
		return inbound.ForkOutput{}, err // parent snapshot pre-exists (REF1)
	}
	repo, err := commands.store.GetRepo(ctx, in.RepoID)
	if err != nil && !errors.Is(err, domain.ErrNotFound) {
		return inbound.ForkOutput{}, err
	}
	if repo.ContextProtocol == 1 {
		events, err := commands.ListHistory(ctx, in.RepoID)
		if err != nil {
			return inbound.ForkOutput{}, err
		}
		bindings, err := domain.ProjectContextBranches(events)
		if err != nil {
			return inbound.ForkOutput{}, err
		}
		key := make([]byte, 16)
		if _, err := rand.Read(key); err != nil {
			return inbound.ForkOutput{}, err
		}
		id := hex.EncodeToString(key)
		event := domain.HistoryEvent{ID: id, RepoID: string(in.RepoID), BranchID: id, Branch: in.NewBranch, Kind: "birth", Source: in.FromSnapshot, Target: in.FromSnapshot, BindingParent: bindings.Released[in.NewBranch], CreatedAt: time.Now().UTC()}
		if commands.recorder == nil {
			return inbound.ForkOutput{}, fmt.Errorf("context history storage unavailable")
		}
		if err := commands.recorder.RecordHistory(ctx, event); err != nil {
			return inbound.ForkOutput{}, err
		}
		return inbound.ForkOutput{Branch: in.NewBranch, Head: in.FromSnapshot}, nil
	}
	ref := domain.Ref{Kind: domain.RefBranch, Name: in.NewBranch, RepoID: in.RepoID, Target: in.FromSnapshot}
	if err := commands.store.CompareAndSwapRef(ctx, in.RepoID, ref, ""); err != nil {
		return inbound.ForkOutput{}, err // already exists, ErrRefConflict (F2)
	}
	return inbound.ForkOutput{Branch: in.NewBranch, Head: in.FromSnapshot}, nil
}

func (commands *contextBranchOperations) join(ctx context.Context, in inbound.JoinInput) (inbound.JoinOutput, error) {
	graph, err := commands.loadJoinGraph(ctx, in.RepoID)
	if err != nil {
		return inbound.JoinOutput{}, err
	}
	plan, err := domain.PlanJoin(graph, domain.JoinRequest{RepoID: in.RepoID, Branch: in.TargetBranch,
		BranchID: in.BranchID, Source: in.Snapshot, IncludeDescendants: in.IncludeDescendants})
	if err != nil {
		if in.PlanRevision != "" && (errors.Is(err, domain.ErrConflict) || errors.Is(err, domain.ErrValidation)) {
			return inbound.JoinOutput{}, domain.ErrJoinPreviewChanged
		}
		return inbound.JoinOutput{}, err
	}
	if in.PlanRevision != "" && (plan.Revision() != in.PlanRevision || plan.ExpectedHead != in.ExpectedHead || plan.BranchID != in.BranchID) {
		return inbound.JoinOutput{}, domain.ErrJoinPreviewChanged
	}
	var forkName string
	if plan.RemainingTip != "" {
		forkName, err = commands.joinForkRefName(ctx, in.RepoID, in.TargetBranch, plan.RemainingTip)
		if err != nil {
			return inbound.JoinOutput{}, err
		}
	}
	mutation, err := plan.Mutation(forkName)
	if err != nil {
		return inbound.JoinOutput{}, err
	}
	if err := commands.store.ApplyJoin(ctx, mutation); err != nil {
		return inbound.JoinOutput{}, err
	}
	output := inbound.JoinOutput{Branch: plan.Branch, Head: plan.NewHead, ForkBranch: forkName}
	if commands.pending == nil {
		return output, domain.ErrCatalogUnsupported
	}
	return output, commands.pending.ReconcileSharedPending(ctx, in.RepoID)
}

func (commands *contextBranchOperations) loadJoinGraph(ctx context.Context, repo domain.ContentHash) (domain.JoinGraph, error) {
	if err := domain.ValidateContentHash(repo); err != nil {
		return domain.JoinGraph{}, err
	}
	snaps, err := commands.store.ListSnapshots(ctx, repo, "")
	if err != nil {
		return domain.JoinGraph{}, err
	}
	refs, err := commands.store.ListRefs(ctx, repo)
	if err != nil {
		return domain.JoinGraph{}, err
	}
	pendings, err := commands.store.ListPendings(ctx, repo)
	if err != nil {
		return domain.JoinGraph{}, err
	}
	logs, err := commands.store.ReadReflog(ctx, repo)
	if err != nil {
		return domain.JoinGraph{}, err
	}
	members, err := domain.ProjectSnapshotBranchMemberships(ctx, snaps, logs)
	if err != nil {
		return domain.JoinGraph{}, err
	}
	return domain.JoinGraph{Snapshots: snaps, Refs: refs, Pendings: pendings, Memberships: members}, nil
}

func (commands *contextBranchOperations) joinForkRefName(ctx context.Context, repoID domain.ContentHash, branch string, tip domain.ContentHash) (string, error) {
	short := strings.TrimPrefix(string(tip), "sha256:")
	if len(short) > 10 {
		short = short[:10]
	}
	base := domain.SessionRefPrefix(branch) + short
	for attempt := 0; attempt < 10; attempt++ {
		name := base
		if attempt > 0 {
			name = fmt.Sprintf("%s-%d", base, attempt+1)
		}
		if existing, err := commands.store.GetRef(ctx, repoID, domain.RefSession, name); err == nil {
			if existing.Target == tip {
				return name, nil
			}
			continue
		} else if !errors.Is(err, domain.ErrNotFound) {
			return "", fmt.Errorf("lookup join session ref %q: %w", name, err)
		}
		return name, nil
	}
	return "", fmt.Errorf("%w: fork branch name exhausted for %s", domain.ErrConflict, tip)
}

func (commands *contextBranchOperations) previewJoin(ctx context.Context, in inbound.JoinPreviewInput) (inbound.JoinPreviewOutput, error) {
	if commands.access == nil {
		return inbound.JoinPreviewOutput{}, domain.ErrForbidden
	}
	if err := commands.access.AuthorizeJoin(ctx, in.RepoID, in.ActorID, false); err != nil {
		return inbound.JoinPreviewOutput{}, err
	}
	graph, err := commands.loadJoinGraph(ctx, in.RepoID)
	if err != nil {
		return inbound.JoinPreviewOutput{}, err
	}
	found := false
	for _, snap := range graph.Snapshots {
		found = found || snap.ID == in.Snapshot
	}
	if !found {
		return inbound.JoinPreviewOutput{}, domain.ErrNotFound
	}
	out := inbound.JoinPreviewOutput{Snapshot: in.Snapshot, Branches: []inbound.JoinBranchOption{}, DropTargets: []domain.ContentHash{}}
	for _, ref := range graph.Refs {
		if ref.Kind == domain.RefBranch && ref.Name != domain.HeadRefName && ref.Target != "" && graph.Memberships[in.Snapshot][ref.Name] {
			out.Branches = append(out.Branches, inbound.JoinBranchOption{Branch: ref.Name, BranchID: ref.BranchID})
		}
	}
	sort.Slice(out.Branches, func(index, other int) bool { return out.Branches[index].Branch < out.Branches[other].Branch })
	branch := in.Branch
	if branch == "" && len(out.Branches) == 1 {
		branch = out.Branches[0].Branch
	}
	if branch == "" {
		out.Reason = "branch_required"
		return out, nil
	}
	plan, err := domain.PlanJoin(graph, domain.JoinRequest{RepoID: in.RepoID, Source: in.Snapshot, Branch: branch})
	if err != nil {
		var denied *domain.JoinPolicyError
		if errors.As(err, &denied) {
			out.Reason = denied.Code
			out.Branch = branch
			return out, nil
		}
		return out, err
	}
	out.Branch, out.BranchID, out.ExpectedHead = plan.Branch, plan.BranchID, plan.ExpectedHead
	out.Tip, out.Descendants = plan.Segment[len(plan.Segment)-1], len(plan.Segment)-1
	out.OnlyRevision = plan.Revision()
	whole := plan
	whole.NewHead, whole.RemainingTip = out.Tip, ""
	out.AllRevision = whole.Revision()
	byID := make(map[domain.ContentHash]domain.Snapshot, len(graph.Snapshots))
	for _, snap := range graph.Snapshots {
		byID[snap.ID] = snap
	}
	seen := map[domain.ContentHash]bool{}
	for id := plan.ExpectedHead; id != "" && !seen[id]; {
		seen[id] = true
		out.DropTargets = append(out.DropTargets, id)
		snap := byID[id]
		if len(snap.Parents) == 0 {
			break
		}
		id = snap.Parents[0]
	}
	return out, nil
}
