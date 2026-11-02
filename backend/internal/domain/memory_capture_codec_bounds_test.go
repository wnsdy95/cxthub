package domain

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
)

func memoryCaptureTestSizedJSON(size int) []byte {
	raw := bytes.Repeat([]byte(" "), size)
	raw[0], raw[len(raw)-1] = '{', '}'
	return raw
}

func memoryCaptureTestChunks(count, size int) []MemoryCapturePart {
	dependency := HashContent([]byte("chunked dependency"))
	value := bytes.Repeat([]byte("m"), size)
	parts := make([]MemoryCapturePart, count)
	for index := range parts {
		parts[index] = MemoryCapturePart{Kind: "memory", Key: fmt.Sprintf("%s:%08d", dependency, index), Value: value}
	}
	return parts
}

func memoryCaptureTestPins(count, size int) []MemoryCapturePart {
	value := memoryCaptureTestSizedJSON(size)
	parts := make([]MemoryCapturePart, count)
	for index := range parts {
		kind := "pr_pin"
		if index%2 == 0 {
			kind = "git_pin"
		}
		parts[index] = MemoryCapturePart{Kind: kind, Key: fmt.Sprintf("%08d", index), Value: value}
	}
	return parts
}

func memoryCaptureTestDependencies(count int) []MemoryCapturePart {
	parts := make([]MemoryCapturePart, count)
	for index := range parts {
		dependency := fmt.Sprintf("sha256:%064x", index)
		parts[index] = MemoryCapturePart{Kind: "absent", Key: dependency}
		if index%2 == 0 {
			parts[index] = MemoryCapturePart{Kind: "memory", Key: dependency + ":00000000", Value: []byte("m")}
		}
	}
	return parts
}

func memoryCaptureTestGraphRows(count int) []MemoryCapturePart {
	parts := make([]MemoryCapturePart, count)
	for index := range parts {
		parts[index] = MemoryCapturePart{Kind: "graph:snapshot", Key: fmt.Sprintf("%08d", index), Value: []byte(`{}`)}
	}
	return parts
}

func memoryCaptureTestFullGraphBytes() []MemoryCapturePart {
	remaining := MaxPendingProjectionInputBytes
	value := memoryCaptureTestSizedJSON(MaxMemoryCapturePartBytes)
	var parts []MemoryCapturePart
	for index := 0; remaining > 0; index++ {
		key := fmt.Sprintf("%08d", index)
		cost := len("snapshot") + len(key) + 128
		if remaining-cost < len(value) {
			value = memoryCaptureTestSizedJSON(remaining - cost)
		}
		parts = append(parts, MemoryCapturePart{Kind: "graph:snapshot", Key: key, Value: value})
		remaining -= cost + len(value)
	}
	return parts
}

