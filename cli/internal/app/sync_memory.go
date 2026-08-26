package app

import (
	"context"
	"errors"
	"fmt"
	"net/http"

	"github.com/wnsdy95/cxthub/cli/internal/domain"
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

// pushMemoryPlan optimistically sends the newest attachment. The common case
// is one request. If the server is more than one causal generation behind, it
// reads the authoritative pointer and replays only the missing suffix oldest
// first. A remote pointer outside the local chain is a true divergence.
func (s *SyncRepoService) pushMemoryPlan(ctx context.Context, repoID string, plan memoryPushPlan) error {
	if len(plan.chain) == 0 {
		return nil
	}
	if err := s.remote.PushMemory(ctx, repoID, plan.chain[0].digest); err == nil {
		return nil
	} else if !isMemoryAttachmentConflict(err) {
		return err
	}

	remoteHash := domain.ContentHash("")
	remoteDigest, err := s.remote.PullMemory(ctx, repoID, plan.snapshotID)
	switch {
	case err == nil:
		remoteHash, err = domain.MemoryDigestHash(remoteDigest)
		if err != nil {
			return err
		}
		if err := validateMemoryAttachmentObject(remoteDigest, remoteHash, plan.snapshotID); err != nil {
			return err
		}
	case isMemoryAttachmentNotFound(err):
		// Empty server pointer: replay from the local root.
	default:
		return err
	}

	start := len(plan.chain) - 1
	if remoteHash != "" {
		start = -1
		for i, object := range plan.chain {
			if object.hash == remoteHash {
				start = i - 1
				break
			}
		}
		if start == -1 && plan.chain[0].hash != remoteHash {
			return fmt.Errorf("%w: remote memory attachment diverged for snapshot %s", domain.ErrSyncConflict, plan.snapshotID)
		}
	}
	for i := start; i >= 0; i-- {
		if err := s.remote.PushMemory(ctx, repoID, plan.chain[i].digest); err != nil {
			if isMemoryAttachmentConflict(err) {
				return fmt.Errorf("%w: memory attachment changed during push for snapshot %s", domain.ErrSyncConflict, plan.snapshotID)
			}
			return err
		}
	}
	return nil
}

// pushMemoryPlanFromKnown avoids transmitting an already-current large digest.
// The manifest hash is only a pointer advertisement; every sent object still
// goes through server CAS, so a concurrent change becomes a 409 rather than a
// stale overwrite.
func (s *SyncRepoService) pushMemoryPlanFromKnown(ctx context.Context, repoID string, plan memoryPushPlan, remoteHash domain.ContentHash) error {
	if len(plan.chain) == 0 {
		return nil
	}
	if remoteHash == plan.chain[0].hash {
		return nil
	}
	start := len(plan.chain) - 1
	if remoteHash != "" {
		start = -1
		for i, object := range plan.chain {
			if object.hash == remoteHash {
				start = i - 1
				break
			}
		}
		if start < 0 {
			return fmt.Errorf("%w: remote memory attachment diverged for snapshot %s", domain.ErrSyncConflict, plan.snapshotID)
		}
	}
	for i := start; i >= 0; i-- {
		if err := s.remote.PushMemory(ctx, repoID, plan.chain[i].digest); err != nil {
			if isMemoryAttachmentConflict(err) {
				return fmt.Errorf("%w: memory attachment changed during push for snapshot %s", domain.ErrSyncConflict, plan.snapshotID)
			}
			return err
		}
	}
	return nil
}

// preflightKnownRemoteMemory classifies the manifest's attachment without
// mutating either replica. A hash outside the local chain is ambiguous: it can
// be a normal remote descendant (this checkout is stale) or a true fork. Read
// the immutable remote chain to prove which case it is. A remote descendant is
// skipped by push and will be adopted by pull; a fork fails closed.
func (s *SyncRepoService) preflightKnownRemoteMemory(ctx context.Context, repoID string, plan memoryPushPlan, remoteHash domain.ContentHash) (bool, error) {
	if len(plan.chain) == 0 || remoteHash == "" {
		return false, nil
	}
	for _, object := range plan.chain {
		if object.hash == remoteHash {
			return false, nil
		}
	}

	remoteDigest, err := s.remote.PullMemory(ctx, repoID, plan.snapshotID)
	if err != nil {
		if isMemoryAttachmentNotFound(err) {
			return false, fmt.Errorf("%w: remote memory attachment changed during preflight for snapshot %s", domain.ErrSyncConflict, plan.snapshotID)
		}
		return false, err
	}
	actualHash, err := domain.MemoryDigestHash(remoteDigest)
	if err != nil {
		return false, err
	}
	if actualHash != remoteHash {
		return false, fmt.Errorf("%w: remote memory attachment changed during preflight for snapshot %s", domain.ErrSyncConflict, plan.snapshotID)
	}
	if err := validateMemoryAttachmentObject(remoteDigest, remoteHash, plan.snapshotID); err != nil {
		return false, err
	}
	loader := &memoryPullLoader{
		service:    s,
		ctx:        ctx,
		repoID:     repoID,
		snapshotID: plan.snapshotID,
		staged:     map[domain.ContentHash]domain.MemoryDigest{remoteHash: remoteDigest},
	}
	remoteAhead, err := memoryAttachmentAncestor(loader, plan.chain[0].hash, remoteHash)
	if err != nil {
		return false, err
	}
	if remoteAhead {
		return true, nil
	}
	return false, fmt.Errorf("%w: remote memory attachment diverged for snapshot %s", domain.ErrSyncConflict, plan.snapshotID)
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
