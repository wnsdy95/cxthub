package domain

import (
	"errors"
	"fmt"
	"math"
	"reflect"
	"strings"
	"testing"
)

const associationDiscoveryEpoch = "e66213bd-003e-47d1-b965-e690036bfb08"

func associationDiscoveryFixture() GitAssociationDiscovery {
	return GitAssociationDiscovery{RepoID: HashContent([]byte("association discovery")), GitOrigin: "https://github.com/acme/api.git", Version: 2}
}

func associationDiscoveryCompleted() GitAssociationDiscovery {
	discovery := associationDiscoveryFixture()
	discovery.Checkpoint = &CatalogCheckpoint{Version: CatalogVersion, RepoID: discovery.RepoID, Epoch: associationDiscoveryEpoch, Sequence: 3}
	return discovery
}

func associationDiscoveryPartial(mode string) GitAssociationDiscovery {
	discovery := associationDiscoveryFixture()
	if mode == "delta" {
		discovery = associationDiscoveryCompleted()
	}
	discovery.Cursor, discovery.Epoch, discovery.Through, discovery.Mode = "page-2", associationDiscoveryEpoch, 8, mode
	return discovery
}

func associationDiscoveryPage(discovery GitAssociationDiscovery, mode string, through int64, cursor string, entries ...CatalogEntry) CatalogPage {
	page := CatalogPage{Version: CatalogVersion, RepoID: discovery.RepoID, Epoch: associationDiscoveryEpoch, Through: through, Mode: mode, Entries: entries, NextCursor: cursor}
	if cursor == "" {
		page.Checkpoint = &CatalogCheckpoint{Version: CatalogVersion, RepoID: discovery.RepoID, Epoch: page.Epoch, Sequence: through}
	}
	return page
}

func cloneAssociationDiscovery(discovery GitAssociationDiscovery) GitAssociationDiscovery {
	if discovery.Checkpoint != nil {
		checkpoint := *discovery.Checkpoint
		discovery.Checkpoint = &checkpoint
	}
	return discovery
}

func assertAssociationAdvanceRejected(t *testing.T, discovery GitAssociationDiscovery, page CatalogPage) {
	t.Helper()
	before := cloneAssociationDiscovery(discovery)
	next, err := discovery.Advance(page)
	if !errors.Is(err, ErrIntegrity) || !reflect.DeepEqual(next, GitAssociationDiscovery{}) {
		t.Fatalf("invalid page published progress: next=%+v err=%v", next, err)
	}
	if !reflect.DeepEqual(discovery, before) {
		t.Fatalf("failed advance mutated prior state: before=%+v after=%+v", before, discovery)
	}
}

func TestGitAssociationDiscoveryBaselinePartialAndFinal(t *testing.T) {
	discovery := associationDiscoveryFixture()
	request := discovery.Request()
	if discovery.Validate() != nil || request.Validate() != nil || request.Version != CatalogVersion || request.Limit != MaxCatalogLimit || request.After != nil || request.Cursor != "" {
		t.Fatal("invalid baseline request", discovery, request)
	}
	for _, cursor := range []string{"page-2", "page-3"} {
		before := cloneAssociationDiscovery(discovery)
		page := associationDiscoveryPage(discovery, "baseline", 8, cursor, CatalogEntry{Sequence: 2, Kind: "history", Key: cursor + "-event-2"}, CatalogEntry{Sequence: 1, Kind: "history", Key: cursor + "-event-1"})
		next, err := discovery.Advance(page)
		if err != nil || next.Validate() != nil || next.Version != discovery.Version+1 || next.Checkpoint != nil || next.Cursor != cursor || next.Epoch != page.Epoch || next.Through != 8 || next.Mode != "baseline" {
			t.Fatalf("partial baseline published a complete checkpoint or lost frontier: %+v %v", next, err)
		}
		if !reflect.DeepEqual(discovery, before) {
			t.Fatal("advance mutated baseline input")
		}
		request = next.Request()
		if request.Validate() != nil || request.After != nil || request.Cursor != cursor || request.Limit != MaxCatalogLimit {
			t.Fatal("baseline did not resume through opaque cursor", request)
		}
		discovery = next
	}
	page := associationDiscoveryPage(discovery, "baseline", 8, "", CatalogEntry{Sequence: 8, Kind: "history", Key: "last"})
	complete, err := discovery.Advance(page)
	if err != nil || complete.Version != discovery.Version+1 || !reflect.DeepEqual(complete.Checkpoint, page.Checkpoint) || complete.Cursor != "" || complete.Epoch != "" || complete.Through != 0 || complete.Mode != "" {
		t.Fatalf("final baseline did not publish exactly its checkpoint: %+v %v", complete, err)
	}
	request = complete.Request()
	if request.Validate() != nil || request.Cursor != "" || !reflect.DeepEqual(request.After, page.Checkpoint) {
		t.Fatal("completed baseline did not start a delta", request)
	}
}

