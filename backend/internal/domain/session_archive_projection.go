package domain

import (
	"reflect"
	"sort"
)

type sessionArchiveRepository struct {
	snapshots    map[ContentHash]Snapshot
	conflicts    map[ContentHash]bool
	groups       map[ContentHash][]Snapshot
	history      []HistoryEvent
	publications map[[6]string]HistoryEvent
	births       map[string][]HistoryEvent
}

func ProjectSessionArchives(records []SessionArchive, snapshots []Snapshot, history []HistoryEvent, defaultBranch string) []SessionArchiveView {
	repositories := map[ContentHash]*sessionArchiveRepository{}
	for _, record := range records {
		if repositories[record.RepoID] == nil {
			repositories[record.RepoID] = &sessionArchiveRepository{
				snapshots: map[ContentHash]Snapshot{},
				conflicts: map[ContentHash]bool{},
				groups:    map[ContentHash][]Snapshot{},
			}
		}
	}
	for _, snapshot := range snapshots {
		if repository := repositories[snapshot.RepoID]; repository != nil {
			if snapshot.ID == "" || repository.conflicts[snapshot.ID] {
				continue
			}
			if previous, exists := repository.snapshots[snapshot.ID]; exists && !reflect.DeepEqual(previous, snapshot) {
				repository.conflicts[snapshot.ID] = true
				delete(repository.snapshots, snapshot.ID)
				continue
			}
			repository.snapshots[snapshot.ID] = snapshot
		}
	}
	for _, repository := range repositories {
		for _, snapshot := range repository.snapshots {
			key := SessionArchiveKey(snapshot)
			repository.groups[key] = append(repository.groups[key], snapshot)
		}
	}
	for _, event := range history {
		if repository := repositories[ContentHash(event.RepoID)]; repository != nil {
			repository.history = append(repository.history, event)
		}
	}
	views := make([]SessionArchiveView, 0, len(records))
	for _, record := range records {
		repository := repositories[record.RepoID]
		members := repository.groups[record.Key]
		anchor, anchorExists := repository.snapshots[record.SnapshotID]
		anchorMatches := anchorExists && SessionArchiveKey(anchor) == record.Key
		if anchorExists && !anchorMatches {
			members = append(append([]Snapshot{}, members...), anchor)
		}
		view := SessionArchiveView{SessionArchive: record, SnapshotIDs: []ContentHash{}, Origin: SessionOrigin{MainBranch: defaultBranch}}
		latest := sessionArchiveLatestCapture(members)
		for _, snapshot := range members {
			view.SnapshotIDs = append(view.SnapshotIDs, snapshot.ID)
		}
		sort.Slice(view.SnapshotIDs, func(left, right int) bool { return view.SnapshotIDs[left] < view.SnapshotIDs[right] })
		view.LatestSnapshotID, view.Message, view.Branch = latest.ID, latest.Message, latest.Branch
		view.Author, view.UpdatedAt = latest.Author, latest.CreatedAt
		if anchorMatches {
			if parent, known := sessionArchiveParent(members, repository.snapshots); known {
				view.Origin.ParentSnapshotID = parent.ID
				view.Origin.ParentSessionID = parent.SessionID
				view.Origin.ParentProvider = parent.Provider
			}
			repository.projectMain(&view, defaultBranch)
		}
		views = append(views, view)
	}
	sort.Slice(views, func(left, right int) bool {
		if !views[left].ArchivedAt.Equal(views[right].ArchivedAt) {
			return views[left].ArchivedAt.After(views[right].ArchivedAt)
		}
		if views[left].Key != views[right].Key {
			return views[left].Key < views[right].Key
		}
		return views[left].RepoID < views[right].RepoID
	})
	return views
}

func sessionArchiveLatestCapture(members []Snapshot) Snapshot {
	nonterminal := map[ContentHash]bool{}
	for _, snapshot := range members {
		for _, parent := range snapshot.Parents {
			nonterminal[parent] = true
		}
	}
	var latest Snapshot
	for _, snapshot := range members {
		terminal, latestTerminal := !nonterminal[snapshot.ID], !nonterminal[latest.ID]
		newer := snapshot.CreatedAt.After(latest.CreatedAt) || (snapshot.CreatedAt.Equal(latest.CreatedAt) && snapshot.ID < latest.ID)
		if latest.ID == "" || (terminal && !latestTerminal) || (terminal == latestTerminal && newer) {
			latest = snapshot
		}
	}
	return latest
}

func sessionArchiveNaturalParents(snapshot Snapshot) ([]ContentHash, bool) {
	if snapshot.Grafted || snapshot.GraftSeq != 0 || len(snapshot.GraftParents) != 0 {
		return nil, false
	}
	return snapshot.Parents, true
}

