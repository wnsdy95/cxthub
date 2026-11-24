package app

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/inbound"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

// anchoredSummaryCommits is the most commits one read of summaries names.
const anchoredSummaryCommits = 200

// anchoredMemoryCommits is how many of the newest commits the server holds a
// walk along a branch looks at.
const anchoredMemoryCommits = 1000

func (s *Service) anchoredSummaryStore() (outbound.AnchoredSummaryStore, error) {
	store, ok := s.meta.(outbound.AnchoredSummaryStore)
	if !ok {
		return nil, fmt.Errorf("%w: anchored summary storage unavailable", domain.ErrConflict)
	}
	return store, nil
}

// PutAnchoredSummary stores the summary a member's CLI made of one of that
// member's sessions at a commit. The session's report must hold a segment
// that ends at the commit: a summary is what was done up to a commit, and a
// session that made none has no summary. The same text again changes nothing.
func (s *Service) PutAnchoredSummary(ctx context.Context, repo domain.ContentHash, provider domain.ProviderKind, sessionID, commit string, body domain.AnchoredSummaryBody) (domain.AnchoredSummaryReceipt, error) {
	if err := anchoredSessionKey(repo, provider, sessionID); err != nil {
		return domain.AnchoredSummaryReceipt{}, err
	}
	if !domain.ValidAnchoredCommitSHA(commit) {
		return domain.AnchoredSummaryReceipt{}, fmt.Errorf("%w: a summary is stored at a full commit number", domain.ErrValidation)
	}
	body = body.Canonical()
	if err := body.Validate(); err != nil {
		return domain.AnchoredSummaryReceipt{}, err
	}
	hash, err := body.Hash()
	if err != nil {
		return domain.AnchoredSummaryReceipt{}, err
	}
	ctx = auditOperation(ctx, "anchored_summary.stored")
	return repositoryWrite(ctx, s, repo, func(bound context.Context) (domain.AnchoredSummaryReceipt, error) {
		sessions, err := s.anchoredSessionStore()
		if err != nil {
			return domain.AnchoredSummaryReceipt{}, err
		}
		summaries, err := s.anchoredSummaryStore()
		if err != nil {
			return domain.AnchoredSummaryReceipt{}, err
		}
		actor, err := anchoredSessionActor(bound)
		if err != nil {
			return domain.AnchoredSummaryReceipt{}, err
		}
		session, err := sessions.GetAnchoredSession(bound, repo, provider, sessionID)
		if err != nil {
			return domain.AnchoredSummaryReceipt{}, err
		}
		if session.Uploader != actor {
			// Another account's session is not there to be written to.
			return domain.AnchoredSummaryReceipt{}, domain.ErrNotFound
		}
		if session.Body.SegmentOfCommit(commit) == 0 {
			return domain.AnchoredSummaryReceipt{}, fmt.Errorf("%w: no segment of the session ends at that commit", domain.ErrValidation)
		}
		now := time.Now().UTC()
		next := domain.AnchoredSummary{RepoID: repo, Provider: provider, SessionID: sessionID, Commit: commit, BodyHash: hash, Body: body, CreatedAt: now, UpdatedAt: now}
		existing, err := summaries.GetAnchoredSummary(bound, repo, provider, sessionID, commit)
		switch {
		case errors.Is(err, domain.ErrNotFound):
		case err != nil:
			return domain.AnchoredSummaryReceipt{}, err
		case existing.BodyHash == hash:
			return domain.AnchoredSummaryReceipt{BodyHash: hash}, nil
		default:
			next.CreatedAt = existing.CreatedAt
		}
		if err := summaries.PutAnchoredSummary(bound, next); err != nil {
			return domain.AnchoredSummaryReceipt{}, err
		}
		return domain.AnchoredSummaryReceipt{BodyHash: hash, Changed: true}, nil
	})
}

// ListAnchoredSummaries lists the summaries at the commits that the caller
// may read, by commit, tool, then session. A summary is read under the rule
// of the segment it belongs to: by the session's uploader always, by anyone
// else when that segment is open.
func (s *Service) ListAnchoredSummaries(ctx context.Context, repo domain.ContentHash, commits []string) ([]domain.AnchoredSummary, error) {
	if err := validateHashes(repo); err != nil {
		return nil, err
	}
	if len(commits) == 0 || len(commits) > anchoredSummaryCommits {
		return nil, fmt.Errorf("%w: summaries are read for 1 to %d commits", domain.ErrValidation, anchoredSummaryCommits)
	}
	for _, commit := range commits {
		if !domain.ValidAnchoredCommitSHA(commit) {
			return nil, fmt.Errorf("%w: summaries are read by full commit numbers", domain.ErrValidation)
		}
	}
	listed, err := repositoryRead(ctx, s, func(bound context.Context) ([]domain.AnchoredSummary, error) {
		reader, err := s.anchoredReader(bound)
		if err != nil {
			return nil, err
		}
		summaries, err := s.anchoredSummaryStore()
		if err != nil {
			return nil, err
		}
		stored, err := summaries.ListCommitAnchoredSummaries(bound, repo, commits)
		if err != nil {
			return nil, err
		}
		return reader.summaries(bound, repo, stored)
	})
	if err != nil {
		return nil, err
	}
	s.nameAnchoredSummaries(ctx, listed)
	return listed, nil
}

