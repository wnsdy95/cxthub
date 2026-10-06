package app

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"reflect"
	"sort"
	"strings"
	"time"

	"github.com/wnsdy95/cxthub/cli/internal/domain"
	"github.com/wnsdy95/cxthub/cli/internal/ports/inbound"
	"github.com/wnsdy95/cxthub/cli/internal/ports/outbound"
)

// StagingService separates observation (add) from publication (commit). It uses
// Save's existing preservation/outbox mechanism when a frozen object already
// exists, and the store's working-commit transaction for final publication.
type StagingService struct {
	git       outbound.GitContext
	code      outbound.CodePosition
	store     outbound.SessionStore
	index     outbound.StagingStore
	save      *SaveSessionService
	retention outbound.ObjectRetention
}

func NewStagingService(git outbound.GitContext, code outbound.CodePosition, store outbound.SessionStore, index outbound.StagingStore, captures map[domain.ProviderKind]outbound.CaptureSource, codecs map[domain.ProviderKind]outbound.ProviderCodec, capture outbound.SessionCapture, outbox outbound.SyncOutbox) *StagingService {
	retention, _ := store.(outbound.ObjectRetention)
	return &StagingService{git: git, code: code, store: store, index: index, save: NewSaveSessionService(git, captures, codecs, store, capture, outbox), retention: retention}
}

var _ inbound.Staging = (*StagingService)(nil)

func (s *StagingService) Inspect(ctx context.Context, cwd string) (domain.StagingIndex, error) {
	repo, err := s.git.CurrentRepo(ctx, cwd)
	if err != nil {
		return domain.StagingIndex{}, err
	}
	index, _, err := s.index.ReadStaging(ctx, repo.ID)
	return index, err
}
func (s *StagingService) selection(ctx context.Context, cwd string) (domain.Repo, domain.StagingIndex, domain.WorkingPosition, error) {
	repo, err := s.git.CurrentRepo(ctx, cwd)
	if err != nil {
		return repo, domain.StagingIndex{}, domain.WorkingPosition{}, err
	}
	index, p, err := s.index.ReadStaging(ctx, repo.ID)
	if err != nil {
		return repo, index, p, err
	}
	if s.code == nil {
		return repo, index, p, fmt.Errorf("current Git commit reader unavailable")
	}
	sha, err := s.code.CurrentCommit(ctx, cwd)
	if err != nil {
		return repo, index, p, err
	}
	if err = domain.ValidateStagingCode(sha); err != nil {
		return repo, index, p, err
	}
	branch, err := s.git.CurrentBranch(ctx, cwd)
	if err != nil {
		return repo, index, p, err
	}
	if branch == "HEAD" {
		branch = ""
	}
	if p.GitCommit != sha {
		return repo, index, p, fmt.Errorf("Git and context commits differ; synchronize the worktree selection before staging: %w", domain.ErrCodePositionMismatch)
	}
	if p.GitBranch() != branch {
		return repo, index, p, fmt.Errorf("Git and context selections differ; synchronize the worktree selection before staging: %w", domain.ErrSelectionChanged)
	}
	return repo, index, p, nil
}

func (s *StagingService) Stage(ctx context.Context, in inbound.StageInput) (result domain.StagingIndex, err error) {
	if gate, ok := s.store.(outbound.CaptureTrackingGate); ok {
		err = gate.WithCaptureTrackingGate(ctx, func(locked context.Context) error {
			result, err = s.stage(locked, in)
			return err
		})
		return result, err
	}
	return s.stage(ctx, in)
}

