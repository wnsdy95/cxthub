package domain

import (
	"context"
	"encoding/json"
	"fmt"
	"runtime"
	"slices"
	"strings"
	"testing"
)

func repositoryDecoderCostFixture(test testing.TB, count int) (ContentHash, []RepositoryViewSourceEntry) {
	test.Helper()
	repo := HashContent([]byte("decoder cost"))
	entries := []RepositoryViewSourceEntry{
		{Kind: "repository", Key: string(repo), Value: []byte(fmt.Sprintf(`{"id":%q,"context_protocol":1,"remote_url_supported":true,"git_remote_url_supported":true}`, repo))},
		{Kind: "protocol", Key: string(repo), Value: []byte(`{"context_protocol":1}`)},
	}
	for index := range count {
		identity := HashContent([]byte(fmt.Sprint(index)))
		row := Snapshot{ID: identity, RepoID: repo, DocHash: identity, Branch: "main", Branches: []string{"main"}, Parents: []ContentHash{}, Models: []string{"codex"}}
		raw, err := json.Marshal(row)
		if err != nil {
			test.Fatal(err)
		}
		entries = append(entries, RepositoryViewSourceEntry{Kind: "snapshot", Key: string(identity), Value: raw})
	}
	return repo, entries
}

func TestRepositorySourceDecoderRetentionBounds(test *testing.T) {
	ctx := context.Background()
	for _, count := range []int{1000, 10000} {
		repo, entries := repositoryDecoderCostFixture(test, count)
		runtime.GC()
		var before, after runtime.MemStats
		runtime.ReadMemStats(&before)
		_, _, base, err := DecodeRepositoryViewSourceReusing(ctx, repo, entries, nil)
		if err != nil {
			test.Fatal(err)
		}
		runtime.GC()
		runtime.ReadMemStats(&after)
		retained := 0
		for key, row := range base.rows {
			retained += len(key[0]) + len(key[1]) + len(row.raw) + 128
		}
		if len(base.rows) > maxRepositorySourceDecoderRows || retained > maxRepositorySourceDecoderBytes || len(base.rows) == 0 {
			test.Fatal("unbounded or missing retained rows", len(base.rows), retained)
		}
		test.Logf("snapshot_rows=%d retained_rows=%d retained_source_cost=%d incremental_heap_after_gc=%d", count, len(base.rows), retained, int64(after.HeapAlloc)-int64(before.HeapAlloc))
		_, want, err := DecodeRepositoryViewSource(ctx, repo, entries)
		if err != nil {
			test.Fatal(err)
		}
		_, got, _, err := DecodeRepositoryViewSourceReusing(ctx, repo, entries, base)
		if err != nil || len(want.Snapshots) != len(got.Snapshots) || len(got.Snapshots) != count {
			test.Fatal("bounded reuse omitted source rows", err)
		}
		runtime.KeepAlive(base)
		runtime.KeepAlive(entries)
	}
	repo, entries := repositoryDecoderCostFixture(test, 1)
	for index := range entries {
		if entries[index].Kind == "snapshot" {
			var row Snapshot
			if err := json.Unmarshal(entries[index].Value, &row); err != nil {
				test.Fatal(err)
			}
			row.Message = strings.Repeat("x", maxRepositorySourceDecoderBytes)
			entries[index].Value, _ = json.Marshal(row)
		}
	}
	_, view, decoder, err := DecodeRepositoryViewSourceReusing(ctx, repo, entries, nil)
	if err != nil || len(view.Snapshots) != 1 || len(decoder.rows) != 2 {
		test.Fatal("over-budget row must be read without retention", err)
	}
}

func TestRepositorySourceDecoderCumulativeByteCapAndReordering(test *testing.T) {
	repo, entries := repositoryDecoderCostFixture(test, 64)
	for index := range entries {
		if entries[index].Kind == "snapshot" {
			var row Snapshot
			if err := json.Unmarshal(entries[index].Value, &row); err != nil {
				test.Fatal(err)
			}
			row.Message = strings.Repeat("x", 80<<10)
			entries[index].Value, _ = json.Marshal(row)
		}
	}
	base := repositoryDecoderParity(test, repo, entries, nil)
	if base == nil || len(base.rows) >= len(entries) || len(base.rows) >= maxRepositorySourceDecoderRows {
		test.Fatal("fixture did not exhaust cumulative byte budget before row budget")
	}
	reordered := slices.Clone(entries)
	slices.Reverse(reordered)
	metadata, view, next, err := DecodeRepositoryViewSourceReusing(context.Background(), repo, reordered, base)
	if err != nil || metadata.ID != repo || len(view.Snapshots) != 64 {
		test.Fatal("reordered partial retention changed view", err)
	}
	removed, retainedBytes := 0, 0
	for key := range base.rows {
		if _, retained := next.rows[key]; !retained {
			removed++
		}
	}
	for key, row := range next.rows {
		retainedBytes += len(key[0]) + len(key[1]) + len(row.raw) + 128
	}
	if removed == 0 || retainedBytes > maxRepositorySourceDecoderBytes {
		test.Fatal("fixture did not cross retention boundary or exceeded budget")
	}
	for index := range view.Snapshots {
		view.Snapshots[index].Branches[0] = "mutated"
	}
	repositoryDecoderParity(test, repo, entries, base)
	repositoryDecoderParity(test, repo, reordered, next)
}

func BenchmarkRepositorySourceDecoder(bench *testing.B) {
	ctx := context.Background()
	for _, count := range []int{100, 1000} {
		repo, entries := repositoryDecoderCostFixture(bench, count)
		_, _, base, err := DecodeRepositoryViewSourceReusing(ctx, repo, entries, nil)
		if err != nil {
			bench.Fatal(err)
		}
		for _, mode := range []string{"full", "retain", "reuse"} {
			bench.Run(fmt.Sprintf("rows_%d/%s", count, mode), func(bench *testing.B) {
				bench.ReportAllocs()
				for bench.Loop() {
					switch mode {
					case "reuse":
						_, _, _, err = DecodeRepositoryViewSourceReusing(ctx, repo, entries, base)
					case "retain":
						_, _, _, err = DecodeRepositoryViewSourceReusing(ctx, repo, entries, nil)
					default:
						_, _, err = DecodeRepositoryViewSource(ctx, repo, entries)
					}
					if err != nil {
						bench.Fatal(err)
					}
				}
			})
		}
	}
}
