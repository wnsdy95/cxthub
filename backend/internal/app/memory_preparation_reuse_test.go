package app

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

func TestMemoryPreparationDirectBuildMatchesPersistedReplay(test *testing.T) {
	ctx := context.Background()
	for _, mode := range []string{"lineage", "branch", "stored"} {
		for _, content := range []string{"", "claims", "prompt"} {
			test.Run(mode+"/"+content, func(test *testing.T) {
				captured, _ := memoryBundleFixture(test, mode == "branch")
				captured.content = content
				if mode == "stored" {
					captured.selection.MemoryHash = captured.Dependencies()[0].Hash
				}
				input, err := captured.Export(ctx)
				if err != nil {
					test.Fatal(err)
				}
				replayed, err := BuildMemoryRead(ctx, input, memoryBundleFactory{})
				if err != nil {
					test.Fatal(err)
				}
				direct, err := buildCapturedMemoryRead(ctx, captured, input.ID)
				if err != nil || !reflect.DeepEqual(direct, replayed) {
					test.Fatalf("direct preparation differs from persisted replay: %v", err)
				}
				again, err := captured.Export(ctx)
				if err != nil || !reflect.DeepEqual(again, input) {
					test.Fatal("building mutated captured input", err)
				}
				if err := domain.ValidateMemoryReadBundle(ctx, direct); err != nil {
					test.Fatal(err)
				}
			})
		}
	}
}

func TestMemoryPreparationDirectBuildRejectsInvalidCapture(test *testing.T) {
	for _, scenario := range []string{"nil", "reader", "evidence", "generation", "repo", "graph", "identity", "canceled"} {
		test.Run(scenario, func(test *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			captured, _ := memoryBundleFixture(test, false)
			identity := hh("input")
			want := domain.ErrIntegrity
			switch scenario {
			case "nil":
				captured = nil
			case "reader":
				captured.reader = nil
			case "evidence":
				captured.evidence = nil
			case "generation":
				captured.generation.Version++
			case "repo":
				captured.reader.repo = hh("foreign")
			case "graph":
				captured.graph.checkpoint.Sequence++
			case "identity":
				identity = "invalid"
			case "canceled":
				cancel()
				want = context.Canceled
			}
			result, err := buildCapturedMemoryRead(ctx, captured, identity)
			if !errors.Is(err, want) || !reflect.DeepEqual(result, domain.MemoryReadBundle{}) {
				test.Fatal("invalid capture produced output", err)
			}
		})
	}
}

func TestMemoryPreparationExportRetainsObjectValidation(test *testing.T) {
	for _, scenario := range []string{"claim-version", "fragment-source", "previous-memory"} {
		test.Run(scenario, func(test *testing.T) {
			captured, request := memoryBundleFixture(test, false)
			digest := domain.MemoryDigest{SnapshotID: request.Selection.SnapshotID, Summary: "valid hash is not valid schema"}
			switch scenario {
			case "claim-version":
				digest.ClaimsVersion = domain.MemoryClaimsVersion + 1
			case "fragment-source":
				digest.Fragments = []domain.MemoryFragment{{SourceSnapshot: "invalid", Summary: "invalid source"}}
			case "previous-memory":
				digest.PreviousMemoryHash = "invalid"
			}
			replaceMemoryReadDigest(test, captured, digest)
			if _, err := captured.Export(context.Background()); !errors.Is(err, domain.ErrIntegrity) {
				test.Fatal("direct preparation lost replay schema validation", err)
			}
		})
	}
}
