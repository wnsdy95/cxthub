package domain

func ProjectPendingView(base RepositoryView) (PendingView, error) {
	view := PendingView{Revision: base.Revision, Pending: base.Pending,
		ArchivedSessions: base.ArchivedSessions, Graph: base.Graph, ReadGeneration: base.ReadGeneration}
	byID := make(map[ContentHash]Snapshot, len(base.Snapshots))
	for _, snapshot := range base.Snapshots {
		byID[snapshot.ID] = snapshot
	}
	seen := map[ContentHash]bool{}
	for _, pending := range view.Pending {
		if seen[pending.Target] {
			continue
		}
		snapshot, ok := byID[pending.Target]
		if !ok {
			return PendingView{}, ErrNotFound
		}
		snapshot.Branches = nil
		view.Snapshots = append(view.Snapshots, snapshot)
		seen[pending.Target] = true
	}
	if view.Snapshots == nil {
		view.Snapshots = []Snapshot{}
	}
	return view, nil
}
