package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/wnsdy95/cxthub/cli/internal/domain"
	"github.com/wnsdy95/cxthub/cli/internal/ports/inbound"
	"github.com/wnsdy95/cxthub/cli/internal/ports/outbound"
)

// HistoryQueryService reads either local ancestry or the server's pinned query.
// It cannot register a repository, repair a journal, or move a working position.
type HistoryQueryService struct {
	git    outbound.GitContext
	code   outbound.CodePosition
	local  outbound.SnapshotListReader
	remote outbound.ContextQueryReader
}

func NewHistoryQueryService(git outbound.GitContext, code outbound.CodePosition, local outbound.SnapshotListReader, remote outbound.ContextQueryReader) *HistoryQueryService {
	return &HistoryQueryService{git: git, code: code, local: local, remote: remote}
}
func (s *HistoryQueryService) QueryHistory(ctx context.Context, in inbound.HistoryQueryInput) (domain.HistoryQueryResult, error) {
	return s.queryHistory(ctx, in, nil)
}

func (s *HistoryQueryService) ObserveLocalHistory(ctx context.Context, cwd string) (domain.HistoryQueryResult, domain.ContentHash, error) {
	var catalogRevision domain.ContentHash
	out, err := s.queryHistory(ctx, inbound.HistoryQueryInput{Cwd: cwd}, &catalogRevision)
	return out, catalogRevision, err
}

