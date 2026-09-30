package domain

import (
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"math/rand"
	"reflect"
	"strings"
	"testing"
)

func accumulatorTestInputs() []MemoryDigest {
	fallback := renderExtractiveFallbackSummary([]string{"\uc0ac\uc6a9\uc790 intent", "repeat"}, []string{"outcome"})
	return []MemoryDigest{
		{},
		{Summary: "opaque", KeyFacts: []string{"old", "", "old"}, OpenTasks: []string{"old task"}, ClaimsVersion: 3},
		{Summary: "OPAQUE extended", KeyFacts: []string{"new"}},
		{Summary: "different opaque", OpenTasks: []string{"replacement"}, TasksAuthoritative: true},
		{SnapshotID: "empty", Provider: ProviderCodex, ClaimsVersion: 2},
		{SnapshotID: "A", Summary: " original ", KeyFacts: []string{"old", "new"}, OpenTasks: []string{"old task"}},
		{SnapshotID: "B", Summary: "fresh", TasksAuthoritative: true, OpenTasks: []string{"replacement"}},
		{SnapshotID: "C", Summary: "ignored rendered carry", Fragments: []MemoryFragment{{SourceSnapshot: "A", Summary: " original ", KeyFacts: []string{"old", "new"}, OpenTasks: []string{"old task"}}}},
		{SnapshotID: "D", Fragments: []MemoryFragment{{SourceSnapshot: "D", Claims: []MemoryClaim{{Kind: "decision", Text: "why"}}}}},
		{SnapshotID: "E", ClaimsVersion: 1, Fragments: []MemoryFragment{{SourceSnapshot: "E", Claims: []MemoryClaim{{Kind: "code", Text: "scope", Code: &MemoryCodeScope{Commit: "sha", Paths: []string{"b", "a"}}}}}}},
		{Summary: "top level", KeyFacts: []string{}, OpenTasks: []string{}, Fragments: []MemoryFragment{{Summary: "source missing", Claims: []MemoryClaim{{Kind: "decision", Text: "dropped"}}}}},
		{SnapshotID: "F", Summary: fallback},
		{SnapshotID: "G", Summary: "baseline\n\n" + extractiveFallbackDeltaMarker + "\n" + fallback},
		{SnapshotID: "H", Summary: "baseline", PreviousMemoryHash: "previous", GraftCoverage: &MemoryGraftCoverage{ProjectionVersion: 1, ProjectionComplete: true, LineageFingerprint: "lineage", GraftSeq: 7, GraftParents: []ContentHash{"p"}, PinnedSources: []ContentHash{"s"}}},
		{SnapshotID: "I", Fragments: []MemoryFragment{{SourceSnapshot: "I", OpenTasks: []string{"after authority"}}, {SourceSnapshot: "B", Summary: "fresh", TasksAuthoritative: true, OpenTasks: []string{"replacement"}}}},
	}
}

func assertAccumulatorDigest(t *testing.T, a *MemoryAccumulator, sequence []MemoryDigest) {
	t.Helper()
	got, err := a.Digest()
	if err != nil {
		t.Fatal(err)
	}
	want := MergeDigestSequence(sequence...)
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("sequence length %d changed merge semantics\ngot: %#v\nwant: %#v\nsequence: %#v", len(sequence), got, want, sequence)
	}
}

func TestMemoryAccumulatorDifferential(t *testing.T) {
	inputs := accumulatorTestInputs()
	a := NewMemoryAccumulator(1 << 20)
	assertAccumulatorDigest(t, a, nil)
	// Every ordered pair, including invalid-source and empty-input transitions.
	for _, first := range inputs {
		for _, second := range inputs {
			a := NewMemoryAccumulator(1 << 20)
			if err := a.Add(first); err != nil {
				t.Fatal(err)
			}
			assertAccumulatorDigest(t, a, []MemoryDigest{first})
			if err := a.Add(second); err != nil {
				t.Fatal(err)
			}
			assertAccumulatorDigest(t, a, []MemoryDigest{first, second})
		}
	}
	rng := rand.New(rand.NewSource(987))
	for trial := 0; trial < 1000; trial++ {
		a := NewMemoryAccumulator(1 << 20)
		var sequence []MemoryDigest
		for i, n := 0, 1+rng.Intn(12); i < n; i++ {
			d := inputs[rng.Intn(len(inputs))]
			sequence = append(sequence, d)
			if err := a.Add(d); err != nil {
				t.Fatal(err)
			}
			assertAccumulatorDigest(t, a, sequence)
		}
	}
}

