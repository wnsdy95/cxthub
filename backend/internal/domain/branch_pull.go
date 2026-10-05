package domain

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"sort"
)

var ErrBranchPullUnsupported = errors.New("branch pull planning unsupported by this storage adapter")

const BranchPullVersion = 1
const MaxBranchPullRoots = 256

// BranchPullRequest selects evidence, not a local adoption or a branch ACL.
type BranchPullRequest struct {
	Version          int           `json:"version"`
	Branch           string        `json:"branch"`
	ObservationRoots []ContentHash `json:"observation_roots,omitempty"`
}

func (r BranchPullRequest) Validate() error {
	if r.Version != BranchPullVersion || ValidateBranchName(r.Branch) != nil || len(r.ObservationRoots) > MaxBranchPullRoots {
		return fmt.Errorf("%w: invalid branch pull request", ErrValidation)
	}
	for _, id := range r.ObservationRoots {
		if ValidateContentHash(id) != nil {
			return fmt.Errorf("%w: invalid observation root", ErrValidation)
		}
	}
	return nil
}

// BranchPullPlan describes complete snapshot/history dependencies and frozen
// memory roots. State tokens verify subsequent metadata reads. Clients validate
// immutable memory owners and ancestry before observation CAS, including warm
// plans. This response neither prevalidates memory bodies nor leases objects.
type BranchPullPlan struct {
	Version         int                         `json:"version"`
	RepoID          ContentHash                 `json:"repo_id"`
	Branch          string                      `json:"branch"`
	ContextProtocol int                         `json:"context_protocol"`
	SelectedRef     Ref                         `json:"selected_ref"`
	Refs            []Ref                       `json:"refs"`
	History         []HistoryEvent              `json:"history"`
	SnapshotIndex   []ContentHash               `json:"snapshot_index"`
	SnapshotStates  map[ContentHash]ContentHash `json:"snapshot_states"`
	AbsentRoots     []ContentHash               `json:"absent_roots"`
	SettingsObjects []BranchPullSettings        `json:"settings_objects"`
}

type BranchPullSettings struct {
	Kind string      `json:"kind"`
	Hash ContentHash `json:"hash"`
}

