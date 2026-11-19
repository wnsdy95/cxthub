package app

import (
	"context"
	"errors"
	"sort"
	"time"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

// anchoredRemoteFacts is what the server's own read of the Git hosting
// provider says about the commits and branches session reports name.
type anchoredRemoteFacts struct {
	// read is false until the provider was read for the repository's current
	// origin. Nothing is then known to be on the remote, nor missing from it.
	read bool
	// complete is false while the last read left commits unread. A commit
	// that is not stored may then still be on the remote.
	complete bool
	held     map[string]struct{}
	copies   map[domain.RemoteCommitKey][]string
	pushes   map[string][]time.Time
}

func anchoredCommitKey(commit domain.AnchoredCommit) (domain.RemoteCommitKey, bool) {
	if commit.AuthoredAt == nil || commit.AuthorEmail == "" || commit.Subject == "" {
		return domain.RemoteCommitKey{}, false
	}
	return domain.RemoteCommitKey{AuthoredAt: commit.AuthoredAt.UTC(), AuthorEmail: commit.AuthorEmail, Subject: commit.Subject}, true
}

// remoteGitCache returns what the server read from the hosting provider for
// the repository's current origin. It answers false when nothing was read
// yet, or what is stored was read from another origin.
func remoteGitCache(ctx context.Context, meta outbound.MetadataStore, repo domain.ContentHash) (outbound.RemoteGitStore, domain.RemoteGitState, bool, error) {
	remote, ok := meta.(outbound.RemoteGitStore)
	if !ok {
		return nil, domain.RemoteGitState{}, false, nil
	}
	current, err := meta.GetRepo(ctx, repo)
	if err != nil {
		return nil, domain.RemoteGitState{}, false, err
	}
	state, err := remote.GetRemoteGitState(ctx, repo)
	if errors.Is(err, domain.ErrNotFound) || (err == nil && (state.GitOrigin != current.GitRemoteURL || state.SyncedAt == nil)) {
		return nil, domain.RemoteGitState{}, false, nil
	}
	if err != nil {
		return nil, domain.RemoteGitState{}, false, err
	}
	return remote, state, true, nil
}

// readAnchoredRemoteFacts reads, for the commits and branches the sessions
// name, what the remote holds: the commit itself, a copy of it (same author
// time, author and subject), and when each branch was seen pushed. A store
// without the provider cache, or a cache of another origin, gives no facts.
func readAnchoredRemoteFacts(ctx context.Context, meta outbound.MetadataStore, repo domain.ContentHash, sessions []domain.AnchoredSession) (anchoredRemoteFacts, error) {
	facts := anchoredRemoteFacts{held: map[string]struct{}{}, copies: map[domain.RemoteCommitKey][]string{}, pushes: map[string][]time.Time{}}
	remote, state, ok, err := remoteGitCache(ctx, meta, repo)
	if err != nil || !ok {
		return facts, err
	}
	facts.read, facts.complete = true, !state.Remaining
	seen, branches := map[string]struct{}{}, map[string]struct{}{}
	var shas []string
	commits := map[string]domain.AnchoredCommit{}
	for _, session := range sessions {
		for _, segment := range session.Body.Segments {
			for _, commit := range segment.Commits {
				if _, dup := seen[commit.SHA]; !dup {
					seen[commit.SHA] = struct{}{}
					shas = append(shas, commit.SHA)
					commits[commit.SHA] = commit
				}
			}
		}
		if part := session.Body.Uncommitted; part != nil {
			for _, place := range part.Places {
				if place.Branch != "" {
					branches[place.Branch] = struct{}{}
				}
			}
		}
	}
	missing, err := remote.MissingRemoteCommits(ctx, repo, shas)
	if err != nil {
		return facts, err
	}
	absent := make(map[string]struct{}, len(missing))
	var keys []domain.RemoteCommitKey
	wanted := map[domain.RemoteCommitKey]struct{}{}
	for _, sha := range missing {
		absent[sha] = struct{}{}
		if key, ok := anchoredCommitKey(commits[sha]); ok {
			if _, dup := wanted[key]; !dup {
				wanted[key] = struct{}{}
				keys = append(keys, key)
			}
		}
	}
	for _, sha := range shas {
		if _, gone := absent[sha]; !gone {
			facts.held[sha] = struct{}{}
		}
	}
	if len(keys) > 0 {
		found, err := remote.FindRemoteCommitCopies(ctx, repo, keys)
		if err != nil {
			return facts, err
		}
		for _, commit := range found {
			facts.copies[commit.Key()] = append(facts.copies[commit.Key()], commit.SHA)
		}
	}
	if log, ok := meta.(outbound.RemotePushStore); ok && len(branches) > 0 {
		names := make([]string, 0, len(branches))
		for name := range branches {
			names = append(names, name)
		}
		sort.Strings(names)
		pushes, err := log.ListRemotePushes(ctx, repo, names)
		if err != nil {
			return facts, err
		}
		for _, push := range pushes {
			facts.pushes[push.Branch] = append(facts.pushes[push.Branch], push.SeenAt)
		}
	}
	return facts, nil
}

// of answers whether the remote holds a commit a session made, and otherwise
// which remote commits are copies of it.
func (f anchoredRemoteFacts) of(commit domain.AnchoredCommit) (bool, []string) {
	if _, held := f.held[commit.SHA]; held {
		return true, nil
	}
	key, ok := anchoredCommitKey(commit)
	if !ok {
		return false, nil
	}
	return false, f.copies[key]
}

// onRemote is the fact the publication rule asks for: the remote commit that
// stands for a commit a session made.
func (f anchoredRemoteFacts) onRemote(commit domain.AnchoredCommit) (string, bool) {
	held, copies := f.of(commit)
	switch {
	case held:
		return commit.SHA, true
	case len(copies) > 0:
		return copies[0], true
	}
	return "", false
}

// openAnchoredSessions applies the publication rule to the repository's
// sessions and stores what is newly open. With only set, that one session is
// looked at; otherwise all of them, parents before the sessions they spawned,
// so that a session opened in this run opens what it spawned in the same run.
//
// It only ever adds: what the facts no longer support stays open.
func openAnchoredSessions(ctx context.Context, meta outbound.MetadataStore, repo domain.ContentHash, only *anchoredKey, at time.Time) error {
	sessions, ok := meta.(outbound.AnchoredSessionStore)
	if !ok {
		return nil
	}
	publications, ok := meta.(outbound.AnchoredPublicationStore)
	if !ok {
		return nil
	}
	all, err := sessions.ListRepositoryAnchoredSessions(ctx, repo)
	if err != nil || len(all) == 0 {
		return err
	}
	looked := all
	if only != nil {
		looked = nil
		for _, session := range all {
			if session.Provider == only.provider && session.SessionID == only.id {
				looked = append(looked, session)
			}
		}
	}
	facts, err := readAnchoredRemoteFacts(ctx, meta, repo, looked)
	if err != nil {
		return err
	}
	stored, err := publications.ListAnchoredPublications(ctx, repo)
	if err != nil {
		return err
	}
	open := make(map[anchoredKey][]domain.AnchoredPublishedRange, len(stored))
	for _, publication := range stored {
		open[anchoredKey{publication.Provider, publication.SessionID}] = publication.Ranges
	}
	// A spawned session is looked at after its parent. A chain of parents is
	// at most as long as the list, so the rounds end.
	for round := 0; round <= len(looked); round++ {
		changedAny := false
		for _, session := range looked {
			key := anchoredKey{session.Provider, session.SessionID}
			publishable := session.Body.PublishableRanges(domain.AnchoredPublicationFacts{
				OnRemote: facts.onRemote,
				PushedAt: func(branch string) []time.Time { return facts.pushes[branch] },
				ParentOpen: func(parent string, ordinal int) bool {
					return domain.OrdinalOpen(open[anchoredKey{session.Provider, parent}], ordinal)
				},
			})
			merged, changed := domain.AddPublishedRanges(open[key], publishable, at)
			if !changed {
				continue
			}
			if err := publications.PutAnchoredPublication(ctx, repo, domain.AnchoredPublication{Provider: session.Provider, SessionID: session.SessionID, Ranges: merged}); err != nil {
				return err
			}
			open[key], changedAny = merged, true
		}
		if !changedAny {
			break
		}
	}
	return nil
}
