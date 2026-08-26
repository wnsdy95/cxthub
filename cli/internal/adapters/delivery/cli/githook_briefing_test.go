package cli

import (
	"fmt"
	"testing"

	"github.com/wnsdy95/cxthub/cli/internal/domain"
)

func briefingHash(label string) domain.ContentHash {
	return domain.HashContent([]byte("pull-briefing-" + label))
}

func TestPullBriefingDeltaUsesCursorAndGraftReachability(t *testing.T) {
	a := briefingHash("local-a")
	x := briefingHash("incoming-root-x")
	b := briefingHash("incoming-b")
	c := briefingHash("incoming-c")
	d := briefingHash("later-d")
	snapshots := []domain.Snapshot{
		{ID: a, Message: "local A"},
		{ID: x, Message: domain.HookMessagePrefix + " transient root"},
		{ID: b, Parents: []domain.ContentHash{x}, GraftParents: []domain.ContentHash{a}, Message: "team B"},
		{ID: c, Parents: []domain.ContentHash{b}, Message: "team C"},
		// The previous cursor C is reachable from D only through an overlay edge.
		{ID: d, Parents: []domain.ContentHash{x}, GraftParents: []domain.ContentHash{c}, Message: "team D"},
	}

	first, complete := pullBriefingDelta(snapshots, c, a, "")
	if !complete || len(first) != 2 || first[0].ID != b || first[1].ID != c {
		t.Fatalf("first delta=%+v complete=%v, want B,C", first, complete)
	}
	repeated, complete := pullBriefingDelta(snapshots, c, a, c)
	if !complete || len(repeated) != 0 {
		t.Fatalf("repeated delta=%+v complete=%v, want empty", repeated, complete)
	}
	later, complete := pullBriefingDelta(snapshots, d, a, c)
	if !complete || len(later) != 1 || later[0].ID != d {
		t.Fatalf("later graft delta=%+v complete=%v, want only D", later, complete)
	}
}

func TestPullBriefingDeltaFallsBackAfterCursorRewrite(t *testing.T) {
	local := briefingHash("rewrite-local")
	staleCursor := briefingHash("rewrite-stale-cursor")
	remote := briefingHash("rewrite-remote")
	snapshots := []domain.Snapshot{
		{ID: local, Message: "local"},
		{ID: staleCursor, Message: "stale cursor"},
		{ID: remote, Parents: []domain.ContentHash{local}, Message: "rewritten team context"},
	}
	delta, complete := pullBriefingDelta(snapshots, remote, local, staleCursor)
	if !complete || len(delta) != 1 || delta[0].ID != remote {
		t.Fatalf("rewrite delta=%+v complete=%v", delta, complete)
	}
}

func TestPullBriefingDeltaDoesNotAdvanceAcrossMissingObjects(t *testing.T) {
	missing := briefingHash("missing-parent")
	remote := briefingHash("missing-remote")
	delta, complete := pullBriefingDelta([]domain.Snapshot{{
		ID: remote, Parents: []domain.ContentHash{missing}, Message: "visible but incomplete",
	}}, remote, "", "")
	if complete || len(delta) != 1 || delta[0].ID != remote {
		t.Fatalf("incomplete delta=%+v complete=%v", delta, complete)
	}
}

func TestPullBriefingDeltaRetainsNewestTwelveInChronologicalOrder(t *testing.T) {
	local := briefingHash("bounded-local")
	previous := local
	snapshots := []domain.Snapshot{{ID: local, Message: "local"}}
	var ids []domain.ContentHash
	for i := 1; i <= 15; i++ {
		id := briefingHash(fmt.Sprintf("bounded-%02d", i))
		snapshots = append(snapshots, domain.Snapshot{
			ID:      id,
			Parents: []domain.ContentHash{previous},
			Message: fmt.Sprintf("team %02d", i),
		})
		ids = append(ids, id)
		previous = id
	}

	delta, complete := pullBriefingDelta(snapshots, previous, local, "")
	if !complete || len(delta) != 12 {
		t.Fatalf("bounded delta len=%d complete=%v", len(delta), complete)
	}
	for i, snap := range delta {
		if want := ids[i+3]; snap.ID != want {
			t.Fatalf("delta[%d]=%s, want %s", i, snap.ID, want)
		}
	}
}
