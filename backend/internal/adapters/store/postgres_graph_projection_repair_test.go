//go:build postgres

package store

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

func graphProjectionRepairFixture() (domain.ContentHash, projectionRead) {
	repo := domain.HashContent([]byte("graph output repair"))
	checkpoint := domain.RepositoryViewSourceCheckpoint{Version: domain.RepositoryViewSourceVersion,
		Scope: domain.RepositoryGraphSourceScope, RepoID: repo, Epoch: "11111111-1111-4111-8111-111111111111", Sequence: 3}
	return repo, projectionRead{PendingProjectionRead: domain.PendingProjectionRead{Enabled: true, Ready: true,
		Dataset: "22222222-2222-4222-8222-222222222222", Source: checkpoint, Applied: checkpoint}, complete: true, rules: domain.GraphViewProjectionRules}
}

func TestGraphProjectionOutputCorruptionClassification(t *testing.T) {
	repo, read := graphProjectionRepairFixture()
	for _, cause := range []error{domain.ErrIntegrity, domain.ErrValidation, domain.ErrRepositoryViewSourceLimit} {
		for _, lagged := range []bool{false, true} {
			candidate := read
			if lagged {
				candidate.Source.Sequence++
				candidate.Ready = false
			}
			failure := graphProjectionOutputCorruption(context.Background(), repo, candidate, fmt.Errorf("output: %w", cause))
			if failure == nil || failure.Validate() != nil || failure.Dataset != read.Dataset || failure.Applied != read.Applied || !errors.Is(failure, cause) {
				t.Fatalf("lost compatible output receipt: %+v", failure)
			}
		}
	}
	for _, cause := range []error{nil, errors.New("network"), context.Canceled, context.DeadlineExceeded,
		&pgconn.PgError{Code: "57014"}, domain.ErrDocumentIdentityUpgradeRequired,
		errors.Join(domain.ErrIntegrity, context.Canceled), errors.Join(domain.ErrIntegrity, &pgconn.PgError{Code: "40001"})} {
		if failure := graphProjectionOutputCorruption(context.Background(), repo, read, cause); failure != nil {
			t.Fatalf("non-output failure produced receipt: %v", cause)
		}
	}
	for _, mutate := range []func(*projectionRead){
		func(candidate *projectionRead) { candidate.complete = false },
		func(candidate *projectionRead) { candidate.Enabled = false },
		func(candidate *projectionRead) { candidate.NeedsAttention = true },
		func(candidate *projectionRead) { candidate.rules = domain.GraphProjectionRules },
		func(candidate *projectionRead) { candidate.Dataset = "" },
		func(candidate *projectionRead) { candidate.Applied.Epoch = candidate.Dataset },
		func(candidate *projectionRead) { candidate.Applied.Sequence++ },
		func(candidate *projectionRead) { candidate.Source.Scope = domain.RepositoryViewSourceScope },
		func(candidate *projectionRead) { candidate.Source.RepoID = domain.HashContent([]byte("foreign")) },
		func(candidate *projectionRead) { candidate.Applied.RepoID = domain.HashContent([]byte("foreign")) },
	} {
		candidate := read
		mutate(&candidate)
		if failure := graphProjectionOutputCorruption(context.Background(), repo, candidate, domain.ErrIntegrity); failure != nil {
			t.Fatalf("incompatible metadata produced receipt: %+v", candidate)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if failure := graphProjectionOutputCorruption(ctx, repo, read, domain.ErrIntegrity); failure != nil {
		t.Fatal("canceled context produced receipt")
	}
}

func TestGraphProjectionRepairRejectsBorrowedContextsAndInvalidReceipts(t *testing.T) {
	storage := &PostgresStore{}
	repo, read := graphProjectionRepairFixture()
	failure := *graphProjectionOutputCorruption(context.Background(), repo, read, domain.ErrIntegrity)
	for _, bound := range []context.Context{
		outbound.WithDocReadOnly(context.Background()),
		context.WithValue(context.Background(), repositoryTxKey{}, &repositoryTx{owner: storage}),
		context.WithValue(context.Background(), repositoryTxKey{}, &repositoryTx{owner: storage, readOnly: true}),
		context.WithValue(context.Background(), repositoryTxKey{}, &repositoryTx{owner: &PostgresStore{}}),
	} {
		if err := storage.RequestGraphProjectionRepair(bound, failure); !errors.Is(err, domain.ErrConflict) {
			t.Fatalf("borrowed repair transaction: %v", err)
		}
	}
	if err := storage.RequestGraphProjectionRepair(context.Background(), domain.GraphProjectionCorruption{}); !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("invalid receipt reached storage: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := storage.RequestGraphProjectionRepair(ctx, failure); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled repair reached storage: %v", err)
	}
}

func TestGraphProjectionRepairRollingWindow(t *testing.T) {
	now := time.Date(2026, 10, 19, 1, 0, 0, 0, time.UTC)
	for _, test := range []struct {
		name        string
		rebuilds    []time.Time
		want        []time.Time
		quarantined bool
	}{
		{name: "first", want: []time.Time{now}},
		{name: "three", rebuilds: []time.Time{now.Add(-59 * time.Minute), now.Add(-time.Minute)}, want: []time.Time{now.Add(-59 * time.Minute), now.Add(-time.Minute), now}},
		{name: "fourth", rebuilds: []time.Time{now.Add(-59 * time.Minute), now.Add(-2 * time.Minute), now.Add(-time.Minute)}, want: []time.Time{now.Add(-59 * time.Minute), now.Add(-2 * time.Minute), now.Add(-time.Minute)}, quarantined: true},
		{name: "boundary", rebuilds: []time.Time{now.Add(-time.Hour), now.Add(-2 * time.Minute), now.Add(-time.Minute)}, want: []time.Time{now.Add(-2 * time.Minute), now.Add(-time.Minute), now}},
		{name: "expired", rebuilds: []time.Time{now.Add(-2 * time.Hour), now.Add(-time.Hour)}, want: []time.Time{now}},
	} {
		t.Run(test.name, func(t *testing.T) {
			before := append([]time.Time(nil), test.rebuilds...)
			got, quarantined := graphProjectionRepairWindow(test.rebuilds, now)
			if !reflect.DeepEqual(got, test.want) || quarantined != test.quarantined || !reflect.DeepEqual(test.rebuilds, before) {
				t.Fatalf("window=%v quarantine=%v", got, quarantined)
			}
		})
	}
}
