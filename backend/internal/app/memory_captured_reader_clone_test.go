package app

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

func capturedMetadataCloneFixture() *capturedMemoryReader {
	repo := domain.HashContent([]byte("metadata clone repository"))
	created := time.Date(2026, time.October, 19, 1, 2, 3, 456, time.UTC)
	return &capturedMemoryReader{repo: repo, view: domain.RepositoryView{
		Snapshots: []domain.Snapshot{
			{ID: domain.HashContent([]byte("first")), RepoID: repo, Branch: "main", Branches: []string{"main", "feature"}, Parents: []domain.ContentHash{domain.HashContent([]byte("parent"))}, GraftParents: []domain.ContentHash{domain.HashContent([]byte("graft"))}, Models: []string{"model"}, CreatedAt: created},
			{ID: domain.HashContent([]byte("second")), RepoID: repo, Branch: "feature", Branches: []string{}, Parents: []domain.ContentHash{}, GraftParents: []domain.ContentHash{}, Models: []string{}, CreatedAt: created},
			{ID: domain.HashContent([]byte("third")), RepoID: repo, Branch: "main"},
		},
		History: []domain.HistoryEvent{
			{ID: "creation", Creation: &domain.GitCreation{Evidence: "process-argv", Command: []string{"git", "switch", "-c", "feature"}, StartRef: "HEAD"}, CreatedAt: created},
			{ID: "merge", PR: &domain.PullRequestMerge{Number: 1, BaseBranch: "main", HeadBranch: "feature", HeadSHA: "head", MergeSHA: "merge"}, CreatedAt: created},
			{ID: "empty", Creation: &domain.GitCreation{Command: []string{}}, PR: &domain.PullRequestMerge{}},
			{ID: "nil"},
			{ID: "nil command", Creation: &domain.GitCreation{}},
		},
	}}
}

func TestCapturedMemoryMetadataClonesMatchJSON(test *testing.T) {
	ctx := context.Background()
	for _, mode := range []string{"populated", "nil", "empty"} {
		test.Run(mode, func(test *testing.T) {
			reader := capturedMetadataCloneFixture()
			switch mode {
			case "nil":
				reader.view.Snapshots, reader.view.History = nil, nil
			case "empty":
				reader.view.Snapshots, reader.view.History = []domain.Snapshot{}, []domain.HistoryEvent{}
			}
			for _, branch := range []string{"", "main", "feature", "missing"} {
				selected := reader.view.Snapshots
				if branch != "" {
					selected = nil
					for _, snapshot := range reader.view.Snapshots {
						if snapshot.Branch == branch {
							selected = append(selected, snapshot)
						}
					}
				}
				want, err := cloneMemoryInput(ctx, selected)
				if err != nil {
					test.Fatal(err)
				}
				got, err := reader.ListSnapshots(ctx, reader.repo, branch)
				if err != nil || !reflect.DeepEqual(got, want) {
					test.Fatalf("snapshot list differs from JSON clone for branch %q: got=%#v want=%#v error=%v", branch, got, want, err)
				}
			}
			for _, snapshot := range reader.view.Snapshots {
				want, err := cloneMemoryInput(ctx, snapshot)
				if err != nil {
					test.Fatal(err)
				}
				got, err := reader.GetSnapshot(ctx, reader.repo, snapshot.ID)
				if err != nil || !reflect.DeepEqual(got, want) {
					test.Fatalf("snapshot lookup differs from JSON clone: got=%#v want=%#v error=%v", got, want, err)
				}
			}
			want, err := cloneMemoryInput(ctx, reader.view.History)
			if err != nil {
				test.Fatal(err)
			}
			got, err := reader.ListHistoryEvents(ctx, reader.repo)
			if err != nil || !reflect.DeepEqual(got, want) {
				test.Fatalf("history list differs from JSON clone: got=%#v want=%#v error=%v", got, want, err)
			}
		})
	}
}