// ReadAnchoredMemory lists the summaries along a branch that the caller may
// read, the summary of the newest commit first. The walk goes back from the
// branch's newest commit through the commits the server read from the
// hosting provider, so after the branch's own commits come those of the
// branch it was made from. A commit a pull request was merged as, also by a
// squash, stands for that pull request's commits; a commit that was rebased
// or cherry-picked is found at its copy. A branch the remote does not hold
// is read as the default branch.
//
// local names the newest commits of the branch as the caller's own folder
// has it, the newest first. They are looked at first, in that order: what a
// member committed and did not push, or pushed a moment ago, is newer than
// anything the server read of that branch.
func (s *Service) ReadAnchoredMemory(ctx context.Context, repo domain.ContentHash, branch string, local []string, limit int) (domain.AnchoredMemory, error) {
	if err := validateHashes(repo); err != nil {
		return domain.AnchoredMemory{}, err
	}
	valid := limit >= 1 && limit <= domain.AnchoredMemoryLimit && len(local) <= domain.AnchoredMemoryLocalCommits && (branch == "" || domain.ValidRemoteBranchName(branch))
	for _, sha := range local {
		valid = valid && domain.ValidRemoteCommitSHA(sha)
	}
	if !valid {
		return domain.AnchoredMemory{}, fmt.Errorf("%w: summaries along a branch are read for a branch name, 1 to %d at a time, with at most %d full commit numbers of the caller's folder",
			domain.ErrValidation, domain.AnchoredMemoryLimit, domain.AnchoredMemoryLocalCommits)
	}
	memory, err := repositoryRead(ctx, s, func(bound context.Context) (domain.AnchoredMemory, error) {
		memory := domain.AnchoredMemory{Summaries: []domain.AnchoredSummary{}}
		reader, err := s.anchoredReader(bound)
		if err != nil {
			return memory, err
		}
		summaries, err := s.anchoredSummaryStore()
		if err != nil {
			return memory, err
		}
		remote, state, read, err := remoteGitCache(bound, s.meta, repo)
		if err != nil {
			return memory, err
		}
		var ancestors []domain.RemoteCommit
		merged := map[string]domain.RemotePull{}
		if read {
			if ancestors, merged, err = walkAnchoredMemory(bound, repo, remote, state.DefaultBranch, branch, &memory); err != nil {
				return memory, err
			}
		}
		// Where summaries are looked for, newest first: each commit, and after
		// a commit a pull request was merged as, that pull request's commits.
		position, asked := map[string]int{}, []string{}
		one := func(sha string) {
			if _, dup := position[sha]; !dup {
				position[sha] = len(asked)
				asked = append(asked, sha)
			}
		}
		place := func(sha string) {
			one(sha)
			if pull, ok := merged[sha]; ok {
				for index := len(pull.Commits) - 1; index >= 0; index-- {
					one(pull.Commits[index])
				}
			}
		}
		for _, sha := range local {
			place(sha)
		}
		for _, commit := range ancestors {
			place(commit.SHA)
		}
		if read {
			// A commit a session made may be on the remote as a copy only: its
			// summary stands where the copy does.
			sessions, err := reader.store.ListRepositoryAnchoredSessions(bound, repo)
			if err != nil {
				return memory, err
			}
			facts, err := readAnchoredRemoteFacts(bound, s.meta, repo, sessions)
			if err != nil {
				return memory, err
			}
			for _, session := range sessions {
				for _, segment := range session.Body.Segments {
					for _, commit := range segment.Commits {
						if _, direct := position[commit.SHA]; direct {
							continue
						}
						held, copies := facts.of(commit)
						at := -1
						for _, sha := range copies {
							if found, ok := position[sha]; ok && (at < 0 || found < at) {
								at = found
							}
						}
						if !held && at >= 0 {
							position[commit.SHA] = at
							asked = append(asked, commit.SHA)
						}
					}
				}
			}
		}
		if len(asked) == 0 {
			return memory, nil
		}
		kept, err := summaries.ListCommitAnchoredSummaries(bound, repo, asked)
		if err != nil {
			return memory, err
		}
		readable, err := reader.summaries(bound, repo, kept)
		if err != nil {
			return memory, err
		}
		sort.SliceStable(readable, func(left, right int) bool { return position[readable[left].Commit] < position[readable[right].Commit] })
		if len(readable) > limit {
			readable = readable[:limit]
		}
		memory.Summaries = readable
		return memory, nil
	})
	if err != nil {
		return domain.AnchoredMemory{}, err
	}
	s.nameAnchoredSummaries(ctx, memory.Summaries)
	return memory, nil
}