func TestMemoryAccumulatorResetReplacementBoundary(t *testing.T) {
	inputs := accumulatorTestInputs()
	for _, replacement := range inputs {
		for _, suffix := range inputs {
			a := NewMemoryAccumulator(1 << 20)
			for _, d := range inputs {
				if err := a.Add(d); err != nil {
					t.Fatal(err)
				}
			}
			if err := a.Reset(replacement); err != nil {
				t.Fatal(err)
			}
			assertAccumulatorDigest(t, a, []MemoryDigest{replacement})
			if err := a.Add(suffix); err != nil {
				t.Fatal(err)
			}
			assertAccumulatorDigest(t, a, []MemoryDigest{replacement, suffix})
			if err := a.Add(inputs[5]); err != nil {
				t.Fatal(err)
			}
			assertAccumulatorDigest(t, a, []MemoryDigest{replacement, suffix, inputs[5]})
		}
	}
}

func TestMemoryAccumulatorDuplicateArchivesRetainOneUnion(t *testing.T) {
	base := MemoryDigest{SnapshotID: "archive", Fragments: []MemoryFragment{{SourceSnapshot: "A", Summary: strings.Repeat("memory ", 1000), Claims: []MemoryClaim{{Kind: "decision", Text: "preserve reason"}}}, {SourceSnapshot: "B", Summary: "last", TasksAuthoritative: true, OpenTasks: []string{"current"}}}}
	renderMemoryFragments(&base)
	a := NewMemoryAccumulator(32 << 10)
	for i := 0; i < 206; i++ {
		d := base
		d.PreviousMemoryHash = ContentHash(fmt.Sprint(i))
		if err := a.Add(d); err != nil {
			t.Fatalf("duplicate %d: %v", i, err)
		}
	}
	if a.single != nil || len(a.fragments) != 2 || len(a.index) != 2 || a.fragmentEntries != 4 {
		t.Fatalf("retained duplicated archive: fragments=%d entries=%d index=%d", len(a.fragments), a.fragmentEntries, len(a.index))
	}
	want := MergeDigestSequence(base, base)
	want.PreviousMemoryHash = "205"
	got, err := a.Digest()
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("digest changed: %v", err)
	}
}

func TestMemoryAccumulatorFourteenMegabyteUnionWithin64MiB(t *testing.T) {
	if testing.Short() {
		t.Skip("large archive")
	}
	d := MemoryDigest{SnapshotID: "archive", Provider: ProviderCodex}
	for i := 0; i < 204; i++ {
		d.Fragments = append(d.Fragments, MemoryFragment{SourceSnapshot: ContentHash(fmt.Sprint(i)), Summary: fmt.Sprintf("%03d:", i) + strings.Repeat("x", 35190)})
	}
	renderMemoryFragments(&d)
	wire, err := json.Marshal(d)
	if err != nil {
		t.Fatal(err)
	}
	if len(wire) < 14_000_000 || len(wire) > 15_000_000 {
		t.Fatalf("wrong fixture scale: %d", len(wire))
	}
	a := NewMemoryAccumulator(64 << 20)
	// Cumulative input exceeds 64 MiB, while retained and final content do not.
	for i := 0; i < 6; i++ {
		if err := a.Add(d); err != nil {
			t.Fatalf("archive %d: %v", i, err)
		}
	}
	got, err := a.Digest()
	if err != nil {
		t.Fatal(err)
	}
	if len(a.fragments) != 204 || !reflect.DeepEqual(got, d) {
		t.Fatal("large union changed")
	}
}

