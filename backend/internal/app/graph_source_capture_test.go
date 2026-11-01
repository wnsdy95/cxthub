package app

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

type graphCapturePages struct {
	pages    []domain.RepositoryViewSourcePage
	requests []domain.RepositoryViewSourceRequest
	err      error
}

func (source *graphCapturePages) RepositoryGraphSourceChanges(_ context.Context, _ domain.ContentHash, request domain.RepositoryViewSourceRequest) (domain.RepositoryViewSourcePage, error) {
	source.requests = append(source.requests, request)
	if len(source.pages) == 0 {
		if source.err != nil {
			return domain.RepositoryViewSourcePage{}, source.err
		}
		return domain.RepositoryViewSourcePage{}, domain.ErrNotFound
	}
	page := source.pages[0]
	source.pages = source.pages[1:]
	return page, nil
}

func graphCaptureFixture() (domain.RepositoryViewSourceCheckpoint, []domain.RepositoryViewSourceEntry) {
	repo := domain.HashContent([]byte("captured graph"))
	checkpoint := domain.RepositoryViewSourceCheckpoint{Version: 1, Scope: domain.RepositoryGraphSourceScope, RepoID: repo, Epoch: "12345678-abcd-4321-9876-123456789abc", Sequence: 7}
	metadata, _ := json.Marshal(map[string]any{"id": repo, "context_protocol": 1, "default_branch": "main", "remote_url_supported": true, "git_remote_url_supported": true})
	return checkpoint, []domain.RepositoryViewSourceEntry{
		{Sequence: 3, Kind: "git_tree", Key: strings.Repeat("a", 40), Value: json.RawMessage(`{"tree":{}}`)},
		{Sequence: 1, Kind: "protocol", Key: string(repo), Value: json.RawMessage(`{"context_protocol":1}`)},
		{Sequence: 1, Kind: "repository", Key: string(repo), Value: metadata},
	}
}

func graphCapturePage(checkpoint domain.RepositoryViewSourceCheckpoint, mode string, entries []domain.RepositoryViewSourceEntry, cursor string) domain.RepositoryViewSourcePage {
	page := domain.RepositoryViewSourcePage{Version: checkpoint.Version, Scope: checkpoint.Scope, RepoID: checkpoint.RepoID, Epoch: checkpoint.Epoch, Through: checkpoint.Sequence, Mode: mode, Entries: entries, NextCursor: cursor}
	if cursor == "" {
		page.Checkpoint = &checkpoint
	}
	return page
}

func TestCaptureGraphSourceCompleteBaselineAndDelta(t *testing.T) {
	ctx := context.Background()
	checkpoint, entries := graphCaptureFixture()
	baseline := &graphCapturePages{pages: []domain.RepositoryViewSourcePage{graphCapturePage(checkpoint, "baseline", entries[:1], "next"), graphCapturePage(checkpoint, "baseline", entries[1:], "")}}
	captured, err := CaptureGraphSource(ctx, baseline, checkpoint.RepoID, nil)
	if err != nil || captured.Checkpoint() != checkpoint {
		t.Fatalf("capture: %+v %v", captured.Checkpoint(), err)
	}
	if baseline.requests[0].After != nil || baseline.requests[1].Cursor != "next" || baseline.requests[1].After != nil {
		t.Fatalf("invalid requests: %+v", baseline.requests)
	}
	entries[0].Value[2] = 'X'
	metadata, view, proof, err := captured.Inputs(ctx)
	if err != nil || metadata.ID != checkpoint.RepoID || view.DefaultBranch != "main" || len(proof) != 1 || bytes.Contains(proof[0].Value, []byte("X")) {
		t.Fatalf("owned source: %+v %+v %v", metadata, proof, err)
	}
	proof[0].Value[2] = 'Y'
	_, _, freshProof, err := captured.Inputs(ctx)
	if err != nil || bytes.Contains(freshProof[0].Value, []byte("Y")) {
		t.Fatalf("owned result: %v", err)
	}
	next := checkpoint
	next.Sequence = 12
	changes := []domain.RepositoryViewSourceEntry{
		{Sequence: 9, Kind: "git_tree", Key: entries[0].Key, Deleted: true},
		{Sequence: 11, Kind: "git_observation", Key: "later", Value: json.RawMessage(`{"observation":{}}`)},
		{Sequence: 12, Kind: "git_observation", Key: "later", Value: json.RawMessage(`{"observation":{"version":2}}`)},
	}
	delta := &graphCapturePages{pages: []domain.RepositoryViewSourcePage{graphCapturePage(next, "delta", changes[:1], "delta-next"), graphCapturePage(next, "delta", changes[1:], "")}}
	updated, err := CaptureGraphSource(ctx, delta, checkpoint.RepoID, &captured)
	if err != nil || updated.Checkpoint() != next || !reflect.DeepEqual(delta.requests[0].After, &checkpoint) {
		t.Fatalf("delta: %+v %v", updated.Checkpoint(), err)
	}
	fresh := &graphCapturePages{pages: []domain.RepositoryViewSourcePage{graphCapturePage(next, "baseline", []domain.RepositoryViewSourceEntry{changes[2], entries[1], entries[2]}, "")}}
	rebuilt, err := CaptureGraphSource(ctx, fresh, checkpoint.RepoID, nil)
	if err != nil || !reflect.DeepEqual(updated, rebuilt) {
		t.Fatalf("delta differs from baseline: %v\n%+v\n%+v", err, updated, rebuilt)
	}
	if captured.Checkpoint() != checkpoint || captured.entries[0].Kind != "git_tree" {
		t.Fatal("delta mutated baseline")
	}
	empty := &graphCapturePages{pages: []domain.RepositoryViewSourcePage{graphCapturePage(next, "delta", nil, "")}}
	unchanged, err := CaptureGraphSource(ctx, empty, next.RepoID, &updated)
	if err != nil || !reflect.DeepEqual(unchanged, updated) {
		t.Fatalf("empty delta: %v", err)
	}
}

