package app

import (
	"context"
	"errors"
	"fmt"
	"net/http"

	"github.com/wnsdy95/cxthub/cli/internal/domain"
	"github.com/wnsdy95/cxthub/cli/internal/ports/outbound"
)

const maxMemoryAttachmentDepth = 1024

type memoryAttachmentObject struct {
	hash   domain.ContentHash
	digest domain.MemoryDigest
}

type memoryPushPlan struct {
	snapshotID domain.ContentHash
	// newest first; fallback replay walks this in reverse.
	chain []memoryAttachmentObject
}

func validateMemoryAttachmentObject(digest domain.MemoryDigest, hash, snapshotID domain.ContentHash) error {
	if digest.SnapshotID != snapshotID {
		return domain.ErrHashMismatch
	}
	if err := domain.ValidateOptionalContentHash(digest.PreviousMemoryHash); err != nil {
		return err
	}
	got, err := domain.MemoryDigestHash(digest)
	if err != nil {
		return err
	}
	if got != hash {
		return domain.ErrHashMismatch
	}
	return nil
}

func (s *SyncRepoService) localMemoryPushPlan(ctx context.Context, snapshotID, current domain.ContentHash) (memoryPushPlan, error) {
	plan := memoryPushPlan{snapshotID: snapshotID}
	seen := map[domain.ContentHash]bool{}
	for hash := current; hash != ""; {
		if len(plan.chain) >= maxMemoryAttachmentDepth || seen[hash] {
			return memoryPushPlan{}, fmt.Errorf("%w: invalid memory attachment chain", domain.ErrHashMismatch)
		}
		seen[hash] = true
		digest, err := s.store.GetMemory(ctx, hash)
		if err != nil {
			return memoryPushPlan{}, err
		}
		if err := validateMemoryAttachmentObject(digest, hash, snapshotID); err != nil {
			return memoryPushPlan{}, err
		}
		plan.chain = append(plan.chain, memoryAttachmentObject{hash: hash, digest: digest})
		hash = digest.PreviousMemoryHash
	}
	return plan, nil
}

func isMemoryAttachmentConflict(err error) bool {
	if errors.Is(err, domain.ErrSyncConflict) {
		return true
	}
	var statusErr statusCodeError
	return errors.As(err, &statusErr) && statusErr.StatusCode() == http.StatusConflict
}

func isMemoryAttachmentNotFound(err error) bool {
	if errors.Is(err, domain.ErrNotFound) {
		return true
	}
	var statusErr statusCodeError
	return errors.As(err, &statusErr) && statusErr.StatusCode() == http.StatusNotFound
}

// A bounded retry rereads the authoritative pointer after CAS contention. Only
// a verified causal fork is a sync conflict; ordinary concurrent progress is not.
const maxMemoryPushReplans = 4