func (s *StagingService) stage(ctx context.Context, in inbound.StageInput) (result domain.StagingIndex, err error) {
	if len(in.Sessions) == 0 {
		return result, fmt.Errorf("add requires explicitly resolved source sessions")
	}
	if s.retention == nil {
		return result, fmt.Errorf("staging requires coordinated object retention")
	}
	err = s.retention.WithObjectsRetained(ctx, func() error {
		repo, index, p, err := s.selection(ctx, in.Cwd)
		if err != nil {
			return err
		}
		if in.ExpectedRevision != "" && index.Revision != in.ExpectedRevision {
			return domain.ErrIndexChanged
		}
		operations, err := s.index.ListStagingCommits(ctx, repo.ID)
		if err != nil {
			return err
		}
		included, err := s.stagingAncestry(ctx, repo.ID, p.Snapshot)
		if err != nil {
			return err
		}
		prior := append([]domain.StagedSession{}, index.Entries...)
		for _, op := range operations {
			prior = append(prior, op.Index.Entries...)
		}
		sort.Slice(prior, func(i, j int) bool { return prior[i].Events > prior[j].Events })
		next := index
		next.Entries = append([]domain.StagedSession{}, index.Entries...)
		sourceSeen := map[domain.ContentHash]bool{}
		for _, input := range in.Sessions {
			if input.Path == "" {
				return fmt.Errorf("provider source must be resolved before add")
			}
			sourceID := domain.HashContent([]byte(string(input.Provider) + "\x00" + input.Path))
			if sourceSeen[sourceID] {
				return fmt.Errorf("duplicate source selector")
			}
			sourceSeen[sourceID] = true
			capt, ok := s.save.captures[input.Provider]
			if !ok {
				return domain.ErrUnsupportedProvider
			}
			codec, ok := s.save.codecs[input.Provider]
			if !ok {
				return domain.ErrUnsupportedProvider
			}
			if !s.save.capture.Eligible(repo.LocalPath, input.Path) {
				return domain.ErrNoActiveSession
			}
			env, hash, bytes, _, err := s.save.capture.Project(ctx, repo.LocalPath, input.Path, capt, codec, true)
			if err != nil {
				return fmt.Errorf("freeze %s session: %w", input.Provider, err)
			}
			if env.SourceProvider != input.Provider || env.SessionOriginID == "" || (input.SessionID != "" && env.SessionOriginID != input.SessionID) {
				return fmt.Errorf("provider session changed while staging: %w", domain.ErrHashMismatch)
			}
			doc, err := s.store.GetDoc(ctx, hash)
			if err != nil {
				return err
			}
			generation := hash
			for _, old := range prior {
				if old.Provider != input.Provider || old.SessionID != env.SessionOriginID || old.SourceID != sourceID || old.Events > len(doc.CIR.Events) {
					continue
				}
				oldDoc, err := s.store.GetDoc(ctx, old.DocHash)
				if err != nil {
					return err
				}
				if stagingPrefix(oldDoc, doc) {
					generation = old.Generation
					break
				}
			}
			entry := domain.StagedSession{Provider: input.Provider, SessionID: env.SessionOriginID, SourceID: sourceID, Generation: generation, DocHash: hash, Events: len(doc.CIR.Events), CapturedBytes: bytes, CodeCommit: p.GitCommit, Branch: p.Branch, BranchID: p.BranchID, Base: p.Snapshot, CapturedAt: time.Now().UTC()}
			entry.Key = domain.StagedSessionKey(entry.Provider, entry.SessionID, sourceID, generation)
			for _, op := range operations {
				if !op.LocalFinalized || !included[op.Position.Snapshot] {
					continue
				}
				for _, old := range op.Index.Entries {
					if old.Key == entry.Key && old.Events > entry.StartEvent && old.Events <= entry.Events {
						oldDoc, err := s.store.GetDoc(ctx, old.DocHash)
						if err != nil {
							return err
						}
						if stagingPrefix(oldDoc, doc) {
							entry.StartEvent = old.Events
						}
					}
				}
			}
			found := false
			for n, old := range next.Entries {
				if old.Key == entry.Key {
					next.Entries[n] = entry
					found = true
					break
				}
			}
			if !found {
				next.Entries = append(next.Entries, entry)
			}
		}
		// Recheck Git after capture; the store then CASes the complete position and
		// index under one lock. No source or index is discarded on either conflict.
		_, _, after, err := s.selection(ctx, in.Cwd)
		if err != nil {
			return err
		}
		if !reflect.DeepEqual(after, p) {
			return domain.ErrSelectionChanged
		}
		next.Sequence++
		next = next.WithRevision()
		if err = s.index.CompareAndSwapStaging(ctx, index.Revision, next, p); err != nil {
			return err
		}
		result = next
		return nil
	})
	return
}

