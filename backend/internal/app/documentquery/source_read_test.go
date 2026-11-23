package documentquery

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"testing"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

type sourceReadSnapshots struct {
	reader   SnapshotReader
	calls    int
	failure  error
	snapshot *domain.Snapshot
}

func (reader *sourceReadSnapshots) GetSnapshot(ctx context.Context, repo, hash domain.ContentHash) (domain.Snapshot, error) {
	reader.calls++
	if reader.failure != nil {
		return domain.Snapshot{}, reader.failure
	}
	if reader.snapshot != nil {
		return *reader.snapshot, nil
	}
	return reader.reader.GetSnapshot(ctx, repo, hash)
}

type sourceReadScope struct {
	Scope
	test *testing.T
}

func (scope sourceReadScope) WithinDocumentRead(context.Context, domain.ContentHash, func(context.Context) error) error {
	scope.test.Fatal("source opened another admitted read")
	return domain.ErrIntegrity
}

func (scope sourceReadScope) WithinDocumentSnapshot(context.Context, domain.ContentHash, func(context.Context) error) error {
	scope.test.Fatal("source opened another snapshot")
	return domain.ErrIntegrity
}

func assertSourceReadFailure(test *testing.T, source Source, failure error) {
	test.Helper()
	if entry, err := source.Entry(0); !errors.Is(err, failure) || entry != (domain.DocRangeEntry{}) {
		test.Fatal("failed source exposed entry metadata", entry, err)
	}
	if location, err := source.Location(0); !errors.Is(err, failure) || location != (domain.DocEventIndex{}) {
		test.Fatal("failed source exposed location metadata", location, err)
	}
	if raw, err := source.Read(0, 1); !errors.Is(err, failure) || raw != nil {
		test.Fatal("failed source exposed bytes", raw, err)
	}
}

func TestSourceReadZeroInvalidAndCanceledOpenFailBeforePorts(test *testing.T) {
	assertSourceReadFailure(test, Source{}, domain.ErrValidation)
	if source := (Source{}); source.Binding() != (domain.DocReadGenerationBinding{}) || source.Total() != 0 || source.Root() || source.StreamBytes() != 0 || !reflect.DeepEqual(source.Envelope(), domain.CIREnvelope{}) {
		test.Fatal("zero source exposed metadata")
	}
	queries, err := New(constructorPrefixConfig(test, LegacyCompatibility))
	if err != nil {
		test.Fatal(err)
	}
	repo, hash := domain.HashContent([]byte("source repo")), domain.HashContent([]byte("source hash"))
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	for _, open := range []func(context.Context, domain.ContentHash, domain.ContentHash) (Source, error){queries.OpenSource, queries.OpenViewSource} {
		for _, request := range []struct {
			ctx        context.Context
			repo, hash domain.ContentHash
			failure    error
		}{
			{ctx: canceled, repo: repo, hash: hash, failure: context.Canceled},
			{ctx: nil, repo: repo, hash: hash, failure: domain.ErrValidation},
			{ctx: context.Background(), repo: "invalid", hash: hash, failure: domain.ErrIntegrity},
			{ctx: context.Background(), repo: repo, hash: "", failure: domain.ErrIntegrity},
		} {
			source, err := open(request.ctx, request.repo, request.hash)
			if !errors.Is(err, request.failure) || !reflect.DeepEqual(source, Source{}) {
				test.Fatal("invalid or canceled source open reached storage or returned content", err)
			}
		}
	}
	var missing *Queries
	for _, queries := range []*Queries{missing, {}} {
		if source, err := queries.OpenSource(context.Background(), repo, hash); !errors.Is(err, domain.ErrValidation) || !reflect.DeepEqual(source, Source{}) {
			test.Fatal("unconfigured source owner did not fail closed", err)
		}
	}
}

func TestSourceReadInvalidAccessAndReaderErrorsFailClosed(test *testing.T) {
	calls := 0
	failure := fmt.Errorf("source read: %w", domain.ErrConflict)
	source := Source{ctx: context.Background(), source: docViewSource{total: 1,
		entry: func(int) (domain.DocRangeEntry, error) {
			calls++
			return domain.DocRangeEntry{}, failure
		},
		location: func(int) (domain.DocEventIndex, error) {
			calls++
			return domain.DocEventIndex{}, failure
		},
		read: func(int, int) ([]byte, error) {
			calls++
			return nil, failure
		},
	}}
	for _, position := range []int{-1, 1, int(^uint(0) >> 1)} {
		if entry, err := source.Entry(position); !errors.Is(err, domain.ErrValidation) || entry != (domain.DocRangeEntry{}) {
			test.Fatal("invalid source entry was accepted", position, err)
		}
		if location, err := source.Location(position); !errors.Is(err, domain.ErrValidation) || location != (domain.DocEventIndex{}) {
			test.Fatal("invalid source location was accepted", position, err)
		}
	}
	for _, request := range [][2]int{{-1, 1}, {0, 0}, {0, -1}} {
		if raw, err := source.Read(request[0], request[1]); !errors.Is(err, domain.ErrValidation) || raw != nil {
			test.Fatal("invalid source range was accepted", request, err)
		}
	}
	if calls != 0 {
		test.Fatal("invalid facade access reached a reader", calls)
	}
	assertSourceReadFailure(test, source, domain.ErrConflict)
	if calls != 3 {
		test.Fatal("valid access did not delegate exact existing reader errors", calls)
	}
	assertSourceReadFailure(test, Source{ctx: context.Background(), source: docViewSource{total: 1}}, domain.ErrValidation)
}