func (s *SyncRepoService) pushMemoryPlan(ctx context.Context, repoID string, plan memoryPushPlan) error {
	if len(plan.chain) == 0 {
		return nil
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := s.remote.PushMemory(ctx, repoID, plan.chain[0].digest); err == nil {
		return nil
	} else if !isMemoryAttachmentConflict(err) {
		return err
	}
	start, err := s.readMemoryPushStart(ctx, repoID, plan)
	if err != nil {
		return err
	}
	return s.replayMemorySuffix(ctx, repoID, plan, start)
}

// The known catalog avoids transmitting an already-current digest. A stale
// catalog is only a hint; the server still CAS-checks every attachment write.
func (s *SyncRepoService) pushMemoryPlanFromKnown(ctx context.Context, repoID string, plan memoryPushPlan, remoteHash domain.ContentHash) error {
	if len(plan.chain) == 0 {
		return nil
	}
	start, known := memoryPushStart(plan, remoteHash)
	if !known {
		var err error
		start, err = s.readMemoryPushStart(ctx, repoID, plan)
		if err != nil {
			return err
		}
	}
	return s.replayMemorySuffix(ctx, repoID, plan, start)
}

func memoryPushStart(plan memoryPushPlan, remoteHash domain.ContentHash) (int, bool) {
	if remoteHash == "" {
		return len(plan.chain) - 1, true
	}
	for i, object := range plan.chain {
		if object.hash == remoteHash {
			return i - 1, true
		}
	}
	return 0, false
}

func (s *SyncRepoService) replayMemorySuffix(ctx context.Context, repoID string, plan memoryPushPlan, start int) error {
	replans := 0
	for i := start; i >= 0; {
		if err := ctx.Err(); err != nil {
			return err
		}
		err := s.remote.PushMemory(ctx, repoID, plan.chain[i].digest)
		if err == nil {
			i--
			continue
		}
		if !isMemoryAttachmentConflict(err) {
			return err
		}
		if replans >= maxMemoryPushReplans {
			return fmt.Errorf("%w: snapshot %s; retry push without force", domain.ErrMemoryContention, plan.snapshotID)
		}
		replans++
		i, err = s.readMemoryPushStart(ctx, repoID, plan)
		if err != nil {
			return err
		}
	}
	return nil
}

// readMemoryPushStart validates the actual current digest and any necessary
// ancestry. It never writes a local pointer or overwrites a remote descendant.
func (s *SyncRepoService) readMemoryPushStart(ctx context.Context, repoID string, plan memoryPushPlan) (int, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	current, err := s.remote.PullMemory(ctx, repoID, plan.snapshotID)
	if isMemoryAttachmentNotFound(err) {
		return len(plan.chain) - 1, nil
	}
	if err != nil {
		return 0, err
	}
	hash, err := domain.MemoryDigestHash(current)
	if err != nil {
		return 0, err
	}
	if err := validateMemoryAttachmentObject(current, hash, plan.snapshotID); err != nil {
		return 0, err
	}
	if start, known := memoryPushStart(plan, hash); known {
		return start, nil
	}
	loader := &memoryPullLoader{service: s, ctx: ctx, repoID: repoID, snapshotID: plan.snapshotID, staged: map[domain.ContentHash]domain.MemoryDigest{hash: current}}
	ahead, err := memoryAttachmentAncestor(loader, plan.chain[0].hash, hash)
	if err != nil {
		return 0, err
	}
	if ahead {
		return -1, nil
	}
	return 0, fmt.Errorf("%w: remote memory attachment diverged for snapshot %s", domain.ErrSyncConflict, plan.snapshotID)
}

// Preflight classifies a current pointer outside the local chain. Concurrent
// catalog changes are checked against the actual returned digest, not assumed
// to be forks. Publication still performs CAS and repeats this proof on conflict.
func (s *SyncRepoService) preflightKnownRemoteMemory(ctx context.Context, repoID string, plan memoryPushPlan, remoteHash domain.ContentHash) (bool, error) {
	if len(plan.chain) == 0 {
		return false, nil
	}
	if _, known := memoryPushStart(plan, remoteHash); known {
		return false, nil
	}
	start, err := s.readMemoryPushStart(ctx, repoID, plan)
	return start < 0, err
}

type memoryPullLoader struct {
	service    *SyncRepoService
	ctx        context.Context
	repoID     string
	snapshotID domain.ContentHash
	staged     map[domain.ContentHash]domain.MemoryDigest
	loaded     map[domain.ContentHash]domain.MemoryDigest
}

func (l *memoryPullLoader) load(hash domain.ContentHash) (domain.MemoryDigest, error) {
	if digest, ok := l.loaded[hash]; ok {
		return digest, nil
	}
	if digest, ok := l.staged[hash]; ok {
		if l.loaded != nil {
			l.loaded[hash] = digest
		}
		return digest, nil
	}
	if digest, err := l.service.store.GetMemory(l.ctx, hash); err == nil {
		if err := validateMemoryAttachmentObject(digest, hash, l.snapshotID); err != nil {
			return domain.MemoryDigest{}, err
		}
		if l.loaded == nil {
			l.loaded = map[domain.ContentHash]domain.MemoryDigest{}
		}
		l.loaded[hash] = digest
		return digest, nil
	} else if !errors.Is(err, domain.ErrNotFound) {
		return domain.MemoryDigest{}, err
	}
	digest, err := l.service.remote.PullMemoryObject(l.ctx, l.repoID, hash)
	if err != nil {
		return domain.MemoryDigest{}, err
	}
	if err := validateMemoryAttachmentObject(digest, hash, l.snapshotID); err != nil {
		return domain.MemoryDigest{}, err
	}
	if l.staged == nil {
		l.staged = map[domain.ContentHash]domain.MemoryDigest{}
	}
	l.staged[hash] = digest
	if l.loaded == nil {
		l.loaded = map[domain.ContentHash]domain.MemoryDigest{}
	}
	l.loaded[hash] = digest
	return digest, nil
}

func memoryAttachmentAncestor(loader *memoryPullLoader, ancestor, descendant domain.ContentHash) (bool, error) {
	if ancestor == descendant {
		return true, nil
	}
	seen := map[domain.ContentHash]bool{}
	for hash := descendant; hash != ""; {
		if len(seen) >= maxMemoryAttachmentDepth || seen[hash] {
			return false, fmt.Errorf("%w: invalid memory attachment chain", domain.ErrHashMismatch)
		}
		seen[hash] = true
		digest, err := loader.load(hash)
		if err != nil {
			return false, err
		}
		hash = digest.PreviousMemoryHash
		if hash == ancestor {
			return true, nil
		}
	}
	return false, nil
}

// restoreMemoryArchive runs only after a missing attachment target. The push
// holds local object retention throughout. No retry of arbitrary failures and
// no ref publication until this exact archive and its causal suffix succeed.
func (s *SyncRepoService) restoreMemoryArchive(ctx context.Context, repoID string, plan memoryPushPlan) error {
	publisher, ok := s.remote.(outbound.MemoryArchivePublisher)
	if !ok {
		return fmt.Errorf("%w: remote cannot restore memory archive %s atomically", domain.ErrNotFound, plan.snapshotID)
	}
	if len(plan.chain) == 0 {
		return domain.ErrHashMismatch
	}
	snap, err := s.store.GetSnapshot(ctx, plan.snapshotID)
	if err != nil {
		return err
	}
	snap.RepoID = repoID
	doc, err := s.store.GetDoc(ctx, snap.DocHash)
	if err != nil {
		return err
	}
	root := plan.chain[len(plan.chain)-1]
	if err := publisher.PublishMemoryArchive(ctx, repoID, snap, doc, root.digest); err != nil {
		return fmt.Errorf("restore memory archive %s: %w", plan.snapshotID, err)
	}
	return s.pushMemoryPlanFromKnown(ctx, repoID, plan, root.hash)
}
