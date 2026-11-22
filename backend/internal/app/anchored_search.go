package app

import (
	"context"
	"sort"
	"strconv"
	"strings"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/inbound"
)

// anchoredSearchSession is one conversation the query is looked for in: the
// document that holds it, and the session as the reader may read it.
type anchoredSearchSession struct {
	doc  domain.ContentHash
	view domain.AnchoredSession
}

// anchoredSearchFound is what the repository itself answers: everything but
// the conversations, and the conversations to read.
type anchoredSearchFound struct {
	result   domain.AnchoredSearchResult
	sessions []anchoredSearchSession
}

// SearchAnchored looks for one query in everything the sessions tab shows
// and the caller may read: pull request titles, branch names, commit
// subjects, the conversations of sessions and manual notes. A part of
// another account's session that is not open is never searched into an
// answer, because a hit is placed in the session as the reader is given it.
func (s *Service) SearchAnchored(ctx context.Context, repo domain.ContentHash, raw string) (domain.AnchoredSearchResult, error) {
	if err := validateHashes(repo); err != nil {
		return domain.AnchoredSearchResult{}, err
	}
	query, err := domain.NormalizeAnchoredSearchQuery(raw)
	if err != nil {
		return domain.AnchoredSearchResult{}, err
	}
	found, err := repositoryRead(ctx, s, func(bound context.Context) (anchoredSearchFound, error) {
		return s.searchAnchoredRepository(bound, repo, query)
	})
	if err != nil {
		return domain.AnchoredSearchResult{}, err
	}
	result := found.result
	notes, err := s.ListAnchoredNotes(ctx, repo)
	if err != nil {
		return domain.AnchoredSearchResult{}, err
	}
	count := 0
	for _, note := range notes {
		snippet, ok := domain.AnchoredSnippet(note.Body, query)
		if !ok {
			continue
		}
		if count == domain.AnchoredSearchKindLimit {
			result.Truncated = true
			break
		}
		count++
		target, written := note.Target, note.UpdatedAt
		result.Hits = append(result.Hits, domain.AnchoredSearchHit{Kind: domain.AnchoredHitNote, Title: note.AuthorName, Snippet: snippet,
			NoteID: note.ID, Target: &target, At: &written})
	}
	segments, truncated, err := s.searchAnchoredConversations(ctx, repo, query, found.sessions)
	if err != nil {
		return domain.AnchoredSearchResult{}, err
	}
	result.Hits = append(result.Hits, segments...)
	result.Truncated = result.Truncated || truncated
	return result, nil
}

