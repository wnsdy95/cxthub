package domain

import "slices"

func (snapshot Snapshot) Clone() Snapshot {
	snapshot.Branches = slices.Clone(snapshot.Branches)
	snapshot.Parents = slices.Clone(snapshot.Parents)
	snapshot.GraftParents = slices.Clone(snapshot.GraftParents)
	snapshot.Models = slices.Clone(snapshot.Models)
	return snapshot
}

func (event HistoryEvent) Clone() HistoryEvent {
	if event.Creation != nil {
		creation := *event.Creation
		creation.Command = slices.Clone(creation.Command)
		event.Creation = &creation
	}
	if event.PR != nil {
		merge := *event.PR
		event.PR = &merge
	}
	return event
}