func TestCapturedMemoryMetadataClonesIsolateConcurrentReaders(test *testing.T) {
	reader, expected := capturedMetadataCloneFixture(), capturedMetadataCloneFixture()
	ctx := context.Background()
	var workers sync.WaitGroup
	for range 8 {
		workers.Go(func() {
			for _, branch := range []string{"", "main"} {
				rows, err := reader.ListSnapshots(ctx, reader.repo, branch)
				if err != nil || len(rows) == 0 {
					test.Error("snapshot list failed", err)
					return
				}
				rows[0].Branches[0] = "changed"
				rows[0].Parents[0] = domain.HashContent([]byte("changed"))
				rows[0].GraftParents[0] = domain.HashContent([]byte("changed"))
				rows[0].Models[0] = "changed"
				rows[0].Branch = "changed"
			}
			snapshot, err := reader.GetSnapshot(ctx, reader.repo, reader.view.Snapshots[0].ID)
			if err != nil {
				test.Error(err)
				return
			}
			snapshot.Branches[0], snapshot.Models[0] = "changed", "changed"
			snapshot.Parents[0], snapshot.GraftParents[0] = "changed", "changed"
			history, err := reader.ListHistoryEvents(ctx, reader.repo)
			if err != nil {
				test.Error(err)
				return
			}
			history[0].Creation.Command[0] = "changed"
			history[0].Creation.StartRef = "changed"
			history[1].PR.BaseBranch = "changed"
			history[2].Creation.Command = append(history[2].Creation.Command, "changed")
			history[2].PR.Number++
			history[0].ID = "changed"
		})
	}
	workers.Wait()
	if !reflect.DeepEqual(reader.view, expected.view) {
		test.Fatal("caller mutation changed captured metadata")
	}
}

func TestCapturedMemoryMetadataClonesCancellationAndScope(test *testing.T) {
	reader := capturedMetadataCloneFixture()
	operations := []struct {
		name string
		read func(context.Context, domain.ContentHash) (any, error)
	}{
		{"snapshots", func(ctx context.Context, repo domain.ContentHash) (any, error) {
			return reader.ListSnapshots(ctx, repo, "")
		}},
		{"filtered", func(ctx context.Context, repo domain.ContentHash) (any, error) {
			return reader.ListSnapshots(ctx, repo, "main")
		}},
		{"unmatched", func(ctx context.Context, repo domain.ContentHash) (any, error) {
			return reader.ListSnapshots(ctx, repo, "missing")
		}},
		{"snapshot", func(ctx context.Context, repo domain.ContentHash) (any, error) {
			return reader.GetSnapshot(ctx, repo, reader.view.Snapshots[0].ID)
		}},
		{"history", func(ctx context.Context, repo domain.ContentHash) (any, error) {
			return reader.ListHistoryEvents(ctx, repo)
		}},
	}
	for _, operation := range operations {
		test.Run(operation.name, func(test *testing.T) {
			measured := &capturedMemoryCancelAfterCheck{Context: context.Background()}
			if _, err := operation.read(measured, reader.repo); err != nil {
				test.Fatal(err)
			}
			for after := 0; after < measured.checks; after++ {
				ctx, cancel := context.WithCancel(context.Background())
				bound := &capturedMemoryCancelAfterCheck{Context: ctx, after: after, cancel: cancel}
				if after == 0 {
					cancel()
				}
				got, err := operation.read(bound, reader.repo)
				cancel()
				if !errors.Is(err, context.Canceled) || !reflect.ValueOf(got).IsZero() {
					test.Fatalf("cancellation after check %d returned partial metadata: %#v %v", after, got, err)
				}
			}
			got, err := operation.read(context.Background(), domain.HashContent([]byte("foreign")))
			if !errors.Is(err, domain.ErrIntegrity) || !reflect.ValueOf(got).IsZero() {
				test.Fatal("foreign repository returned metadata", err)
			}
		})
	}
	if got, err := reader.GetSnapshot(context.Background(), reader.repo, domain.HashContent([]byte("missing"))); !errors.Is(err, domain.ErrNotFound) || !reflect.DeepEqual(got, domain.Snapshot{}) {
		test.Fatal("missing snapshot returned metadata", err)
	}
	if !reflect.DeepEqual(reader.view, capturedMetadataCloneFixture().view) {
		test.Fatal("failed reads changed captured metadata")
	}
}

