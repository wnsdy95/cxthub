package app

import (
	"context"
	"fmt"

	"github.com/wnsdy95/cxthub/cli/internal/domain"
	"github.com/wnsdy95/cxthub/cli/internal/ports/outbound"
)

// MeasuredAgentCapabilities retains the runtime's model/window authority while
// replacing the requirement for exact hidden input with explicit scoped reserve
// accounting. Preparation can read feedback but cannot write or execute turns.
type MeasuredAgentCapabilities struct {
	Runtime      outbound.AgentCapabilityReader
	Observations outbound.AgentInputCalibrationReader
}

func (r MeasuredAgentCapabilities) AgentCapability(ctx context.Context, provider domain.ProviderKind, model string) (domain.AgentHostCapability, error) {
	if err := ctx.Err(); err != nil {
		return domain.AgentHostCapability{}, err
	}
	if r.Runtime == nil {
		return domain.AgentHostCapability{}, domain.ErrProviderCapabilityUnknown
	}
	c, err := r.Runtime.AgentCapability(ctx, provider, model)
	if err != nil {
		return domain.AgentHostCapability{}, err
	}
	switch c.InputAccountingPolicy {
	case domain.NativeEstimateReserveV1:
		if c.Calibration != (domain.AgentInputCalibration{}) {
			return domain.AgentHostCapability{}, domain.ErrProviderCapabilityUnknown
		}
		// This policy has an ephemeral native baseline, not reusable measured
		// overhead. Preserve it and never consult the calibration store.
		return c, ctx.Err()
	case "", domain.MeasuredInputReserveV1:
	default:
		return domain.AgentHostCapability{}, domain.ErrProviderCapabilityUnknown
	}
	c.InputAccountingPolicy = domain.MeasuredInputReserveV1
	scope, err := c.CalibrationScope()
	if err != nil {
		return domain.AgentHostCapability{}, err
	}
	c.Calibration = domain.AgentInputCalibration{Scope: scope}
	if r.Observations != nil {
		c.Calibration, err = r.Observations.ReadAgentInputCalibration(ctx, scope)
		if err != nil {
			return domain.AgentHostCapability{}, err
		}
	}
	if err := c.Calibration.Validate(scope); err != nil {
		return domain.AgentHostCapability{}, err
	}
	return c, ctx.Err()
}

// RecordAgentInputObservation is separate from preparation. Only a native
// adapter's correlated first-request outcome may call it; no model execution or
// automatic retry occurs here. A caller must reprepare and reauthorize before
// using the returned retry eligibility, and must record its retry attempt.
func RecordAgentInputObservation(ctx context.Context, store outbound.AgentInputCalibrationStore, p domain.AgentContextPackage, observation domain.AgentInputObservation) (domain.AgentInputCalibration, bool, error) {
	if err := ctx.Err(); err != nil {
		return domain.AgentInputCalibration{}, false, err
	}
	if store == nil || p.Budget == nil || p.Budget.InputAccountingPolicy != domain.MeasuredInputReserveV1 {
		return domain.AgentInputCalibration{}, false, domain.ErrProviderCapabilityUnknown
	}
	if err := p.ValidateIdentity(); err != nil {
		return domain.AgentInputCalibration{}, false, err
	}
	feedback, retry, err := p.Budget.ObserveInput(p.ID, p.Usage, observation)
	if err != nil {
		return domain.AgentInputCalibration{}, false, err
	}
	stored, err := store.MergeAgentInputCalibration(ctx, feedback)
	if err != nil {
		return domain.AgentInputCalibration{}, false, err
	}
	merged, err := stored.Merge(feedback)
	if err != nil || merged != stored {
		return domain.AgentInputCalibration{}, false, fmt.Errorf("%w: input calibration was not retained", domain.ErrHashMismatch)
	}
	return stored, retry, ctx.Err()
}