func (s *HistoryQueryService) queryHistory(ctx context.Context, in inbound.HistoryQueryInput, catalogRevision *domain.ContentHash) (domain.HistoryQueryResult, error) {
	out := domain.HistoryQueryResult{Version: domain.QueryContractVersion, Snapshots: []domain.Snapshot{}, Missing: []domain.ContentHash{}, Selection: domain.HistorySelection{Ref: "HEAD", Scope: "current", Source: "local_ancestry"}}
	if (in.All && in.Retained) || ((in.All || in.Retained) && (in.Ref != "" || in.Branch != "" || in.Position != "")) || (in.Ref != "" && in.Branch != "") || in.Server && (in.All || in.Retained) {
		return out, fmt.Errorf("invalid_arguments: incompatible history scopes")
	}
	if err := domain.ValidateOptionalContentHash(in.Position); err != nil {
		return out, err
	}
	repo, err := s.git.CurrentRepo(ctx, in.Cwd)
	if err != nil {
		return out, err
	}
	name := in.Ref
	if in.Branch != "" {
		name = in.Branch
	}
	if name == "" {
		name = "HEAD"
	}
	out.Selection.Ref = name
	// Resolve aliases read-only. Capture labels never select history membership.
	if name != "HEAD" && domain.ValidateContentHash(domain.ContentHash(name)) != nil {
		if aliases, ok := s.local.(outbound.LocalBranchReader); ok {
			b, e := aliases.ResolveLocalBranch(ctx, string(repo.ID), name)
			if e == nil {
				if b.Inactive {
					return out, domain.ErrBranchArchived
				}
				name = b.Branch
			} else if !errors.Is(e, domain.ErrNotFound) {
				return out, e
			}
		}
	}
	if in.Server {
		if s.remote == nil || s.code == nil {
			return out, fmt.Errorf("server context query unavailable")
		}
		// Server reads do not scan the entire local archive. Only the selected ref
		// and actual Git position are inputs to the authoritative transaction.
		refs, e := s.local.ListRefs(ctx, string(repo.ID))
		if e != nil {
			return out, e
		}
		target, branch, e := historyRef(refs, name)
		if in.Position != "" {
			target = in.Position
			e = nil
			// A pinned hash without an explicit ref/branch is detached. HEAD's
			// logical branch must not supply unrelated integration ancestry.
			if in.Ref == "" && in.Branch == "" {
				branch = ""
			}
			if in.Branch != "" {
				branch = name
			}
		}
		if e != nil && !errors.Is(e, domain.ErrNotFound) {
			return out, e
		}
		if e != nil && name != "HEAD" {
			return out, e
		}
		// A pinned HEAD deliberately has no symbolic ref. Recover the logical
		// branch from its read-only worktree cursor, never Snapshot.Branch.
		if name == "HEAD" && in.Position == "" {
			if reader, ok := s.local.(outbound.WorkingPositionQueryReader); ok {
				p, readErr := reader.ReadWorkingPosition(ctx, string(repo.ID))
				if readErr == nil {
					if p.Snapshot != "" && p.Snapshot != target {
						return out, domain.ErrSelectionChanged
					}
					branch = p.Branch
				} else if !errors.Is(readErr, domain.ErrNotFound) {
					return out, readErr
				}
			}
		}
		code := ""
		// A named reference has its own server-verified code selection. Only HEAD
		// and internally pinned input preparation use this worktree's actual SHA.
		if name == "HEAD" || in.Position != "" {
			code, e = s.code.CurrentCommit(ctx, in.Cwd)
			if e != nil {
				return out, e
			}
			if !domain.ValidGitOID(code) {
				return out, fmt.Errorf("code_position_mismatch: no committed Git position")
			}
		}
		if branch == "" && target == "" {
			branch, e = s.git.CurrentBranch(ctx, in.Cwd)
			if e != nil {
				return out, e
			}
		}
		position := string(target)
		if position == "" {
			position = branch
		}
		if position == "" {
			return out, domain.ErrNotFound
		}
		view, e := s.remote.QueryContext(ctx, string(repo.ID), domain.ContextSelection{Branch: branch, Position: position, CodeCommit: code, Scope: "current"})
		if e != nil {
			return out, e
		}
		if e = domain.ValidateContextQuery(string(repo.ID), domain.ContextSelection{Position: string(target), CodeCommit: code}, view); e != nil {
			return out, e
		}
		if view.Branch != branch {
			return out, domain.ErrSelectionChanged
		}
		out.Selection.Source = "server"
		out.Selection.CodeCommit = code
		if view.Inclusion != nil {
			out.Selection.CodeCommit = view.Inclusion.CodeCommit
		}
		out.Selection.Branch = view.Branch
		out.StateHash = view.StateHash
		out.Revision = &view.Revision
		out.Position = view.Position
		out.Snapshots = view.Snapshots
		out.Inclusion = view.Inclusion
		out.ServerChecked = true
		out.Complete = true
		if view.Inclusion == nil {
			out.Complete = false
		} else {
			for _, m := range view.Inclusion.Merges {
				if m.State == "review" {
					out.Complete = false
				}
			}
		}
		return out, nil
	}
	var snaps []domain.Snapshot
	var refs []domain.Ref
	if catalogRevision == nil {
		snaps, refs, err = s.stableCatalog(ctx, string(repo.ID))
	} else {
		snaps, refs, *catalogRevision, err = s.readCatalog(ctx, string(repo.ID))
	}
	if err != nil {
		return out, err
	}
	var roots []domain.ContentHash
	switch {
	case in.Retained:
		out.Selection.Scope = "retained"
		out.Selection.Ref = ""
		for _, snap := range snaps {
			roots = append(roots, snap.ID)
		}
	case in.All:
		out.Selection.Scope = "all_refs"
		out.Selection.Ref = ""
		for _, r := range refs {
			if r.Target != "" {
				roots = append(roots, r.Target)
			}
		}
	default:
		target, branch, e := historyRef(refs, name)
		if in.Position != "" {
			target = in.Position
			e = nil
			if in.Ref == "" && in.Branch == "" {
				branch = ""
			}
			if in.Branch != "" {
				branch = name
			}
		}
		if e != nil {
			if name != "HEAD" || !errors.Is(e, domain.ErrNotFound) || len(snaps) > 0 {
				return out, e
			}
		} else {
			out.Position = target
			out.Selection.Branch = branch
			roots = []domain.ContentHash{target}
		}
	}
	out.Snapshots, out.Missing, err = domain.ReachableHistory(ctx, snaps, roots)
	if err != nil {
		return out, err
	}
	out.Complete = len(out.Missing) == 0
	raw, err := json.Marshal(out)
	if err != nil {
		return out, err
	}
	out.StateHash = domain.HashContent(raw)
	return out, nil
}
func historyRef(refs []domain.Ref, name string) (domain.ContentHash, string, error) {
	if strings.HasPrefix(name, "sha256:") {
		id := domain.ContentHash(name)
		if err := domain.ValidateContentHash(id); err != nil {
			return "", "", err
		}
		return id, "", nil
	}
	if name == "HEAD" {
		for _, r := range refs {
			if r.Kind == domain.RefHEAD {
				if r.Target != "" {
					return r.Target, r.Symbolic, nil
				}
				if r.Symbolic != "" {
					for _, b := range refs {
						if b.Kind == domain.RefBranch && b.Name == r.Symbolic {
							return b.Target, r.Symbolic, nil
						}
					}
				}
			}
		}
		return "", "", domain.ErrNotFound
	}
	var selected *domain.Ref
	for _, r := range refs {
		if (r.Kind == domain.RefBranch || r.Kind == domain.RefTag) && r.Name == name {
			if selected != nil {
				return "", "", fmt.Errorf("%w: ambiguous branch/tag %q", domain.ErrInvalidRef, name)
			}
			copy := r
			selected = &copy
		}
	}
	if selected == nil {
		return "", "", fmt.Errorf("%w: context ref %q", domain.ErrNotFound, name)
	}
	branch := ""
	if selected.Kind == domain.RefBranch {
		branch = selected.Name
	}
	return selected.Target, branch, nil
}

