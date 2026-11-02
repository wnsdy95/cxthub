//go:build postgres

package store

import (
	"context"
	"errors"
	"fmt"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"
	"unsafe"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

func pgRootWitnessFixtureSources(prepared *preparedStreamingRootDoc) map[domain.ContentHash]pgRootDependencySource {
	sources := make(map[domain.ContentHash]pgRootDependencySource)
	for position, chunk := range prepared.root.manifest.Chunks {
		if _, exists := sources[chunk.Hash]; exists {
			continue
		}
		sources[chunk.Hash] = pgRootDependencySource{
			hash: chunk.Hash, version: int64(position + 1),
			incarnation: fmt.Sprintf("00000000-0000-4000-8000-%012x", position+1),
		}
	}
	return sources
}

func pgRootWitnessPublicationFixture(test *testing.T, events int) *pgStreamingRootPublication {
	test.Helper()
	prepared := preparedRootResourcesFixture(test, events)
	witness, err := newPGRootSourceWitness(prepared, pgRootWitnessFixtureSources(prepared))
	if err != nil {
		test.Fatal(err)
	}
	indexes, err := preparePGRootIndexes(context.Background(), prepared)
	if err != nil {
		test.Fatal(err)
	}
	publication := &pgStreamingRootPublication{preparedStreamingRootDoc: prepared, source: witness, indexes: indexes}
	test.Cleanup(func() { _ = publication.Close() })
	return publication
}

func TestPGRootSourceWitnessBindsPreparedRoot(test *testing.T) {
	prepared := preparedRootResourcesFixture(test, 1)
	witness, err := newPGRootSourceWitness(prepared, pgRootWitnessFixtureSources(prepared))
	if err != nil {
		test.Fatal(err)
	}
	if witness.repo != prepared.job.RepoID || witness.identity != prepared.job.DocIdentity ||
		witness.document != prepared.job.DocHash || witness.job != prepared.job.ID || witness.jobVersion != prepared.job.Version ||
		witness.manifest != domain.HashContent(prepared.root.canonical) || witness.rule != pgRootSourceRuleVersion {
		test.Fatal("witness did not capture the prepared root and verification rule", witness)
	}
	if err := witness.matches(prepared.job, prepared.root.canonical); err != nil {
		test.Fatal("captured preparation did not match", err)
	}
	for _, binding := range []string{"repo", "identity", "document", "job", "job-version", "canonical", "manifest-hash", "zero-rule", "future-rule"} {
		test.Run(binding, func(test *testing.T) {
			current, job := witness, prepared.job
			canonical := append([]byte(nil), prepared.root.canonical...)
			want := domain.ErrConflict
			switch binding {
			case "repo":
				job.RepoID = domain.HashContent([]byte("other repository"))
			case "identity":
				job.DocIdentity = domain.DocumentIdentityLegacy
			case "document":
				job.DocHash = domain.HashContent([]byte("other document"))
			case "job":
				job.ID += "-other"
			case "job-version":
				job.Version++
			case "canonical":
				canonical[0] ^= 1
				want = domain.ErrIntegrity
			case "manifest-hash":
				current.manifest = domain.HashContent([]byte("other canonical manifest"))
				want = domain.ErrIntegrity
			case "zero-rule":
				current.rule = 0
				want = domain.ErrIntegrity
			case "future-rule":
				current.rule++
				want = domain.ErrIntegrity
			}
			if err := current.matches(job, canonical); !errors.Is(err, want) {
				test.Fatalf("changed binding matched: got %v, want %v", err, want)
			}
		})
	}
	for _, missing := range []*pgRootSourceWitness{nil, {}} {
		if err := missing.matches(prepared.job, prepared.root.canonical); !errors.Is(err, domain.ErrIntegrity) {
			test.Fatal("absent witness matched", err)
		}
	}
}

func TestPGRootSourceWitnessOwnsSortedUniqueDependencies(test *testing.T) {
	rangeSpoolTempDir(test)
	fixture := rootFixture(test, strings.Repeat("x", 3*domain.ConversationManifestChunkBytes))
	job := rootPublicationJob(test, domain.HashContent([]byte(test.Name())), fixture).Claim(time.Now().UTC(), time.Minute)
	prepared, err := prepareStreamingRootJob(context.Background(), job, &domain.CanonicalDocVerifier{}, func(_ context.Context, hash domain.ContentHash) ([]byte, error) {
		return fixture.bodies[hash], nil
	})
	if err != nil {
		test.Fatal(err)
	}
	test.Cleanup(func() { _ = prepared.Close() })
	sources := pgRootWitnessFixtureSources(prepared)
	if len(sources) < 3 || len(prepared.root.manifest.Chunks) <= len(sources) {
		test.Fatal("fixture must have several unique dependencies and a repeated chunk")
	}
	witness, err := newPGRootSourceWitness(prepared, sources)
	if err != nil {
		test.Fatal(err)
	}
	if len(witness.chunks) != len(sources) {
		test.Fatal("witness does not cover exactly the unique dependencies", len(witness.chunks), len(sources))
	}
	for position, source := range witness.chunks {
		if source != sources[source.hash] || position > 0 && witness.chunks[position-1].hash >= source.hash {
			test.Fatal("dependency tuples are not exact, unique and ordered", witness.chunks)
		}
		if got, exists := witness.dependency(source.hash); !exists || got != source {
			test.Fatal("ordered lookup lost a captured tuple", source, got, exists)
		}
	}
	captured := append([]pgRootDependencySource(nil), witness.chunks...)
	for hash, source := range sources {
		source.version++
		source.incarnation = "22222222-2222-4222-8222-222222222222"
		sources[hash] = source
	}
	clear(sources)
	if !reflect.DeepEqual(witness.chunks, captured) {
		test.Fatal("witness retained the caller's mutable dependency map")
	}
	for _, source := range captured {
		if got, exists := witness.dependency(source.hash); !exists || got != source {
			test.Fatal("map mutation changed dependency lookup", source, got, exists)
		}
	}
	for _, missing := range []domain.ContentHash{"", domain.ContentHash("sha256:" + strings.Repeat("0", 64)), domain.HashContent([]byte("unobserved dependency")), domain.ContentHash("sha256:" + strings.Repeat("f", 64))} {
		if got, exists := witness.dependency(missing); exists || got != (pgRootDependencySource{}) {
			test.Fatal("lookup invented an unobserved dependency", missing, got, exists)
		}
	}
}

func TestPGRootSourceWitnessRejectsInvalidCoverageAndTuples(test *testing.T) {
	for _, damage := range []string{"nil-preparation", "closed-preparation", "nil-sources", "missing-source", "extra-source", "substituted-source", "tuple-hash", "negative-version", "empty-incarnation", "short-incarnation", "compact-incarnation", "malformed-incarnation", "zero-incarnation"} {
		test.Run(damage, func(test *testing.T) {
			prepared := preparedRootResourcesFixture(test, 1)
			sources := pgRootWitnessFixtureSources(prepared)
			hash := prepared.root.manifest.Chunks[0].Hash
			source := sources[hash]
			other := domain.HashContent([]byte("not a root dependency"))
			switch damage {
			case "nil-preparation":
				prepared = nil
			case "closed-preparation":
				if err := prepared.Close(); err != nil {
					test.Fatal(err)
				}
			case "nil-sources":
				sources = nil
			case "missing-source":
				delete(sources, hash)
			case "extra-source", "substituted-source":
				if damage == "substituted-source" {
					delete(sources, hash)
				}
				source.hash = other
				sources[other] = source
			case "tuple-hash":
				source.hash = other
				sources[hash] = source
			case "negative-version":
				source.version = -1
				sources[hash] = source
			case "empty-incarnation", "short-incarnation", "compact-incarnation", "malformed-incarnation", "zero-incarnation":
				switch damage {
				case "empty-incarnation":
					source.incarnation = ""
				case "short-incarnation":
					source.incarnation = "not-a-uuid"
				case "compact-incarnation":
					source.incarnation = "11111111111141118111111111111111"
				case "malformed-incarnation":
					source.incarnation = strings.Repeat("z", 36)
				case "zero-incarnation":
					source.incarnation = "00000000-0000-0000-0000-000000000000"
				}
				sources[hash] = source
			}
			witness, err := newPGRootSourceWitness(prepared, sources)
			if !errors.Is(err, domain.ErrIntegrity) || !reflect.DeepEqual(witness, pgRootSourceWitness{}) {
				test.Fatal("invalid preparation or source yielded a witness", witness, err)
			}
		})
	}
}

func TestPGRootSourceWitnessAcceptsSourceVersionBounds(test *testing.T) {
	prepared := preparedRootResourcesFixture(test, 1)
	hash := prepared.root.manifest.Chunks[0].Hash
	for _, version := range []int64{0, 1, 1<<63 - 1} {
		sources := pgRootWitnessFixtureSources(prepared)
		source := sources[hash]
		source.version = version
		sources[hash] = source
		witness, err := newPGRootSourceWitness(prepared, sources)
		if err != nil {
			test.Fatal("valid source version was rejected", version, err)
		}
		if got, exists := witness.dependency(hash); !exists || got != source {
			test.Fatal("source version changed during capture", version, got, exists)
		}
	}
}

func TestPGRootSourceWitnessAcceptsEmptyRoot(test *testing.T) {
	prepared := preparedRootResourcesFixture(test, 0)
	for _, sources := range []map[domain.ContentHash]pgRootDependencySource{nil, {}} {
		witness, err := newPGRootSourceWitness(prepared, sources)
		if err != nil || len(witness.chunks) != 0 {
			test.Fatal("empty root required a dependency", witness, err)
		}
		if err := witness.matches(prepared.job, prepared.root.canonical); err != nil {
			test.Fatal("empty root witness did not match", err)
		}
		if got, exists := witness.dependency(domain.HashContent([]byte("absent"))); exists || got != (pgRootDependencySource{}) {
			test.Fatal("empty root lookup invented a dependency", got, exists)
		}
		indexes, err := preparePGRootIndexes(context.Background(), prepared)
		if err != nil {
			test.Fatal(err)
		}
		test.Cleanup(func() { _ = indexes.Close() })
		publication := &pgStreamingRootPublication{preparedStreamingRootDoc: prepared, source: witness, indexes: indexes}
		if err := publication.matches(prepared.job); err != nil {
			test.Fatal("empty root publication did not match", err)
		}
		if resources, err := publication.RetainedResources(); err != nil || resources.SpoolBytes != 0 || resources.HeapBytes <= 0 || resources.HeapBytes >= 16<<20 {
			test.Fatal("empty root publication resources", resources, err)
		}
	}
}

func TestPGRootPublicationRequiresSourceWitness(test *testing.T) {
	publication := pgRootWitnessPublicationFixture(test, 1)
	job := publication.job
	if err := publication.matches(job); err != nil {
		test.Fatal("valid publication did not match", err)
	}
	publication.source = pgRootSourceWitness{}
	for _, invalid := range []*pgStreamingRootPublication{nil, {}, publication} {
		if err := invalid.matches(job); !errors.Is(err, domain.ErrIntegrity) {
			test.Fatal("publication matched without a source witness or preparation", err)
		}
		if resources, err := invalid.RetainedResources(); !errors.Is(err, domain.ErrIntegrity) || resources != (outbound.RootPublicationResources{}) {
			test.Fatal("publication reported resources without a source witness or preparation", resources, err)
		}
	}
}

func TestPGRootPublicationResourcesChargeOwnerHeader(test *testing.T) {
	publication := pgRootWitnessPublicationFixture(test, 0)
	base, err := publication.preparedStreamingRootDoc.RetainedResources()
	if err != nil {
		test.Fatal(err)
	}
	minimum := rootPublicationHeapEstimate{}
	minimum.allocation(1, int64(unsafe.Sizeof(*publication)))
	resources, err := publication.RetainedResources()
	if minimum.err != nil || err != nil || resources.HeapBytes-base.HeapBytes < minimum.bytes || resources.SpoolBytes != base.SpoolBytes {
		test.Fatal("publication owner header was not conservatively charged", base, resources, minimum, err)
	}
}

func TestPGRootPublicationResourcesChargeSourceCapacityAndStrings(test *testing.T) {
	for _, part := range []string{"tuple-capacity", "live-hash", "live-incarnation", "spare-hash", "spare-incarnation"} {
		test.Run(part, func(test *testing.T) {
			publication := pgRootWitnessPublicationFixture(test, 1)
			before, err := publication.RetainedResources()
			if err != nil {
				test.Fatal(err)
			}
			const extra = 4096
			previous, minimum := rootPublicationHeapEstimate{}, rootPublicationHeapEstimate{}
			switch part {
			case "tuple-capacity":
				prior := publication.source.chunks
				grown := make([]pgRootDependencySource, len(prior), cap(prior)+extra)
				copy(grown, prior)
				publication.source.chunks = grown
				width := int64(unsafe.Sizeof(pgRootDependencySource{}))
				previous.allocation(int64(cap(prior)), width)
				minimum.allocation(int64(cap(grown)), width)
			case "live-hash":
				previous.allocation(int64(len(publication.source.chunks[0].hash)), 1)
				publication.source.chunks[0].hash += domain.ContentHash(strings.Repeat("h", extra))
				minimum.allocation(int64(len(publication.source.chunks[0].hash)), 1)
			case "live-incarnation":
				previous.allocation(int64(len(publication.source.chunks[0].incarnation)), 1)
				publication.source.chunks[0].incarnation += strings.Repeat("i", extra)
				minimum.allocation(int64(len(publication.source.chunks[0].incarnation)), 1)
			case "spare-hash", "spare-incarnation":
				prior := publication.source.chunks
				grown := make([]pgRootDependencySource, cap(prior)+1)
				copy(grown, prior)
				if part == "spare-hash" {
					grown[len(grown)-1].hash = domain.ContentHash(strings.Repeat("h", extra))
				} else {
					grown[len(grown)-1].incarnation = strings.Repeat("i", extra)
				}
				publication.source.chunks = grown[:len(prior)]
				minimum.allocation(extra, 1)
			}
			after, err := publication.RetainedResources()
			if previous.err != nil || minimum.err != nil || err != nil || after.HeapBytes-before.HeapBytes < minimum.bytes-previous.bytes || after.SpoolBytes != before.SpoolBytes {
				test.Fatal("owned source capacity or string bytes were not conservatively charged", before, after, minimum.bytes-previous.bytes, err)
			}
		})
	}
}

func TestPGRootPublicationCloseClearsWitnessAndSpool(test *testing.T) {
	for _, mode := range []string{"success", "close-error"} {
		test.Run(mode, func(test *testing.T) {
			publication := pgRootWitnessPublicationFixture(test, 1)
			job := publication.job
			path := publication.spool.file.Name()
			if mode == "close-error" {
				if err := publication.spool.file.Close(); err != nil {
					test.Fatal(err)
				}
			}
			first := publication.Close()
			if (first == nil) != (mode == "success") || publication.Close() != first {
				test.Fatal("publication close lost its original result", first)
			}
			if !reflect.DeepEqual(publication.source, pgRootSourceWitness{}) ||
				!reflect.DeepEqual(publication.job, domain.DocFinalizationJob{}) || !reflect.DeepEqual(publication.root, preparedRootDoc{}) ||
				publication.spool.offsets != nil || publication.spool.writer != nil {
				test.Fatal("closed publication retained source or prepared metadata")
			}
			if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
				test.Fatal("closed publication retained its spool", err)
			}
			if err := publication.matches(job); err == nil {
				test.Fatal("closed publication matched its old job")
			}
			if resources, err := publication.RetainedResources(); err == nil || resources != (outbound.RootPublicationResources{}) {
				test.Fatal("closed publication reported retained resources", resources, err)
			}
		})
	}
}