func TestCapturedMemoryMetadataClonesRequireValidatedSource(test *testing.T) {
	ctx := context.Background()
	checkpoint, entries := graphCaptureFixture()
	snapshot := capturedMetadataCloneFixture().view.Snapshots[0].Clone()
	snapshot.RepoID, snapshot.DocHash = checkpoint.RepoID, snapshot.ID
	raw, err := json.Marshal(snapshot)
	if err != nil {
		test.Fatal(err)
	}
	entries = append(entries, domain.RepositoryViewSourceEntry{Kind: "snapshot", Key: string(snapshot.ID), Sequence: checkpoint.Sequence, Value: raw})
	base := CapturedGraphSource{checkpoint: checkpoint, entries: entries}
	if _, _, _, err := base.prepareInputs(ctx); err != nil {
		test.Fatal("valid capture source rejected", err)
	}
	for _, scenario := range []struct {
		name string
		raw  []byte
	}{
		{"unsupported identity", bytes.Replace(raw, []byte("{"), []byte(`{"doc_identity":"unsupported",`), 1)},
		{"null identity", bytes.Replace(raw, []byte("{"), []byte(`{"doc_identity":null,`), 1)},
		{"duplicate identity aliases", bytes.Replace(raw, []byte("{"), []byte(`{"doc_identity":"","DOC_IDENTITY":"",`), 1)},
		{"unknown snapshot field", bytes.Replace(raw, []byte("{"), []byte(`{"unknown_field":true,`), 1)},
		{"invalid year", bytes.Replace(raw, []byte(snapshot.CreatedAt.Format(time.RFC3339Nano)), []byte("10000-10-19T01:02:03Z"), 1)},
		{"unserializable timezone", bytes.Replace(raw, []byte(snapshot.CreatedAt.Format(time.RFC3339Nano)), []byte("2026-10-19T01:02:03+24:00"), 1)},
	} {
		test.Run(scenario.name, func(test *testing.T) {
			for _, cached := range []bool{false, true} {
				changed := base
				if !cached {
					changed.decoded = nil
				}
				previous := changed.decoded
				changed.entries = append([]domain.RepositoryViewSourceEntry{}, entries...)
				changed.entries[len(changed.entries)-1].Value = scenario.raw
				metadata, view, evidence, err := changed.prepareInputs(ctx)
				if !errors.Is(err, domain.ErrIntegrity) || !reflect.DeepEqual(metadata, domain.Repo{}) || !reflect.DeepEqual(view, domain.RepositoryView{}) || evidence != nil || changed.decoded != previous {
					if len(view.Snapshots) != 0 {
						_, oldErr := cloneMemoryInput(ctx, view.Snapshots)
						test.Logf("previous JSON-copy error: %v", oldErr)
					}
					test.Fatalf("invalid source reached typed metadata reader, cached=%t: %v", cached, err)
				}
			}
		})
	}
}