func TestCaptureGraphSourceRejectsIncompleteAndMixedPages(t *testing.T) {
	for _, name := range []string{"missing-final", "missing-checkpoint", "wrong-checkpoint", "changed-version", "changed-epoch", "changed-mode", "changed-repo", "changed-scope", "replayed-row", "replayed-cursor", "empty-nonfinal", "both-cursors", "future-row", "unknown-kind", "baseline-tombstone", "array", "huge-key", "huge-value"} {
		t.Run(name, func(t *testing.T) {
			checkpoint, entries := graphCaptureFixture()
			first := graphCapturePage(checkpoint, "baseline", entries[:1], "next")
			last := graphCapturePage(checkpoint, "baseline", entries[1:], "")
			switch name {
			case "missing-checkpoint":
				last.Checkpoint = nil
			case "wrong-checkpoint":
				last.Checkpoint.Sequence++
			case "changed-version":
				last.Through++
			case "changed-epoch":
				last.Epoch = "22345678-abcd-4321-9876-123456789abc"
			case "changed-mode":
				last.Mode = "delta"
			case "changed-repo":
				last.RepoID = domain.HashContent([]byte("other"))
			case "changed-scope":
				last.Scope = domain.RepositoryViewSourceScope
			case "replayed-row":
				last.Entries = entries
			case "replayed-cursor":
				last.NextCursor, last.Checkpoint = "next", nil
			case "empty-nonfinal":
				first.Entries = nil
			case "both-cursors":
				first.Checkpoint = &checkpoint
			case "future-row":
				last.Entries[0].Sequence = 8
			case "unknown-kind":
				last.Entries[0].Kind = "other"
			case "baseline-tombstone":
				last.Entries[0].Deleted, last.Entries[0].Value = true, nil
			case "array":
				last.Entries[0].Value = json.RawMessage(`[]`)
			case "huge-key":
				last.Entries[0].Key = strings.Repeat("z", domain.MaxRepositoryViewSourceKeyBytes+1)
			case "huge-value":
				last.Entries[0].Value = append(json.RawMessage(`{"large":"`), []byte(strings.Repeat("z", domain.MaxRepositoryViewSourceImageBytes))...)
				last.Entries[0].Value = append(last.Entries[0].Value, '"', '}')
			}
			pages := []domain.RepositoryViewSourcePage{first, last}
			if name == "missing-final" {
				pages = pages[:1]
			}
			got, err := CaptureGraphSource(context.Background(), &graphCapturePages{pages: pages}, checkpoint.RepoID, nil)
			if err == nil || got.checkpoint.Epoch != "" || got.entries != nil {
				t.Fatalf("incomplete/mixed result exposed: %+v %v", got, err)
			}
		})
	}
}