func TestMemoryCaptureLimitRejectionBeforeCloneOrHash(test *testing.T) {
	cases := []struct {
		name  string
		parts func() []MemoryCapturePart
		want  error
	}{
		{"part count", func() []MemoryCapturePart { return make([]MemoryCapturePart, MaxMemoryCaptureParts+1) }, ErrRepositoryViewSourceLimit},
		{"part bytes", func() []MemoryCapturePart {
			return []MemoryCapturePart{{Kind: "graph:snapshot", Key: "key", Value: make([]byte, MaxMemoryCapturePartBytes+1)}}
		}, ErrRepositoryViewSourceLimit},
		{"key bytes", func() []MemoryCapturePart {
			return []MemoryCapturePart{{Kind: "graph:snapshot", Key: strings.Repeat("k", MaxRepositoryViewSourceKeyBytes+1)}}
		}, ErrRepositoryViewSourceLimit},
		{"kind bytes", func() []MemoryCapturePart { return []MemoryCapturePart{{Kind: strings.Repeat("k", 65), Key: "key"}} }, ErrRepositoryViewSourceLimit},
		{"chunk bytes", func() []MemoryCapturePart { return memoryCaptureTestChunks(1, MemoryCaptureChunkBytes+1) }, ErrMemoryProjectionLimit},
		{"memory bytes", func() []MemoryCapturePart {
			return memoryCaptureTestChunks(MaxMemoryCaptureObjectBytes/MemoryCaptureChunkBytes+1, MemoryCaptureChunkBytes)
		}, ErrMemoryProjectionLimit},
		{"dependencies", func() []MemoryCapturePart { return memoryCaptureTestDependencies(MaxMemoryCaptureObjects + 1) }, ErrMemoryProjectionLimit},
		{"combined pin count", func() []MemoryCapturePart { return memoryCaptureTestPins(MaxMemoryCaptureObjects+1, 2) }, ErrMemoryProjectionLimit},
		{"combined pin bytes", func() []MemoryCapturePart {
			return memoryCaptureTestPins(MaxMemoryCapturePinBytes/MaxMemoryCapturePartBytes+1, MaxMemoryCapturePartBytes)
		}, ErrMemoryProjectionLimit},
		{"graph rows", func() []MemoryCapturePart { return memoryCaptureTestGraphRows(MaxPendingProjectionInputRows + 1) }, ErrRepositoryViewSourceLimit},
		{"graph bytes", func() []MemoryCapturePart {
			return append(memoryCaptureTestFullGraphBytes(), MemoryCapturePart{Kind: "graph:snapshot", Key: "extra", Value: []byte(`{}`)})
		}, ErrRepositoryViewSourceLimit},
		{"overall bytes", func() []MemoryCapturePart {
			parts := append(memoryCaptureTestFullGraphBytes(), memoryCaptureTestChunks(MaxMemoryCaptureObjectBytes/MemoryCaptureChunkBytes, MemoryCaptureChunkBytes)...)
			pins := memoryCaptureTestPins(MaxMemoryCaptureObjects, MaxMemoryCapturePinBytes/MaxMemoryCaptureObjects)
			for index := range pins {
				pins[index].Key = strings.Repeat("k", MaxRepositoryViewSourceKeyBytes-len(pins[index].Key)) + pins[index].Key
			}
			return append(parts, pins...)
		}, ErrRepositoryViewSourceLimit},
	}
	for _, entry := range cases {
		test.Run(entry.name, func(test *testing.T) {
			parts := entry.parts()
			original := append([]MemoryCapturePart(nil), parts...)
			header := memoryCaptureTestHeader()
			if bundle, err := SealMemoryCapture(context.Background(), header, parts); !errors.Is(err, entry.want) || !reflect.DeepEqual(bundle, MemoryCaptureBundle{}) {
				test.Fatal("oversized seal result", err)
			}
			if err := ValidateMemoryCapture(context.Background(), MemoryCaptureBundle{ID: HashContent(nil), Header: header, Parts: parts}); !errors.Is(err, entry.want) {
				test.Fatal("oversized validation did not stop before identity checks", err)
			}
			if !reflect.DeepEqual(parts, original) {
				test.Fatal("failed operation mutated input")
			}
		})
	}
	header := memoryCaptureTestHeader()
	header.Generation.Graph.RepoID = ContentHash(strings.Repeat("x", MaxRepositoryViewSourceKeyBytes+1))
	if _, err := SealMemoryCapture(context.Background(), header, memoryCaptureTestParts()); !errors.Is(err, ErrRepositoryViewSourceLimit) {
		test.Fatal("unbounded header string reached content hash validation", err)
	}
	header = memoryCaptureTestHeader()
	header.Selection.Branch = strings.Repeat("&", 1024)
	memoryCaptureTestSeal(test, header, memoryCaptureTestParts())
}

func TestMemoryCaptureExactLimits(test *testing.T) {
	cases := []struct {
		name  string
		parts func() []MemoryCapturePart
	}{
		{"part bytes and key", func() []MemoryCapturePart {
			return []MemoryCapturePart{{Kind: "graph:snapshot", Key: strings.Repeat("k", MaxRepositoryViewSourceKeyBytes), Value: memoryCaptureTestSizedJSON(MaxMemoryCapturePartBytes)}}
		}},
		{"memory bytes", func() []MemoryCapturePart {
			return memoryCaptureTestChunks(MaxMemoryCaptureObjectBytes/MemoryCaptureChunkBytes, MemoryCaptureChunkBytes)
		}},
		{"combined dependencies", func() []MemoryCapturePart { return memoryCaptureTestDependencies(MaxMemoryCaptureObjects) }},
		{"combined pin count", func() []MemoryCapturePart { return memoryCaptureTestPins(MaxMemoryCaptureObjects, 2) }},
		{"combined pin bytes", func() []MemoryCapturePart {
			return memoryCaptureTestPins(MaxMemoryCapturePinBytes/MaxMemoryCapturePartBytes, MaxMemoryCapturePartBytes)
		}},
		{"graph rows", func() []MemoryCapturePart { return memoryCaptureTestGraphRows(MaxPendingProjectionInputRows) }},
		{"graph source accounting excludes prefix", memoryCaptureTestFullGraphBytes},
		{"part count is not dependency count", func() []MemoryCapturePart { return memoryCaptureTestChunks(MaxMemoryCaptureParts, 1) }},
	}
	for _, entry := range cases {
		test.Run(entry.name, func(test *testing.T) {
			memoryCaptureTestSeal(test, memoryCaptureTestHeader(), entry.parts())
		})
	}
}

func TestMemoryCaptureCancellationDuringSort(test *testing.T) {
	parts := memoryCaptureTestDependencies(MaxMemoryCaptureObjects)
	ctx := &memoryCaptureTestContext{Context: context.Background(), cancelAfter: len(parts) + 3}
	if bundle, err := SealMemoryCapture(ctx, memoryCaptureTestHeader(), parts); !errors.Is(err, context.Canceled) || !reflect.DeepEqual(bundle, MemoryCaptureBundle{}) {
		test.Fatal("sorting did not preserve cancellation", err)
	}
}