// One fresh, canonical observation; no cache, mutation or stability guarantee.
func (s *HistoryQueryService) readCatalog(ctx context.Context, repo string) ([]domain.Snapshot, []domain.Ref, domain.ContentHash, error) {
	if err := ctx.Err(); err != nil {
		return nil, nil, "", err
	}
	var snaps []domain.Snapshot
	var e error
	if catalog, ok := s.local.(outbound.SnapshotCatalogReader); ok {
		snaps, e = catalog.ListSnapshotCatalog(ctx, repo)
	} else {
		snaps, e = s.local.ListSnapshots(ctx, repo, "")
	}
	if e != nil {
		return nil, nil, "", e
	}
	if len(snaps) > 100000 {
		return nil, nil, "", fmt.Errorf("local history exceeds 100000 snapshots; use --server for a selected branch")
	}
	refs, e := s.local.ListRefs(ctx, repo)
	if e != nil {
		return nil, nil, "", e
	}
	snaps = append([]domain.Snapshot(nil), snaps...)
	refs = append([]domain.Ref(nil), refs...)
	sort.Slice(snaps, func(i, j int) bool { return snaps[i].ID < snaps[j].ID })
	sort.Slice(refs, func(i, j int) bool {
		if refs[i].Kind == refs[j].Kind {
			return refs[i].Name < refs[j].Name
		}
		return refs[i].Kind < refs[j].Kind
	})
	for _, v := range snaps {
		if v.RepoID != repo {
			return nil, nil, "", domain.ErrHashMismatch
		}
	}
	for _, v := range refs {
		if v.RepoID != repo {
			return nil, nil, "", domain.ErrHashMismatch
		}
	}
	raw, e := json.Marshal(struct {
		Snapshots []domain.Snapshot
		Refs      []domain.Ref
	}{snaps, refs})
	if e != nil {
		return nil, nil, "", e
	}
	if err := ctx.Err(); err != nil {
		return nil, nil, "", err
	}
	return snaps, refs, domain.HashContent(raw), nil
}

// Compare complete observations, including memory attachments and grafts. A
// continuously changing archive returns a retryable conflict, never a mixed list.
func (s *HistoryQueryService) stableCatalog(ctx context.Context, repo string) ([]domain.Snapshot, []domain.Ref, error) {
	_, _, previous, err := s.readCatalog(ctx, repo)
	if err != nil {
		return nil, nil, err
	}
	for attempt := 0; attempt < 3; attempt++ {
		if err := ctx.Err(); err != nil {
			return nil, nil, err
		}
		snaps, refs, current, e := s.readCatalog(ctx, repo)
		if e != nil {
			return nil, nil, e
		}
		if previous == current {
			return snaps, refs, nil
		}
		previous = current
	}
	return nil, nil, domain.ErrSelectionChanged
}

var _ inbound.LocalHistoryObserver = (*HistoryQueryService)(nil)

var _ inbound.HistoryQuery = (*HistoryQueryService)(nil)