func TestGitAssociationDiscoveryDeltaStartResumeAndFinal(t *testing.T) {
	discovery := associationDiscoveryCompleted()
	originalCheckpoint := *discovery.Checkpoint
	request := discovery.Request()
	if request.Validate() != nil || request.Cursor != "" || !reflect.DeepEqual(request.After, &originalCheckpoint) {
		t.Fatal("delta did not start at the complete checkpoint", request)
	}
	for _, cursor := range []string{"delta-page-2", "delta-page-3"} {
		page := associationDiscoveryPage(discovery, "delta", 8, cursor, CatalogEntry{Sequence: 4, Kind: "history", Key: cursor})
		next, err := discovery.Advance(page)
		if err != nil || next.Validate() != nil || next.Version != discovery.Version+1 || !reflect.DeepEqual(next.Checkpoint, &originalCheckpoint) || next.Cursor != cursor || next.Epoch != page.Epoch || next.Through != 8 || next.Mode != "delta" {
			t.Fatalf("partial delta advanced complete checkpoint or lost frontier: %+v %v", next, err)
		}
		request = next.Request()
		if request.Validate() != nil || request.After != nil || request.Cursor != cursor || request.Limit != MaxCatalogLimit {
			t.Fatal("delta resume mixed checkpoint and cursor", request)
		}
		discovery = next
	}
	page := associationDiscoveryPage(discovery, "delta", 8, "", CatalogEntry{Sequence: 8, Kind: "history", Key: "last"})
	complete, err := discovery.Advance(page)
	if err != nil || complete.Validate() != nil || complete.Version != discovery.Version+1 || !reflect.DeepEqual(complete.Checkpoint, page.Checkpoint) || complete.Cursor != "" || complete.Epoch != "" || complete.Through != 0 || complete.Mode != "" {
		t.Fatalf("final delta did not publish exactly its checkpoint: %+v %v", complete, err)
	}
}

func TestGitAssociationDiscoveryEmptyCompletePages(t *testing.T) {
	for _, scenario := range []struct {
		name      string
		discovery GitAssociationDiscovery
		mode      string
		through   int64
	}{
		{"empty baseline", associationDiscoveryFixture(), "baseline", 0},
		{"unchanged delta", associationDiscoveryCompleted(), "delta", 3},
		{"empty final baseline page", associationDiscoveryPartial("baseline"), "baseline", 8},
		{"empty final delta page", associationDiscoveryPartial("delta"), "delta", 8},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			page := associationDiscoveryPage(scenario.discovery, scenario.mode, scenario.through, "")
			next, err := scenario.discovery.Advance(page)
			if err != nil || next.Validate() != nil || !reflect.DeepEqual(next.Checkpoint, page.Checkpoint) || next.Cursor != "" {
				t.Fatal("valid empty complete page rejected", next, err)
			}
		})
	}
}

