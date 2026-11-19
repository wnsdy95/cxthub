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

// anchoredRestoreBatch is how many sessions one restoration request rebuilds.
// Each costs two document reads per capture.
const anchoredRestoreBatch = 20

type anchoredCaptureGroup struct {
	key      anchoredKey
	captures []domain.Snapshot
	commits  map[domain.ContentHash][]string
}

// newest is the capture that holds the conversation: the latest, then the
// largest snapshot.
func (g anchoredCaptureGroup) newest() domain.Snapshot {
	newest := g.captures[0]
	for _, capture := range g.captures[1:] {
		if capture.CreatedAt.After(newest.CreatedAt) || (capture.CreatedAt.Equal(newest.CreatedAt) && capture.ID > newest.ID) {
			newest = capture
		}
	}
	return newest
}

// upToDate reports whether a stored restored report was rebuilt from the
// same newest capture. Captures are added, never changed, and a new one is
// the newest, so nothing else would change the report.
func (g anchoredCaptureGroup) upToDate(stored domain.AnchoredSession) bool {
	return stored.Body.Content != nil && stored.Body.Content.Doc == g.newest().DocHash
}

// RestoreAnchoredSessions rebuilds, from the captures the server kept before
// session reports, the sessions of the repository that no CLI has reported.
// One request rebuilds up to a batch of sessions and says whether any are
// left. A session a CLI reported is left alone; a restored one that its
// captures still explain the same way is not read again.
func (s *Service) RestoreAnchoredSessions(ctx context.Context, repo domain.ContentHash) (domain.AnchoredRestoreResult, error) {
	result := domain.AnchoredRestoreResult{}
	if err := validateHashes(repo); err != nil {
		return result, err
	}
	actor, err := anchoredSessionActor(ctx)
	if err != nil {
		return result, err
	}
	// Restoring is done for the whole repository, by one who maintains it.
	current, err := s.meta.GetRepo(ctx, repo)
	if err != nil {
		return result, err
	}
	if role, member := s.repositoryRole(ctx, current.RepositoryID, actor); !member || !role.AtLeast(domain.RoleMaintainer) {
		return result, domain.ErrForbidden
	}
	store, err := s.anchoredSessionStore()
	if err != nil {
		return result, err
	}
	history, ok := s.meta.(outbound.HistoryStore)
	if !ok {
		return result, fmt.Errorf("%w: context history storage unavailable", domain.ErrConflict)
	}
	input, err := repositoryRead(ctx, s, func(bound context.Context) (anchoredRestoreInput, error) {
		snapshots, err := s.meta.ListSnapshots(bound, repo, "")
		if err != nil {
			return anchoredRestoreInput{}, err
		}
		events, err := history.ListHistoryEvents(bound, repo)
		if err != nil {
			return anchoredRestoreInput{}, err
		}
		sessions, err := store.ListRepositoryAnchoredSessions(bound, repo)
		if err != nil {
			return anchoredRestoreInput{}, err
		}
		return anchoredRestoreInput{snapshots: snapshots, events: events, sessions: sessions}, nil
	})
	if err != nil {
		return result, err
	}
	groups, stored := input.grouped()
	queries, err := s.DocumentQueries()
	if err != nil {
		return result, err
	}
	// The captures are read by the server for a caller who maintains the
	// repository and could read every one of them; nothing of them is
	// answered, only what the report is rebuilt from.
	reads := inbound.WithSystemActor(ctx)
	rebuilt := 0
	for _, group := range groups {
		existing, has := stored[group.key]
		switch {
		case has && existing.Uploader != domain.AnchoredRestoredUploader:
			result.Reported++
			continue
		case has && group.upToDate(existing):
			result.Unchanged++
			continue
		case rebuilt == anchoredRestoreBatch:
			result.Remaining = true
			continue
		}
		rebuilt++
		captures, err := s.readAnchoredCaptures(reads, queries, repo, group)
		if err != nil {
			if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
				return result, err
			}
			// Its conversation may become readable once it is prepared.
			result.Unreadable++
			result.Remaining = true
			continue
		}
		body, ok := domain.RestoreAnchoredSession(captures)
		if !ok {
			result.Unchanged++
			continue
		}
		changed, err := s.storeRestoredAnchoredSession(ctx, repo, group.key, body)
		if err != nil {
			return result, err
		}
		if changed {
			result.Restored++
		} else {
			result.Unchanged++
		}
	}
	return result, nil
}

type anchoredRestoreInput struct {
	snapshots []domain.Snapshot
	events    []domain.HistoryEvent
	sessions  []domain.AnchoredSession
}

