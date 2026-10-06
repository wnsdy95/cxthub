package domain

import (
	"fmt"
	"reflect"
)

// TrackingHistory validates the compatibility of an observed identity without
// applying it. Only the dedicated tracking command may adopt its evidence.
func TrackingHistory(ref Ref, remote, local []HistoryEvent) ([]HistoryEvent, error) {
	return TrackingHistoryWithMemoryOwners(ref, remote, local, nil)
}

// TrackingHistoryWithMemoryOwners compares effective owners only when supplied
// from verified immutable memory objects. It never normalizes event payloads;
// same-ID comparisons below remain exact. Nil retains the strict field match.
func TrackingHistoryWithMemoryOwners(ref Ref, remote, local []HistoryEvent, owners map[ContentHash]ContentHash) ([]HistoryEvent, error) {
	conflict := func(reason string) ([]HistoryEvent, error) {
		return nil, fmt.Errorf("%w: tracking branch %q: %s; operation remains queued", ErrSyncConflict, ref.Name, reason)
	}
	if err := ValidateRef(ref); err != nil {
		return nil, err
	}
	if ref.Kind != RefBranch || ref.Target == "" || ref.Symbolic != "" {
		return nil, ErrInvalidRef
	}
	for _, events := range [][]HistoryEvent{remote, local} {
		for _, e := range events {
			if err := ValidateHistoryEvent(e); err != nil {
				return nil, err
			}
			if e.RepoID != ref.RepoID {
				return nil, ErrHashMismatch
			}
		}
	}
	ordered, err := OrderHistoryEvents(remote)
	if err != nil {
		return conflict(err.Error())
	}
	observed, err := ProjectContextBranches(ordered)
	if err != nil {
		return conflict(err.Error())
	}
	identity := ref.BranchID
	if identity == "" {
		identity = LegacyContextBranchID(ref.RepoID, ref.Name)
	}
	active, modern := observed.Active[ref.Name]
	if modern && active.ID != identity {
		return conflict("server ref and history disagree on identity")
	}
	if !modern && (observed.Released[ref.Name] != "" || identity != LegacyContextBranchID(ref.RepoID, ref.Name)) {
		return conflict("server identity has no active lifecycle proof")
	}
	// Ordering all events detects conflicting ordinary observations too;
	// ProjectContextBranches alone intentionally ignores non-lifecycle events.
	combined, err := OrderHistoryEvents(append(append([]HistoryEvent{}, ordered...), local...))
	if err != nil {
		return conflict(err.Error())
	}
	merged, err := ProjectContextBranches(combined)
	if err != nil {
		return conflict(err.Error())
	}
	current, activeNow := merged.Active[ref.Name]
	if (modern && (!activeNow || current.ID != identity)) || (!modern && (merged.Identity(ref.RepoID, ref.Name) != identity || merged.Released[ref.Name] != "")) {
		return conflict("local lifecycle conflicts with the server observation")
	}
	byID := make(map[string]HistoryEvent, len(ordered))
	type pin struct {
		code                  string
		target, memory, owner ContentHash
	}
	pinOf := func(e HistoryEvent) (pin, error) {
		owner := e.MemorySource
		if owners != nil && e.MemoryHash != "" {
			owner = owners[e.MemoryHash]
			if ValidateContentHash(owner) != nil || (e.MemorySource != "" && e.MemorySource != owner) || (owner != e.Source && owner != e.Target && owner != e.MemorySource) {
				return pin{}, ErrHashMismatch
			}
		}
		return pin{e.GitAfter, e.Target, e.MemoryHash, owner}, nil
	}
	knownPins := make(map[pin]bool)
	for _, e := range ordered {
		byID[e.ID] = e
		if e.BranchID == identity && e.MemoryPinned && e.Kind != "publish" && e.Kind != "pr-merge" {
			p, err := pinOf(e)
			if err != nil {
				return nil, err
			}
			knownPins[p] = true
		}
	}
	for _, e := range local {
		if e.BranchID != identity || IsBranchBindingEvent(e) || e.Kind == "publish" {
			continue
		}
		if _, known := byID[e.ID]; known {
			continue
		}
		// A local, unpublished selection must not be silently replaced by a
		// remote timestamp. Identical position evidence is safe to retain.
		p, err := pinOf(e)
		if err != nil {
			return nil, err
		}
		if !e.MemoryPinned || !knownPins[p] {
			return conflict("unpublished local context or memory differs from the server observation")
		}
	}
	return ordered, nil
}

// TrackingAttachment is frozen before application. Code is the recorded Git
// ancestor selected for Event.GitAfter, not a timestamp-based approximation.
type TrackingAttachment struct {
	Event       HistoryEvent   `json:"event"`
	ObservedRef Ref            `json:"observed_ref"`
	Proof       []HistoryEvent `json:"proof"`
	Code        string         `json:"code"`
	// Frozen from the verified observation; replay must not consult local grafts.
	Rewound bool `json:"rewound"`
}