func TestGitAssociationDiscoveryFreezesEpochThroughAndMode(t *testing.T) {
	for _, mode := range []string{"baseline", "delta"} {
		for _, mismatch := range []string{"epoch", "through", "mode", "unchanged cursor"} {
			t.Run(mode+"/"+mismatch, func(t *testing.T) {
				discovery := associationDiscoveryPartial(mode)
				page := associationDiscoveryPage(discovery, mode, 8, "page-3", CatalogEntry{Sequence: 5, Kind: "history", Key: "event"})
				switch mismatch {
				case "epoch":
					page.Epoch = "f66213bd-003e-47d1-b965-e690036bfb08"
				case "through":
					page.Through++
				case "mode":
					page.Mode = "delta"
					if mode == "delta" {
						page.Mode = "baseline"
					}
				case "unchanged cursor":
					page.NextCursor = discovery.Cursor
				}
				assertAssociationAdvanceRejected(t, discovery, page)
				if mismatch != "unchanged cursor" {
					page.NextCursor = ""
					page.Checkpoint = &CatalogCheckpoint{Version: CatalogVersion, RepoID: page.RepoID, Epoch: page.Epoch, Sequence: page.Through}
					assertAssociationAdvanceRejected(t, discovery, page)
				}
			})
		}
	}
	for _, mismatch := range []string{"baseline instead of delta", "epoch", "through before checkpoint", "sequence at checkpoint", "sequence before checkpoint"} {
		t.Run("delta-start/"+mismatch, func(t *testing.T) {
			discovery := associationDiscoveryCompleted()
			page := associationDiscoveryPage(discovery, "delta", 8, "page-2", CatalogEntry{Sequence: 4, Kind: "history", Key: "event"})
			switch mismatch {
			case "baseline instead of delta":
				page.Mode = "baseline"
			case "epoch":
				page.Epoch = "f66213bd-003e-47d1-b965-e690036bfb08"
			case "through before checkpoint":
				page.Through, page.Entries[0].Sequence = 2, 1
			case "sequence at checkpoint":
				page.Entries[0].Sequence = discovery.Checkpoint.Sequence
			case "sequence before checkpoint":
				page.Entries[0].Sequence = discovery.Checkpoint.Sequence - 1
			}
			assertAssociationAdvanceRejected(t, discovery, page)
		})
	}
}

func TestGitAssociationDiscoveryInvalidStateFailsClosed(t *testing.T) {
	for _, scenario := range []string{"repo", "origin", "version", "zero version with checkpoint", "zero version with cursor", "orphan epoch", "orphan through", "orphan mode", "cursor without frontier", "oversized cursor", "invalid epoch", "negative through", "invalid mode", "baseline with checkpoint", "delta without checkpoint", "delta epoch", "delta through", "checkpoint version", "checkpoint repo", "checkpoint sequence"} {
		t.Run(scenario, func(t *testing.T) {
			discovery := associationDiscoveryFixture()
			switch scenario {
			case "repo":
				discovery.RepoID = "invalid"
			case "origin":
				discovery.GitOrigin = ""
			case "version":
				discovery.Version = -1
			case "zero version with checkpoint":
				discovery = associationDiscoveryCompleted()
				discovery.Version = 0
			case "zero version with cursor":
				discovery = associationDiscoveryPartial("baseline")
				discovery.Version = 0
			case "orphan epoch":
				discovery.Epoch = associationDiscoveryEpoch
			case "orphan through":
				discovery.Through = 1
			case "orphan mode":
				discovery.Mode = "baseline"
			case "cursor without frontier":
				discovery.Cursor = "page-2"
			case "oversized cursor", "invalid epoch", "negative through", "invalid mode", "baseline with checkpoint":
				discovery = associationDiscoveryPartial("baseline")
				switch scenario {
				case "oversized cursor":
					discovery.Cursor = strings.Repeat("x", (16<<10)+1)
				case "invalid epoch":
					discovery.Epoch = "invalid"
				case "negative through":
					discovery.Through = -1
				case "invalid mode":
					discovery.Mode = "other"
				case "baseline with checkpoint":
					discovery.Checkpoint = associationDiscoveryCompleted().Checkpoint
				}
			case "delta without checkpoint", "delta epoch", "delta through":
				discovery = associationDiscoveryPartial("delta")
				switch scenario {
				case "delta without checkpoint":
					discovery.Checkpoint = nil
				case "delta epoch":
					discovery.Epoch = "f66213bd-003e-47d1-b965-e690036bfb08"
				case "delta through":
					discovery.Through = discovery.Checkpoint.Sequence - 1
				}
			case "checkpoint version", "checkpoint repo", "checkpoint sequence":
				discovery = associationDiscoveryCompleted()
				switch scenario {
				case "checkpoint version":
					discovery.Checkpoint.Version++
				case "checkpoint repo":
					discovery.Checkpoint.RepoID = HashContent([]byte("foreign repo"))
				case "checkpoint sequence":
					discovery.Checkpoint.Sequence = -1
				}
			}
			if !errors.Is(discovery.Validate(), ErrValidation) {
				t.Fatal("invalid discovery state accepted", discovery)
			}
			page := associationDiscoveryPage(associationDiscoveryFixture(), "baseline", 1, "", CatalogEntry{Sequence: 1, Kind: "history", Key: "event"})
			assertAssociationAdvanceRejected(t, discovery, page)
			reset, err := discovery.Reset()
			if !errors.Is(err, ErrValidation) || !reflect.DeepEqual(reset, GitAssociationDiscovery{}) {
				t.Fatal("invalid discovery state reset into publishable progress", reset, err)
			}
		})
	}
}