func stagingPrefix(old, next domain.SessionDoc) bool {
	if old.CIR.Envelope.SourceProvider != next.CIR.Envelope.SourceProvider || old.CIR.Envelope.SessionOriginID != next.CIR.Envelope.SessionOriginID || len(old.CIR.Events) > len(next.CIR.Events) {
		return false
	}
	// Event IDs, ordering, provider metadata and compaction replacements all
	// participate. Similar text alone is never a source identity/coverage proof.
	return len(old.CIR.Events) == 0 || reflect.DeepEqual(old.CIR.Events, next.CIR.Events[:len(old.CIR.Events)])
}

func (s *StagingService) Unstage(ctx context.Context, in inbound.UnstageInput) (domain.StagingIndex, error) {
	repo, err := s.git.CurrentRepo(ctx, in.Cwd)
	if err != nil {
		return domain.StagingIndex{}, err
	}
	index, p, err := s.index.ReadStaging(ctx, repo.ID)
	if err != nil {
		return index, err
	}
	if in.ExpectedRevision != "" && index.Revision != in.ExpectedRevision {
		return index, domain.ErrIndexChanged
	}
	if in.All && len(in.Keys) > 0 || !in.All && len(in.Keys) == 0 {
		return index, fmt.Errorf("select staged entries or all, exclusively")
	}
	selected := map[domain.ContentHash]bool{}
	for _, key := range in.Keys {
		if err := domain.ValidateContentHash(key); err != nil {
			return index, err
		}
		selected[key] = true
	}
	next := index
	next.Entries = []domain.StagedSession{}
	for _, e := range index.Entries {
		if in.All || selected[e.Key] {
			delete(selected, e.Key)
		} else {
			next.Entries = append(next.Entries, e)
		}
	}
	if len(selected) > 0 {
		return index, fmt.Errorf("selected staging entry no longer exists: %w", domain.ErrNotFound)
	}
	next.Sequence++
	next = next.WithRevision()
	if err = s.index.CompareAndSwapStaging(ctx, index.Revision, next, p); err != nil {
		return index, err
	}
	return next, nil
}

