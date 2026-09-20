package app

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

func TestQueryMemoryPositionsIsReadOnlyAndScoped(t *testing.T) {
	f := newEffectiveFixture(t)
	ctx := context.Background()
	before, _ := f.svc.RepositoryRevision(ctx, f.repo)
	history, _ := f.svc.ListHistory(ctx, f.repo)
	reads := f.reader.reads
	f.reader.offline = true
	got, err := f.svc.QueryMemoryPositions(ctx, f.repo, f.a, "")
	if err != nil || got.Reason != "unique" || got.CodeCommit != effectiveOID(2) {
		t.Fatalf("result %+v %v", got, err)
	}
	if _, err = f.svc.QueryMemoryPositions(ctx, f.repo, hh("missing"), ""); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("missing snapshot: %v", err)
	}
	if _, err = f.svc.QueryMemoryPositions(ctx, hh("other repo"), f.a, ""); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("cross repository: %v", err)
	}
	if _, err = f.svc.QueryMemoryPositions(ctx, f.repo, "bad", ""); !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("invalid snapshot: %v", err)
	}
	after, _ := f.svc.RepositoryRevision(ctx, f.repo)
	afterHistory, _ := f.svc.ListHistory(ctx, f.repo)
	if before != after || !reflect.DeepEqual(history, afterHistory) || reads != f.reader.reads {
		t.Fatal("position query modified state or contacted provider")
	}
}
