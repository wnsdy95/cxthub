//go:build postgres

package store

import (
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

func TestDocDisplayLifecyclePGPriorConflictPrecedesEpochRetry(test *testing.T) {
	fixture, job, prepared := docCatalogProtocolFixturePG(test)
	step := prepareDocDisplayLifecycleStepPG(test, fixture, job, prepared, nil)
	if saved, err := fixture.storage.SaveDocEventDisplay(fixture.ctx, job, step); err != nil || !reflect.DeepEqual(saved, step.Preparation()) {
		test.Fatal("could not advance the actual epoch-bound checkpoint", err)
	}
	advanceLiveDocDisplayLifecycleEpochFixturePG(test, fixture, job)
	before := docDisplayLifecycleImagePG(test, fixture)
	result, err := fixture.storage.SaveDocEventDisplay(fixture.ctx, job, step)
	if !errors.Is(err, domain.ErrConflict) || errors.Is(err, domain.ErrDocDisplayIncarnationChanged) || !reflect.DeepEqual(result, domain.DocEventDisplayPreparation{}) || docDisplayLifecycleImagePG(test, fixture) != before {
		test.Fatal("a superseded predecessor became retryable only because the epoch later changed", result, err)
	}
}

func TestDocDisplayLifecyclePGCompletedPeerKeepsHistoricalEpoch(test *testing.T) {
	fixture, job, completed, _ := docCatalogProgressCompletedPeersPG(test, 1)
	original := readDocDisplayLifecycleFactPG(test, fixture.ctx, fixture.storage.pool, job)
	changed := advanceLiveDocDisplayLifecycleEpochFixturePG(test, fixture, job)
	if changed.epoch == original.epoch {
		test.Fatal("fixture did not separate the current lifecycle from completed provenance")
	}
	for _, fresh := range []bool{false, true} {
		receiver, claim := fixture, job
		if fresh {
			receiver = docDisplayPG(test, fixture.doc.cir.Events...)
			enqueueDocEventDisplayQueue(test, receiver, 0)
			claim = receiver.claim(test)
		}
		adopted, err := receiver.storage.LoadDocEventDisplay(receiver.ctx, claim)
		if err != nil || !reflect.DeepEqual(adopted, completed) {
			test.Fatal("verified completed CAS could not be adopted independently of the newer private epoch", err)
		}
		fact := readDocDisplayLifecycleFactPG(test, receiver.ctx, receiver.storage.pool, claim)
		if fact.epoch != changed.epoch || fact.jobEpoch != original.epoch || fact.generation != original.generation || fact.checkpoint != docCatalogUseCheckpointPG(test, completed) {
			test.Fatal("completed peer adoption relabeled historical provenance with the current epoch", fact)
		}
		if err := receiver.storage.CompleteDocEventDisplay(receiver.ctx, claim, fact.checkpoint, time.Now().UTC()); err != nil {
			test.Fatal("completed historical CAS required the private epoch to become current again", err)
		}
	}
}