// SelectBranchPullDependencies is pure: no provider, document or storage reads.
// All code observations of the selected identity are retained because the local
// Git ancestry to be selected is not known by this transfer operation.
func SelectBranchPullDependencies(ctx context.Context, repo Repo, request BranchPullRequest, refs []Ref, history []HistoryEvent, snapshots []Snapshot) (BranchPullPlan, error) {
	var zero BranchPullPlan
	if err := request.Validate(); err != nil {
		return zero, err
	}
	if ValidateContentHash(repo.ID) != nil || (repo.ContextProtocol != 0 && repo.ContextProtocol != 1) {
		return zero, ErrIntegrity
	}
	out := BranchPullPlan{Version: BranchPullVersion, RepoID: repo.ID, Branch: request.Branch, ContextProtocol: repo.ContextProtocol,
		Refs: []Ref{}, History: []HistoryEvent{}, SnapshotIndex: []ContentHash{}, SnapshotStates: map[ContentHash]ContentHash{},
		AbsentRoots: []ContentHash{}, SettingsObjects: []BranchPullSettings{}}
	for _, ref := range refs {
		if ref.RepoID != repo.ID || ValidateRef(ref) != nil {
			return zero, ErrIntegrity
		}
		if ref.Kind == RefBranch && ref.Name == request.Branch {
			if out.SelectedRef.Name != "" {
				return zero, ErrIntegrity
			}
			out.SelectedRef = ref
		}
		e, lifecycle, err := ParseBranchLifecycleRef(ref)
		if err != nil {
			return zero, ErrIntegrity
		}
		if lifecycle && e.Branch == request.Branch {
			out.Refs = append(out.Refs, ref)
		}
	}
	latest, hasLifecycle, err := LatestBranchLifecycle(out.Refs, request.Branch)
	if err != nil {
		return zero, ErrIntegrity
	}
	if out.SelectedRef.Name == "" || out.SelectedRef.Target == "" {
		if hasLifecycle && latest.State == BranchArchived {
			return zero, ErrBranchArchived
		}
		return zero, ErrNotFound
	}
	if out.SelectedRef.Symbolic != "" {
		return zero, ErrIntegrity
	}
	if out.SelectedRef.BranchID == "" && hasLifecycle && latest.State == BranchArchived && latest.Target == out.SelectedRef.Target {
		return zero, ErrBranchArchived
	}
	out.Refs = append(out.Refs, out.SelectedRef)
	sort.Slice(out.Refs, func(i, j int) bool { return out.Refs[i].Name < out.Refs[j].Name })
	for _, e := range history {
		if err := ctx.Err(); err != nil {
			return zero, err
		}
		if e.RepoID != string(repo.ID) || ValidateHistoryEvent(e) != nil {
			return zero, ErrIntegrity
		}
	}
	ordered, err := OrderHistoryEvents(history)
	if err != nil {
		return zero, fmt.Errorf("%w: %v", ErrIntegrity, err)
	}
	projection, err := ProjectContextBranches(ordered)
	if err != nil {
		return zero, fmt.Errorf("%w: %v", ErrIntegrity, err)
	}
	identity := out.SelectedRef.BranchID
	if identity == "" {
		identity = LegacyContextBranchID(string(repo.ID), request.Branch)
	}
	active, modern := projection.Active[request.Branch]
	if modern && active.ID != identity {
		return zero, ErrRefConflict
	}
	if !modern && (projection.Released[request.Branch] != "" || identity != LegacyContextBranchID(string(repo.ID), request.Branch)) {
		return zero, ErrRefConflict
	}

	wanted := map[string]bool{}
	byEvent := map[string]HistoryEvent{}
	type sourcePin struct {
		identity, code string
		target         ContentHash
	}
	sources := map[sourcePin][]string{}
	for _, e := range ordered {
		byEvent[e.ID] = e
		if e.BranchID == identity || (IsBranchBindingEvent(e) && (e.Branch == request.Branch || e.PreviousBranch == request.Branch)) {
			wanted[e.ID] = true
		}
		if branchPullPinnedEvent(e) {
			key := sourcePin{e.BranchID, e.GitAfter, e.Target}
			sources[key] = append(sources[key], e.ID)
		}
	}
	for _, e := range ordered {
		if e.BranchID != identity || e.Kind != "pr-merge" || !e.PRCompleted {
			continue
		}
		pins := sources[sourcePin{e.SourceBranchID, e.PR.HeadSHA, e.Source}]
		if len(pins) == 0 {
			return zero, fmt.Errorf("%w: completed PR source pin missing", ErrIntegrity)
		}
		for _, id := range pins {
			wanted[id] = true
		}
	}
	queue := make([]string, 0, len(wanted))
	for id := range wanted {
		queue = append(queue, id)
	}
	for len(queue) > 0 {
		if err := ctx.Err(); err != nil {
			return zero, err
		}
		id := queue[len(queue)-1]
		queue = queue[:len(queue)-1]
		for _, dep := range []string{byEvent[id].BindingParent, byEvent[id].NameParent} {
			if dep != "" && !wanted[dep] {
				wanted[dep] = true
				queue = append(queue, dep)
			}
		}
	}
	for _, e := range ordered {
		if wanted[e.ID] {
			out.History = append(out.History, e)
		}
	}
	scoped, err := ProjectContextBranches(out.History)
	if err != nil || scoped.Active[request.Branch] != projection.Active[request.Branch] || scoped.Released[request.Branch] != projection.Released[request.Branch] {
		return zero, ErrIntegrity
	}

	byID := make(map[ContentHash]Snapshot, len(snapshots))
	for _, s := range snapshots {
		if s.RepoID != repo.ID || ValidateContentHash(s.ID) != nil {
			return zero, ErrIntegrity
		}
		if previous, ok := byID[s.ID]; ok && !reflect.DeepEqual(previous, s) {
			return zero, ErrIntegrity
		}
		byID[s.ID] = s
	}
	roots := []ContentHash{out.SelectedRef.Target}
	for _, r := range out.Refs {
		roots = append(roots, r.Target)
	}
	for _, e := range out.History {
		roots = append(roots, e.Source, e.Target, e.SharedTarget, e.MemorySource)
	}
	optional := map[ContentHash]bool{}
	for _, id := range request.ObservationRoots {
		if optional[id] {
			continue
		}
		optional[id] = true
		if _, ok := byID[id]; ok {
			roots = append(roots, id)
		} else {
			out.AbsentRoots = append(out.AbsentRoots, id)
		}
	}
	state := map[ContentHash]uint8{}
	settings := map[ContentHash]string{}
	var visit func(ContentHash) error
	visit = func(id ContentHash) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if id == "" || state[id] == 2 {
			return nil
		}
		if state[id] == 1 {
			return fmt.Errorf("%w: cyclic snapshot dependency", ErrIntegrity)
		}
		s, ok := byID[id]
		if !ok || s.DocHash != id || ValidateOptionalContentHash(s.MemoryHash) != nil || s.GraftSeq > MaxGraftSeq {
			return fmt.Errorf("%w: invalid or missing snapshot dependency %s", ErrIntegrity, id)
		}
		state[id] = 1
		for _, p := range s.ReachabilityParents() {
			if ValidateContentHash(p) != nil {
				return ErrIntegrity
			}
			if err := visit(p); err != nil {
				return err
			}
		}
		for _, setting := range []BranchPullSettings{{"claude", s.ClaudeSettings}, {"agents", s.AgentsSettings}, {"codex", s.CodexSettings}} {
			if setting.Hash == "" {
				continue
			}
			if ValidateContentHash(setting.Hash) != nil || (settings[setting.Hash] != "" && settings[setting.Hash] != setting.Kind) {
				return ErrIntegrity
			}
			settings[setting.Hash] = setting.Kind
		}
		token, err := SnapshotStateHash(s)
		if err != nil {
			return err
		}
		out.SnapshotStates[id] = token
		out.SnapshotIndex = append(out.SnapshotIndex, id)
		state[id] = 2
		return nil
	}
	for _, id := range roots {
		if err := visit(id); err != nil {
			return zero, err
		}
	}
	for hash, kind := range settings {
		out.SettingsObjects = append(out.SettingsObjects, BranchPullSettings{Kind: kind, Hash: hash})
	}
	sort.Slice(out.SettingsObjects, func(i, j int) bool { return out.SettingsObjects[i].Hash < out.SettingsObjects[j].Hash })
	sort.Slice(out.SnapshotIndex, func(i, j int) bool { return out.SnapshotIndex[i] < out.SnapshotIndex[j] })
	sort.Slice(out.AbsentRoots, func(i, j int) bool { return out.AbsentRoots[i] < out.AbsentRoots[j] })
	return out, nil
}

// Same ordinary pin eligibility as tracking: rename/archive are lifecycle only.
func branchPullPinnedEvent(e HistoryEvent) bool {
	if !e.MemoryPinned || e.Target == "" {
		return false
	}
	switch e.Kind {
	case "birth", "attach", "orphan", "position", "advance":
		return true
	}
	return false
}