func TestCaptureGraphSourceRejectsResetAndCancellation(t *testing.T) {
	checkpoint, entries := graphCaptureFixture()
	captured, err := CaptureGraphSource(context.Background(), &graphCapturePages{pages: []domain.RepositoryViewSourcePage{graphCapturePage(checkpoint, "baseline", entries, "")}}, checkpoint.RepoID, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, resetErr := range []error{domain.ErrCatalogResetRequired, context.DeadlineExceeded} {
		got, err := CaptureGraphSource(context.Background(), &graphCapturePages{err: resetErr}, checkpoint.RepoID, &captured)
		if !errors.Is(err, resetErr) || got.checkpoint.Epoch != "" {
			t.Fatalf("reset replaced with partial state: %v", err)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	storage := &graphCapturePages{}
	if _, err := CaptureGraphSource(ctx, storage, checkpoint.RepoID, nil); !errors.Is(err, context.Canceled) || len(storage.requests) != 0 {
		t.Fatalf("canceled capture read source: %v", err)
	}
	if _, _, _, err := captured.Inputs(ctx); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
}

func TestCaptureGraphSourceRejectsInvalidDelta(t *testing.T) {
	checkpoint, entries := graphCaptureFixture()
	previous, err := CaptureGraphSource(context.Background(), &graphCapturePages{pages: []domain.RepositoryViewSourcePage{graphCapturePage(checkpoint, "baseline", entries, "")}}, checkpoint.RepoID, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"baseline-response", "same-sequence", "reverse-sequence", "changed-epoch", "earlier-checkpoint", "tombstone-payload", "wrong-previous-repo"} {
		t.Run(name, func(t *testing.T) {
			next := checkpoint
			next.Sequence += 2
			changes := []domain.RepositoryViewSourceEntry{
				{Sequence: next.Sequence - 1, Kind: "git_tree", Key: "b", Value: json.RawMessage(`{}`)},
				{Sequence: next.Sequence, Kind: "git_tree", Key: "a", Value: json.RawMessage(`{}`)},
			}
			mode := "delta"
			basis := previous
			switch name {
			case "baseline-response":
				mode = "baseline"
			case "same-sequence":
				changes[0].Sequence = checkpoint.Sequence
			case "reverse-sequence":
				changes[0].Sequence, changes[1].Sequence = changes[1].Sequence, changes[0].Sequence
			case "changed-epoch":
				next.Epoch = "22345678-abcd-4321-9876-123456789abc"
			case "earlier-checkpoint":
				next.Sequence = checkpoint.Sequence - 1
			case "tombstone-payload":
				changes[0].Deleted = true
			case "wrong-previous-repo":
				basis.checkpoint.RepoID = domain.HashContent([]byte("other"))
			}
			storage := &graphCapturePages{pages: []domain.RepositoryViewSourcePage{graphCapturePage(next, mode, changes, "")}}
			got, err := CaptureGraphSource(context.Background(), storage, checkpoint.RepoID, &basis)
			if err == nil || got.entries != nil || got.Checkpoint().Epoch != "" {
				t.Fatalf("accepted invalid delta: %+v %v", got.Checkpoint(), err)
			}
		})
	}
}

type graphSourceLargeCursors struct {
	checkpoint domain.RepositoryViewSourceCheckpoint
	reads      int
}

func (source *graphSourceLargeCursors) RepositoryGraphSourceChanges(_ context.Context, _ domain.ContentHash, _ domain.RepositoryViewSourceRequest) (domain.RepositoryViewSourcePage, error) {
	source.reads++
	key := fmt.Sprintf("%08d", source.reads)
	return graphCapturePage(source.checkpoint, "baseline", []domain.RepositoryViewSourceEntry{{Kind: "git_tree", Key: key, Value: json.RawMessage(`{}`)}}, key+strings.Repeat("x", (16<<10)-len(key))), nil
}

func TestCaptureGraphSourceBoundsRetainedCursors(t *testing.T) {
	checkpoint, _ := graphCaptureFixture()
	storage := &graphSourceLargeCursors{checkpoint: checkpoint}
	got, err := CaptureGraphSource(context.Background(), storage, checkpoint.RepoID, nil)
	if !errors.Is(err, domain.ErrRepositoryViewSourceLimit) || got.entries != nil || storage.reads > domain.MaxPendingProjectionInputBytes/(16<<10)+1 {
		t.Fatalf("cursor memory escaped budget: reads=%d error=%v", storage.reads, err)
	}
}

func TestCaptureGraphSourceMatchesExactSourcePageLimit(t *testing.T) {
	checkpoint, _ := graphCaptureFixture()
	entries := []domain.RepositoryViewSourceEntry{}
	for index := range 4 {
		key := fmt.Sprintf("sha256:%064x", index)
		size := domain.MaxRepositoryViewSourcePageBytes/4 - len(key) - 128
		raw := json.RawMessage(`{"value":"` + strings.Repeat("x", size-len(`{"value":""}`)) + `"}`)
		entries = append(entries, domain.RepositoryViewSourceEntry{Kind: "snapshot", Key: key, Value: raw})
	}
	storage := &graphCapturePages{pages: []domain.RepositoryViewSourcePage{graphCapturePage(checkpoint, "baseline", entries, "")}}
	got, err := CaptureGraphSource(context.Background(), storage, checkpoint.RepoID, nil)
	if err != nil || len(got.entries) != 4 {
		t.Fatalf("source's valid 4 MiB page rejected: %v", err)
	}
}

func TestCaptureGraphSourceChecksSizeBeforeJSON(t *testing.T) {
	checkpoint, _ := graphCaptureFixture()
	entry := domain.RepositoryViewSourceEntry{Kind: "git_tree", Key: "tree", Value: bytes.Repeat([]byte("!"), domain.MaxRepositoryViewSourceImageBytes+1)}
	storage := &graphCapturePages{pages: []domain.RepositoryViewSourcePage{graphCapturePage(checkpoint, "baseline", []domain.RepositoryViewSourceEntry{entry}, "")}}
	if _, err := CaptureGraphSource(context.Background(), storage, checkpoint.RepoID, nil); !errors.Is(err, domain.ErrRepositoryViewSourceLimit) {
		t.Fatalf("invalid oversized JSON parsed before resource check: %v", err)
	}
}
