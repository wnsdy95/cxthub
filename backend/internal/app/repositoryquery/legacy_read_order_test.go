package repositoryquery

import (
	"testing"
	"time"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

func TestLegacyGraphLoaderNormalizesMetadataAndArchivesWithoutMutatingStorage(test *testing.T) {
	fixture := newLegacyReadFixture(test, PinnedSnapshots)
	probe := fixture.probe
	zone := time.FixedZone("database", 9*60*60)
	stamp := time.Date(2026, time.November, 7, 6, 10, 20, 123456789, zone)
	activity := stamp.Add(time.Second)
	for index := range probe.view.Snapshots {
		probe.view.Snapshots[index].CreatedAt = probe.view.Snapshots[index].CreatedAt.In(zone)
	}
	for index := range probe.view.History {
		probe.view.History[index].CreatedAt = probe.view.History[index].CreatedAt.In(zone)
	}
	probe.view.Pending[0].SessionID, probe.view.Pending[1].SessionID = "alpha", "Zeta"
	for index := range probe.view.Pending {
		probe.view.Pending[index].UpdatedAt = stamp
		probe.view.Pending[index].ActivityAt = &activity
	}
	probe.view.Unsync = []domain.Unsync{{RepoID: probe.repo, User: "owner", Branch: "main", Target: probe.view.Snapshots[0].ID, UpdatedAt: stamp}}
	for index := range probe.view.Reflog {
		probe.view.Reflog[index].CreatedAt = stamp.Add(-time.Duration(index) * time.Second)
	}
	anchor := probe.view.Snapshots[1]
	probe.archives = []domain.SessionArchive{{RepoID: probe.repo, Key: domain.SessionArchiveKey(anchor), SnapshotID: anchor.ID,
		Provider: anchor.Provider, SessionID: anchor.SessionID, ArchivedAt: stamp, ArchivedBy: "owner"}}
	originalView, originalArchives := capturedQueryJSON(test, probe.view), capturedQueryJSON(test, probe.archives)
	view, err := fixture.load(fixture.ctx, probe.repo)
	if err != nil {
		test.Fatal(err)
	}
	if view.Pending[0].SessionID != "Zeta" || view.Pending[1].SessionID != "alpha" {
		test.Fatal("pending order still depends on storage collation")
	}
	if len(view.ArchivedSessions) != 1 || view.ArchivedSessions[0].ArchivedAt != stamp.UTC() || view.ArchivedSessions[0].UpdatedAt != anchor.CreatedAt.UTC() {
		test.Fatal("archive projection retained storage timezone or changed its instant")
	}
	for index, snapshot := range view.Snapshots {
		if snapshot.CreatedAt != probe.view.Snapshots[index].CreatedAt.UTC() {
			test.Fatal("snapshot normalization regressed")
		}
	}
	for index, event := range view.History {
		if event.CreatedAt != probe.view.History[index].CreatedAt.UTC() {
			test.Fatal("history normalization changed its instant")
		}
	}
	for index, log := range view.Reflog {
		if log.CreatedAt != probe.view.Reflog[index].CreatedAt.UTC() || log.Name != probe.view.Reflog[index].Name {
			test.Fatal("reflog normalization changed its order or instant")
		}
	}
	for _, pending := range view.Pending {
		if pending.UpdatedAt != anchor.CreatedAt.UTC() || pending.ActivityAt == &activity || *pending.ActivityAt != activity.UTC() {
			test.Fatal("pending normalization changed snapshot/activity semantics or retained storage pointers")
		}
	}
	if view.Unsync[0].UpdatedAt != stamp.UTC() || view.Pending[0].ActivityAt == view.Pending[1].ActivityAt {
		test.Fatal("metadata normalization retained timezone or shared activity pointers")
	}
	view.ArchivedSessions[0].ArchivedAt = time.Time{}
	view.Reflog[0].CreatedAt = time.Time{}
	*view.Pending[0].ActivityAt = time.Time{}
	if string(capturedQueryJSON(test, probe.view)) != string(originalView) || string(capturedQueryJSON(test, probe.archives)) != string(originalArchives) || probe.view.Pending[0].ActivityAt != &activity {
		test.Fatal("normalized projection mutated raw storage slices or pointers")
	}
}
