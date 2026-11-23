//go:build postgres

package store

import (
	"testing"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

func (fixture docDisplayPGFixture) discoveryStep(test *testing.T, job domain.DocDisplayJob, prior *domain.DocFieldDiscoveryState, length int) domain.DocFieldDiscoveryStep {
	test.Helper()
	var state domain.DocFieldDiscoveryState
	if prior == nil {
		var err error
		state, err = domain.NewDocFieldDiscoveryState(job.Target.Event.Hash, job.Target.Event.Length)
		if err != nil {
			test.Fatal(err)
		}
	} else {
		state = *prior
	}
	length = min(length, state.EventBytes-state.Next)
	from := job.Target.Event.Offset + state.Next
	step, err := domain.AdvanceDocFieldDiscovery(fixture.ctx, state, fixture.stream[from:from+length])
	if err != nil {
		test.Fatal(err)
	}
	return step
}

func (fixture docDisplayPGFixture) discover(test *testing.T, job domain.DocDisplayJob, prepared domain.DocDisplayPreparation) domain.DocDisplayPreparation {
	test.Helper()
	for prepared.Checkpoint == "" {
		step := fixture.discoveryStep(test, job, prepared.Discovery, domain.DocFieldDiscoveryStepBytes)
		var err error
		prepared, err = fixture.storage.SaveDocDisplayDiscovery(fixture.ctx, job, step)
		if err != nil {
			test.Fatal(err)
		}
	}
	return prepared
}

func (fixture docDisplayPGFixture) cacheSource(test *testing.T, index int) domain.DocDisplayStringSource {
	test.Helper()
	source := fixture.source(test, index)
	raw, err := source.Encode()
	if err != nil {
		test.Fatal(err)
	}
	if _, err := fixture.storage.pool.Exec(fixture.ctx, `INSERT INTO doc_display_sources_v1(event_hash,profile,source_hash,bytes)
 VALUES($1,$2,$3,$4)`, source.EventHash, source.Profile, domain.HashContent(raw), raw); err != nil {
		test.Fatal(err)
	}
	return source
}