func TestCapturedMemoryMetadataClonesRequireValidatedHistoryTime(test *testing.T) {
	ctx := context.Background()
	checkpoint, entries := graphCaptureFixture()
	event := domain.HistoryEvent{
		ID: "0123456789abcdef0123456789abcdef", RepoID: string(checkpoint.RepoID),
		BranchID: "history-time-main", Branch: "main", Kind: "birth",
		CreatedAt: time.Date(2026, time.October, 19, 1, 2, 3, 456, time.UTC),
	}
	raw, err := json.Marshal(event)
	if err != nil {
		test.Fatal(err)
	}
	entries = append(entries, domain.RepositoryViewSourceEntry{Kind: "history", Key: event.ID, Sequence: checkpoint.Sequence, Value: raw})
	base := CapturedGraphSource{checkpoint: checkpoint, entries: entries}
	if _, view, _, err := base.prepareInputs(ctx); err != nil || len(view.History) != 1 {
		test.Fatal("valid history source rejected", err)
	}
	invalid := bytes.Replace(raw, []byte(event.CreatedAt.Format(time.RFC3339Nano)), []byte("2026-10-19T01:02:03+24:00"), 1)
	var decoded domain.HistoryEvent
	if err := json.Unmarshal(invalid, &decoded); err != nil {
		test.Fatal("fixture must exercise a decoded but unserializable timestamp", err)
	}
	if _, err := cloneMemoryInput(ctx, decoded); err == nil {
		test.Fatal("legacy JSON copy accepted the invalid history timezone")
	}
	for _, mode := range []string{"cold", "reusing"} {
		test.Run(mode, func(test *testing.T) {
			changed := base
			if mode == "cold" {
				changed.decoded = nil
			}
			previous := changed.decoded
			changed.entries = append([]domain.RepositoryViewSourceEntry{}, entries...)
			changed.entries[len(changed.entries)-1].Value = invalid
			metadata, view, evidence, err := changed.prepareInputs(ctx)
			if !errors.Is(err, domain.ErrIntegrity) || !reflect.DeepEqual(metadata, domain.Repo{}) || !reflect.DeepEqual(view, domain.RepositoryView{}) || evidence != nil || changed.decoded != previous {
				test.Fatal("invalid history time reached typed metadata reader or replaced the decoder", err)
			}
			changed.entries = entries
			if _, view, _, err := changed.prepareInputs(ctx); err != nil || len(view.History) != 1 || !reflect.DeepEqual(view.History[0], event) {
				test.Fatal("failed history decoding poisoned valid retry", err)
			}
		})
	}
}

func TestCapturedMemoryMetadataClonesDoNotValidatePrivateTypedValues(test *testing.T) {
	ctx := context.Background()
	for _, scenario := range []string{"identity", "snapshot time", "history time"} {
		test.Run(scenario, func(test *testing.T) {
			reader := capturedMetadataCloneFixture()
			invalidTime := time.Date(10000, time.October, 19, 0, 0, 0, 0, time.UTC)
			switch scenario {
			case "identity":
				reader.view.Snapshots[0].DocIdentity = "unsupported"
			case "snapshot time":
				reader.view.Snapshots[0].CreatedAt = invalidTime
			case "history time":
				reader.view.History[0].CreatedAt = invalidTime
			}
			if scenario == "history time" {
				if _, err := cloneMemoryInput(ctx, reader.view.History); err == nil {
					test.Fatal("legacy copy must reject invalid typed history")
				}
				rows, err := reader.ListHistoryEvents(ctx, reader.repo)
				if err != nil || !reflect.DeepEqual(rows[0], reader.view.History[0]) {
					test.Fatal("typed history copy changed privately supplied values", err)
				}
				return
			}
			if _, err := cloneMemoryInput(ctx, reader.view.Snapshots); err == nil {
				test.Fatal("legacy copy must reject invalid typed snapshot")
			}
			rows, err := reader.ListSnapshots(ctx, reader.repo, "")
			if err != nil || !reflect.DeepEqual(rows[0], reader.view.Snapshots[0]) {
				test.Fatal("typed snapshot list changed privately supplied values", err)
			}
			snapshot, err := reader.GetSnapshot(ctx, reader.repo, reader.view.Snapshots[0].ID)
			if err != nil || !reflect.DeepEqual(snapshot, reader.view.Snapshots[0]) {
				test.Fatal("typed snapshot copy changed privately supplied values", err)
			}
		})
	}
}