func TestMemoryAccumulatorUniqueAndMetadataLimitsAreAtomic(t *testing.T) {
	a := NewMemoryAccumulator(2048)
	base := MemoryDigest{SnapshotID: "A", Summary: "keep"}
	if err := a.Add(base); err != nil {
		t.Fatal(err)
	}
	for _, d := range []MemoryDigest{
		{SnapshotID: "huge", Summary: strings.Repeat("x", 4096)},
		{SnapshotID: "B", Fragments: []MemoryFragment{{SourceSnapshot: "small", Summary: "staged"}, {SourceSnapshot: "huge", Summary: strings.Repeat("x", 4096)}}},
		{SnapshotID: "B", Provider: ProviderKind(strings.Repeat("p", 4096))},
		{SnapshotID: "B", GraftCoverage: &MemoryGraftCoverage{PinnedSources: []ContentHash{ContentHash(strings.Repeat("p", 4096))}}},
	} {
		if err := a.Add(d); !errors.Is(err, ErrMemoryProjectionLimit) {
			t.Fatalf("Add: %v", err)
		}
		assertAccumulatorDigest(t, a, []MemoryDigest{base})
		if err := a.Reset(d); !errors.Is(err, ErrMemoryProjectionLimit) {
			t.Fatalf("Reset: %v", err)
		}
		assertAccumulatorDigest(t, a, []MemoryDigest{base})
	}
	if err := a.Add(MemoryDigest{SnapshotID: "B", Summary: "next"}); err != nil {
		t.Fatal(err)
	}
	before, _ := a.Digest()
	if err := a.Add(MemoryDigest{SnapshotID: "C", Summary: strings.Repeat("x", 1600)}); !errors.Is(err, ErrMemoryProjectionLimit) {
		t.Fatalf("cumulative unique bound: %v", err)
	}
	after, _ := a.Digest()
	if !reflect.DeepEqual(before, after) {
		t.Fatal("failed staged addition changed state")
	}
}

func TestMemoryAccumulatorRenderedJSONLimit(t *testing.T) {
	for _, summary := range []string{strings.Repeat("x", 500), strings.Repeat("\ud55c", 166), strings.Repeat("<", 83)} {
		a := NewMemoryAccumulator(1024)
		d := MemoryDigest{Fragments: []MemoryFragment{{SourceSnapshot: "A", Summary: summary}}}
		if err := a.Add(d); err != nil {
			t.Fatal(err)
		}
		if err := a.Add(MemoryDigest{}); err != nil {
			t.Fatal(err)
		}
		if _, err := a.Digest(); !errors.Is(err, ErrMemoryProjectionLimit) {
			t.Fatalf("rendered/escaped overflow accepted: %v", err)
		}
	}
}

func TestMemoryAccumulatorEntryLimit(t *testing.T) {
	a := NewMemoryAccumulator(8 << 20)
	d := MemoryDigest{}
	for i := 0; i < memoryAccumulatorMaxEntries; i++ {
		d.Fragments = append(d.Fragments, MemoryFragment{SourceSnapshot: ContentHash(fmt.Sprint(i))})
	}
	if err := a.Add(d); err != nil {
		t.Fatal(err)
	}
	if err := a.Add(MemoryDigest{}); err != nil {
		t.Fatal(err)
	}
	if _, err := a.Digest(); err != nil {
		t.Fatal(err)
	}
	if err := a.Add(MemoryDigest{SnapshotID: "extra", Summary: "extra"}); !errors.Is(err, ErrMemoryProjectionLimit) {
		t.Fatalf("entry overflow: %v", err)
	}
	if len(a.fragments) != memoryAccumulatorMaxEntries {
		t.Fatal("entry overflow mutated state")
	}
	if err := a.Reset(MemoryDigest{KeyFacts: make([]string, memoryAccumulatorMaxEntries+1)}); !errors.Is(err, ErrMemoryProjectionLimit) {
		t.Fatalf("empty entry flood: %v", err)
	}
}

