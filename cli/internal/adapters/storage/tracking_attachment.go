package storage

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"

	"github.com/wnsdy95/cxthub/cli/internal/domain"
	"github.com/wnsdy95/cxthub/cli/internal/ports/outbound"
)

// One accepted operation excludes subsequent ref writers until its redo ends.
// The separate immutable receipt closes the store-success / Git-journal-ack gap.
type trackingAttachmentJournal struct {
	Version         int                               `json:"version"`
	Commit          outbound.TrackingAttachmentCommit `json:"commit"`
	ProofHash       domain.ContentHash                `json:"proof_hash"`
	BeforeBinding   *localBranchRecord                `json:"before_binding,omitempty"`
	BeforeLifecycle *domain.Ref                       `json:"before_lifecycle,omitempty"`
	Lifecycle       *domain.Ref                       `json:"lifecycle,omitempty"`
}

type trackingAttachmentReceipt struct {
	Version     int                `json:"version"`
	RepoID      string             `json:"repo_id"`
	OperationID string             `json:"operation_id"`
	ProofHash   domain.ContentHash `json:"proof_hash"`
}

func (s *FileStore) trackingAttachmentPath() string {
	return filepath.Join(s.storeDir(), "tracking-attachment.json")
}

func trackingAttachmentHash(a domain.TrackingAttachment) (domain.ContentHash, error) {
	if err := domain.ValidateTrackingAttachment(a); err != nil {
		return "", err
	}
	ordered, err := domain.OrderHistoryEvents(a.Proof)
	if err != nil {
		return "", err
	}
	a.Proof = ordered
	raw, err := json.Marshal(a)
	return domain.HashContent(raw), err
}

func trackingBinding(a domain.TrackingAttachment) localBranchRecord {
	e := a.Event
	if e.LocalBranch == "" {
		e.LocalBranch = e.Branch
	}
	return localBranchRecord{Event: e}
}

func trackingRef(a domain.TrackingAttachment) domain.Ref {
	ref := a.ObservedRef
	ref.BranchID = a.Event.BranchID
	return ref
}

func (s *FileStore) trackingReceiptPath(a domain.TrackingAttachment) string {
	return filepath.Join(s.storeDir(), "tracking-attachments", a.Event.ID+".json")
}

func (s *FileStore) trackingAttachmentApplied(a domain.TrackingAttachment, hash domain.ContentHash) (bool, error) {
	raw, err := readCxtFile(s.trackingReceiptPath(a))
	if os.IsNotExist(err) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	var receipt trackingAttachmentReceipt
	want := trackingAttachmentReceipt{1, a.Event.RepoID, a.Event.ID, hash}
	if json.Unmarshal(raw, &receipt) != nil || receipt != want {
		return false, domain.ErrHashMismatch
	}
	return true, nil
}

func (s *FileStore) CommitTrackingAttachment(ctx context.Context, c outbound.TrackingAttachmentCommit) error {
	hash, err := trackingAttachmentHash(c.Attachment)
	if err != nil {
		return err
	}
	if c.Position != nil && c.Position.Next.Selection != nil && c.Position.Next.Selection.WorktreeID == "" {
		position, selection := *c.Position, *c.Position.Next.Selection
		selection.WorktreeID = position.Next.WorktreeID
		position.Next.Selection = &selection
		c.Position = &position
	}
	if err := validateTrackingCommit(c); err != nil {
		return err
	}
	return s.WithObjectsRetained(ctx, func() error {
		return s.withRefMutationLock(ctx, func() error {
			// Expected state can legitimately be obsolete after success. Never replay
			// a completed attachment over another writer's newer selection or ref.
			if done, err := s.trackingAttachmentApplied(c.Attachment, hash); err != nil || done {
				return err
			}
			j, err := s.prepareTrackingAttachment(ctx, c, hash)
			if err != nil {
				return err
			}
			if err := ctx.Err(); err != nil {
				return err
			}
			raw, err := json.Marshal(j)
			if err != nil {
				return err
			}
			// No applied history, binding, lifecycle or position precedes acceptance.
			if err := writeAtomic(s.trackingAttachmentPath(), raw); err != nil {
				return err
			}
			return s.recoverTrackingAttachment()
		})
	})
}

