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

// anchoredReadActor returns the account that reads, or the empty string for
// a reader who is not signed in: whoever may view the repository reads what
// is open of it. Nothing is stored for the empty account, so such a reader
// is given only what is open.
func anchoredReadActor(ctx context.Context) (string, error) {
	actor, system := inbound.RepositoryActor(ctx)
	if system {
		return "", domain.ErrForbidden
	}
	return actor, nil
}

// anchoredOwn reports whether what an account stored is the reader's own.
func anchoredOwn(reader, account string) bool { return reader != "" && reader == account }

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
		// A session that was removed from the server is not taken again
		// until it is allowed again, whoever sends it.
		if removals, ok := s.meta.(outbound.AnchoredRemovalStore); ok {
			if _, err := removals.GetAnchoredSessionRemoval(bound, repo, provider, sessionID); err == nil {
				return domain.AnchoredSessionReceipt{}, fmt.Errorf("%w: this session was removed from the server; it is uploaded again only after it is allowed again", domain.ErrConflict)
			} else if !errors.Is(err, domain.ErrNotFound) {
				return domain.AnchoredSessionReceipt{}, err
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
		// The document the earlier report named is no longer the session's
		// conversation: a session that grew, or was masked another way, has a
		// new one. It is deleted when nothing else uses it. A deletion that
		// fails does not fail the report; the document then stays until the
		// session is removed.
		if earlier := existing.Body.Content; earlier != nil && (body.Content == nil || body.Content.Doc != earlier.Doc) {
			_ = s.dropAnchoredContent(bound, store, repo, earlier.Doc)
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
	actor, err := anchoredReadActor(ctx)
	if err != nil {
		return anchoredReader{}, err
	}
	return anchoredReader{actor: actor, store: store, open: open}, nil
}

// listed returns the session as view does, and answers false for an archived
// session too: it is what every list, search and read along a branch uses.
// An archived session is opened by its key alone.
func (r anchoredReader) listed(ctx context.Context, repo domain.ContentHash, session domain.AnchoredSession) (domain.AnchoredSession, bool, error) {
	if session.Archived() {
		return domain.AnchoredSession{}, false, nil
	}
	return r.view(ctx, repo, session)
}

// view returns the session as this reader may read it: all of it for its
// uploader, what is open for anyone else. It answers false when the reader
// may read nothing of it.
func (r anchoredReader) view(ctx context.Context, repo domain.ContentHash, session domain.AnchoredSession) (domain.AnchoredSession, bool, error) {
	if anchoredOwn(r.actor, session.Uploader) {
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
	summaries, err := repositoryRead(ctx, s, func(bound context.Context) ([]domain.AnchoredSessionSummary, error) {
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
			own := anchoredOwn(reader.actor, session.Uploader)
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
				if parent, ok := byKey[parentKey]; ok && (anchoredOwn(reader.actor, parent.Uploader) || domain.OrdinalOpen(open[parentKey], spawn.AtOrdinal)) {
					if place, found := parent.Body.SpawnedIn(*spawn); found {
						summary.SpawnedIn = &place
					}
				}
			}
			summaries = append(summaries, summary)
		}
		return summaries, nil
	})
	if err != nil {
		return nil, err
	}
	accounts := map[string]struct{}{}
	for _, summary := range summaries {
		if summary.Shared {
			accounts[summary.Uploader] = struct{}{}
		}
	}
	names := s.anchoredAccountNames(ctx, accounts)
	for index := range summaries {
		if summaries[index].Shared {
			summaries[index].UploaderName = names[summaries[index].Uploader]
		}
	}
	return summaries, nil
}

// anchoredAccountNames answers the public handle of each account that has
// one: who a session or a note is by, for a reader who may not read the
// member list. An account whose handle cannot be read is left out.
func (s *Service) anchoredAccountNames(ctx context.Context, accounts map[string]struct{}) map[string]string {
	names := map[string]string{}
	if s.repositories == nil {
		return names
	}
	for account := range accounts {
		if user, err := s.repositories.GetUser(ctx, account); err == nil && user.Username != "" {
			names[account] = user.Username
		}
	}
	return names
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
			view, ok, err := reader.listed(bound, repo, session)
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

// errAnchoredArchiveNotOwn says that the caller is not the session's uploader.
var errAnchoredArchiveNotOwn = errors.New("anchored session: not the caller's own")

// SetAnchoredSessionArchived archives a session or restores it. The session's
// uploader may, and so may whoever manages the repository. An archived
// session is left out of every list, search and read along a branch and is
// opened only from the list of archived sessions; nothing of it is removed.
// Asking for the state a session already has changes nothing.
func (s *Service) SetAnchoredSessionArchived(ctx context.Context, repo domain.ContentHash, provider domain.ProviderKind, sessionID string, archived bool) (domain.AnchoredSessionArchive, error) {
	if err := anchoredSessionKey(repo, provider, sessionID); err != nil {
		return domain.AnchoredSessionArchive{}, err
	}
	action := "anchored_session.restored"
	if archived {
		action = "anchored_session.archived"
	}
	set := func(bound context.Context, own bool) (domain.AnchoredSessionArchive, error) {
		store, err := s.anchoredSessionStore()
		if err != nil {
			return domain.AnchoredSessionArchive{}, err
		}
		actor, err := anchoredSessionActor(bound)
		if err != nil {
			return domain.AnchoredSessionArchive{}, err
		}
		session, err := store.GetAnchoredSession(bound, repo, provider, sessionID)
		if own && (errors.Is(err, domain.ErrNotFound) || (err == nil && session.Uploader != actor)) {
			// Whether the session exists is not said to a caller who is not
			// its uploader before that caller is known to manage the repository.
			return domain.AnchoredSessionArchive{}, errAnchoredArchiveNotOwn
		}
		if err != nil {
			return domain.AnchoredSessionArchive{}, err
		}
		if archived == session.Archived() {
			return domain.AnchoredSessionArchive{Archived: archived, ArchivedAt: session.ArchivedAt}, nil
		}
		var at *time.Time
		by := ""
		if archived {
			now := time.Now().UTC()
			at, by = &now, actor
		}
		if err := store.SetAnchoredSessionArchive(bound, repo, provider, sessionID, at, by); err != nil {
			return domain.AnchoredSessionArchive{}, err
		}
		return domain.AnchoredSessionArchive{Archived: archived, ArchivedAt: at}, nil
	}
	ctx = context.WithValue(auditOperation(ctx, action), revisionScopeKey{}, "none")
	receipt, err := repositoryWrite(ctx, s, repo, func(bound context.Context) (domain.AnchoredSessionArchive, error) { return set(bound, true) })
	if !errors.Is(err, errAnchoredArchiveNotOwn) {
		return receipt, err
	}
	return repositoryWrite(writeAction(ctx, "manage"), s, repo, func(bound context.Context) (domain.AnchoredSessionArchive, error) { return set(bound, false) })
}

// dropAnchoredContent deletes a conversation document that no stored report
// of the repository names and no snapshot holds. A document still in use is
// left as it is.
func (s *Service) dropAnchoredContent(ctx context.Context, store outbound.AnchoredSessionStore, repo, doc domain.ContentHash) error {
	if doc == "" {
		return nil
	}
	used, err := store.AnchoredContentInUse(ctx, repo, doc)
	if err != nil || used {
		return err
	}
	return s.blobs.DeleteDoc(ctx, repo, doc)
}

func (s *Service) anchoredRemovalStore() (outbound.AnchoredRemovalStore, error) {
	store, ok := s.meta.(outbound.AnchoredRemovalStore)
	if !ok {
		return nil, fmt.Errorf("%w: removal storage unavailable", domain.ErrConflict)
	}
	return store, nil
}

// RemoveAnchoredSession removes a session from the server: its report with
// its commit links, its summaries and its open ranges, and its conversation
// document when nothing else in the repository uses it. The sessions it
// spawned, as far as they belong to the same account, are removed with it.
// The session's uploader may, and so may whoever manages the repository.
//
// A removal is remembered for every removed session: no report is taken for
// it again until it is allowed again. Removing what is already removed
// changes nothing and answers the removal that stands.
func (s *Service) RemoveAnchoredSession(ctx context.Context, repo domain.ContentHash, provider domain.ProviderKind, sessionID string) (domain.AnchoredSessionRemovalReceipt, error) {
	none := domain.AnchoredSessionRemovalReceipt{}
	if err := anchoredSessionKey(repo, provider, sessionID); err != nil {
		return none, err
	}
	remove := func(bound context.Context, own bool) (domain.AnchoredSessionRemovalReceipt, error) {
		store, err := s.anchoredSessionStore()
		if err != nil {
			return none, err
		}
		removals, err := s.anchoredRemovalStore()
		if err != nil {
			return none, err
		}
		actor, err := anchoredSessionActor(bound)
		if err != nil {
			return none, err
		}
		session, err := store.GetAnchoredSession(bound, repo, provider, sessionID)
		if errors.Is(err, domain.ErrNotFound) {
			standing, standingErr := removals.GetAnchoredSessionRemoval(bound, repo, provider, sessionID)
			switch {
			case standingErr == nil && (!own || standing.Uploader == actor):
				return domain.AnchoredSessionRemovalReceipt{Removed: []domain.AnchoredSessionRemovalView{standing.View(actor)}}, nil
			case standingErr != nil && !errors.Is(standingErr, domain.ErrNotFound):
				return none, standingErr
			case own:
				// Whether the session exists is not said to a caller who is
				// not its uploader before that caller is known to manage
				// the repository.
				return none, errAnchoredArchiveNotOwn
			}
			return none, err
		}
		if err != nil {
			return none, err
		}
		if own && session.Uploader != actor {
			return none, errAnchoredArchiveNotOwn
		}
		// The session, then the sessions it spawned, each after its parent.
		owned, err := store.ListAnchoredSessions(bound, repo, session.Uploader)
		if err != nil {
			return none, err
		}
		doomed := []domain.AnchoredSession{session}
		for index := 0; index < len(doomed); index++ {
			for _, candidate := range owned {
				if candidate.Provider == provider && candidate.Body.Sub && candidate.Body.Parent == doomed[index].SessionID && candidate.SessionID != sessionID {
					seen := false
					for _, already := range doomed {
						seen = seen || already.SessionID == candidate.SessionID
					}
					if !seen {
						doomed = append(doomed, candidate)
					}
				}
			}
		}
		now := time.Now().UTC()
		receipt := domain.AnchoredSessionRemovalReceipt{Removed: make([]domain.AnchoredSessionRemovalView, 0, len(doomed))}
		for _, each := range doomed {
			if err := store.DeleteAnchoredSession(bound, repo, each.Provider, each.SessionID); err != nil {
				return none, err
			}
			removal := domain.AnchoredSessionRemoval{RepoID: repo, Provider: each.Provider, SessionID: each.SessionID, Uploader: each.Uploader, RemovedBy: actor, RemovedAt: now}
			if err := removals.PutAnchoredSessionRemoval(bound, removal); err != nil {
				return none, err
			}
			receipt.Removed = append(receipt.Removed, removal.View(actor))
		}
		// The documents go after every report that named them is gone. One
		// that cannot be deleted fails the removal as a whole, so that what
		// the caller asked to be removed is never reported as removed while
		// its conversation is still stored.
		for _, each := range doomed {
			if content := each.Body.Content; content != nil {
				if err := s.dropAnchoredContent(bound, store, repo, content.Doc); err != nil {
					return none, err
				}
			}
		}
		return receipt, nil
	}
	ctx = context.WithValue(auditOperation(ctx, "anchored_session.removed"), revisionScopeKey{}, "none")
	receipt, err := repositoryWrite(ctx, s, repo, func(bound context.Context) (domain.AnchoredSessionRemovalReceipt, error) { return remove(bound, true) })
	if !errors.Is(err, errAnchoredArchiveNotOwn) {
		return receipt, err
	}
	return repositoryWrite(writeAction(ctx, "manage"), s, repo, func(bound context.Context) (domain.AnchoredSessionRemovalReceipt, error) { return remove(bound, false) })
}

// AllowAnchoredSession takes back the removal of a session, so that it is
// uploaded again by the next run of a CLI that still holds the tool's
// records. Nothing comes back by itself. The account the session belonged to
// may, and so may whoever manages the repository.
func (s *Service) AllowAnchoredSession(ctx context.Context, repo domain.ContentHash, provider domain.ProviderKind, sessionID string) error {
	if err := anchoredSessionKey(repo, provider, sessionID); err != nil {
		return err
	}
	allow := func(bound context.Context, own bool) (struct{}, error) {
		removals, err := s.anchoredRemovalStore()
		if err != nil {
			return struct{}{}, err
		}
		actor, err := anchoredSessionActor(bound)
		if err != nil {
			return struct{}{}, err
		}
		standing, err := removals.GetAnchoredSessionRemoval(bound, repo, provider, sessionID)
		if own && (errors.Is(err, domain.ErrNotFound) || (err == nil && standing.Uploader != actor)) {
			return struct{}{}, errAnchoredArchiveNotOwn
		}
		if err != nil {
			return struct{}{}, err
		}
		return struct{}{}, removals.DeleteAnchoredSessionRemoval(bound, repo, provider, sessionID)
	}
	ctx = context.WithValue(auditOperation(ctx, "anchored_session.allowed"), revisionScopeKey{}, "none")
	_, err := repositoryWrite(ctx, s, repo, func(bound context.Context) (struct{}, error) { return allow(bound, true) })
	if !errors.Is(err, errAnchoredArchiveNotOwn) {
		return err
	}
	_, err = repositoryWrite(writeAction(ctx, "manage"), s, repo, func(bound context.Context) (struct{}, error) { return allow(bound, false) })
	return err
}

// ListAnchoredSessionRemovals lists the caller's own sessions that were
// removed from the server and not allowed again.
func (s *Service) ListAnchoredSessionRemovals(ctx context.Context, repo domain.ContentHash) ([]domain.AnchoredSessionRemovalView, error) {
	if err := validateHashes(repo); err != nil {
		return nil, err
	}
	return repositoryRead(ctx, s, func(bound context.Context) ([]domain.AnchoredSessionRemovalView, error) {
		removals, err := s.anchoredRemovalStore()
		if err != nil {
			return nil, err
		}
		actor, err := anchoredSessionActor(bound)
		if err != nil {
			return nil, err
		}
		stored, err := removals.ListAnchoredSessionRemovals(bound, repo, actor)
		if err != nil {
			return nil, err
		}
		views := make([]domain.AnchoredSessionRemovalView, 0, len(stored))
		for _, removal := range stored {
			views = append(views, removal.View(actor))
		}
		return views, nil
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
		if anchoredOwn(reader.actor, session.Uploader) {
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