// walkAnchoredMemory reads the commits of a branch on the remote, newest
// first: each commit the branch's newest commit descends from, among the
// newest commits the server holds. It also answers the merged pull requests
// by the commit each became, and says in memory which branch it walked.
func walkAnchoredMemory(ctx context.Context, repo domain.ContentHash, remote outbound.RemoteGitStore, defaultBranch, branch string, memory *domain.AnchoredMemory) ([]domain.RemoteCommit, map[string]domain.RemotePull, error) {
	merged := map[string]domain.RemotePull{}
	pulls, err := remote.ListRemotePulls(ctx, repo)
	if err != nil {
		return nil, nil, err
	}
	for _, pull := range pulls {
		if pull.MergeSHA != "" {
			merged[pull.MergeSHA] = pull
		}
	}
	branches, err := remote.ListRemoteBranches(ctx, repo)
	if err != nil {
		return nil, nil, err
	}
	heads := make(map[string]string, len(branches))
	for _, listed := range branches {
		heads[listed.Name] = listed.SHA
	}
	name := branch
	if _, held := heads[name]; !held {
		name = defaultBranch
	}
	head, held := heads[name]
	if !held {
		return nil, merged, nil
	}
	memory.Branch, memory.Head = name, head
	stored, err := remote.ListRemoteCommits(ctx, repo, domain.RemoteCommitCursor{}, anchoredMemoryCommits)
	if err != nil {
		return nil, nil, err
	}
	bySHA := make(map[string]domain.RemoteCommit, len(stored))
	for _, commit := range stored {
		bySHA[commit.SHA] = commit
	}
	// The head and what it descends from, among the commits held.
	var ancestors []domain.RemoteCommit
	seen, queue := map[string]struct{}{head: {}}, []string{head}
	for len(queue) > 0 {
		commit, ok := bySHA[queue[0]]
		queue = queue[1:]
		if !ok {
			continue
		}
		ancestors = append(ancestors, commit)
		for _, parent := range commit.Parents {
			if _, dup := seen[parent]; !dup {
				seen[parent] = struct{}{}
				queue = append(queue, parent)
			}
		}
	}
	sort.SliceStable(ancestors, func(left, right int) bool {
		if !ancestors[left].CommittedAt.Equal(ancestors[right].CommittedAt) {
			return ancestors[left].CommittedAt.After(ancestors[right].CommittedAt)
		}
		return ancestors[left].SHA > ancestors[right].SHA
	})
	memory.Walked = len(ancestors)
	return ancestors, merged, nil
}

// summaries keeps of the stored summaries those the reader may read and whose
// session is not archived, and
// says for each which segment it belongs to and whose session it is.
func (r anchoredReader) summaries(ctx context.Context, repo domain.ContentHash, stored []domain.AnchoredSummary) ([]domain.AnchoredSummary, error) {
	readable := []domain.AnchoredSummary{}
	views := map[anchoredKey]*domain.AnchoredSession{}
	for _, summary := range stored {
		key := anchoredKey{summary.Provider, summary.SessionID}
		view, seen := views[key]
		if !seen {
			session, err := r.store.GetAnchoredSession(ctx, repo, summary.Provider, summary.SessionID)
			if err != nil && !errors.Is(err, domain.ErrNotFound) {
				return nil, err
			}
			if err == nil {
				given, ok, err := r.listed(ctx, repo, session)
				if err != nil {
					return nil, err
				}
				if ok {
					given.Uploader = session.Uploader
					view = &given
				}
			}
			views[key] = view
		}
		if view == nil {
			continue
		}
		segment := view.Body.SegmentOfCommit(summary.Commit)
		if segment == 0 {
			continue
		}
		summary.SegmentNo, summary.Uploader = segment, view.Uploader
		readable = append(readable, summary)
	}
	return readable, nil
}

// nameAnchoredSummaries gives each summary its uploader's public handle.
func (s *Service) nameAnchoredSummaries(ctx context.Context, summaries []domain.AnchoredSummary) {
	accounts := map[string]struct{}{}
	for _, summary := range summaries {
		accounts[summary.Uploader] = struct{}{}
	}
	names := s.anchoredAccountNames(ctx, accounts)
	for index := range summaries {
		summaries[index].UploaderName = names[summaries[index].Uploader]
	}
}

var _ inbound.AnchoredSummaries = (*Service)(nil)