func validateTrackingCommit(c outbound.TrackingAttachmentCommit) error {
	if err := domain.ValidateTrackingAttachment(c.Attachment); err != nil {
		return err
	}
	e, ref := c.Attachment.Event, c.Attachment.ObservedRef
	if before := c.ExpectedRef; before != nil {
		if domain.ValidateRef(*before) != nil || before.RepoID != ref.RepoID || before.Kind != domain.RefBranch || before.Name != ref.Name || before.Symbolic != "" || before.Target == "" {
			return domain.ErrHashMismatch
		}
	}
	events := append(append([]domain.HistoryEvent{}, c.Attachment.Proof...), e)
	if c.Position == nil {
		_, err := domain.OrderHistoryEvents(events)
		return err
	}
	p := c.Position.Next
	local := trackingBinding(c.Attachment).Event.LocalBranch
	if p.WorktreeID == "" || p.WorktreeID != e.WorktreeID || p.RepoID != e.RepoID || p.Branch != e.Branch || p.BranchID != e.BranchID || p.GitBranch() != local || p.GitCommit != e.GitAfter || p.Snapshot != e.Target || p.SharedTarget != ref.Target || p.MemoryHash != e.MemoryHash || p.MemorySource != e.MemorySource || !p.MemoryPinned || p.Rewound != c.Attachment.Rewound || p.Orphan || p.Selection == nil {
		return domain.ErrHashMismatch
	}
	selection := p.Selection
	if domain.ValidateHistoryEvent(*selection) != nil || selection.Kind != "position" || selection.RepoID != p.RepoID || selection.Branch != p.Branch || selection.BranchID != p.BranchID || selection.LocalBranch != p.LocalBranch || selection.WorktreeID != p.WorktreeID || selection.GitAfter != p.GitCommit || selection.Target != p.Snapshot || selection.MemoryHash != p.MemoryHash || selection.MemorySource != p.MemorySource || !selection.MemoryPinned {
		return domain.ErrHashMismatch
	}
	var source domain.ContentHash
	var code string
	if old := c.Position.Expected; old != nil {
		if old.WorktreeID != p.WorktreeID || old.RepoID != p.RepoID {
			return domain.ErrHashMismatch
		}
		for _, id := range []domain.ContentHash{old.Snapshot, old.SharedTarget, old.MemoryHash, old.MemorySource} {
			if err := domain.ValidateOptionalContentHash(id); err != nil {
				return err
			}
		}
		if old.Selection != nil && domain.ValidateHistoryEvent(*old.Selection) != nil {
			return domain.ErrHashMismatch
		}
		source, code = old.Snapshot, old.GitCommit
	}
	if selection.Source != source || selection.GitBefore != code {
		return domain.ErrHashMismatch
	}
	_, err := domain.OrderHistoryEvents(append(events, *selection))
	return err
}