func TestMemoryAccumulatorExactIdentityAndCollisionBucket(t *testing.T) {
	f := MemoryFragment{SourceSnapshot: "A", Summary: "same", KeyFacts: []string{"a", "b"}, Claims: []MemoryClaim{{Kind: "code", Text: "same", Code: &MemoryCodeScope{Commit: "c", Paths: []string{"a", "b"}}}}}
	variants := []MemoryFragment{f}
	for i := 0; i < 7; i++ {
		v := cloneAccumulatorFragment(f)
		switch i {
		case 0:
			v.SourceSnapshot = "B"
		case 1:
			v.TasksAuthoritative = true
		case 2:
			v.KeyFacts = []string{"b", "a"}
		case 3:
			v.Claims[0].Kind = "decision"
		case 4:
			v.Claims[0].Code.Commit = "different"
		case 5:
			v.Claims[0].Code.Paths = []string{"b", "a"}
		case 6:
			v.Claims[0].Text = "different"
		}
		variants = append(variants, v)
	}
	a := NewMemoryAccumulator(1 << 20)
	d := MemoryDigest{Fragments: variants}
	if err := a.Add(d); err != nil {
		t.Fatal(err)
	}
	if err := a.Add(d); err != nil {
		t.Fatal(err)
	}
	if len(a.fragments) != len(variants) {
		t.Fatal("distinct provenance collapsed")
	}
	collision := MemoryFragment{SourceSnapshot: "collision", Summary: "new"}
	raw, _ := json.Marshal(collision)
	key := sha256.Sum256(raw)
	// Force a non-equal canonical value into this key's bucket. Production
	// hashing remains SHA-256; both collision and duplicate paths are exercised.
	a.index[key] = []int{0}
	for i := 0; i < 2; i++ {
		if err := a.Add(MemoryDigest{Fragments: []MemoryFragment{collision}}); err != nil {
			t.Fatal(err)
		}
	}
	if len(a.fragments) != len(variants)+1 || len(a.index[key]) != 2 {
		t.Fatal("hash collision lost or duplicated a contribution")
	}
	for _, pair := range [][2]MemoryFragment{
		{{SourceSnapshot: "A"}, {SourceSnapshot: "A", KeyFacts: []string{}, OpenTasks: []string{}, Claims: []MemoryClaim{}}},
		{{SourceSnapshot: "A", Summary: "\xff"}, {SourceSnapshot: "A", Summary: "\xfe"}},
		{{SourceSnapshot: "A", Summary: "\xff"}, {SourceSnapshot: "A", Summary: "\ufffd"}},
		{{SourceSnapshot: "A", Claims: []MemoryClaim{{Code: &MemoryCodeScope{Paths: nil}}}}, {SourceSnapshot: "A", Claims: []MemoryClaim{{Code: &MemoryCodeScope{Paths: []string{}}}}}},
	} {
		x, _ := json.Marshal(pair[0])
		y, _ := json.Marshal(pair[1])
		if accumulatorFragmentsEqual(pair[0], pair[1]) != (string(x) == string(y)) {
			t.Fatal("canonical equality differs from JSON")
		}
	}
}

func TestMemoryAccumulatorJSONCounterMatchesEncoder(t *testing.T) {
	var allBytes []byte
	for i := 0; i < 256; i++ {
		allBytes = append(allBytes, byte(i))
	}
	text := string(allBytes) + "\ud55c😀\u2028\u2029<&>\\\"\n"
	inputs := accumulatorTestInputs()
	inputs = append(inputs, MemoryDigest{SnapshotID: ContentHash(text), Summary: text, Provider: ProviderKind(text), KeyFacts: []string{"", text}, OpenTasks: []string{}, ClaimsVersion: ^uint32(0), GraftCoverage: &MemoryGraftCoverage{GraftSeq: ^uint64(0)}, Fragments: []MemoryFragment{{SourceSnapshot: ContentHash(text), Summary: text, Claims: []MemoryClaim{{Kind: text, Text: text, Code: &MemoryCodeScope{Commit: text, Paths: []string{text}}}}}}})
	for _, d := range inputs {
		raw, err := json.Marshal(d)
		if err != nil {
			t.Fatal(err)
		}
		h := sha256.New()
		s := accumulatorJSON{limit: len(raw), hash: h}
		s.value(reflect.ValueOf(d))
		if s.failed || s.n != len(raw) || string(h.Sum(nil)) != string(sha256Bytes(raw)) {
			t.Fatalf("streamed JSON differs: size %d want %d failed %v", s.n, len(raw), s.failed)
		}
		s = accumulatorJSON{limit: len(raw) - 1}
		s.value(reflect.ValueOf(d))
		if !s.failed {
			t.Fatal("exact byte limit not enforced")
		}
	}
}