func TestGitAssociationDiscoveryAdvanceErrorsDoNotPublishProgress(t *testing.T) {
	for _, scenario := range []string{"version", "scope", "epoch", "negative through", "mode", "delta before baseline", "too many entries", "zero sequence", "negative sequence", "future sequence", "empty key", "invalid kind", "partial with checkpoint", "empty partial", "oversized next cursor", "final without checkpoint", "checkpoint version", "checkpoint scope", "checkpoint epoch", "checkpoint sequence"} {
		t.Run(scenario, func(t *testing.T) {
			discovery := associationDiscoveryFixture()
			page := associationDiscoveryPage(discovery, "baseline", 8, "", CatalogEntry{Sequence: 1, Kind: "history", Key: "event"})
			switch scenario {
			case "version":
				page.Version++
			case "scope":
				page.RepoID = HashContent([]byte("foreign repo"))
			case "epoch":
				page.Epoch = "invalid"
			case "negative through":
				page.Through = -1
			case "mode":
				page.Mode = "other"
			case "delta before baseline":
				page.Mode = "delta"
			case "too many entries":
				for number := 1; number <= MaxCatalogLimit; number++ {
					page.Entries = append(page.Entries, CatalogEntry{Sequence: 1, Kind: "history", Key: fmt.Sprintf("event-%d", number)})
				}
			case "zero sequence":
				page.Entries[0].Sequence = 0
			case "negative sequence":
				page.Entries[0].Sequence = -1
			case "future sequence":
				page.Entries[0].Sequence = page.Through + 1
			case "empty key":
				page.Entries[0].Key = ""
			case "invalid kind":
				page.Entries[0].Kind = "unknown"
			case "partial with checkpoint":
				page.NextCursor = "page-2"
			case "empty partial":
				page.NextCursor, page.Checkpoint, page.Entries = "page-2", nil, nil
			case "oversized next cursor":
				page.NextCursor, page.Checkpoint = strings.Repeat("x", (16<<10)+1), nil
			case "final without checkpoint":
				page.Checkpoint = nil
			case "checkpoint version":
				page.Checkpoint.Version++
			case "checkpoint scope":
				page.Checkpoint.RepoID = HashContent([]byte("foreign repo"))
			case "checkpoint epoch":
				page.Checkpoint.Epoch = "f66213bd-003e-47d1-b965-e690036bfb08"
			case "checkpoint sequence":
				page.Checkpoint.Sequence--
			}
			assertAssociationAdvanceRejected(t, discovery, page)
		})
	}
}

