package domain

import "context"

func ProjectSnapshotBranchMemberships(ctx context.Context, snapshots []Snapshot, reflog []RefLogEntry) (map[ContentHash]map[string]bool, error) {
	members := make(map[ContentHash]map[string]bool, len(snapshots))
	add := func(id ContentHash, branch string) {
		if id == "" || branch == "" || branch == "HEAD" {
			return
		}
		if members[id] == nil {
			members[id] = map[string]bool{}
		}
		members[id][branch] = true
	}
	for _, snapshot := range snapshots {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		add(snapshot.ID, snapshot.Branch)
	}
	for _, entry := range reflog {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if entry.Kind == RefBranch {
			add(entry.Old, entry.Name)
			add(entry.New, entry.Name)
		}
	}
	return members, ctx.Err()
}