func (s *StagingService) Commit(ctx context.Context, in inbound.StagingCommitInput) (result domain.StagingCommit, err error) {
	if s.retention == nil {
		return result, fmt.Errorf("staging requires coordinated object retention")
	}
	err = s.retention.WithObjectsRetained(ctx, func() error {
		repo, index, p, err := s.selection(ctx, in.Cwd)
		if err != nil {
			return err
		}
		if in.ExpectedRevision != "" && index.Revision != in.ExpectedRevision {
			return domain.ErrIndexChanged
		}
		if len(index.Entries) == 0 {
			return domain.ErrEmptyIndex
		}
		if p.Branch == "" {
			return fmt.Errorf("manual staged publication requires a named Git/context branch; detached captures remain preserved")
		}
		for _, e := range index.Entries {
			if e.CodeCommit != p.GitCommit {
				return fmt.Errorf("staged source %s belongs to another Git commit; re-add it or unstage it: %w", e.Key, domain.ErrCodePositionMismatch)
			}
			if e.BranchID != p.BranchID || e.Branch != p.Branch {
				return fmt.Errorf("staged source %s belongs to another code selection; re-add it or unstage it: %w", e.Key, domain.ErrSelectionChanged)
			}
		}
		ref, err := s.store.GetRef(ctx, repo.ID, domain.RefBranch, p.Branch)
		if errors.Is(err, domain.ErrNotFound) {
			ref = domain.Ref{RepoID: repo.ID, Kind: domain.RefBranch, Name: p.Branch, BranchID: p.BranchID}
		} else if err != nil {
			return err
		}
		if ref.BranchID == "" {
			ref.BranchID = p.BranchID
		}
		if ref.BranchID != p.BranchID || (p.Rewound && ref.Target != p.SharedTarget) {
			return domain.ErrSyncConflict
		}
		var random [16]byte
		if _, err := rand.Read(random[:]); err != nil {
			return err
		}
		result = domain.StagingCommit{Version: domain.StagingVersion, ID: hex.EncodeToString(random[:]), Index: index, ExpectedPosition: p, ExpectedRef: ref, CreatedAt: time.Now().UTC()}
		entries := append([]domain.StagedSession{}, index.Entries...)
		sort.Slice(entries, func(i, j int) bool {
			if entries[i].CapturedAt.Equal(entries[j].CapturedAt) {
				return entries[i].Key < entries[j].Key
			}
			return entries[i].CapturedAt.Before(entries[j].CapturedAt)
		})
		tip := p.Snapshot
		if !p.Rewound && ref.Target != "" {
			tip = ref.Target
		}
		for _, entry := range entries {
			doc, err := s.store.GetDoc(ctx, entry.DocHash)
			if err != nil {
				return err
			}
			if doc.CIR.Envelope.SourceProvider != entry.Provider || doc.CIR.Envelope.SessionOriginID != entry.SessionID || len(doc.CIR.Events) != entry.Events {
				return domain.ErrHashMismatch
			}
			snap, err := s.store.GetSnapshot(ctx, entry.DocHash)
			if errors.Is(err, domain.ErrNotFound) {
				snap = domain.Snapshot{ID: entry.DocHash, DocHash: entry.DocHash, RepoID: repo.ID, Branch: p.Branch, Provider: entry.Provider, SessionID: entry.SessionID, Fidelity: doc.CIR.Envelope.Fidelity, Models: doc.CIR.Envelope.OrderedModels(), CompactionCount: doc.CIR.Envelope.CompactionCount, Author: in.Author, Message: in.Message, CreatedAt: entry.CapturedAt}
				if snap.Message == "" {
					snap.Message = "staged session"
				}
				if tip != "" && tip != snap.ID {
					snap.Parents = []domain.ContentHash{tip}
				}
				if p.MemoryHash != "" {
					memory, err := s.store.GetMemory(ctx, p.MemoryHash)
					if err != nil {
						return err
					}
					inherited := domain.MergeDigests(memory, domain.MemoryDigest{SnapshotID: snap.ID, Provider: snap.Provider})
					inherited.PreviousMemoryHash = ""
					snap.MemoryHash, err = s.store.PutMemory(ctx, inherited)
					if err != nil {
						return err
					}
				}
				if err = s.store.PutSnapshot(ctx, snap); err != nil {
					return err
				}
			} else if err != nil {
				return err
			}
			if snap.RepoID != repo.ID || snap.DocHash != entry.DocHash || snap.Provider != entry.Provider || snap.SessionID != entry.SessionID {
				return domain.ErrHashMismatch
			}
			if strings.HasPrefix(snap.Message, domain.HookMessagePrefix) {
				message := in.Message
				if message == "" {
					message = "staged session"
				}
				if s.save.outbox == nil {
					return fmt.Errorf("staged publication outbox unavailable")
				}
				if err := s.save.outbox.EnqueuePromotion(ctx, repo.LocalPath, snap.ID, message); err != nil {
					return err
				}
				promoted := snap
				promoted.Message = message
				if err := s.store.PutSnapshot(ctx, promoted); err != nil {
					return err
				}
				snap = promoted
			}
			// Existing hook objects preserve their immutable natural parents. Reuse the
			// existing queue-before-overlay path for previously unjoined siblings.
			included, err := s.stagedReachable(ctx, repo.ID, tip, snap.ID)
			if err != nil {
				return err
			}
			if !included {
				if tip != "" {
					contains, err := s.stagedReachable(ctx, repo.ID, snap.ID, tip)
					if err != nil {
						return err
					}
					if !contains {
						if s.save.outbox == nil {
							return fmt.Errorf("staged preservation outbox unavailable")
						}
						if err := s.save.graftLocalAndQueue(ctx, repo.LocalPath, snap.ID, tip); err != nil {
							return err
						}
					}
				}
				tip = snap.ID
			}
			// A separately keyed publication permits several contributors at one SHA.
			key := domain.HashContent([]byte(result.ID + "\x00" + string(entry.Key)))
			result.Publications = append(result.Publications, domain.HistoryEvent{ID: string(key)[7:39], RepoID: repo.ID, WorktreeID: p.WorktreeID, Branch: p.Branch, BranchID: p.BranchID, LocalBranch: p.LocalBranch, Kind: "publish", Source: snap.ID, Target: snap.ID, MemoryHash: snap.MemoryHash, MemoryPinned: true, GitAfter: p.GitCommit, CreatedAt: result.CreatedAt})
		}
		selected, err := s.store.GetSnapshot(ctx, tip)
		if err != nil {
			return err
		}
		next := p
		next.Snapshot = tip
		next.SharedTarget = tip
		next.Rewound = false
		next.MemoryHash = selected.MemoryHash
		next.MemorySource = ""
		if next.MemoryHash == "" && p.MemoryHash != "" {
			memory, err := s.store.GetMemory(ctx, p.MemoryHash)
			if err != nil {
				return err
			}
			next.MemoryHash, next.MemorySource = p.MemoryHash, memory.SnapshotID
		}
		next.MemoryPinned = true
		next.Selection = &domain.HistoryEvent{ID: result.ID, RepoID: repo.ID, WorktreeID: p.WorktreeID, Branch: p.Branch, BranchID: p.BranchID, LocalBranch: p.LocalBranch, Kind: "publish", Source: tip, Target: tip, MemoryHash: next.MemoryHash, MemorySource: next.MemorySource, MemoryPinned: true, GitAfter: p.GitCommit, CreatedAt: result.CreatedAt}
		result.Position = next
		result.Ref = ref
		result.Ref.Target = tip
		if p.Rewound && ref.Target != "" && ref.Target != tip {
			key := domain.HashContent([]byte(result.ID + "/advance"))
			result.Advance = &domain.HistoryEvent{ID: string(key)[7:39], RepoID: repo.ID, Branch: p.Branch, BranchID: p.BranchID, WorktreeID: p.WorktreeID, Kind: "advance", Source: ref.Target, Target: tip, MemoryHash: next.MemoryHash, MemorySource: next.MemorySource, MemoryPinned: true, GitBefore: p.GitCommit, GitAfter: p.GitCommit, CreatedAt: result.CreatedAt}
		}
		_, _, after, err := s.selection(ctx, in.Cwd)
		if err != nil {
			return err
		}
		if !reflect.DeepEqual(after, p) {
			return domain.ErrSelectionChanged
		}
		result, err = s.index.FinalizeStagingCommit(ctx, result)
		return err
	})
	return
}

