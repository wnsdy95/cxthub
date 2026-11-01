package app

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/wnsdy95/cxthub/backend/internal/adapters/store"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

type memoryBundleFactory struct{}

func (memoryBundleFactory) BuildGraphSourceReader(ctx context.Context, checkpoint domain.RepositoryViewSourceCheckpoint, metadata domain.Repo, entries []domain.RepositoryViewSourceEntry) (outbound.GraphEvidenceReader, error) {
	return store.NewGraphSourceReader(ctx, checkpoint, metadata, entries)
}

func memoryBundleFixture(test *testing.T, branch bool) (*CapturedEffectiveMemory, domain.EffectiveMemoryRequest) {
	test.Helper()
	ctx := context.Background()
	checkpoint, entries := graphCaptureFixture()
	entries = entries[1:]
	entries[1].Value = bytes.Replace(entries[1].Value, []byte(`"git_remote_url_supported":true`), []byte(`"git_remote_url_supported":true,"git_remote_url":"https://github.com/example/frozen"`), 1)
	snapshot := domain.Snapshot{ID: hh(test.Name()), RepoID: checkpoint.RepoID, Branch: "main"}
	snapshot.DocHash = snapshot.ID
	digest := domain.MemoryDigest{SnapshotID: snapshot.ID, Summary: strings.Repeat("memory \ud55c\uae00 <>&\n", 7000), KeyFacts: []string{"one", "two"}, OpenTasks: []string{"three"}}
	raw, err := json.Marshal(digest)
	if err != nil {
		test.Fatal(err)
	}
	snapshot.MemoryHash = domain.HashContent(raw)
	appendEntry := func(kind, key string, value any) {
		encoded, err := json.Marshal(value)
		if err != nil {
			test.Fatal(err)
		}
		entries = append(entries, domain.RepositoryViewSourceEntry{Kind: kind, Key: key, Sequence: 1, Value: encoded})
	}
	appendEntry("snapshot", string(snapshot.ID), snapshot)
	appendEntry("ref", `["branch","main"]`, domain.Ref{RepoID: checkpoint.RepoID, Kind: domain.RefBranch, Name: "main", Target: snapshot.ID})
	appendEntry("revision", string(checkpoint.RepoID), domain.RepositoryRevision{Graph: 4, Pending: 2})
	slices.SortFunc(entries, func(left, right domain.RepositoryViewSourceEntry) int {
		if ordered := strings.Compare(left.Kind, right.Kind); ordered != 0 {
			return ordered
		}
		return strings.Compare(left.Key, right.Key)
	})
	graph := CapturedGraphSource{checkpoint: checkpoint, entries: entries}
	metadata, view, proof, err := graph.Inputs(ctx)
	if err != nil {
		test.Fatal(err)
	}
	evidence, err := (memoryBundleFactory{}).BuildGraphSourceReader(ctx, checkpoint, metadata, proof)
	if err != nil {
		test.Fatal(err)
	}
	request := domain.EffectiveMemoryRequest{Selection: domain.EffectiveMemorySelection{SnapshotID: snapshot.ID, CodeCommit: effectiveOID(3)}, Limit: 1}
	if branch {
		request.Selection.Branch = "main"
	}
	reader := &capturedMemoryReader{repo: checkpoint.RepoID, view: view, pinsCaptured: branch, objects: map[domain.ContentHash]json.RawMessage{snapshot.MemoryHash: raw}}
	generation := domain.RepositoryMemorySourceGeneration{Version: 1, Graph: checkpoint, Epoch: "23456789-abcd-4321-9876-123456789abc", Sequence: 3}
	return &CapturedEffectiveMemory{generation: generation, graph: graph, selection: request.Selection, reader: reader, evidence: evidence}, request
}

func TestMemoryCaptureBundleRoundTrip(test *testing.T) {
	ctx := context.Background()
	for _, branch := range []bool{false, true} {
		captured, request := memoryBundleFixture(test, branch)
		want, err := captured.Project(ctx, request)
		if err != nil || want.NextCursor == "" {
			test.Fatalf("fixture needs continuation, branch=%v: %v", branch, err)
		}
		bundle, err := captured.Export(ctx)
		if err != nil {
			test.Fatal(err)
		}
		var memoryChunks int
		for _, part := range bundle.Parts {
			if part.Kind == "memory" {
				memoryChunks++
			}
		}
		if memoryChunks < 2 {
			test.Fatal("fixture must persist multiple memory blocks")
		}
		restored, err := RestoreCapturedEffectiveMemory(ctx, bundle, memoryBundleFactory{})
		if err != nil {
			test.Fatal(err)
		}
		for pageIndex := 0; ; pageIndex++ {
			got, err := restored.Project(ctx, request)
			if err != nil || !reflect.DeepEqual(got, want) {
				test.Fatalf("page %d mismatch: %v", pageIndex, err)
			}
			if got.NextCursor == "" {
				break
			}
			request.Cursor, request.Limit = got.NextCursor, 2
			want, err = captured.Project(ctx, request)
			if err != nil || pageIndex > 100 {
				test.Fatal("page progression", err)
			}
		}
		again, err := restored.Export(ctx)
		if err != nil || !reflect.DeepEqual(again, bundle) {
			test.Fatal("re-export changed immutable bundle", err)
		}
		for _, part := range bundle.Parts {
			if len(part.Value) != 0 {
				part.Value[0] ^= 1
			}
		}
		got, err := restored.Project(ctx, request)
		if err != nil || !reflect.DeepEqual(got, want) {
			test.Fatal("caller changed restored bytes", err)
		}
		original, err := captured.Export(ctx)
		if err != nil || original.ID != again.ID {
			test.Fatal("export leaked original captured bytes", err)
		}
	}
}

