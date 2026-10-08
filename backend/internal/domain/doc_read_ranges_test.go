package domain

import (
	"context"
	"errors"
	"reflect"
	"sync"
	"testing"
)

func TestReadPlanRangesPreserveLocationsAndLegacySequences(t *testing.T) {
	for name, plan := range metadataReadPlans(t) {
		t.Run(name, func(t *testing.T) {
			full, err := plan.Build(nil)
			if err != nil {
				t.Fatal(err)
			}
			want := full
			want.Events = append([]DocEventIndex{}, full.Events...)
			for i := range want.Events {
				want.Events[i].Role, want.Events[i].Text = "", ""
			}
			got, err := plan.BuildRangesContext(context.Background())
			if err != nil || !reflect.DeepEqual(got, want) {
				t.Fatal("range projection differs beyond role/text", err)
			}
			for i, seq := range []int{0, 7, 10, 20, 30, 31, 40} {
				if got.Events[i].Seq != seq {
					t.Fatalf("sequence at %d = %d, want %d", i, got.Events[i].Seq, seq)
				}
			}
			// Outputs from concurrent projections must not alias each other or the
			// plan; a later role/search projection must retain its original data.
			var wg sync.WaitGroup
			for range 4 {
				wg.Go(func() {
					for range 3 {
						out, err := plan.BuildRangesContext(context.Background())
						if err != nil || !reflect.DeepEqual(out, want) {
							t.Error("shared range plan changed", err)
							return
						}
						out.Envelope.SourceModels[0] = "caller mutation"
						out.Events[0] = DocEventIndex{Seq: -1, Role: "caller mutation"}
					}
				})
			}
			wg.Wait()
			again, err := plan.Build(nil)
			if err != nil || !reflect.DeepEqual(again, full) {
				t.Fatal("range output mutation changed full projection", err)
			}
		})
	}
}

func TestReadPlanRangesCancellation(t *testing.T) {
	for name, plan := range metadataReadPlans(t) {
		t.Run(name, func(t *testing.T) {
			checks := 0
			want, err := plan.BuildRangesContext(canonicalAdmissionContext{Context: context.Background(), check: func() { checks++ }})
			if err != nil || checks <= plan.EventCount() {
				t.Fatalf("missing range cancellation checks: %d, %v", checks, err)
			}
			for at := 1; at <= checks; at++ {
				ctx, cancel := context.WithCancel(context.Background())
				calls := 0
				got, err := plan.BuildRangesContext(canonicalAdmissionContext{Context: ctx, check: func() {
					calls++
					if calls == at {
						cancel()
					}
				}})
				cancel()
				if !errors.Is(err, context.Canceled) || !reflect.DeepEqual(got, DocReadIndex{}) {
					t.Fatalf("checkpoint %d returned partial ranges: %v", at, err)
				}
			}
			again, err := plan.BuildRangesContext(context.Background())
			if err != nil || !reflect.DeepEqual(again, want) {
				t.Fatal("cancellation mutated range plan", err)
			}
		})
	}
}

func TestReadPlanRangesEmptyAndUnverified(t *testing.T) {
	if _, err := (DocReadPlan{}).BuildRangesContext(context.Background()); !errors.Is(err, ErrIntegrity) {
		t.Fatal("zero range plan accepted", err)
	}
	cir := CIRDocument{Envelope: CIREnvelope{CIRVersion: "2"}, Events: []CIREvent{}}
	raw, err := CanonicalBytes(cir)
	if err != nil {
		t.Fatal(err)
	}
	legacy, err := VerifySessionDoc(SessionDoc{Hash: HashContent(raw), CIR: cir})
	if err != nil {
		t.Fatal(err)
	}
	manifest, bodies, err := ConversationManifestForCIR(cir)
	if err != nil {
		t.Fatal(err)
	}
	var verifier CanonicalDocVerifier
	root, err := verifier.VerifyConversationManifestDoc(context.Background(), manifestProofHash(t, manifest), manifest, manifestProofLoad(bodies))
	if err != nil {
		t.Fatal(err)
	}
	for name, doc := range map[string]VerifiedSessionDoc{"legacy": legacy, "root": root} {
		t.Run(name, func(t *testing.T) {
			plan, err := doc.PlanReadIndexContext(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			got, err := plan.BuildRangesContext(context.Background())
			if err != nil || got.Hash != doc.Hash() || got.Version != 1 || got.Events == nil || len(got.Events) != 0 {
				t.Fatal("empty range projection changed", err)
			}
			ctx, cancel := context.WithCancel(context.Background())
			cancel()
			if got, err := plan.BuildRangesContext(ctx); !errors.Is(err, context.Canceled) || !reflect.DeepEqual(got, DocReadIndex{}) {
				t.Fatal("empty range plan ignored cancellation", err)
			}
		})
	}
}