// Unlike Save's historical best-effort helper, staging cannot treat unreadable
// ancestry as permission to replace a ref. Missing/corrupt metadata fails closed.
func (s *StagingService) stagedReachable(ctx context.Context, repo string, from, target domain.ContentHash) (bool, error) {
	if from == "" {
		return false, nil
	}
	queue := []domain.ContentHash{from}
	seen := map[domain.ContentHash]bool{}
	for len(queue) > 0 {
		if err := ctx.Err(); err != nil {
			return false, err
		}
		id := queue[len(queue)-1]
		queue = queue[:len(queue)-1]
		if id == target {
			return true, nil
		}
		if seen[id] {
			continue
		}
		seen[id] = true
		snap, err := s.store.GetSnapshot(ctx, id)
		if err != nil {
			return false, err
		}
		if snap.RepoID != repo {
			return false, domain.ErrHashMismatch
		}
		queue = append(queue, snap.ReachabilityParents()...)
	}
	return false, nil
}
func (s *StagingService) ResumeCommit(ctx context.Context, cwd, id string) (domain.StagingCommit, error) {
	repo, err := s.git.CurrentRepo(ctx, cwd)
	if err != nil {
		return domain.StagingCommit{}, err
	}
	return s.index.ResumeStagingCommit(ctx, repo.ID, id)
}

// Coverage is local to the selected ancestry. Retained future/incomparable
// receipts are audit history, not proof that this selected code includes them.
func (s *StagingService) stagingAncestry(ctx context.Context, repo string, tip domain.ContentHash) (map[domain.ContentHash]bool, error) {
	seen := map[domain.ContentHash]bool{}
	queue := []domain.ContentHash{tip}
	for len(queue) > 0 {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		id := queue[len(queue)-1]
		queue = queue[:len(queue)-1]
		if id == "" || seen[id] {
			continue
		}
		if len(seen) >= 100000 {
			return nil, fmt.Errorf("selected staging ancestry exceeds local verification limit")
		}
		snapshot, err := s.store.GetSnapshot(ctx, id)
		if err != nil {
			return nil, err
		}
		if snapshot.RepoID != repo {
			return nil, domain.ErrHashMismatch
		}
		seen[id] = true
		queue = append(queue, snapshot.ReachabilityParents()...)
	}
	return seen, nil
}
