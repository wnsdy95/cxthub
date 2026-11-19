package app

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/inbound"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

func (s *Service) anchoredSessionStore() (outbound.AnchoredSessionStore, error) {
	store, ok := s.meta.(outbound.AnchoredSessionStore)
	if !ok {
		return nil, fmt.Errorf("%w: anchored session storage unavailable", domain.ErrConflict)
	}
	return store, nil
}

func anchoredSessionKey(repo domain.ContentHash, provider domain.ProviderKind, sessionID string) error {
	if err := validateHashes(repo); err != nil {
		return err
	}
	if !domain.ValidAnchoredSessionKey(provider, sessionID) {
		return fmt.Errorf("%w: anchored session key", domain.ErrValidation)
	}
	return nil
}

// anchoredSessionActor returns the account a report is stored for. A report
// always belongs to a signed-in account; the server never files one itself.
func anchoredSessionActor(ctx context.Context) (string, error) {
	actor, system := inbound.RepositoryActor(ctx)
	if system || actor == "" {
		return "", domain.ErrForbidden
	}
	return actor, nil
}

// PutAnchoredSession stores the report a member's CLI computed for one
// session, replacing that session's earlier report. A session belongs to the
// account that first reported it. The same content again changes nothing.
//
// A report does not move the repository revision: nothing a browser shows
// reads it yet, so there is nothing for an open page to refetch.
func (s *Service) PutAnchoredSession(ctx context.Context, repo domain.ContentHash, provider domain.ProviderKind, sessionID string, body domain.AnchoredSessionBody) (domain.AnchoredSessionReceipt, error) {
	if err := anchoredSessionKey(repo, provider, sessionID); err != nil {
		return domain.AnchoredSessionReceipt{}, err
	}
	if err := body.Validate(); err != nil {
		return domain.AnchoredSessionReceipt{}, err
	}
	body = body.Canonical()
	hash, err := body.Hash()
	if err != nil {
		return domain.AnchoredSessionReceipt{}, err
	}
	ctx = context.WithValue(auditOperation(ctx, "anchored_session.reported"), revisionScopeKey{}, "none")
	return repositoryWrite(ctx, s, repo, func(bound context.Context) (domain.AnchoredSessionReceipt, error) {
		store, err := s.anchoredSessionStore()
		if err != nil {
			return domain.AnchoredSessionReceipt{}, err
		}
		actor, err := anchoredSessionActor(bound)
		if err != nil {
			return domain.AnchoredSessionReceipt{}, err
		}
		// A report may only name content this repository holds. The document
		// is uploaded first, through the path that verifies and counts it.
		if content := body.Content; content != nil {
			have, err := s.blobs.HasDocs(bound, repo, []domain.ContentHash{content.Doc})
			if err != nil {
				return domain.AnchoredSessionReceipt{}, err
			}
			if len(have) != 1 || have[0] != content.Doc {
				return domain.AnchoredSessionReceipt{}, fmt.Errorf("%w: anchored session: the content document is not in this repository", domain.ErrValidation)
			}
		}
		now := time.Now().UTC()
		next := domain.AnchoredSession{RepoID: repo, Provider: provider, SessionID: sessionID, Uploader: actor,
			Revision: 1, BodyHash: hash, CreatedAt: now, UpdatedAt: now, Body: body}
		existing, err := store.GetAnchoredSession(bound, repo, provider, sessionID)
		switch {
		case errors.Is(err, domain.ErrNotFound):
		case err != nil:
			return domain.AnchoredSessionReceipt{}, err
		case existing.Uploader != actor:
			return domain.AnchoredSessionReceipt{}, fmt.Errorf("%w: this identifier is already reported by another account", domain.ErrConflict)
		case existing.BodyHash == hash:
			return domain.AnchoredSessionReceipt{Revision: existing.Revision, BodyHash: hash}, nil
		default:
			next.Revision, next.CreatedAt = existing.Revision+1, existing.CreatedAt
		}
		if err := store.PutAnchoredSession(bound, next); err != nil {
			return domain.AnchoredSessionReceipt{}, err
		}
		// What the report explains may already be on the remote.
		if err := openAnchoredSessions(bound, s.meta, repo, &anchoredKey{provider, sessionID}, now); err != nil {
			return domain.AnchoredSessionReceipt{}, err
		}
		return domain.AnchoredSessionReceipt{Revision: next.Revision, BodyHash: hash, Changed: true}, nil
	})
}

func (s *Service) anchoredPublicationStore() (outbound.AnchoredPublicationStore, error) {
	store, ok := s.meta.(outbound.AnchoredPublicationStore)
	if !ok {
		return nil, fmt.Errorf("%w: publication storage unavailable", domain.ErrConflict)
	}
	return store, nil
}