// grouped returns the input as the captures of each
// session in key order with the commits each capture published, and the
// stored reports by key.
func (in anchoredRestoreInput) grouped() ([]anchoredCaptureGroup, map[anchoredKey]domain.AnchoredSession) {
	published := map[domain.ContentHash][]string{}
	for _, event := range in.events {
		if event.Kind != "publish" || event.Source == "" || event.Source != event.Target || !domain.ValidAnchoredCommitSHA(event.GitAfter) {
			continue
		}
		published[event.Source] = append(published[event.Source], event.GitAfter)
	}
	byKey := map[anchoredKey]*anchoredCaptureGroup{}
	for _, snapshot := range in.snapshots {
		if !domain.ValidAnchoredSessionKey(snapshot.Provider, snapshot.SessionID) || snapshot.DocHash == "" {
			continue
		}
		key := anchoredKey{snapshot.Provider, snapshot.SessionID}
		group := byKey[key]
		if group == nil {
			group = &anchoredCaptureGroup{key: key, commits: map[domain.ContentHash][]string{}}
			byKey[key] = group
		}
		group.captures = append(group.captures, snapshot)
		group.commits[snapshot.ID] = published[snapshot.ID]
	}
	groups := make([]anchoredCaptureGroup, 0, len(byKey))
	for _, group := range byKey {
		groups = append(groups, *group)
	}
	sort.Slice(groups, func(left, right int) bool {
		if groups[left].key.provider != groups[right].key.provider {
			return groups[left].key.provider < groups[right].key.provider
		}
		return groups[left].key.id < groups[right].key.id
	})
	stored := make(map[anchoredKey]domain.AnchoredSession, len(in.sessions))
	for _, session := range in.sessions {
		stored[anchoredKey{session.Provider, session.SessionID}] = session
	}
	return groups, stored
}

// readAnchoredCaptures reads, for each capture, how many events its
// conversation holds and whether all of it is the beginning of the newest
// capture's conversation.
func (s *Service) readAnchoredCaptures(ctx context.Context, queries anchoredDocumentReads, repo domain.ContentHash, group anchoredCaptureGroup) ([]domain.AnchoredCapture, error) {
	newest := group.newest()
	captures := make([]domain.AnchoredCapture, 0, len(group.captures))
	for _, snapshot := range group.captures {
		own, err := queries.ReadDocViewEvents(ctx, repo, snapshot.DocHash, "", 0, 1, "all")
		if err != nil {
			return nil, err
		}
		capture := domain.AnchoredCapture{Snapshot: snapshot.ID, Doc: snapshot.DocHash, Identity: snapshot.DocIdentity, CreatedAt: snapshot.CreatedAt,
			Commits: group.commits[snapshot.ID], Events: own.Total, Prefix: snapshot.ID == newest.ID}
		if !capture.Prefix && own.Total > 0 {
			shared, err := queries.ReadDocViewEvents(ctx, repo, newest.DocHash, snapshot.ID, 0, 1, "all")
			if err != nil {
				return nil, err
			}
			capture.Prefix = shared.Inherited == own.Total
		}
		captures = append(captures, capture)
	}
	return captures, nil
}

type anchoredDocumentReads interface {
	ReadDocViewEvents(ctx context.Context, repo, hash, base domain.ContentHash, offset, limit int, view string) (domain.DocViewPage, error)
}

// storeRestoredAnchoredSession stores a rebuilt report under the restored
// uploader, unless a CLI reported the session meanwhile, and opens what of it
// is pushed.
func (s *Service) storeRestoredAnchoredSession(ctx context.Context, repo domain.ContentHash, key anchoredKey, body domain.AnchoredSessionBody) (bool, error) {
	if err := body.Validate(); err != nil {
		return false, err
	}
	hash, err := body.Hash()
	if err != nil {
		return false, err
	}
	ctx = context.WithValue(auditOperation(ctx, "anchored_session.restored"), revisionScopeKey{}, "none")
	return repositoryWrite(ctx, s, repo, func(bound context.Context) (bool, error) {
		store, err := s.anchoredSessionStore()
		if err != nil {
			return false, err
		}
		now := time.Now().UTC()
		next := domain.AnchoredSession{RepoID: repo, Provider: key.provider, SessionID: key.id, Uploader: domain.AnchoredRestoredUploader,
			Revision: 1, BodyHash: hash, CreatedAt: now, UpdatedAt: now, Body: body}
		existing, err := store.GetAnchoredSession(bound, repo, key.provider, key.id)
		switch {
		case errors.Is(err, domain.ErrNotFound):
		case err != nil:
			return false, err
		case existing.Uploader != domain.AnchoredRestoredUploader, existing.BodyHash == hash:
			return false, nil
		default:
			next.Revision, next.CreatedAt = existing.Revision+1, existing.CreatedAt
		}
		if err := store.PutAnchoredSession(bound, next); err != nil {
			return false, err
		}
		return true, openAnchoredSessions(bound, s.meta, repo, &key, now)
	})
}
