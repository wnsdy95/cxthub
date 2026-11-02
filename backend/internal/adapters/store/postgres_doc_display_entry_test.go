//go:build postgres

package store

import (
	"errors"
	"testing"
	"time"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

func TestDocDisplayPGLoadUsesOneLockedEntryProof(test *testing.T) {
	fixture := docDisplayPG(test)
	target := fixture.targets[0]
	target.Profile = domain.DocDisplayProjectionProfile
	if _, err := fixture.storage.EnqueueDocDisplay(fixture.ctx, target, time.Now().UTC()); err != nil {
		test.Fatal(err)
	}
	job := fixture.claim(test)
	trace := &docRangeScopeTracePG{}
	storage := docRangeBoundaryPGTracedStore(test, fixture.storage, fixture.ctx, trace)
	for range 2 {
		before := trace.facts()
		identities := trace.identities.Load()
		state, err := storage.LoadDocEventDisplay(fixture.ctx, job)
		if err != nil || state.Source.EventHash != job.Target.Event.Hash || state.Source.Kind != domain.EventToolResult {
			test.Fatal("load lost the locked canonical entry", state, err)
		}
		after := trace.facts()
		if after[0] != before[0]+1 || trace.identities.Load() != identities+1 || after[1] <= before[1] {
			test.Fatal("write load duplicated or cached its target proof", before, after, identities, trace.identities.Load())
		}
	}
	if _, err := fixture.storage.pool.Exec(fixture.ctx, `UPDATE blobs SET bytes=bytes WHERE hash=$1`, fixture.doc.hash); err != nil {
		test.Fatal(err)
	}
	if _, err := storage.LoadDocEventDisplay(fixture.ctx, job); !errors.Is(err, domain.ErrConflict) {
		test.Fatal("new write load reused a prior source incarnation", err)
	}
}
