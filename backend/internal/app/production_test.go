package app

import (
	"github.com/wnsdy95/cxthub/backend/internal/adapters/store"
	"strings"

	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
	"testing"
)

type productionFixture struct{ outbound.ProductionStore }
type noTransactions struct {
	productionFixture
	WithinRepository int
}
type noSharedRuntime struct {
	productionFixture
	AllowRequest int
}
type noAccessLock struct {
	productionFixture
	LockRepositoryAccess int
}
type noOutbox struct {
	productionFixture
	EnqueueNotification int
}
type noRevision struct {
	productionFixture
	AdvanceEvidenceRevision int
}
type noFence struct {
	productionFixture
	FencePRJob int
}

func TestProductionStoreRejectsMissingCapabilities(t *testing.T) {
	for _, row := range []struct {
		value any
		want  string
	}{
		{nil, "nil"}, {(*productionFixture)(nil), "nil"},
		{noTransactions{}, "repository write/read transactions"}, {noSharedRuntime{}, "shared authentication/rate limits"},
		{noAccessLock{}, "repository access locks"}, {noOutbox{}, "notification outbox"},
		{noRevision{}, "evidence revisions"}, {noFence{}, "PR lease fencing"},
		{store.NewFSStore(t.TempDir()), "repository write/read transactions"},
	} {
		if err := ValidateProductionStore(row.value); err == nil || !strings.Contains(err.Error(), row.want) {
			t.Fatalf("%T: %v, want %s", row.value, err, row.want)
		}
	}
	if err := ValidateProductionStore(productionFixture{}); err != nil {
		t.Fatal(err)
	}
}