func TestMemoryCaptureBundleRejectsIncompleteAndForgedInputs(test *testing.T) {
	for _, scenario := range []string{"missing-memory", "missing-repository", "bad-logical-hash", "wrong-pin-repo", "changed-root", "unknown-memory-field"} {
		test.Run(scenario, func(test *testing.T) {
			ctx := context.Background()
			captured, _ := memoryBundleFixture(test, scenario == "wrong-pin-repo")
			bundle, err := captured.Export(ctx)
			if err != nil {
				test.Fatal(err)
			}
			switch scenario {
			case "missing-memory":
				bundle.Parts = slices.DeleteFunc(bundle.Parts, func(part domain.MemoryCapturePart) bool { return part.Kind == "memory" })
			case "missing-repository":
				bundle.Parts = slices.DeleteFunc(bundle.Parts, func(part domain.MemoryCapturePart) bool { return part.Kind == "graph:repository" })
			case "bad-logical-hash":
				for index := range bundle.Parts {
					if bundle.Parts[index].Kind == "memory" {
						bundle.Parts[index].Value = bytes.Replace(bundle.Parts[index].Value, []byte("memory"), []byte("forged"), 1)
					}
				}
			case "wrong-pin-repo":
				pin := (domain.PRContributionMemory{RepoID: hh("foreign"), ContributionID: strings.Repeat("a", 64), PublicationID: strings.Repeat("b", 32), MemoryPinned: true}).WithID()
				raw, err := json.Marshal(pin)
				if err != nil {
					test.Fatal(err)
				}
				bundle.Parts = append(bundle.Parts, domain.MemoryCapturePart{Kind: "pr_pin", Key: pin.ID, Value: raw})
			case "changed-root":
				bundle.ID = hh("foreign-capture")
			case "unknown-memory-field":
				for index := range bundle.Parts {
					if bundle.Parts[index].Kind == "memory" && strings.HasSuffix(bundle.Parts[index].Key, ":00000000") {
						bundle.Parts[index].Value = bytes.Replace(bundle.Parts[index].Value, []byte("snapshot_id"), []byte("unknown_key"), 1)
					}
				}
			}
			if scenario != "changed-root" {
				bundle, err = domain.SealMemoryCapture(ctx, bundle.Header, bundle.Parts)
				if err != nil {
					return
				}
			}
			if restored, err := RestoreCapturedEffectiveMemory(ctx, bundle, memoryBundleFactory{}); err == nil || restored != nil {
				test.Fatal("restored incomplete or invalid inputs", err)
			}
		})
	}
}

func TestMemoryCaptureBundleCancellationAndScope(test *testing.T) {
	captured, request := memoryBundleFixture(test, false)
	bundle, err := captured.Export(context.Background())
	if err != nil {
		test.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := captured.Export(ctx); !errors.Is(err, context.Canceled) {
		test.Fatal("export ignored cancellation", err)
	}
	if got, err := RestoreCapturedEffectiveMemory(ctx, bundle, memoryBundleFactory{}); got != nil || !errors.Is(err, context.Canceled) {
		test.Fatal("restore ignored cancellation", err)
	}
	if got, err := RestoreCapturedEffectiveMemory(context.Background(), bundle, nil); got != nil || !errors.Is(err, domain.ErrCatalogUnsupported) {
		test.Fatal("restore accepted missing proof factory", err)
	}
	restored, err := RestoreCapturedEffectiveMemory(context.Background(), bundle, memoryBundleFactory{})
	if err != nil {
		test.Fatal(err)
	}
	request.Selection.CodeCommit = effectiveOID(4)
	if _, err := restored.Project(context.Background(), request); !errors.Is(err, domain.ErrValidation) {
		test.Fatal("restored capture allowed a new code position", err)
	}
}

func TestMemoryCaptureBundlePreservesAbsenceObservation(test *testing.T) {
	ctx := context.Background()
	captured, _ := memoryBundleFixture(test, false)
	absent := hh("observed absent memory")
	captured.reader.objects[absent] = nil
	bundle, err := captured.Export(ctx)
	if err != nil {
		test.Fatal(err)
	}
	restored, err := RestoreCapturedEffectiveMemory(ctx, bundle, memoryBundleFactory{})
	if err != nil {
		test.Fatal(err)
	}
	if _, err := restored.reader.GetMemory(ctx, captured.reader.repo, absent); !errors.Is(err, domain.ErrNotFound) {
		test.Fatal("lost absence observation", err)
	}
	if _, err := restored.reader.GetMemory(ctx, captured.reader.repo, hh("never captured")); !errors.Is(err, domain.ErrIntegrity) {
		test.Fatal("uncaptured input became observed absence", err)
	}
}

func TestMemoryCaptureBundleEmptyObjectIsNotAbsence(test *testing.T) {
	ctx := context.Background()
	captured, _ := memoryBundleFixture(test, false)
	absent := hh("observed absent memory")
	captured.reader.objects[absent] = nil
	bundle, err := captured.Export(ctx)
	if err != nil {
		test.Fatal(err)
	}
	for index := range bundle.Parts {
		if bundle.Parts[index].Kind == "absent" {
			bundle.Parts[index].Kind = "memory"
			bundle.Parts[index].Key += ":00000000"
		}
	}
	malformed, err := domain.SealMemoryCapture(ctx, bundle.Header, bundle.Parts)
	if err == nil {
		if restored, err := RestoreCapturedEffectiveMemory(ctx, malformed, memoryBundleFactory{}); restored != nil || !errors.Is(err, domain.ErrIntegrity) {
			test.Fatal("empty object was accepted as an absence observation", err)
		}
	} else if !errors.Is(err, domain.ErrIntegrity) {
		test.Fatal("empty object did not fail integrity validation", err)
	}
}