func TestGitAssociationDiscoveryResetPreservesScopeAndFencesVersion(t *testing.T) {
	for _, discovery := range []GitAssociationDiscovery{associationDiscoveryFixture(), associationDiscoveryPartial("baseline"), associationDiscoveryCompleted(), associationDiscoveryPartial("delta")} {
		before := cloneAssociationDiscovery(discovery)
		next, err := discovery.Reset()
		want := GitAssociationDiscovery{RepoID: discovery.RepoID, GitOrigin: discovery.GitOrigin, Version: discovery.Version + 1}
		if err != nil || next.Validate() != nil || !reflect.DeepEqual(next, want) || !reflect.DeepEqual(discovery, before) {
			t.Fatalf("reset changed scope, retained progress or mutated input: next=%+v prior=%+v err=%v", next, discovery, err)
		}
	}
}

func TestGitAssociationDiscoveryVersionOverflowFailsClosed(t *testing.T) {
	discovery := associationDiscoveryFixture()
	discovery.Version = math.MaxInt64 - 1
	page := associationDiscoveryPage(discovery, "baseline", 0, "")
	next, err := discovery.Advance(page)
	if err != nil || next.Version != math.MaxInt64 || next.Validate() != nil {
		t.Fatal("last representable advance rejected", next, err)
	}
	reset, err := discovery.Reset()
	if err != nil || reset.Version != math.MaxInt64 || reset.Validate() != nil {
		t.Fatal("last representable reset rejected", reset, err)
	}
	assertAssociationAdvanceRejected(t, next, associationDiscoveryPage(next, "delta", 0, ""))
	reset, err = next.Reset()
	if !errors.Is(err, ErrValidation) || !reflect.DeepEqual(reset, GitAssociationDiscovery{}) {
		t.Fatal("overflow reset published wrapped version", reset, err)
	}
}

func TestGitAssociationDiscoveryReturnedCheckpointsAreCopies(t *testing.T) {
	t.Run("request", func(t *testing.T) {
		discovery := associationDiscoveryCompleted()
		request, independent := discovery.Request(), discovery.Request()
		request.After.Sequence++
		if discovery.Checkpoint.Sequence != 3 || independent.After.Sequence != 3 {
			t.Fatal("request checkpoint aliases stored state or another request")
		}
	})
	t.Run("final page", func(t *testing.T) {
		discovery := associationDiscoveryCompleted()
		page := associationDiscoveryPage(discovery, "delta", 8, "", CatalogEntry{Sequence: 4, Kind: "history", Key: "event"})
		next, err := discovery.Advance(page)
		if err != nil || next.Checkpoint == nil {
			t.Fatal("final fixture failed", next, err)
		}
		next.Checkpoint.Sequence++
		if page.Checkpoint.Sequence != 8 || discovery.Checkpoint.Sequence != 3 {
			t.Fatal("returned final checkpoint aliases page or input state")
		}
		page.Checkpoint.Sequence++
		if next.Checkpoint.Sequence != 9 {
			t.Fatal("page checkpoint mutation changed returned state")
		}
	})
	t.Run("partial delta", func(t *testing.T) {
		discovery := associationDiscoveryCompleted()
		page := associationDiscoveryPage(discovery, "delta", 8, "page-2", CatalogEntry{Sequence: 4, Kind: "history", Key: "event"})
		next, err := discovery.Advance(page)
		if err != nil || next.Checkpoint == nil || next.Checkpoint.Sequence != 3 {
			t.Fatal("partial delta fixture failed", next, err)
		}
		next.Checkpoint.Sequence++
		if discovery.Checkpoint.Sequence != 3 {
			t.Fatal("partial delta checkpoint aliases prior complete state")
		}
	})
}
