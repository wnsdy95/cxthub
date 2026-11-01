package app

import (
	"context"
	"errors"
	"testing"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/inbound"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

type docDisplayMaintenanceProbe struct {
	calls int
	fail  error
}

func (probe *docDisplayMaintenanceProbe) StepDocDisplayMaintenance(ctx context.Context) (domain.DocDisplayMaintenanceStep, error) {
	_, system := inbound.RepositoryActor(ctx)
	if !system {
		return domain.DocDisplayMaintenanceStep{}, domain.ErrForbidden
	}
	probe.calls++
	return domain.DocDisplayMaintenanceStep{}, probe.fail
}

func TestDocDisplayMaintenanceExplicitAndReadOnlyBoundary(test *testing.T) {
	service := NewService(nil, nil, nil, nil, nil)
	if err := service.ProcessDocDisplayMaintenance(context.Background(), 1); err != nil {
		test.Fatal("unconfigured legacy service acquired maintenance work", err)
	}
	probe := &docDisplayMaintenanceProbe{}
	service.ConfigureDocumentDisplayMaintenance(probe)
	if err := service.ProcessDocDisplayMaintenance(outbound.WithDocReadOnly(context.Background()), 1); !errors.Is(err, domain.ErrForbidden) || probe.calls != 0 {
		test.Fatal("read-only scope reached maintenance", err, probe.calls)
	}
	if err := service.ProcessDocDisplayMaintenance(context.Background(), 2); err != nil || probe.calls != 2 {
		test.Fatal("bounded worker did not use explicit maintenance port", err, probe.calls)
	}
	probe.fail = domain.ErrNotFound
	if processed, err := service.processDocDisplayMaintenance(context.Background(), 3); err != nil || processed != 0 || probe.calls != 3 {
		test.Fatal("idle source was reported as useful work", processed, err, probe.calls)
	}
	probe.fail = domain.ErrIntegrity
	if err := service.ProcessDocDisplayMaintenance(context.Background(), 1); !errors.Is(err, domain.ErrIntegrity) {
		test.Fatal("maintenance integrity failure was hidden", err)
	}
}