func sessionArchiveParent(members []Snapshot, snapshots map[ContentHash]Snapshot) (Snapshot, bool) {
	inside := map[ContentHash]bool{}
	for _, snapshot := range members {
		inside[snapshot.ID] = true
	}
	boundaries := map[ContentHash]bool{}
	indegree := map[ContentHash]int{}
	children := map[ContentHash][]ContentHash{}
	root := false
	for _, snapshot := range members {
		parents, known := sessionArchiveNaturalParents(snapshot)
		if !known {
			return Snapshot{}, false
		}
		if len(parents) == 0 {
			root = true
		}
		seen := map[ContentHash]bool{}
		for _, parent := range parents {
			if seen[parent] {
				continue
			}
			seen[parent] = true
			if _, exists := snapshots[parent]; !exists {
				return Snapshot{}, false
			}
			if inside[parent] {
				indegree[snapshot.ID]++
				children[parent] = append(children[parent], snapshot.ID)
			} else {
				boundaries[parent] = true
			}
		}
	}
	queue := []ContentHash{}
	for id := range inside {
		if indegree[id] == 0 {
			queue = append(queue, id)
		}
	}
	visited := 0
	for len(queue) > 0 {
		id := queue[len(queue)-1]
		queue = queue[:len(queue)-1]
		visited++
		for _, child := range children[id] {
			indegree[child]--
			if indegree[child] == 0 {
				queue = append(queue, child)
			}
		}
	}
	if visited != len(inside) || root || len(boundaries) != 1 {
		return Snapshot{}, false
	}
	for id := range boundaries {
		return snapshots[id], true
	}
	return Snapshot{}, false
}

func sessionArchiveObservationKey(event HistoryEvent) [6]string {
	return [6]string{event.BranchID, event.Branch, event.LocalBranch, event.WorktreeID, event.GitAfter, string(event.Target)}
}

func (repository *sessionArchiveRepository) prepareHistory() {
	ordered, err := OrderHistoryEvents(repository.history)
	repository.history = nil
	if err != nil {
		return
	}
	for _, event := range ordered {
		if ValidateHistoryEvent(event) == nil {
			repository.history = append(repository.history, event)
		}
	}
	if _, err := ProjectContextBranches(repository.history); err != nil {
		repository.history = nil
		return
	}
	observations := map[[6]string]HistoryEvent{}
	repository.births = map[string][]HistoryEvent{}
	for _, event := range repository.history {
		if event.Kind != "publish" && event.Kind != "pr-merge" {
			observations[sessionArchiveObservationKey(event)] = event
		}
		if event.Kind == "birth" || event.Kind == "orphan" {
			repository.births[event.BranchID] = append(repository.births[event.BranchID], event)
		}
	}
	repository.publications = map[[6]string]HistoryEvent{}
	for _, event := range repository.history {
		key := sessionArchiveObservationKey(event)
		if event.Kind == "publish" && IsPublicationProof(event, observations[key]) {
			repository.publications[key] = event
		}
	}
}

func (repository *sessionArchiveRepository) projectMain(view *SessionArchiveView, mainBranch string) {
	if mainBranch == "" {
		return
	}
	eligible := sessionArchiveRepository{}
	for _, event := range repository.history {
		if !event.CreatedAt.After(view.ArchivedAt) {
			eligible.history = append(eligible.history, event)
		}
	}
	eligible.prepareHistory()
	type mainPosition struct {
		snapshot ContentHash
		commit   string
	}
	positions := map[mainPosition]bool{}
	for _, event := range eligible.history {
		if event.Target != view.SnapshotID {
			continue
		}
		if event.Kind == "publish" && event.Branch == mainBranch && ValidateGitOID(event.GitAfter) == nil {
			if _, proven := eligible.publications[sessionArchiveObservationKey(event)]; proven {
				positions[mainPosition{event.Target, event.GitAfter}] = true
			}
		}
		if event.Kind != "position" && event.Kind != "advance" {
			continue
		}
		births := eligible.births[event.BranchID]
		if len(births) != 1 || view.Origin.ParentSnapshotID == "" {
			continue
		}
		birth := births[0]
		creation := birth.Creation
		if birth.Kind != "birth" || birth.Source != view.Origin.ParentSnapshotID || birth.Target != birth.Source || creation == nil || creation.Evidence != "process-argv" || creation.OriginBranch != mainBranch || ValidateGitOID(creation.StartCommit) != nil || ValidateCreationOrigin(eligible.history, birth) != nil {
			continue
		}
		positions[mainPosition{birth.Source, creation.StartCommit}] = true
	}
	if len(positions) != 1 {
		return
	}
	for position := range positions {
		view.Origin.MainSnapshotID = position.snapshot
		view.Origin.MainGitCommit = position.commit
		view.Origin.MainBranch = mainBranch
	}
}
