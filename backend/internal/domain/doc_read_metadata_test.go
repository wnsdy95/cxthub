package domain

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
)

func metadataReadPlans(t *testing.T) map[string]DocReadPlan {
	t.Helper()
	cir := CIRDocument{Envelope: CIREnvelope{CIRVersion: "2", SourceModels: []string{"synthetic"}}, Events: []CIREvent{
		{Seq: 40, Kind: EventCompaction, ReplacementComplete: true, Replacement: []CIREvent{
			{Seq: 999, Kind: EventMessage, Role: RoleUser, Blocks: []ContentBlock{{Type: "text", Text: "nested payload"}}},
		}},
		{Seq: 0, Kind: EventTurn, Role: RoleUser},
		{Seq: 7, Kind: EventMessage, Role: Role("custom-\uac80\uc0c9-\U0001f642\t\"\\" + strings.Repeat("role", 1024)), Blocks: []ContentBlock{
			{Type: "text", Text: "needle \uac80\uc0c9 \U0001f642"}, {Type: "text", Text: "second line"},
		}},
		{Seq: 10, Kind: EventToolCall, CallID: "c", ToolName: "synthetic", Input: map[string]any{"seq": 999, "role": "nested", "payload": strings.Repeat("tool needle", 100)}},
		{Seq: 20, Kind: EventToolResult, CallID: "c", Output: []any{map[string]any{"role": "user", "seq": 999, "text": "tool needle"}}},
		{Seq: 30, Kind: EventReasoning, RedactedSummary: "summary needle"},
		{Seq: 31, Kind: EventMessage, Role: "", Blocks: []ContentBlock{}},
	}}
	raw, err := CanonicalBytes(cir)
	if err != nil {
		t.Fatal(err)
	}
	legacy, err := VerifySessionDoc(SessionDoc{Hash: HashContent(raw), CIR: cir})
	if err != nil {
		t.Fatal(err)
	}
	chunks, ok := PlanDocChunks(raw)
	if !ok {
		t.Fatal("fixture is not chunkable")
	}
	var verifier CanonicalDocVerifier
	chunked, err := verifier.VerifyChunks(context.Background(), HashContent(raw), chunks.Manifest, chunkVerifierLoad(chunks))
	if err != nil {
		t.Fatal(err)
	}
	manifest, bodies, err := ConversationManifestForCIR(cir)
	if err != nil {
		t.Fatal(err)
	}
	root, err := verifier.VerifyConversationManifestDoc(context.Background(), manifestProofHash(t, manifest), manifest, manifestProofLoad(bodies))
	if err != nil {
		t.Fatal(err)
	}
	plans := map[string]DocReadPlan{}
	for name, doc := range map[string]VerifiedSessionDoc{"legacy": legacy, "chunked": chunked, "root": root} {
		plan, err := doc.PlanReadIndexContext(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		plans[name] = plan
	}
	return plans
}

func TestReadPlanBuildCancellation(t *testing.T) {
	for name, plan := range metadataReadPlans(t) {
		known := map[ContentHash]bool{}
		for _, hash := range plan.EventHashes() {
			known[hash] = true
		}
		for mode, build := range map[string]func(context.Context) (DocReadIndex, error){
			"search": func(ctx context.Context) (DocReadIndex, error) { return plan.BuildContext(ctx, nil) },
			"reused": func(ctx context.Context) (DocReadIndex, error) { return plan.BuildContext(ctx, known) },
		} {
			t.Run(name+"/"+mode, func(t *testing.T) {
				checks := 0
				want, err := build(canonicalAdmissionContext{Context: context.Background(), check: func() { checks++ }})
				if err != nil || checks <= plan.EventCount() {
					t.Fatalf("missing in-build cancellation checks: %d, %v", checks, err)
				}
				for at := 1; at <= checks; at++ {
					ctx, cancel := context.WithCancel(context.Background())
					calls := 0
					got, err := build(canonicalAdmissionContext{Context: ctx, check: func() {
						calls++
						if calls == at {
							cancel()
						}
					}})
					cancel()
					if !errors.Is(err, context.Canceled) || !reflect.DeepEqual(got, DocReadIndex{}) {
						t.Fatalf("checkpoint %d returned a partial projection: %v", at, err)
					}
				}
				again, err := build(context.Background())
				if err != nil || !reflect.DeepEqual(again, want) {
					t.Fatal("cancellation mutated plan", err)
				}
			})
		}
	}
}