// searchAnchoredRepository answers the hits the provider cache and the
// reports themselves hold, and lists the conversations the reader may read,
// newest first.
func (s *Service) searchAnchoredRepository(ctx context.Context, repo domain.ContentHash, query string) (anchoredSearchFound, error) {
	found := anchoredSearchFound{result: domain.AnchoredSearchResult{Hits: []domain.AnchoredSearchHit{}}}
	limit := domain.AnchoredSearchKindLimit
	reader, err := s.anchoredReader(ctx)
	if err != nil {
		return found, err
	}
	remote, _, read, err := remoteGitCache(ctx, s.meta, repo)
	if err != nil {
		return found, err
	}
	commits := map[string]struct{}{}
	if read {
		pulls, err := remote.ListRemotePulls(ctx, repo)
		if err != nil {
			return found, err
		}
		count := 0
		for _, pull := range pulls {
			number := strconv.Itoa(pull.Number)
			if !strings.Contains(strings.ToLower(pull.Title), query) && !strings.Contains(strings.ToLower(pull.HeadRef), query) && query != number && query != "#"+number {
				continue
			}
			if count == limit {
				found.result.Truncated = true
				break
			}
			count++
			updated := pull.UpdatedAt
			found.result.Hits = append(found.result.Hits, domain.AnchoredSearchHit{Kind: domain.AnchoredHitPull, Title: pull.Title, Pull: pull.Number, Branch: pull.HeadRef, At: &updated})
		}
		branches, err := remote.ListRemoteBranches(ctx, repo)
		if err != nil {
			return found, err
		}
		count = 0
		for _, branch := range branches {
			if !strings.Contains(strings.ToLower(branch.Name), query) {
				continue
			}
			if count == limit {
				found.result.Truncated = true
				break
			}
			count++
			found.result.Hits = append(found.result.Hits, domain.AnchoredSearchHit{Kind: domain.AnchoredHitBranch, Title: branch.Name, Branch: branch.Name, Commit: branch.SHA})
		}
		stored, err := remote.SearchRemoteCommits(ctx, repo, query, limit+1)
		if err != nil {
			return found, err
		}
		if len(stored) > limit {
			stored, found.result.Truncated = stored[:limit], true
		}
		for _, commit := range stored {
			at := commit.CommittedAt
			commits[commit.SHA] = struct{}{}
			found.result.Hits = append(found.result.Hits, domain.AnchoredSearchHit{Kind: domain.AnchoredHitCommit, Title: commit.Subject, Commit: commit.SHA, At: &at})
		}
	}
	sessions, err := reader.store.ListRepositoryAnchoredSessions(ctx, repo)
	if err != nil {
		return found, err
	}
	sort.SliceStable(sessions, func(left, right int) bool { return sessions[left].UpdatedAt.After(sessions[right].UpdatedAt) })
	local := 0
	for _, session := range sessions {
		view, ok, err := reader.view(ctx, repo, session)
		if err != nil {
			return found, err
		}
		if !ok {
			continue
		}
		// A commit only a report names: one the remote does not hold, or one
		// of a repository the server has not read.
		for _, segment := range view.Body.Segments {
			for _, commit := range segment.Commits {
				if _, listed := commits[commit.SHA]; listed || (!strings.Contains(strings.ToLower(commit.Subject), query) && !strings.HasPrefix(commit.SHA, query)) {
					continue
				}
				commits[commit.SHA] = struct{}{}
				if local == limit {
					found.result.Truncated = true
					continue
				}
				local++
				found.result.Hits = append(found.result.Hits, domain.AnchoredSearchHit{Kind: domain.AnchoredHitCommit, Title: commit.Subject, Commit: commit.SHA, At: commit.CreatedAt})
			}
		}
		if content := session.Body.Content; content != nil {
			found.sessions = append(found.sessions, anchoredSearchSession{doc: content.Doc, view: view})
		}
	}
	return found, nil
}

// searchAnchoredConversations reads the conversations for the query. The
// documents are read by the server for a reader whose access was already
// decided: a matching event is a hit only when it lies in a part of the view
// that reader is given.
func (s *Service) searchAnchoredConversations(ctx context.Context, repo domain.ContentHash, query string, sessions []anchoredSearchSession) ([]domain.AnchoredSearchHit, bool, error) {
	hits, truncated := []domain.AnchoredSearchHit{}, false
	if len(sessions) == 0 {
		return hits, false, nil
	}
	reads := inbound.WithSystemActor(ctx)
	// The store may say at once which documents hold the query at all.
	matching, err := s.MatchingDocHashes(reads, repo, query)
	if err != nil {
		return nil, false, err
	}
	read := 0
	for _, session := range sessions {
		if matching != nil && !matching[session.doc] {
			continue
		}
		if len(hits) >= domain.AnchoredSearchKindLimit || read == domain.AnchoredSearchSessions {
			truncated = true
			break
		}
		read++
		// More events are asked for than are kept: a match in a part that is
		// not open to the reader is passed over.
		events, err := s.SearchDocEvents(reads, repo, session.doc, query, -1, 200)
		if err != nil {
			// A conversation that cannot be read yet is left out; the next
			// search may find it.
			truncated = true
			continue
		}
		kept := 0
		for _, event := range events {
			segmentNo, rest, ok := session.view.Body.EventPart(event.Index)
			if !ok {
				continue
			}
			if kept == domain.AnchoredSearchSessionHits || len(hits) == domain.AnchoredSearchKindLimit {
				truncated = true
				break
			}
			snippet, _ := domain.AnchoredSnippet(event.Text, query)
			index := event.Index
			hits = append(hits, domain.AnchoredSearchHit{Kind: domain.AnchoredHitSegment, Title: event.Role, Snippet: snippet,
				Provider: session.view.Provider, SessionID: session.view.SessionID, SegmentNo: segmentNo, Rest: rest, Event: &index, At: session.view.Body.EndedAt})
			kept++
		}
	}
	return hits, truncated, nil
}

var _ inbound.AnchoredSearch = (*Service)(nil)
