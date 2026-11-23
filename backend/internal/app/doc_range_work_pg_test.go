//go:build postgres

package app

import (
	"context"
	"errors"
	"fmt"

	"github.com/wnsdy95/cxthub/backend/internal/adapters/store"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

type docRangeWorkPGProbe struct {
	*store.PostgresStore
	afterPrepare func(context.Context, outbound.RootPublicationResources) error
	afterStep    func(context.Context, int, bool) error
	job          domain.DocRangeIndexJob
	steps        int
	published    int
	closed       int
	initial      []outbound.RootPublicationResources
	resources    []outbound.RootPublicationResources
}

type docRangeWorkPGPrepared struct {
	outbound.PreparedDocRangeWork
	probe *docRangeWorkPGProbe
}

func (probe *docRangeWorkPGProbe) PrepareDocRangeWork(ctx context.Context, job domain.DocRangeIndexJob) (outbound.PreparedDocRangeWork, error) {
	prepared, err := probe.PostgresStore.PrepareDocRangeWork(ctx, job)
	if prepared == nil || err != nil {
		return prepared, err
	}
	resources, err := prepared.RetainedResources()
	if err != nil {
		return nil, errors.Join(err, prepared.Close())
	}
	probe.initial = append(probe.initial, resources)
	probe.job = job
	work := &docRangeWorkPGPrepared{PreparedDocRangeWork: prepared, probe: probe}
	if probe.afterPrepare != nil {
		return work, probe.afterPrepare(ctx, resources)
	}
	return work, nil
}

func (prepared *docRangeWorkPGPrepared) Step(ctx context.Context, job domain.DocRangeIndexJob) (outbound.DocRangeWorkProgress, error) {
	progress, err := prepared.PreparedDocRangeWork.Step(ctx, job)
	if err != nil {
		return 0, err
	}
	if progress != outbound.DocRangeWorkPending && progress != outbound.DocRangeWorkPublishable {
		return 0, fmt.Errorf("monolithic work returned invalid progress: %d", progress)
	}
	complete := progress == outbound.DocRangeWorkPublishable
	if !complete {
		if err := prepared.PreparedDocRangeWork.Publish(ctx, job); !errors.Is(err, domain.ErrDocRangeIndexPending) {
			return 0, fmt.Errorf("unfinished work accepted publication: %v", err)
		}
	}
	prepared.probe.steps++
	resources, err := prepared.RetainedResources()
	if err != nil {
		return 0, err
	}
	prepared.probe.resources = append(prepared.probe.resources, resources)
	if prepared.probe.afterStep != nil {
		err = prepared.probe.afterStep(ctx, prepared.probe.steps, complete)
	}
	return progress, err
}

func (prepared *docRangeWorkPGPrepared) Publish(ctx context.Context, job domain.DocRangeIndexJob) error {
	if err := prepared.PreparedDocRangeWork.Publish(ctx, job); err != nil {
		return err
	}
	prepared.probe.published++
	return nil
}

func (prepared *docRangeWorkPGPrepared) Close() error {
	prepared.probe.closed++
	return prepared.PreparedDocRangeWork.Close()
}
