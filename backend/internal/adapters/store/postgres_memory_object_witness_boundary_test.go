//go:build postgres

package store

import (
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

func TestMemoryObjectWitnessCaptureIncludesRootInLimit(test *testing.T) {
	for _, count := range []int{domain.MaxMemoryCaptureObjects - 1, domain.MaxMemoryCaptureObjects} {
		test.Run(fmt.Sprint(count), func(test *testing.T) {
			digest := memoryCaptureTestDigest()
			digest.KeyFacts = nil
			manifest := domain.MemoryChunkManifest{Format: domain.MemoryChunkFormatV1, SnapshotID: digest.SnapshotID, Provider: digest.Provider, KeyFacts: digest.KeyFacts, OpenTasks: digest.OpenTasks}
			bodies := make(map[domain.ContentHash][]byte)
			var summary strings.Builder
			for ordinal := range count {
				body := []byte(fmt.Sprintf("bounded-%06d", ordinal))
				hash := domain.HashContent(body)
				manifest.SummaryChunks = append(manifest.SummaryChunks, hash)
				bodies[hash] = body
				summary.Write(body)
			}
			digest.Summary = summary.String()
			storage, ctx, transaction, key := memoryCaptureTestFixture(test, digest, false, false)
			transaction.objects[key] = memoryCaptureTestJSON(test, manifest)
			for hash, body := range bodies {
				transaction.objects[memoryCaptureTestKey{key.repo, "memory_chunk", hash}] = body
			}
			actual, err := storage.CaptureMemoryObject(ctx, key.repo, key.hash, 2<<20)
			if count == domain.MaxMemoryCaptureObjects {
				if !errors.Is(err, domain.ErrMemoryProjectionLimit) || !reflect.DeepEqual(actual, domain.CapturedMemoryObject{}) {
					test.Fatal("capture issued a witness exceeding verification budget", err)
				}
				return
			}
			if err != nil || actual.Digest == nil || !reflect.DeepEqual(*actual.Digest, digest) {
				test.Fatal("maximum valid capture rejected", err)
			}
			set := domain.NewMemoryObjectWitnessSet(key.repo)
			if err := set.Add(actual.Witness); err != nil {
				test.Fatal("unchanged successful capture exceeds current validation limit", err)
			}
			objects, err := set.Objects()
			if err != nil || len(objects) != domain.MaxMemoryCaptureObjects {
				test.Fatal("wrong root-plus-component count", len(objects), err)
			}
		})
	}
}