func ValidateTrackingAttachment(a TrackingAttachment) error {
	e, r := a.Event, a.ObservedRef
	if err := ValidateHistoryEvent(e); err != nil {
		return err
	}
	if err := ValidateRef(r); err != nil {
		return err
	}
	identity := r.BranchID
	if identity == "" {
		identity = LegacyContextBranchID(r.RepoID, r.Name)
	}
	if e.Kind != "attach" || !e.MemoryPinned || e.Source == "" || e.Source != e.Target || e.SharedTarget != r.Target || e.BranchID != identity || e.Branch != r.Name || e.RepoID != r.RepoID || r.Kind != RefBranch || !ValidGitOID(a.Code) || (e.Target == r.Target && a.Rewound) {
		return ErrHashMismatch
	}
	closure, err := TrackingProof(r, a.Proof, a.Code, a.Event.Target)
	if err != nil {
		return err
	}
	ordered, err := OrderHistoryEvents(a.Proof)
	if err != nil {
		return err
	}
	if !reflect.DeepEqual(closure, ordered) {
		return fmt.Errorf("%w: tracking proof contains unrelated history", ErrHashMismatch)
	}
	matches := func(p HistoryEvent) bool {
		if !p.MemoryPinned || p.Target != e.Target || p.MemoryHash != e.MemoryHash {
			return false
		}
		if p.MemorySource == e.MemorySource || (p.MemorySource == "" && p.Source == e.MemorySource) {
			return true
		}
		// The same nonempty pin may spell its verified self-owner explicitly.
		// Compare copies; application/store validation still checks actual bytes
		// and every raw witness's explicit owner before accepting the command.
		return e.MemoryHash != "" && ((p.MemorySource == "" && e.MemorySource == e.Target) || (p.MemorySource == p.Target && e.MemorySource == ""))
	}
	type sourceKey struct{ identity, code string }
	pins := map[sourceKey]bool{}
	for _, p := range a.Proof {
		if !IsPinnedContextEvent(p) || !matches(p) {
			continue
		}
		if p.BranchID == identity && p.GitAfter == a.Code {
			return nil
		}
		pins[sourceKey{p.BranchID, p.GitAfter}] = true
	}
	for _, receipt := range a.Proof {
		if receipt.Kind == "pr-merge" && receipt.PRCompleted && receipt.PR != nil && receipt.BranchID == identity && receipt.PR.MergeSHA == a.Code && receipt.Source == e.Target && pins[sourceKey{receipt.SourceBranchID, receipt.PR.HeadSHA}] {
			return nil
		}
	}
	return fmt.Errorf("%w: tracking attachment has no exact pinned source proof", ErrSyncConflict)
}

func ValidateTrackingAttachmentCompatibilityWithMemoryOwners(local []HistoryEvent, a TrackingAttachment, owners map[ContentHash]ContentHash) error {
	if err := ValidateTrackingAttachment(a); err != nil {
		return err
	}
	// Partial replay may already contain the identical attach event. It is not
	// remote evidence and cannot replace its own frozen proof.
	filtered := make([]HistoryEvent, 0, len(local))
	for _, e := range local {
		if e.ID == a.Event.ID {
			if !reflect.DeepEqual(e, a.Event) {
				return ErrHashMismatch
			}
			continue
		}
		filtered = append(filtered, e)
	}
	_, err := TrackingHistoryWithMemoryOwners(a.ObservedRef, a.Proof, filtered, owners)
	return err
}

// TrackingProof retains only this identity's observations and the lifecycle
// and the selected code point’s completed-PR source dependencies. Other branches stay
// observations; fetching a branch is not repository-wide history adoption.
func TrackingProof(ref Ref, events []HistoryEvent, code string, target ContentHash) ([]HistoryEvent, error) {
	ordered, err := TrackingHistory(ref, events, nil)
	if err != nil {
		return nil, err
	}
	identity := ref.BranchID
	if identity == "" {
		identity = LegacyContextBranchID(ref.RepoID, ref.Name)
	}
	wanted := map[string]bool{}
	type sourceKey struct {
		identity, code string
		target         ContentHash
	}
	sources := map[sourceKey][]string{}
	for _, e := range ordered {
		if e.BranchID == identity {
			wanted[e.ID] = true
		}
		if IsPinnedContextEvent(e) {
			key := sourceKey{e.BranchID, e.GitAfter, e.Target}
			sources[key] = append(sources[key], e.ID)
		}
	}
	for _, e := range ordered {
		if e.BranchID != identity || e.Kind != "pr-merge" || !e.PRCompleted || e.PR == nil || e.PR.MergeSHA != code || e.Source != target {
			continue
		}
		for _, id := range sources[sourceKey{e.SourceBranchID, e.PR.HeadSHA, e.Source}] {
			wanted[id] = true
		}
	}
	return trackingHistoryClosure(ordered, wanted)
}

// AppliedTrackingProof excludes source witnesses from branch-state adoption.
// A foreign lifecycle is applied only when R explicitly depends on it.
func AppliedTrackingProof(a TrackingAttachment) ([]HistoryEvent, error) {
	ordered, err := OrderHistoryEvents(a.Proof)
	if err != nil {
		return nil, err
	}
	wanted := map[string]bool{}
	for _, e := range ordered {
		if e.BranchID == a.Event.BranchID {
			wanted[e.ID] = true
		}
	}
	return trackingHistoryClosure(ordered, wanted)
}

func trackingHistoryClosure(ordered []HistoryEvent, wanted map[string]bool) ([]HistoryEvent, error) {
	byID := map[string]HistoryEvent{}
	for _, e := range ordered {
		byID[e.ID] = e
	}
	queue := make([]string, 0, len(wanted))
	for id := range wanted {
		queue = append(queue, id)
	}
	for len(queue) > 0 {
		id := queue[len(queue)-1]
		queue = queue[:len(queue)-1]
		e := byID[id]
		for _, dep := range HistoryDependencies(e) {
			if dep != "" && !wanted[dep] {
				if _, ok := byID[dep]; !ok {
					return nil, ErrHashMismatch
				}
				wanted[dep] = true
				queue = append(queue, dep)
			}
		}
	}
	out := make([]HistoryEvent, 0, len(wanted))
	for _, e := range ordered {
		if wanted[e.ID] {
			out = append(out, e)
		}
	}
	return out, nil
}
