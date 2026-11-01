package repositoryquery

import (
	"strings"
	"unsafe"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

const ProjectionRetentionBytes = GraphProjectionCacheBytes

type projectionCacheCopy struct {
	bytes int
	owned bool
	full  bool
}

func (copying *projectionCacheCopy) add(count int, width uintptr) {
	if copying.full || count < 0 || width > ProjectionRetentionBytes || count > (ProjectionRetentionBytes-copying.bytes)/max(1, int(width)) {
		copying.full = true
		return
	}
	copying.bytes += count * int(width)
}

func (copying *projectionCacheCopy) text(value string) string {
	if value == "" {
		return value
	}
	copying.add(1, 32)
	copying.add(len(value), 2)
	if copying.owned && !copying.full {
		return strings.Clone(value)
	}
	return value
}

func (copying *projectionCacheCopy) texts(fields ...*string) {
	for _, field := range fields {
		*field = copying.text(*field)
	}
}

func (copying *projectionCacheCopy) hashes(fields ...*domain.ContentHash) {
	for _, field := range fields {
		*field = domain.ContentHash(copying.text(string(*field)))
	}
}

func (copying *projectionCacheCopy) array(count int, width uintptr) {
	copying.add(1, 64)
	copying.add(count, 2*width)
}

func (copying *projectionCacheCopy) stringSlice(input []string) []string {
	if input == nil {
		return nil
	}
	copying.array(len(input), unsafe.Sizeof(string("")))
	output := input
	if copying.owned && !copying.full {
		output = make([]string, len(input))
	}
	for index, value := range input {
		value = copying.text(value)
		if copying.full {
			break
		}
		if copying.owned {
			output[index] = value
		}
	}
	return output
}

func (copying *projectionCacheCopy) hashSlice(input []domain.ContentHash) []domain.ContentHash {
	if input == nil {
		return nil
	}
	copying.array(len(input), unsafe.Sizeof(domain.ContentHash("")))
	output := input
	if copying.owned && !copying.full {
		output = make([]domain.ContentHash, len(input))
	}
	for index, value := range input {
		value = domain.ContentHash(copying.text(string(value)))
		if copying.full {
			break
		}
		if copying.owned {
			output[index] = value
		}
	}
	return output
}

func (copying *projectionCacheCopy) checkpoint(value domain.RepositoryViewSourceCheckpoint) domain.RepositoryViewSourceCheckpoint {
	copying.texts(&value.Scope, &value.Epoch)
	copying.hashes(&value.RepoID)
	return value
}

func (copying *projectionCacheCopy) graphKey(value GraphProjectionKey) GraphProjectionKey {
	copying.add(1, unsafe.Sizeof(value)+64)
	copying.hashes(&value.Repo)
	copying.texts(&value.Position, &value.Origin)
	value.Source = copying.checkpoint(value.Source)
	return value
}

func (copying *projectionCacheCopy) branchKey(value BranchProjectionKey) BranchProjectionKey {
	copying.add(1, unsafe.Sizeof(value)+64)
	copying.hashes(&value.Repo)
	copying.texts(&value.Origin)
	value.Generation = copying.checkpoint(value.Generation)
	return value
}

func (copying *projectionCacheCopy) segmentKey(value contextSegmentCacheKey) contextSegmentCacheKey {
	copying.add(1, unsafe.Sizeof(value)+64)
	copying.hashes(&value.repo)
	copying.texts(&value.branch, &value.position, &value.code, &value.scope, &value.defaultBranch, &value.origin)
	value.source = copying.checkpoint(value.source)
	return value
}

func (copying *projectionCacheCopy) branch(value domain.BranchContext) domain.BranchContext {
	copying.add(1, unsafe.Sizeof(value))
	copying.texts(&value.BranchID, &value.CodeCommit, &value.Reason)
	copying.hashes(&value.SnapshotID)
	value.SnapshotIDs = copying.hashSlice(value.SnapshotIDs)
	value.Roots = copying.hashSlice(value.Roots)
	merges := value.Merges
	if merges != nil {
		copying.array(len(merges), unsafe.Sizeof(domain.BranchContextMerge{}))
		if copying.owned && !copying.full {
			value.Merges = make([]domain.BranchContextMerge, len(merges))
		}
	}
	for index, merge := range merges {
		copying.texts(&merge.DestinationBranchID, &merge.EventID, &merge.MergeSHA, &merge.State, &merge.Reason)
		copying.hashes(&merge.Source, &merge.Before)
		if copying.full {
			break
		}
		if copying.owned {
			value.Merges[index] = merge
		}
	}
	return value
}

func (copying *projectionCacheCopy) branches(input map[string]domain.BranchContext) map[string]domain.BranchContext {
	if input == nil {
		return nil
	}
	copying.array(len(input), unsafe.Sizeof(domain.BranchContext{})+128)
	output := input
	if copying.owned && !copying.full {
		output = make(map[string]domain.BranchContext, len(input))
	}
	for name, branch := range input {
		name, branch = copying.text(name), copying.branch(branch)
		if copying.full {
			break
		}
		if copying.owned {
			output[name] = branch
		}
	}
	return output
}

func (copying *projectionCacheCopy) snapshot(value domain.Snapshot) domain.Snapshot {
	copying.add(1, unsafe.Sizeof(value))
	copying.hashes(&value.ID, &value.RepoID, &value.DocHash, &value.MemoryHash, &value.ClaudeSettings, &value.AgentsSettings, &value.CodexSettings)
	copying.texts(&value.Branch, &value.Message, &value.SessionID, &value.Author.Name, &value.Author.Email, &value.Author.Team)
	value.DocIdentity = domain.DocumentIdentity(copying.text(string(value.DocIdentity)))
	value.Provider = domain.ProviderKind(copying.text(string(value.Provider)))
	value.Fidelity = domain.FidelityTier(copying.text(string(value.Fidelity)))
	value.Branches, value.Models = copying.stringSlice(value.Branches), copying.stringSlice(value.Models)
	value.Parents, value.GraftParents = copying.hashSlice(value.Parents), copying.hashSlice(value.GraftParents)
	return value
}

func (copying *projectionCacheCopy) history(value domain.HistoryEvent) domain.HistoryEvent {
	copying.add(1, unsafe.Sizeof(value))
	copying.texts(&value.RecoveryEvidence, &value.SourceBranchID, &value.ID, &value.RepoID, &value.BranchID, &value.Branch, &value.Kind, &value.LocalBranch, &value.PreviousBranch, &value.BindingParent, &value.NameParent, &value.MemorySelectionParent, &value.GitBefore, &value.GitAfter, &value.WorktreeID)
	copying.hashes(&value.Source, &value.Target, &value.SharedTarget, &value.MemorySource, &value.MemoryHash)
	if value.Creation != nil {
		creation := *value.Creation
		copying.add(1, unsafe.Sizeof(creation)+32)
		copying.texts(&creation.Evidence, &creation.StartRef, &creation.StartCommit, &creation.OriginBranch, &creation.OriginBranchID)
		creation.Command = copying.stringSlice(creation.Command)
		if copying.owned && !copying.full {
			value.Creation = &creation
		}
	}
	if value.PR != nil {
		merge := *value.PR
		copying.add(1, unsafe.Sizeof(merge)+32)
		copying.texts(&merge.BaseBranch, &merge.HeadBranch, &merge.HeadSHA, &merge.MergeSHA)
		if copying.owned && !copying.full {
			value.PR = &merge
		}
	}
	return value
}

func (copying *projectionCacheCopy) bindings(input []domain.CommitContextBinding) []domain.CommitContextBinding {
	if input == nil {
		return nil
	}
	copying.array(len(input), unsafe.Sizeof(domain.CommitContextBinding{}))
	output := input
	if copying.owned && !copying.full {
		output = make([]domain.CommitContextBinding, len(input))
	}
	for index, binding := range input {
		copying.texts(&binding.EventID, &binding.GitCommit, &binding.BranchID, &binding.WorktreeID)
		binding.SegmentIDs = copying.hashSlice(binding.SegmentIDs)
		if copying.full {
			break
		}
		if copying.owned {
			output[index] = binding
		}
	}
	return output
}

func cloneContextBindings(input []domain.CommitContextBinding) []domain.CommitContextBinding {
	if input == nil {
		return nil
	}
	output := make([]domain.CommitContextBinding, len(input))
	for index, binding := range input {
		binding.EventID, binding.GitCommit = strings.Clone(binding.EventID), strings.Clone(binding.GitCommit)
		binding.BranchID, binding.WorktreeID = strings.Clone(binding.BranchID), strings.Clone(binding.WorktreeID)
		if binding.SegmentIDs != nil {
			segments := make([]domain.ContentHash, len(binding.SegmentIDs))
			for position, hash := range binding.SegmentIDs {
				segments[position] = domain.ContentHash(strings.Clone(string(hash)))
			}
			binding.SegmentIDs = segments
		}
		output[index] = binding
	}
	return output
}

func (copying *projectionCacheCopy) basis(value contextSegmentBasis) contextSegmentBasis {
	copying.add(1, unsafe.Sizeof(value)+unsafe.Sizeof(value.view))
	copying.texts(&value.view.Branch)
	copying.hashes(&value.view.DeliveryStateHash, &value.view.StateHash, &value.view.Position)
	if value.view.Segments != nil {
		copying.full = true
		return value
	}
	if value.view.Inclusion != nil {
		inclusion := copying.branch(*value.view.Inclusion)
		if copying.owned && !copying.full {
			value.view.Inclusion = &inclusion
		}
	}
	merges := value.view.Semantics.Merges
	if merges != nil {
		copying.array(len(merges), unsafe.Sizeof(domain.ContextMergeEvidence{}))
		if copying.owned && !copying.full {
			value.view.Semantics.Merges = make([]domain.ContextMergeEvidence, len(merges))
		}
	}
	for index, merge := range merges {
		copying.texts(&merge.EventID, &merge.BirthID, &merge.Lineage)
		if copying.full {
			return value
		}
		if copying.owned {
			value.view.Semantics.Merges[index] = merge
		}
	}
	snapshots := value.view.Snapshots
	if snapshots != nil {
		copying.array(len(snapshots), unsafe.Sizeof(domain.Snapshot{}))
		if copying.owned && !copying.full {
			value.view.Snapshots = make([]domain.Snapshot, len(snapshots))
		}
	}
	for index, snapshot := range snapshots {
		snapshot = copying.snapshot(snapshot)
		if copying.full {
			return value
		}
		if copying.owned {
			value.view.Snapshots[index] = snapshot
		}
	}
	history := value.view.History
	if history != nil {
		copying.array(len(history), unsafe.Sizeof(domain.HistoryEvent{}))
		if copying.owned && !copying.full {
			value.view.History = make([]domain.HistoryEvent, len(history))
		}
	}
	for index, event := range history {
		event = copying.history(event)
		if copying.full {
			return value
		}
		if copying.owned {
			value.view.History[index] = event
		}
	}
	selected := value.selected
	if selected != nil {
		copying.array(len(selected), unsafe.Sizeof(domain.Snapshot{})+128)
		if copying.owned && !copying.full {
			value.selected = make(map[domain.ContentHash]domain.Snapshot, len(selected))
		}
	}
	for hash, snapshot := range selected {
		hash = domain.ContentHash(copying.text(string(hash)))
		snapshot = copying.snapshot(snapshot)
		if copying.full {
			return value
		}
		if copying.owned {
			value.selected[hash] = snapshot
		}
	}
	bindings := value.bindings
	if bindings != nil {
		copying.array(len(bindings), unsafe.Sizeof([]domain.CommitContextBinding{})+128)
		if copying.owned && !copying.full {
			value.bindings = make(map[domain.ContentHash][]domain.CommitContextBinding, len(bindings))
		}
	}
	for hash, list := range bindings {
		hash = domain.ContentHash(copying.text(string(hash)))
		list = copying.bindings(list)
		if copying.full {
			return value
		}
		if copying.owned {
			value.bindings[hash] = list
		}
	}
	return value
}
