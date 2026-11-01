package domain

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

func TestMemoryDigestHashBoundedMatchesWire(test *testing.T) {
	snapshot := HashContent([]byte(test.Name()))
	for _, digest := range []MemoryDigest{
		{},
		{SnapshotID: snapshot, Summary: "\ud55c\uae00 <>&\n\t\"\\\u2028\u2029\x00\xff", KeyFacts: []string{}, OpenTasks: []string{"decision"}, TasksAuthoritative: true, Provider: "codex"},
		{SnapshotID: snapshot, PreviousMemoryHash: snapshot, ClaimsVersion: 1, Fragments: []MemoryFragment{{SourceSnapshot: snapshot, Summary: "fragment", TasksAuthoritative: true, Claims: []MemoryClaim{{Kind: "code", Text: "claim", Code: &MemoryCodeScope{Commit: strings.Repeat("1", 40), Paths: []string{"a", "b"}}}}}}, GraftCoverage: &MemoryGraftCoverage{ProjectionVersion: 2, ProjectionComplete: true, LineageFingerprint: snapshot, GraftParents: []ContentHash{snapshot}, PinnedSources: []ContentHash{snapshot}, GraftSeq: 1}},
	} {
		raw, err := json.Marshal(digest)
		if err != nil {
			test.Fatal(err)
		}
		got, err := MemoryDigestHashBounded(digest, len(raw))
		if err != nil || got != HashContent(raw) {
			test.Fatalf("bounded wire hash changed: got %s want %s: %v", got, HashContent(raw), err)
		}
		for _, limit := range []int{-1, 0, 1, len(raw) - 1} {
			if hash, err := MemoryDigestHashBounded(digest, limit); hash != "" || !errors.Is(err, ErrMemoryProjectionLimit) {
				test.Fatalf("limit %d returned %s, %v", limit, hash, err)
			}
		}
	}
}

func TestMemoryDigestHashBoundedStopsBeforeEscapedExpansion(test *testing.T) {
	digest := MemoryDigest{SnapshotID: HashContent([]byte(test.Name())), Summary: strings.Repeat("<", 1<<20)}
	if hash, err := MemoryDigestHashBounded(digest, 2<<20); hash != "" || !errors.Is(err, ErrMemoryProjectionLimit) {
		test.Fatal("escaped output exceeded bound", hash, err)
	}
	result := testing.Benchmark(func(bench *testing.B) {
		bench.ReportAllocs()
		for range bench.N {
			_, _ = MemoryDigestHashBounded(digest, 2<<20)
		}
	})
	if result.AllocedBytesPerOp() > 64<<10 {
		test.Fatalf("bounded hash allocated expanded JSON: %d bytes/op", result.AllocedBytesPerOp())
	}
}
