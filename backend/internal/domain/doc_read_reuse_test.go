package domain

import (
	"errors"
	"reflect"
	"strings"
	"testing"
)

func TestReadPlanReusesTextWithoutChangingEventLocations(t *testing.T) {
	cir := CIRDocument{Events: []CIREvent{
		{Seq: 4, Kind: EventReasoning, Role: RoleAssistant, RedactedSummary: "summary, with \"quotes\" and \ud55c\uad6d\uc5b4"},
		{Seq: 1, Kind: EventMessage, Role: RoleUser, Blocks: []ContentBlock{{Type: "text", Text: "text } ], and \\ escapes"}}},
		{Seq: 2, Kind: EventToolResult, Role: RoleAssistant},
	}}
	raw, err := CanonicalBytes(cir)
	if err != nil {
		t.Fatal(err)
	}
	v, err := VerifySessionDoc(SessionDoc{Hash: HashContent(raw), CIR: cir})
	if err != nil {
		t.Fatal(err)
	}
	plan, err := v.PlanReadIndex()
	if err != nil {
		t.Fatal(err)
	}
	full, err := plan.Build(nil)
	if err != nil {
		t.Fatal(err)
	}
	keys := plan.EventHashes()
	known := map[ContentHash]bool{keys[0]: true, keys[2]: true}
	keys[0] = HashContent([]byte("caller mutation"))
	got, err := plan.Build(known)
	if err != nil {
		t.Fatal(err)
	}
	for i, ev := range full.Events {
		if known[ev.Hash] {
			ev.Text = ""
		}
		if got.Events[i] != ev {
			t.Fatalf("reused event lost metadata: %+v != %+v", got.Events[i], ev)
		}
	}
	again, err := plan.Build(nil)
	if err != nil || !reflect.DeepEqual(again, full) {
		t.Fatal("reuse changed full projection", err)
	}
	if HashContent(v.Bytes()) != full.Hash {
		t.Fatal("projection changed archive")
	}
	if _, err := (VerifiedSessionDoc{}).PlanReadIndex(); !errors.Is(err, ErrIntegrity) {
		t.Fatal("unverified plan accepted")
	}
	if _, err := (DocReadPlan{}).Build(nil); !errors.Is(err, ErrIntegrity) {
		t.Fatal("zero plan accepted")
	}
}

func BenchmarkReadPlanInheritedPrefix(b *testing.B) {
	cir := CIRDocument{Events: []CIREvent{}}
	for i := 0; i < 16; i++ {
		cir.Events = append(cir.Events, CIREvent{Seq: i, Kind: EventMessage, Role: RoleUser, Blocks: []ContentBlock{{Type: "text", Text: strings.Repeat("synthetic seed, ", 65536)}}})
	}
	raw, err := CanonicalBytes(cir)
	if err != nil {
		b.Fatal(err)
	}
	v, err := VerifySessionDoc(SessionDoc{Hash: HashContent(raw), CIR: cir})
	if err != nil {
		b.Fatal(err)
	}
	plan, err := v.PlanReadIndex()
	if err != nil {
		b.Fatal(err)
	}
	known := map[ContentHash]bool{}
	for _, hash := range plan.EventHashes() {
		known[hash] = true
	}
	for _, reuse := range []bool{false, true} {
		name := "cold"
		if reuse {
			name = "shared-prefix"
		}
		b.Run(name, func(b *testing.B) {
			b.ReportAllocs()
			b.SetBytes(int64(len(raw)))
			for b.Loop() {
				p, err := v.PlanReadIndex()
				if err != nil {
					b.Fatal(err)
				}
				cache := map[ContentHash]bool(nil)
				if reuse {
					cache = known
				}
				if _, err := p.Build(cache); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}