func (s *FileStore) prepareTrackingAttachment(ctx context.Context, c outbound.TrackingAttachmentCommit, hash domain.ContentHash) (trackingAttachmentJournal, error) {
	j := trackingAttachmentJournal{Version: 1, Commit: c, ProofHash: hash}
	if err := validateTrackingCommit(c); err != nil {
		return j, err
	}
	a := c.Attachment
	local, err := s.listHistoryEventsAndPositions(a.Event.RepoID)
	if err != nil {
		return j, err
	}
	owners, err := s.trackingMemoryOwners(ctx, a.ObservedRef, a.Proof, local)
	if err != nil {
		return j, err
	}
	if err := domain.ValidateTrackingAttachmentCompatibilityWithMemoryOwners(local, a, owners); err != nil {
		return j, err
	}
	ref := trackingRef(a)
	current, err := s.GetRef(ctx, ref.RepoID, domain.RefBranch, ref.Name)
	if err != nil && !errors.Is(err, domain.ErrNotFound) {
		return j, err
	}
	if (c.ExpectedRef == nil && err == nil) || (c.ExpectedRef != nil && (err != nil || current != *c.ExpectedRef)) {
		return j, domain.ErrSyncConflict
	}
	refs, err := s.listRefsRaw(ctx, ref.RepoID)
	if err != nil {
		return j, err
	}
	latest, found, err := domain.LatestBranchLifecycle(refs, ref.Name)
	if err != nil {
		return j, err
	}
	if found {
		if latest.State == domain.BranchArchived {
			return j, domain.ErrBranchArchived
		}
		j.BeforeLifecycle = &latest.Ref
	}
	// Check raw state as well: logical absence cannot authorize overwriting an
	// archived residue. Equal targets cannot authorize changing modern identity.
	rawRef, err := s.getRefRaw(ctx, ref.RepoID, domain.RefBranch, ref.Name)
	if err != nil && !errors.Is(err, domain.ErrNotFound) {
		return j, err
	}
	if (c.ExpectedRef == nil && err == nil) || (c.ExpectedRef != nil && (err != nil || rawRef != *c.ExpectedRef)) {
		return j, domain.ErrSyncConflict
	}
	if c.ExpectedRef != nil {
		if rawRef.BranchID != "" && rawRef.BranchID != ref.BranchID {
			state, err := domain.ProjectContextBranches(a.Proof)
			if err != nil || rawRef.BranchID != domain.LegacyContextBranchID(ref.RepoID, ref.Name) || state.Released[ref.Name] != "" {
				return j, domain.ErrSyncConflict
			}
		}
		if err := s.trackingForward(ctx, ref.RepoID, ref.Target, rawRef.Target); err != nil {
			return j, err
		}
	} else {
		generation, err := domain.NextBranchLifecycleGeneration(refs, ref.Name)
		if err != nil {
			return j, err
		}
		lifecycle, err := domain.NewBranchLifecycleRef(ref.RepoID, ref.Name, ref.Target, generation, domain.BranchActive)
		if err != nil {
			return j, err
		}
		j.Lifecycle = &lifecycle
	}
	want := trackingBinding(a)
	before, err := s.readLocalBinding(ref.RepoID, want.Event.LocalBranch)
	if err == nil {
		if !reflect.DeepEqual(before, want) {
			return j, domain.ErrSyncConflict
		}
		j.BeforeBinding = &before
	} else if !errors.Is(err, domain.ErrNotFound) {
		return j, err
	}
	if p := c.Position; p != nil {
		if s.worktreeID != p.Next.WorktreeID || s.gitBranch != p.Next.GitBranch() || s.gitCommit != p.Next.GitCommit {
			return j, domain.ErrCodePositionMismatch
		}
		current, err := s.readPosition()
		if err != nil && !errors.Is(err, domain.ErrNotFound) {
			return j, err
		}
		if (p.Expected == nil && err == nil) || (p.Expected != nil && (err != nil || !reflect.DeepEqual(current, *p.Expected))) {
			return j, domain.ErrSyncConflict
		}
		if _, err := domain.OrderHistoryEvents(append(append(append([]domain.HistoryEvent{}, local...), a.Proof...), *p.Next.Selection)); err != nil {
			return j, err
		}
	}
	if err := validateTrackingAttachmentJournal(j); err != nil {
		return j, err
	}
	return j, s.verifyTrackingObjects(ctx, j)
}

// A recovered position may be this operation's partial after-image at a newer
// Git commit than Code. Exclude only its exact frozen payload, never just its ID.
func trackingCompatibility(local []domain.HistoryEvent, c outbound.TrackingAttachmentCommit, owners map[domain.ContentHash]domain.ContentHash) error {
	if c.Position != nil {
		filtered := make([]domain.HistoryEvent, 0, len(local))
		for _, e := range local {
			if e.ID == c.Position.Next.Selection.ID {
				if !reflect.DeepEqual(e, *c.Position.Next.Selection) {
					return domain.ErrHashMismatch
				}
				continue
			}
			filtered = append(filtered, e)
		}
		local = filtered
	}
	return domain.ValidateTrackingAttachmentCompatibilityWithMemoryOwners(local, c.Attachment, owners)
}

// Only immutable ancestry can prove a safe move here. An unapplied observed
// graft is not authority to overwrite an unpublished local tip.
func (s *FileStore) trackingForward(ctx context.Context, repo string, from, ancestor domain.ContentHash) error {
	seen := map[domain.ContentHash]bool{}
	queue := []domain.ContentHash{from}
	for len(queue) != 0 {
		id := queue[len(queue)-1]
		queue = queue[:len(queue)-1]
		if id == ancestor {
			return nil
		}
		if seen[id] {
			continue
		}
		seen[id] = true
		snap, err := s.GetSnapshot(ctx, id)
		if err != nil {
			return err
		}
		if snap.RepoID != repo {
			return domain.ErrHashMismatch
		}
		queue = append(queue, snap.Parents...)
	}
	return domain.ErrSyncConflict
}

