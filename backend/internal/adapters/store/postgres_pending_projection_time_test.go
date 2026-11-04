//go:build postgres

package store

import (
	"reflect"
	"testing"
	"time"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

func TestPendingProjectionTimestampParityPG(test *testing.T) {
	stamp := time.Date(2026, time.November, 3, 1, 2, 3, 456789123, time.UTC)
	viewAt := func(stamp time.Time) domain.PendingStateView {
		return domain.PendingStateView{
			Pending:          []domain.Pending{{Target: "capture", UpdatedAt: stamp, ActivityAt: &stamp}},
			Snapshots:        []domain.Snapshot{{ID: "capture", Parents: []domain.ContentHash{"parent"}, CreatedAt: stamp}},
			ArchivedSessions: []domain.SessionArchiveView{{SessionArchive: domain.SessionArchive{SnapshotID: "capture", ArchivedAt: stamp}, UpdatedAt: stamp}},
			Hold:             []domain.GraphHoldCluster{{Tips: []domain.Unsync{{Target: "capture", UpdatedAt: stamp}}}},
		}
	}
	want := viewAt(stamp)
	for _, location := range []*time.Location{time.FixedZone("local UTC", 0), time.FixedZone("local Seoul", 9*60*60)} {
		got := viewAt(stamp.In(location))
		if !reflect.DeepEqual(pendingProjectionViewUTCTimesPG(got), want) || !reflect.DeepEqual(got, viewAt(stamp.In(location))) {
			test.Fatal("timestamp comparison lost parity or changed the original pending view")
		}
	}
	for name, mutate := range map[string]func(*domain.PendingStateView){
		"pending instant":  func(view *domain.PendingStateView) { view.Pending[0].UpdatedAt = stamp.Add(time.Nanosecond) },
		"activity instant": func(view *domain.PendingStateView) { *view.Pending[0].ActivityAt = stamp.Add(time.Nanosecond) },
		"snapshot instant": func(view *domain.PendingStateView) { view.Snapshots[0].CreatedAt = stamp.Add(time.Nanosecond) },
		"archive instant":  func(view *domain.PendingStateView) { view.ArchivedSessions[0].ArchivedAt = stamp.Add(time.Nanosecond) },
		"archive updated":  func(view *domain.PendingStateView) { view.ArchivedSessions[0].UpdatedAt = stamp.Add(time.Nanosecond) },
		"hold instant":     func(view *domain.PendingStateView) { view.Hold[0].Tips[0].UpdatedAt = stamp.Add(time.Nanosecond) },
		"pending target":   func(view *domain.PendingStateView) { view.Pending[0].Target = "other" },
		"snapshot edge":    func(view *domain.PendingStateView) { view.Snapshots[0].Parents[0] = "other" },
		"archive target":   func(view *domain.PendingStateView) { view.ArchivedSessions[0].SnapshotID = "other" },
		"hold target":      func(view *domain.PendingStateView) { view.Hold[0].Tips[0].Target = "other" },
		"missing activity": func(view *domain.PendingStateView) { view.Pending[0].ActivityAt = nil },
	} {
		got := viewAt(stamp)
		mutate(&got)
		if reflect.DeepEqual(pendingProjectionViewUTCTimesPG(got), want) {
			test.Fatal("timestamp comparison discarded a semantic difference", name)
		}
	}
	for _, view := range []domain.PendingStateView{{}, {Pending: []domain.Pending{}, Snapshots: []domain.Snapshot{}, ArchivedSessions: []domain.SessionArchiveView{}, Hold: []domain.GraphHoldCluster{}}, {Pending: []domain.Pending{{}}, Hold: []domain.GraphHoldCluster{{}, {Tips: []domain.Unsync{}}}}} {
		if !reflect.DeepEqual(pendingProjectionViewUTCTimesPG(view), view) {
			test.Fatal("comparison changed nil or empty fields")
		}
	}
}
