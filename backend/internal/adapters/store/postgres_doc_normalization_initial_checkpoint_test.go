//go:build postgres

package store

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

func TestNormalizationCheckpointPGInitialEventRetainsAdmissionBounds(test *testing.T) {
	for _, boundary := range []string{"bytes", "heap"} {
		test.Run(boundary, func(test *testing.T) {
			storage, ctx := rootProjectionIsolatedPG(test)
			rangeSpoolTempDir(test)
			repo, _ := docRangeJobPGRepo(test, storage, ctx)
			input := map[string]any{"text": "initial record"}
			if boundary == "heap" {
				for position := 0; position < 4097; position++ {
					input[fmt.Sprintf("field-%05d", position)] = []any{fmt.Sprintf("value-%05d", position), true, nil}
				}
			}
			document := domain.CIRDocument{
				Envelope: domain.CIREnvelope{CIRVersion: domain.CIRVersionV2, SourceProvider: domain.ProviderCodex,
					SessionOriginID: string(repo), Fidelity: domain.FidelityFull},
				Events: []domain.CIREvent{{Kind: domain.EventToolCall, Seq: 1, CallID: "initial", ToolName: "initial", Input: input}},
			}
			fixture := legacyByteStagePGDocument(test, storage, ctx, repo, document)
			stored, err := json.MarshalIndent(document, "", " ")
			if err != nil {
				test.Fatal(err)
			}
			if boundary == "bytes" {
				stored = bytes.Replace(stored, []byte(`"text":`), []byte(`"text":`+strings.Repeat(" ", domain.ConversationManifestChunkBytes+domain.LegacyCanonicalEncodingBytes)), 1)
			}
			work, job, _ := docNormalizationPGStartSource(test, storage, ctx, fixture, stored)
			defer work.Close()
			for quantum := 1; quantum <= legacyNormalizationParserCheckpointQuanta; quantum++ {
				if progress, err := work.Step(ctx, job); err != nil || progress != outbound.DocRangeWorkPending {
					test.Fatal("initial record failed before its checkpoint", quantum, progress, err)
				}
				resources, err := work.normalization.RetainedResources()
				if err != nil {
					test.Fatal(err)
				}
				if work.checkpoint != nil && work.checkpoint.capture != nil {
					state := work.checkpoint.next.State
					position, err := work.checkpoint.capture.ParserInputPosition(ctx)
					if err != nil || !state.Parsing() || state.Count != 0 || state.Written != 0 {
						test.Fatal("first decode checkpoint changed its processing boundary", state, err)
					}
					if boundary == "bytes" && position < domain.ConversationManifestChunkBytes ||
						boundary == "heap" && resources.HeapBytes < domain.ConversationManifestChunkBytes {
						test.Fatal("fixture missed its original admission trigger", boundary, position, resources)
					}
					normalizationParserRestartPGAwaitRoot(test, work, ctx, job, 0)
					normalizationParserRestartPGAssertObjects(test, storage, ctx, job, work, stored)
					if boundary == "bytes" {
						normalizationCheckpointPGColdReclaim(test, storage, ctx, fixture, job, work)
					}
					return
				}
				position, err := work.normalization.preparation.ParserInputPosition(ctx)
				if err != nil || position >= domain.ConversationManifestChunkBytes || resources.HeapBytes >= domain.ConversationManifestChunkBytes {
					test.Fatal("first event deferred the original byte or heap checkpoint", quantum, position, resources, err)
				}
			}
			test.Fatal("initial record exceeded the original decode checkpoint cadence")
		})
	}
}
