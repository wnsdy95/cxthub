package store

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

func streamingRootCostFixture(testingContext testing.TB, sizeMiB int) (domain.DocFinalizationJob, storedChunkReader, int64) {
	testingContext.Helper()
	document := domain.CIRDocument{Envelope: domain.CIREnvelope{CIRVersion: "1", SessionOriginID: "publication-cost", SourceProvider: domain.ProviderCodex}}
	for position := range sizeMiB * 1024 {
		text := fmt.Sprintf("event-%d ", position) + strings.Repeat("x", 1024)
		event := domain.CIREvent{Seq: position, Kind: domain.EventMessage, Role: domain.RoleUser, Blocks: []domain.ContentBlock{{Type: "text", Text: text}}}
		if position%4 != 0 {
			event.Kind, event.Role, event.Blocks = domain.EventToolResult, "", nil
			event.CallID, event.Output = fmt.Sprintf("tool-%d", position), text
		}
		document.Events = append(document.Events, event)
	}
	manifest, bodies, err := domain.ConversationManifestForCIR(document)
	if err != nil {
		testingContext.Fatal(err)
	}
	root, err := domain.CanonicalConversationManifest(manifest)
	if err != nil {
		testingContext.Fatal(err)
	}
	hash, err := domain.ConversationManifestHash(manifest)
	if err != nil {
		testingContext.Fatal(err)
	}
	repo := domain.HashContent([]byte("publication-cost-repository"))
	job, err := domain.NewDocFinalizationJobForRepresentation(repo, domain.DocumentRepresentation{Hash: hash, Identity: domain.DocumentIdentityRootV1, RootManifest: root}, time.Now())
	if err != nil {
		testingContext.Fatal(err)
	}
	stored := make(map[domain.ContentHash][]byte, len(bodies))
	for hash, body := range bodies {
		stored[hash] = docCompress(body)
	}
	read := func(ctx context.Context, hash domain.ContentHash) ([]byte, error) {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		body, exists := stored[hash]
		if !exists {
			return nil, domain.ErrNotFound
		}
		return body, nil
	}
	return job, read, manifest.StreamBytes
}

func BenchmarkStreamingRootPublicationCost(benchmark *testing.B) {
	for _, sizeMiB := range []int{4, 32} {
		benchmark.Run(fmt.Sprintf("%dMiB", sizeMiB), func(benchmark *testing.B) {
			job, read, streamBytes := streamingRootCostFixture(benchmark, sizeMiB)
			ctx := context.Background()
			for _, phase := range []string{"prepare", "compare", "range", "visible"} {
				benchmark.Run(phase, func(benchmark *testing.B) {
					prepared, err := prepareStreamingRootJob(ctx, job, &domain.CanonicalDocVerifier{}, read)
					if err != nil {
						benchmark.Fatal(err)
					}
					defer prepared.Close()
					proof, _, err := domain.BuildVerifiedDocRangeGeneration(ctx, prepared.root.ranges)
					if err != nil {
						benchmark.Fatal(err)
					}
					benchmark.SetBytes(streamBytes)
					benchmark.ReportAllocs()
					benchmark.ResetTimer()
					for benchmark.Loop() {
						switch phase {
						case "prepare":
							var result *preparedStreamingRootDoc
							result, err = prepareStreamingRootJob(ctx, job, &domain.CanonicalDocVerifier{}, read)
							if result != nil {
								if closeErr := result.Close(); closeErr != nil {
									benchmark.Fatal(closeErr)
								}
							}
						case "compare":
							err = prepared.compareCurrent(ctx, read)
						case "range":
							_, _, err = domain.BuildVerifiedDocRangeGeneration(ctx, prepared.root.ranges)
						case "visible":
							_, err = domain.BuildDocVisibleGeneration(ctx, proof, func(context.Context, domain.DocRangeObject) error { return nil })
						}
						if err != nil {
							benchmark.Fatal(err)
						}
					}
				})
			}
		})
	}
}
