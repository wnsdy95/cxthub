package domain

import (
	"context"
	"errors"
	"fmt"
	"testing"
)

func segmentIndex(t *testing.T, session string, words ...string) (Snapshot, DocReadIndex) {
	t.Helper()
	d := SessionDoc{CIR: CIRDocument{Envelope: CIREnvelope{SourceProvider: ProviderKind("codex"), SessionOriginID: session}}}
	for i, w := range words {
		d.CIR.Events = append(d.CIR.Events, CIREvent{Seq: i, Kind: EventMessage, Role: RoleUser, Blocks: []ContentBlock{{Type: "text", Text: w}}})
	}
	raw, err := CanonicalBytes(d.CIR)
	if err != nil {
		t.Fatal(err)
	}
	d.Hash = HashContent(raw)
	idx, err := BuildDocReadIndex(d)
	if err != nil {
		t.Fatal(err)
	}
	return Snapshot{ID: d.Hash, DocHash: d.Hash, RepoID: HashContent([]byte("repo"))}, idx
}
func TestContextSegmentOnlyDeduplicatesProvenNativePrefix(t *testing.T) {
	old, oi := segmentIndex(t, "session-a", "same words")
	now, ni := segmentIndex(t, "session-a", "same words", "same words")
	now.Parents = []ContentHash{old.ID}
	bindings := []CommitContextBinding{{EventID: "receipt"}}
	got, err := ProjectConversationSegment(context.Background(), now, ni, bindings, &old, &oi)
	if err != nil || got.Kind != "verified_prefix" || got.Segment.EventStart != 1 || got.Segment.EventEnd != 2 || got.BaselineSnapshotID != old.ID {
		t.Fatalf("prefix: %+v %v", got, err)
	}
	if len(got.Bindings[0].SegmentIDs) != 1 || got.Bindings[0].SegmentIDs[0] != got.Segment.ID {
		t.Fatal("binding omitted its range identity")
	}
	if bindings[0].SegmentIDs != nil {
		t.Fatal("mutated input binding")
	}
	for _, tc := range []struct {
		name   string
		change func(*Snapshot, *DocReadIndex)
	}{
		{"foreign session", func(_ *Snapshot, i *DocReadIndex) { i.Envelope.SessionOriginID = "session-b" }},
		{"foreign provider", func(_ *Snapshot, i *DocReadIndex) { i.Envelope.SourceProvider = "claude" }},
		{"missing native identity", func(_ *Snapshot, i *DocReadIndex) { i.Envelope.SessionOriginID = "" }},
		{"graft is not prefix evidence", func(s *Snapshot, _ *DocReadIndex) { s.GraftParents = s.Parents; s.Parents = nil }},
		{"compacted source", func(_ *Snapshot, i *DocReadIndex) {
			i.Events = append([]DocEventIndex(nil), i.Events...)
			i.Events[0].Hash = HashContent([]byte("different event"))
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, i := now, ni
			tc.change(&s, &i)
			got, err := ProjectConversationSegment(context.Background(), s, i, bindings, &old, &oi)
			if err != nil || got.Kind != "full_source" || got.Segment.EventStart != 0 {
				t.Fatalf("guessed delta: %+v %v", got, err)
			}
		})
	}
	got, err = ProjectConversationSegment(context.Background(), now, ni, nil, &old, &oi)
	if err != nil || got.Reason != "no_finalized_publication" || got.Segment.EventStart != 0 {
		t.Fatalf("unpublished source: %+v %v", got, err)
	}
	same, err := ProjectConversationSegment(context.Background(), old, oi, bindings, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	old.RepoID = HashContent([]byte("another repo"))
	other, err := ProjectConversationSegment(context.Background(), old, oi, bindings, nil, nil)
	if err != nil || same.Segment.ID == other.Segment.ID {
		t.Fatal("segment identity crossed repository provenance")
	}
}
func TestContextSegmentRejectsIndexDamageAndCancellation(t *testing.T) {
	snap, idx := segmentIndex(t, "s", "hello")
	idx.Events[0].Offset = 4
	if _, err := ProjectConversationSegment(context.Background(), snap, idx, nil, nil, nil); !errors.Is(err, ErrIntegrity) {
		t.Fatalf("bad index: %v", err)
	}
	snap, idx = segmentIndex(t, "s", "hello")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := ProjectConversationSegment(ctx, snap, idx, nil, nil, nil); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation: %v", err)
	}
}
func TestContextSegmentSelectionRequiresBoundedPinnedContinuation(t *testing.T) {
	for _, in := range []ContextSelection{{SegmentLimit: 51}, {SegmentLimit: -1}, {SegmentLimit: 1, SegmentOffset: -1}, {SegmentOffset: 1}, {SegmentLimit: 1, SegmentOffset: 1}, {SegmentLimit: 1, SegmentStateHash: "wrong"}} {
		if ValidateContextSegmentSelection(in) == nil {
			t.Fatalf("accepted %+v", in)
		}
	}
	for _, in := range []ContextSelection{{}, {SegmentLimit: 50}, {SegmentLimit: 1, SegmentOffset: 1, SegmentStateHash: HashContent([]byte("state"))}} {
		if err := ValidateContextSegmentSelection(in); err != nil {
			t.Fatalf("valid %+v: %v", in, err)
		}
	}
}

func BenchmarkContextSegmentPrefix(b *testing.B) {
	for _, n := range []int{10, 1000, 100000} {
		b.Run(fmt.Sprint(n), func(b *testing.B) {
			repo := HashContent([]byte("repo"))
			old := Snapshot{ID: HashContent([]byte("old")), RepoID: repo, DocHash: HashContent([]byte("old doc"))}
			now := Snapshot{ID: HashContent([]byte("new")), RepoID: repo, DocHash: HashContent([]byte("new doc")), Parents: []ContentHash{old.ID}}
			envelope := CIREnvelope{SourceProvider: "codex", SessionOriginID: "benchmark"}
			oi := DocReadIndex{Version: 1, Hash: old.DocHash, Envelope: envelope, Events: make([]DocEventIndex, n)}
			for i := range oi.Events {
				oi.Events[i] = DocEventIndex{Index: i, Offset: i * 11, Length: 10, Hash: HashContent([]byte(fmt.Sprint(i)))}
			}
			ni := DocReadIndex{Version: 1, Hash: now.DocHash, Envelope: envelope, Events: append([]DocEventIndex(nil), oi.Events...)}
			ni.Events = append(ni.Events, DocEventIndex{Index: n, Offset: n * 11, Length: 10, Hash: HashContent([]byte("new"))})
			bindings := []CommitContextBinding{{EventID: "receipt"}}
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				if _, err := ProjectConversationSegment(context.Background(), now, ni, bindings, &old, &oi); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}