type anchoredKey struct {
	provider domain.ProviderKind
	id       string
}

// anchoredReader is who reads and what is open in the repository.
type anchoredReader struct {
	actor string
	store outbound.AnchoredSessionStore
	open  outbound.AnchoredPublicationStore
}

func (s *Service) anchoredReader(ctx context.Context) (anchoredReader, error) {
	store, err := s.anchoredSessionStore()
	if err != nil {
		return anchoredReader{}, err
	}
	open, err := s.anchoredPublicationStore()
	if err != nil {
		return anchoredReader{}, err
	}
	actor, err := anchoredSessionActor(ctx)
	if err != nil {
		return anchoredReader{}, err
	}
	return anchoredReader{actor: actor, store: store, open: open}, nil
}

// view returns the session as this reader may read it: all of it for its
// uploader, what is open for anyone else. It answers false when the reader
// may read nothing of it.
func (r anchoredReader) view(ctx context.Context, repo domain.ContentHash, session domain.AnchoredSession) (domain.AnchoredSession, bool, error) {
	if session.Uploader == r.actor {
		return session, true, nil
	}
	ranges, err := r.open.GetAnchoredPublication(ctx, repo, session.Provider, session.SessionID)
	if err != nil {
		return domain.AnchoredSession{}, false, err
	}
	view, ok := session.OpenView(ranges)
	return view, ok, nil
}

// GetAnchoredSession returns a report in full to the account that uploaded
// it, and what is open of it to another member. A session nothing is open of
// does not exist to them.
func (s *Service) GetAnchoredSession(ctx context.Context, repo domain.ContentHash, provider domain.ProviderKind, sessionID string) (domain.AnchoredSession, error) {
	if err := anchoredSessionKey(repo, provider, sessionID); err != nil {
		return domain.AnchoredSession{}, err
	}
	return repositoryRead(ctx, s, func(bound context.Context) (domain.AnchoredSession, error) {
		reader, err := s.anchoredReader(bound)
		if err != nil {
			return domain.AnchoredSession{}, err
		}
		session, err := reader.store.GetAnchoredSession(bound, repo, provider, sessionID)
		if err != nil {
			return domain.AnchoredSession{}, err
		}
		view, ok, err := reader.view(bound, repo, session)
		if err != nil {
			return domain.AnchoredSession{}, err
		}
		if !ok {
			return domain.AnchoredSession{}, domain.ErrNotFound
		}
		return view, nil
	})
}

// ListAnchoredSessions lists the caller's own reports, and of other members'
// sessions what is open to the team.
func (s *Service) ListAnchoredSessions(ctx context.Context, repo domain.ContentHash) ([]domain.AnchoredSessionSummary, error) {
	if err := validateHashes(repo); err != nil {
		return nil, err
	}
	return repositoryRead(ctx, s, func(bound context.Context) ([]domain.AnchoredSessionSummary, error) {
		reader, err := s.anchoredReader(bound)
		if err != nil {
			return nil, err
		}
		sessions, err := reader.store.ListRepositoryAnchoredSessions(bound, repo)
		if err != nil {
			return nil, err
		}
		publications, err := reader.open.ListAnchoredPublications(bound, repo)
		if err != nil {
			return nil, err
		}
		open := make(map[anchoredKey][]domain.AnchoredPublishedRange, len(publications))
		for _, publication := range publications {
			open[anchoredKey{publication.Provider, publication.SessionID}] = publication.Ranges
		}
		byKey := make(map[anchoredKey]domain.AnchoredSession, len(sessions))
		for _, session := range sessions {
			byKey[anchoredKey{session.Provider, session.SessionID}] = session
		}
		facts, err := readAnchoredRemoteFacts(bound, s.meta, repo, sessions)
		if err != nil {
			return nil, err
		}
		summaries := make([]domain.AnchoredSessionSummary, 0, len(sessions))
		for _, session := range sessions {
			own := session.Uploader == reader.actor
			summary := session.Summary()
			if !own {
				shared, ok := session.OpenSummary(open[anchoredKey{session.Provider, session.SessionID}])
				if !ok {
					continue
				}
				summary = shared
			}
			// Where each commit is now: on the remote itself, or as copies.
			// While the read of the remote is unfinished, a commit that was
			// not found is not said to be missing.
			if facts.read {
				for index := range summary.Commits {
					segment := session.Body.Segments[summary.Commits[index].SegmentNo-1]
					for _, commit := range segment.Commits {
						if commit.SHA != summary.Commits[index].SHA {
							continue
						}
						held, copies := facts.of(commit)
						if held || facts.complete {
							summary.Commits[index].OnRemote = &held
						}
						summary.Commits[index].Copies = copies
					}
				}
			}
			// A spawned session is placed in its parent as the parent is cut
			// now, when the caller may read the parent at that point.
			if spawn := session.Body.Spawn; spawn != nil {
				parentKey := anchoredKey{session.Provider, session.Body.Parent}
				if parent, ok := byKey[parentKey]; ok && (parent.Uploader == reader.actor || domain.OrdinalOpen(open[parentKey], spawn.AtOrdinal)) {
					if place, found := parent.Body.SpawnedIn(*spawn); found {
						summary.SpawnedIn = &place
					}
				}
			}
			summaries = append(summaries, summary)
		}
		return summaries, nil
	})
}