func sha256Bytes(b []byte) []byte { sum := sha256.Sum256(b); return sum[:] }

func TestMemoryAccumulatorOwnsInputsAndOutputs(t *testing.T) {
	d := cloneAccumulatorDigest(accumulatorTestInputs()[9])
	d.GraftCoverage = &MemoryGraftCoverage{PinnedSources: []ContentHash{"pin"}}
	original := cloneAccumulatorDigest(d)
	a := NewMemoryAccumulator(1 << 20)
	if err := a.Add(d); err != nil {
		t.Fatal(err)
	}
	d.Fragments[0].Claims[0].Code.Paths[0] = "input mutation"
	d.GraftCoverage.PinnedSources[0] = "input mutation"
	assertAccumulatorDigest(t, a, []MemoryDigest{original})
	got, _ := a.Digest()
	got.Fragments[0].Claims[0].Code.Paths[0] = "output mutation"
	got.GraftCoverage.PinnedSources[0] = "output mutation"
	assertAccumulatorDigest(t, a, []MemoryDigest{original})
	if err := a.Add(MemoryDigest{}); err != nil {
		t.Fatal(err)
	}
	got, _ = a.Digest()
	got.Fragments[0].Claims[0].Code.Paths[0] = "union output mutation"
	assertAccumulatorDigest(t, a, []MemoryDigest{original, {}})
}

func TestMemoryAccumulatorOpaqueCarryDeduplicatesAndIgnoresDroppedText(t *testing.T) {
	a := NewMemoryAccumulator(2048)
	if err := a.Add(MemoryDigest{Summary: "base"}); err != nil {
		t.Fatal(err)
	}
	facts := make([]string, 20000)
	for i := range facts {
		facts[i] = "same"
	}
	if err := a.Add(MemoryDigest{Summary: "BASE extended", KeyFacts: facts}); err != nil {
		t.Fatal(err)
	}
	got, _ := a.Digest()
	if got.Summary != "BASE extended" || !reflect.DeepEqual(got.KeyFacts, []string{"same"}) {
		t.Fatal("legacy containment or dedup changed")
	}
	if err := a.Add(MemoryDigest{SnapshotID: "A", Summary: "attributed"}); err != nil {
		t.Fatal(err)
	}
	// This opaque text is discarded by the existing merge, so it is not retained
	// or charged. Its metadata still replaces the last input's metadata.
	if err := a.Add(MemoryDigest{Summary: strings.Repeat("unused", 10000), Provider: ProviderCodex}); err != nil {
		t.Fatal(err)
	}
	got, _ = a.Digest()
	if got.Summary != "attributed" || got.Provider != ProviderCodex {
		t.Fatal("dropped opaque contribution changed projection")
	}
}

func TestMemoryAccumulatorNonpositiveBudget(t *testing.T) {
	for _, limit := range []int{-1, 0} {
		a := NewMemoryAccumulator(limit)
		if err := a.Add(MemoryDigest{}); !errors.Is(err, ErrMemoryProjectionLimit) {
			t.Fatalf("limit %d: %v", limit, err)
		}
		if _, err := a.Digest(); !errors.Is(err, ErrMemoryProjectionLimit) {
			t.Fatalf("empty wire limit %d: %v", limit, err)
		}
	}
}