func validateTrackingAttachmentJournal(j trackingAttachmentJournal) error {
	if j.Version != 1 {
		return domain.ErrHashMismatch
	}
	if err := validateTrackingCommit(j.Commit); err != nil {
		return err
	}
	hash, err := trackingAttachmentHash(j.Commit.Attachment)
	if err != nil || hash != j.ProofHash {
		return domain.ErrHashMismatch
	}
	a := j.Commit.Attachment
	if j.BeforeBinding != nil && !reflect.DeepEqual(*j.BeforeBinding, trackingBinding(a)) {
		return domain.ErrHashMismatch
	}
	if (j.Lifecycle == nil) != (j.Commit.ExpectedRef != nil) {
		return domain.ErrHashMismatch
	}
	var generation uint64
	for _, ref := range []*domain.Ref{j.BeforeLifecycle, j.Lifecycle} {
		if ref == nil {
			continue
		}
		event, ok, err := domain.ParseBranchLifecycleRef(*ref)
		if err != nil || !ok || ref.RepoID != a.Event.RepoID || event.Branch != a.Event.Branch || event.State != domain.BranchActive || (ref == j.Lifecycle && event.Target != a.ObservedRef.Target) {
			return domain.ErrHashMismatch
		}
		if ref == j.Lifecycle && event.Generation != generation+1 {
			return domain.ErrHashMismatch
		}
		generation = event.Generation
	}
	return nil
}

func trackingEvents(j trackingAttachmentJournal) []domain.HistoryEvent {
	events := append(append([]domain.HistoryEvent{}, j.Commit.Attachment.Proof...), j.Commit.Attachment.Event)
	if p := j.Commit.Position; p != nil {
		for _, value := range []*domain.WorkingPosition{p.Expected, &p.Next} {
			if value != nil {
				events = append(events, domain.HistoryEvent{Target: value.Snapshot, SharedTarget: value.SharedTarget, MemorySource: value.MemorySource, MemoryHash: value.MemoryHash})
				if value.Selection != nil {
					events = append(events, *value.Selection)
				}
			}
		}
	}
	if ref := j.Commit.ExpectedRef; ref != nil {
		events = append(events, domain.HistoryEvent{Target: ref.Target})
	}
	return events
}

// Rebuilt under the ref lock for each acceptance/recovery, from actual objects.
func (s *FileStore) trackingMemoryOwners(ctx context.Context, ref domain.Ref, groups ...[]domain.HistoryEvent) (map[domain.ContentHash]domain.ContentHash, error) {
	owners := map[domain.ContentHash]domain.ContentHash{}
	identity := ref.BranchID
	if identity == "" {
		identity = domain.LegacyContextBranchID(ref.RepoID, ref.Name)
	}
	for _, events := range groups {
		for _, e := range events {
			if e.BranchID != identity || !e.MemoryPinned || e.MemoryHash == "" || e.Kind == "publish" || e.Kind == "pr-merge" || owners[e.MemoryHash] != "" {
				continue
			}
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			// GetMemory verifies the requested hash and the digest's references.
			memory, err := s.GetMemory(ctx, e.MemoryHash)
			if err != nil {
				return nil, err
			}
			owners[e.MemoryHash] = memory.SnapshotID
		}
	}
	return owners, nil
}

func (s *FileStore) verifyTrackingObjects(ctx context.Context, j trackingAttachmentJournal) error {
	seen := map[domain.ContentHash]bool{}
	for _, e := range trackingEvents(j) {
		for _, id := range []domain.ContentHash{e.Source, e.Target, e.SharedTarget, e.MemorySource} {
			if id == "" || seen[id] {
				continue
			}
			seen[id] = true
			snap, err := s.GetSnapshot(ctx, id)
			if err != nil {
				return err
			}
			if snap.ID != id || snap.DocHash != id || snap.RepoID != j.Commit.Attachment.Event.RepoID {
				return domain.ErrHashMismatch
			}
			if err := s.VerifyStoredDoc(ctx, snap.DocHash); err != nil {
				return err
			}
		}
		if e.MemoryHash != "" {
			memory, err := s.GetMemory(ctx, e.MemoryHash)
			if err != nil {
				return err
			}
			if (e.MemorySource != "" && memory.SnapshotID != e.MemorySource) || (memory.SnapshotID != e.Source && memory.SnapshotID != e.Target && memory.SnapshotID != e.MemorySource) {
				return domain.ErrHashMismatch
			}
		}
	}
	return nil
}

func (s *FileStore) readTrackingAttachment() (trackingAttachmentJournal, error) {
	var j trackingAttachmentJournal
	raw, err := readCxtFile(s.trackingAttachmentPath())
	if err != nil {
		return j, err
	}
	if json.Unmarshal(raw, &j) != nil {
		return j, domain.ErrHashMismatch
	}
	return j, validateTrackingAttachmentJournal(j)
}

