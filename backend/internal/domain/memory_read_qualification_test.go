package domain

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"runtime"
	"slices"
	"testing"
)

func TestMemoryReadQualificationExactBinding(test *testing.T) {
	ctx := context.Background()
	items := memoryReadTestItems(2)
	header := memoryReadTestHeader(test, items)
	hash := HashContent([]byte("qualified memory"))
	header.Dependencies = []MemoryReadDependency{{Hash: hash, Available: true}}
	output := memoryReadTestBuild(test, header, items)
	qualified := MemoryReadQualification{Version: MemoryReadQualificationVersion, ReadID: output.ID, CaptureID: header.CaptureID, Source: header.Source,
		Witnesses: []MemoryObjectWitness{{RepoID: header.Source.Graph.RepoID, Hash: hash, Available: true,
			Root: &MemoryObjectVersion{Hash: hash, Incarnation: "11111111-2222-3333-4444-555555555555", Version: 9007199254740993}}}}
	if err := ValidateMemoryReadQualification(ctx, qualified, output.Header); err != nil {
		test.Fatal(err)
	}
	raw, err := MemoryReadQualificationBytes(ctx, qualified)
	if err != nil {
		test.Fatal(err)
	}
	var restored MemoryReadQualification
	if err := DecodeRepositorySourceJSON(ctx, raw, &restored); err != nil || !reflect.DeepEqual(restored, qualified) {
		test.Fatal("durable qualification round trip", err)
	}
	for _, scenario := range []string{"read", "capture", "source", "rules", "repo", "missing", "availability", "duplicate", "future", "canceled"} {
		test.Run(scenario, func(test *testing.T) {
			altered := memoryReadTestCopy(test, qualified)
			result := memoryReadTestCopy(test, output.Header)
			bound, cancel := context.WithCancel(ctx)
			defer cancel()
			switch scenario {
			case "read":
				altered.ReadID = HashContent([]byte("other"))
			case "capture":
				altered.CaptureID = HashContent([]byte("other"))
			case "source":
				altered.Source.Sequence++
			case "rules":
				result.Rules++
			case "repo":
				altered.Witnesses[0].RepoID = HashContent([]byte("foreign"))
			case "missing":
				altered.Witnesses = nil
			case "availability":
				altered.Witnesses[0].Available = false
				altered.Witnesses[0].Root = nil
			case "duplicate":
				altered.Witnesses = append(altered.Witnesses, altered.Witnesses[0])
			case "future":
				altered.Version++
			case "canceled":
				cancel()
			}
			if err := ValidateMemoryReadQualification(bound, altered, result); err == nil || scenario == "canceled" && !errors.Is(err, context.Canceled) {
				test.Fatal("invalid qualification accepted", err)
			}
		})
	}
	rewritten := memoryReadTestCopy(test, qualified)
	rewritten.Witnesses[0].Root.Version++
	changed, err := MemoryReadQualificationBytes(ctx, rewritten)
	if err != nil || HashContent(raw) == HashContent(changed) {
		test.Fatal("new physical verification reused old qualification identity", err)
	}
}

func TestMemoryReadQualificationBoundsSharedComponentEncoding(test *testing.T) {
	header := memoryReadTestHeader(test, memoryReadTestItems(2))
	qualified := MemoryReadQualification{Version: MemoryReadQualificationVersion, ReadID: HashContent([]byte("read")), CaptureID: header.CaptureID, Source: header.Source}
	var components []MemoryObjectVersion
	for ordinal := range 2048 {
		components = append(components, MemoryObjectVersion{Hash: HashContent([]byte(fmt.Sprint("component", ordinal))),
			Incarnation: "11111111-2222-3333-4444-555555555555", Version: 9007199254740993})
	}
	slices.SortFunc(components, func(left, right MemoryObjectVersion) int {
		return slices.Compare([]byte(left.Hash), []byte(right.Hash))
	})
	for ordinal := range 2048 {
		hash := HashContent([]byte(fmt.Sprint("root", ordinal)))
		qualified.Witnesses = append(qualified.Witnesses, MemoryObjectWitness{RepoID: header.Source.Graph.RepoID, Hash: hash, Available: true,
			Root: &MemoryObjectVersion{Hash: hash, Incarnation: components[0].Incarnation, Version: 1}, Components: components})
	}
	slices.SortFunc(qualified.Witnesses, func(left, right MemoryObjectWitness) int {
		return slices.Compare([]byte(left.Hash), []byte(right.Hash))
	})
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	raw, err := MemoryReadQualificationBytes(context.Background(), qualified)
	runtime.ReadMemStats(&after)
	if !errors.Is(err, ErrMemoryProjectionLimit) || raw != nil {
		test.Fatal("shared component expansion escaped byte budget", err)
	}
	if allocated := after.TotalAlloc - before.TotalAlloc; allocated > 32<<20 {
		test.Fatal("receipt expanded before applying byte budget", allocated)
	}
}
