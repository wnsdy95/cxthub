package domain

import (
	"reflect"
	"strings"
	"testing"
)

func TestMemorySequenceMatchesPairwiseProjection(t *testing.T) {
	inputs := []MemoryDigest{
		{SnapshotID: "A", Summary: strings.Repeat("Original memory. ", 1000), KeyFacts: []string{"retain"}, OpenTasks: []string{"old"}},
		{SnapshotID: "B", Summary: "New merged contribution", TasksAuthoritative: true, OpenTasks: []string{"new"}},
		{SnapshotID: "C", ClaimsVersion: 1, Fragments: []MemoryFragment{{SourceSnapshot: "C", Summary: "Third contribution", Claims: []MemoryClaim{{Kind: "rationale", Text: "Reason preserved"}}}}},
		{Summary: "Unattributed legacy"},
	}
	for n := 1; n <= 8; n++ {
		for seed := 0; seed < 32; seed++ {
			seq := []MemoryDigest{}
			for j := 0; j < n; j++ {
				seq = append(seq, inputs[(seed+j*j)%len(inputs)])
			}
			want := seq[0]
			for _, d := range seq[1:] {
				want = MergeDigests(want, d)
			}
			got := MergeDigestSequence(seq...)
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("changed fold semantics n=%d seed=%d", n, seed)
			}
		}
	}
}