func (s *FileStore) recoverTrackingAttachment() error {
	j, err := s.readTrackingAttachment()
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	ctx := context.Background()
	a := j.Commit.Attachment
	done, err := s.trackingAttachmentApplied(a, j.ProofHash)
	if err != nil {
		return err
	}
	if !done {
		if err := s.verifyTrackingObjects(ctx, j); err != nil {
			return err
		}
		current, err := s.getRefRaw(ctx, a.Event.RepoID, domain.RefBranch, a.Event.Branch)
		if err != nil && !errors.Is(err, domain.ErrNotFound) {
			return err
		}
		before := (j.Commit.ExpectedRef == nil && errors.Is(err, domain.ErrNotFound)) || (j.Commit.ExpectedRef != nil && err == nil && current == *j.Commit.ExpectedRef)
		if !before && (err != nil || current != trackingRef(a)) {
			return domain.ErrSyncConflict
		}
		refs, err := s.listRefsRaw(ctx, a.Event.RepoID)
		if err != nil {
			return err
		}
		latest, found, err := domain.LatestBranchLifecycle(refs, a.Event.Branch)
		if err != nil {
			return err
		}
		if !((!found && j.BeforeLifecycle == nil) || (found && ((j.BeforeLifecycle != nil && latest.Ref == *j.BeforeLifecycle) || (j.Lifecycle != nil && latest.Ref == *j.Lifecycle)))) {
			return domain.ErrSyncConflict
		}
		want := trackingBinding(a)
		binding, err := s.readLocalBinding(a.Event.RepoID, want.Event.LocalBranch)
		if err != nil && !errors.Is(err, domain.ErrNotFound) {
			return err
		}
		if !((errors.Is(err, domain.ErrNotFound) && j.BeforeBinding == nil) || (err == nil && reflect.DeepEqual(binding, want))) {
			return domain.ErrSyncConflict
		}
		owner := *s
		if p := j.Commit.Position; p != nil {
			owner.worktreeID = p.Next.WorktreeID
			current, err := owner.readPosition()
			if err != nil && !errors.Is(err, domain.ErrNotFound) {
				return err
			}
			before := (p.Expected == nil && errors.Is(err, domain.ErrNotFound)) || (p.Expected != nil && err == nil && reflect.DeepEqual(current, *p.Expected))
			if !before && (err != nil || !reflect.DeepEqual(current, p.Next)) {
				return domain.ErrSyncConflict
			}
		}
		local, err := s.listHistoryEventsAndPositions(a.Event.RepoID)
		if err != nil {
			return err
		}
		owners, err := s.trackingMemoryOwners(ctx, a.ObservedRef, a.Proof, local)
		if err != nil {
			return err
		}
		if err := trackingCompatibility(local, j.Commit, owners); err != nil {
			return err
		}
		ordered, err := domain.AppliedTrackingProof(a)
		if err != nil {
			return err
		}
		for _, e := range append(ordered, a.Event) {
			if err := s.putHistoryEvent(e); err != nil {
				return err
			}
		}
		if j.Lifecycle != nil {
			if err := s.putRefRaw(*j.Lifecycle); err != nil {
				return err
			}
		}
		if err := s.putRefRaw(trackingRef(a)); err != nil {
			return err
		}
		if err := s.writeLocalBinding(want); err != nil {
			return err
		}
		if j.Commit.Position != nil {
			if err := owner.writePosition(j.Commit.Position.Next); err != nil {
				return err
			}
		}
		raw, err := json.Marshal(trackingAttachmentReceipt{1, a.Event.RepoID, a.Event.ID, j.ProofHash})
		if err != nil {
			return err
		}
		if err := writeAtomic(s.trackingReceiptPath(a), raw); err != nil {
			return err
		}
	}
	if err := os.Remove(s.trackingAttachmentPath()); err != nil {
		return err
	}
	return syncCxtParents(s.trackingAttachmentPath())
}

func (s *FileStore) HasTrackingAttachmentPin(ctx context.Context, id domain.ContentHash) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}
	if err := domain.ValidateContentHash(id); err != nil {
		return false, err
	}
	j, err := s.readTrackingAttachment()
	if os.IsNotExist(err) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	for _, e := range trackingEvents(j) {
		if id == e.Source || id == e.Target || id == e.SharedTarget || id == e.MemorySource {
			return true, nil
		}
	}
	return false, nil
}

var _ outbound.TrackingAttachmentStore = (*FileStore)(nil)
var _ outbound.TrackingAttachmentPins = (*FileStore)(nil)