// AnchoredCommitSegments returns the segments that end at a commit: of the
// caller's own sessions, and of other members' sessions those that are open.
func (s *Service) AnchoredCommitSegments(ctx context.Context, repo domain.ContentHash, commitSHA string) ([]domain.AnchoredCommitSegment, error) {
	if err := validateHashes(repo); err != nil {
		return nil, err
	}
	if !domain.ValidAnchoredCommitSHA(commitSHA) {
		return nil, fmt.Errorf("%w: commit number", domain.ErrValidation)
	}
	return repositoryRead(ctx, s, func(bound context.Context) ([]domain.AnchoredCommitSegment, error) {
		reader, err := s.anchoredReader(bound)
		if err != nil {
			return nil, err
		}
		links, err := reader.store.ListAnchoredCommitLinks(bound, repo, commitSHA)
		if err != nil {
			return nil, err
		}
		segments := []domain.AnchoredCommitSegment{}
		for _, link := range links {
			session, err := reader.store.GetAnchoredSession(bound, repo, link.Provider, link.SessionID)
			if err != nil {
				return nil, err
			}
			if link.SegmentNo < 1 || link.SegmentNo > len(session.Body.Segments) {
				return nil, fmt.Errorf("%w: anchored commit index names a missing segment", domain.ErrIntegrity)
			}
			view, ok, err := reader.view(bound, repo, session)
			if err != nil {
				return nil, err
			}
			if !ok || view.Body.Segments[link.SegmentNo-1].Hidden {
				continue
			}
			segments = append(segments, domain.AnchoredCommitSegment{Provider: link.Provider, SessionID: link.SessionID,
				SegmentNo: link.SegmentNo, Rule: link.Rule, Segment: view.Body.Segments[link.SegmentNo-1]})
		}
		return segments, nil
	})
}

// AnchoredSessionContentAccess says what the caller may read of a session's
// conversation: all of it for the uploader, the open events for another
// member. The document's hash is for the server's own read; it is never
// answered to a caller.
func (s *Service) AnchoredSessionContentAccess(ctx context.Context, repo domain.ContentHash, provider domain.ProviderKind, sessionID string) (domain.AnchoredContentAccess, error) {
	if err := anchoredSessionKey(repo, provider, sessionID); err != nil {
		return domain.AnchoredContentAccess{}, err
	}
	return repositoryRead(ctx, s, func(bound context.Context) (domain.AnchoredContentAccess, error) {
		reader, err := s.anchoredReader(bound)
		if err != nil {
			return domain.AnchoredContentAccess{}, err
		}
		session, err := reader.store.GetAnchoredSession(bound, repo, provider, sessionID)
		if err != nil {
			return domain.AnchoredContentAccess{}, err
		}
		content := session.Body.Content
		access := domain.AnchoredContentAccess{}
		if content != nil {
			access.Doc, access.Events = content.Doc, content.Events
		}
		if session.Uploader == reader.actor {
			if content == nil {
				return domain.AnchoredContentAccess{}, domain.ErrNotFound
			}
			access.Open = [][2]int{{0, content.Events}}
			return access, nil
		}
		ranges, err := reader.open.GetAnchoredPublication(bound, repo, provider, sessionID)
		if err != nil {
			return domain.AnchoredContentAccess{}, err
		}
		if content == nil || len(ranges) == 0 {
			return domain.AnchoredContentAccess{}, domain.ErrNotFound
		}
		for _, span := range domain.PublishedEvents(ranges) {
			if span[1] > content.Events {
				span[1] = content.Events
			}
			if span[0] < span[1] {
				access.Open = append(access.Open, span)
			}
		}
		if len(access.Open) == 0 {
			return domain.AnchoredContentAccess{}, domain.ErrNotFound
		}
		return access, nil
	})
}

var _ inbound.AnchoredSessions = (*Service)(nil)
