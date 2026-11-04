//go:build postgres

package store

import (
	"bytes"
	"context"
	"errors"
	"reflect"
	"sync"
	"testing"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

func docEventDisplayBoundaryStep(test *testing.T, fixture docDisplayPGFixture, job domain.DocDisplayJob, prepared domain.DocEventDisplayPreparation) domain.DocEventDisplayStep {
	test.Helper()
	encoded, err := prepared.Encode()
	if err != nil {
		test.Fatal(err)
	}
	var result domain.DocEventDisplayStep
	if err := fixture.storage.WithinReadSnapshot(fixture.ctx, func(read context.Context) error {
		reader, err := fixture.storage.OpenDocEventDisplayWork(read, job, domain.HashContent(encoded))
		if err != nil {
			return err
		}
		result, err = reader.PrepareStep(read, prepared, func(read context.Context, offset, length int) ([]byte, error) {
			if err := read.Err(); err != nil {
				return nil, err
			}
			if offset < 0 || length <= 0 || length > domain.DocFieldDiscoveryStepBytes || offset > job.Target.Event.Length-length {
				return nil, domain.ErrIntegrity
			}
			start := job.Target.Event.Offset + offset
			return bytes.Clone(fixture.stream[start : start+length]), nil
		})
		return err
	}); err != nil {
		test.Fatal(err)
	}
	if len(result.Objects()) == 0 {
		test.Fatal("sealed boundary step has no immutable objects")
	}
	return result
}

func docEventDisplayBoundaryObjects(test *testing.T, fixture docDisplayPGFixture, source domain.ContentHash) map[domain.ContentHash][]byte {
	test.Helper()
	rows, err := fixture.storage.pool.Query(fixture.ctx, `SELECT hash,bytes FROM doc_display_objects_v1 WHERE source_hash=$1`, source)
	if err != nil {
		test.Fatal(err)
	}
	defer rows.Close()
	objects := make(map[domain.ContentHash][]byte)
	for rows.Next() {
		var hash domain.ContentHash
		var raw []byte
		if err := rows.Scan(&hash, &raw); err != nil {
			test.Fatal(err)
		}
		objects[hash] = raw
	}
	if err := rows.Err(); err != nil {
		test.Fatal(err)
	}
	return objects
}

func assertDocEventDisplayBoundaryUnpublished(test *testing.T, fixture docDisplayPGFixture, job domain.DocDisplayJob) {
	test.Helper()
	var generations int
	if err := fixture.storage.pool.QueryRow(fixture.ctx, `SELECT count(*) FROM doc_display_generations_v1
 WHERE event_hash=$1 AND profile=$2`, job.Target.Event.Hash, job.Target.DisplayProfile()).Scan(&generations); err != nil || generations != 0 {
		test.Fatal("boundary operation published a generic generation", generations, err)
	}
}

func TestDocEventDisplayPGBoundaryConcurrentSealedStepCAS(test *testing.T) {
	fixture := docEventDisplayQueuePG(test)
	enqueueDocEventDisplayQueue(test, fixture, 1)
	job := fixture.claim(test)
	prepared, err := fixture.storage.LoadDocEventDisplay(fixture.ctx, job)
	if err != nil {
		test.Fatal(err)
	}
	source, err := prepared.Source.Hash()
	if err != nil {
		test.Fatal(err)
	}
	if len(docEventDisplayBoundaryObjects(test, fixture, source)) != 0 {
		test.Fatal("initial preparation unexpectedly contains immutable objects")
	}
	step := docEventDisplayBoundaryStep(test, fixture, job, prepared)
	want := step.Preparation()
	type outcome struct {
		prepared domain.DocEventDisplayPreparation
		err      error
	}
	results := make(chan outcome, 2)
	ready := make(chan struct{}, 2)
	start := make(chan struct{})
	var workers sync.WaitGroup
	for range 2 {
		workers.Go(func() {
			ready <- struct{}{}
			<-start
			state, err := fixture.storage.SaveDocEventDisplay(fixture.ctx, job, step)
			results <- outcome{prepared: state, err: err}
		})
	}
	for range 2 {
		<-ready
	}
	close(start)
	workers.Wait()
	close(results)
	commits, conflicts := 0, 0
	for result := range results {
		switch {
		case result.err == nil:
			commits++
			if !reflect.DeepEqual(result.prepared, want) {
				test.Fatal("committed a different sealed preparation", result.prepared, want)
			}
		case errors.Is(result.err, domain.ErrConflict):
			conflicts++
			if !reflect.DeepEqual(result.prepared, domain.DocEventDisplayPreparation{}) {
				test.Fatal("conflicting save returned partial preparation", result.prepared)
			}
		default:
			test.Fatal("unexpected concurrent save result", result.err)
		}
	}
	if commits != 1 || conflicts != 1 {
		test.Fatal("same-checkpoint saves did not produce one commit and one conflict", commits, conflicts)
	}
	expected := make(map[domain.ContentHash][]byte)
	for _, object := range step.Objects() {
		expected[object.Hash] = object.Bytes
	}
	if actual := docEventDisplayBoundaryObjects(test, fixture, source); !reflect.DeepEqual(actual, expected) {
		test.Fatal("concurrent saves left omitted or additional immutable objects", len(actual), len(expected))
	}
	encoded, err := want.Encode()
	if err != nil {
		test.Fatal(err)
	}
	var checkpoint, storedSource domain.ContentHash
	var stored []byte
	if err := fixture.storage.pool.QueryRow(fixture.ctx, `SELECT preparation_hash,preparation,source_hash
 FROM doc_display_jobs_v1 WHERE `+docDisplayKey(job.Target), job.RepoID, job.DocHash, job.Target.Event.Index).
		Scan(&checkpoint, &stored, &storedSource); err != nil || checkpoint != domain.HashContent(encoded) || !bytes.Equal(stored, encoded) || storedSource != source {
		test.Fatal("concurrent saves did not atomically retain the winning checkpoint and source", err)
	}
	before := fixture.image(test)
	loaded, err := fixture.storage.LoadDocEventDisplay(fixture.ctx, job)
	if err != nil || !reflect.DeepEqual(loaded, want) || fixture.image(test) != before {
		test.Fatal("winning checkpoint did not reload without changes", loaded, err)
	}
	assertDocEventDisplayBoundaryUnpublished(test, fixture, job)
}

func TestDocEventDisplayPGBoundaryOwnershipRevokedBeforeSave(test *testing.T) {
	fixture := docEventDisplayQueuePG(test)
	enqueueDocEventDisplayQueue(test, fixture, 1)
	job := fixture.claim(test)
	prepared, err := fixture.storage.LoadDocEventDisplay(fixture.ctx, job)
	if err != nil {
		test.Fatal(err)
	}
	source, err := prepared.Source.Hash()
	if err != nil {
		test.Fatal(err)
	}
	if len(docEventDisplayBoundaryObjects(test, fixture, source)) != 0 {
		test.Fatal("initial preparation unexpectedly contains immutable objects")
	}
	step := docEventDisplayBoundaryStep(test, fixture, job, prepared)
	tag, err := fixture.storage.pool.Exec(fixture.ctx, `DELETE FROM repo_blobs WHERE repo_id=$1 AND kind='doc' AND hash=$2`, fixture.repo, fixture.doc.hash)
	if err != nil || tag.RowsAffected() != 1 {
		test.Fatal("failed to revoke the actual document ownership", tag.RowsAffected(), err)
	}
	before := fixture.image(test)
	result, err := fixture.storage.SaveDocEventDisplay(fixture.ctx, job, step)
	if !errors.Is(err, domain.ErrConflict) || !reflect.DeepEqual(result, domain.DocEventDisplayPreparation{}) {
		test.Fatal("ownership-revoked save was not fenced without a partial result", result, err)
	}
	if fixture.image(test) != before || len(docEventDisplayBoundaryObjects(test, fixture, source)) != 0 {
		test.Fatal("ownership-revoked save changed the checkpoint or wrote immutable objects")
	}
	assertDocEventDisplayBoundaryUnpublished(test, fixture, job)
}

func TestDocEventDisplayPGBoundaryWorkReaderTransactionAndSourceScope(test *testing.T) {
	fixture := docEventDisplayQueuePG(test)
	enqueueDocEventDisplayQueue(test, fixture, 1)
	job := fixture.claim(test)
	prepared, err := fixture.storage.LoadDocEventDisplay(fixture.ctx, job)
	if err != nil {
		test.Fatal(err)
	}
	step := docEventDisplayBoundaryStep(test, fixture, job, prepared)
	prepared, err = fixture.storage.SaveDocEventDisplay(fixture.ctx, job, step)
	if err != nil {
		test.Fatal(err)
	}
	source, err := prepared.Source.Hash()
	if err != nil {
		test.Fatal(err)
	}
	foreign := docEventDisplayQueuePG(test)
	enqueueDocEventDisplayQueue(test, foreign, 1)
	foreignJob := foreign.claim(test)
	foreignPrepared, err := foreign.storage.LoadDocEventDisplay(foreign.ctx, foreignJob)
	if err != nil {
		test.Fatal(err)
	}
	foreignStep := docEventDisplayBoundaryStep(test, foreign, foreignJob, foreignPrepared)
	if _, err := foreign.storage.SaveDocEventDisplay(foreign.ctx, foreignJob, foreignStep); err != nil {
		test.Fatal(err)
	}
	foreignSource, err := foreignPrepared.Source.Hash()
	if err != nil || foreignSource == source {
		test.Fatal("foreign fixture did not create a different immutable event-source scope", err)
	}
	owned := step.Objects()[0]
	foreignObject := foreignStep.Objects()[0]
	if actual := docEventDisplayBoundaryObjects(test, foreign, foreignSource); !bytes.Equal(actual[foreignObject.Hash], foreignObject.Bytes) {
		test.Fatal("foreign immutable object is absent from its actual source scope")
	}
	if _, present := docEventDisplayBoundaryObjects(test, fixture, source)[foreignObject.Hash]; present {
		test.Fatal("foreign-only object unexpectedly exists in the requesting scope")
	}
	encoded, err := prepared.Encode()
	if err != nil {
		test.Fatal(err)
	}
	before, foreignBefore := fixture.image(test), foreign.image(test)
	var retained outbound.DocEventDisplayWorkReader
	if err := fixture.storage.WithinReadSnapshot(fixture.ctx, func(read context.Context) error {
		reader, err := fixture.storage.OpenDocEventDisplayWork(read, job, domain.HashContent(encoded))
		if err != nil {
			return err
		}
		retained = reader
		if raw, err := reader.LoadObject(read, owned.Hash); err != nil || !bytes.Equal(raw, owned.Bytes) {
			test.Fatal("owning transaction could not read its own scoped object", err)
		}
		if raw, err := reader.LoadObject(read, foreignObject.Hash); !errors.Is(err, domain.ErrIntegrity) || len(raw) != 0 {
			test.Fatal("work reader found an object exclusively in another event-source scope", err)
		}
		if err := fixture.storage.WithinReadSnapshot(fixture.ctx, func(other context.Context) error {
			if raw, err := reader.LoadObject(other, owned.Hash); !errors.Is(err, domain.ErrConflict) || len(raw) != 0 {
				test.Fatal("work reader was reused by another transaction of the same store", err)
			}
			return nil
		}); err != nil {
			return err
		}
		return foreign.storage.WithinReadSnapshot(foreign.ctx, func(other context.Context) error {
			if raw, err := reader.LoadObject(other, owned.Hash); !errors.Is(err, domain.ErrConflict) || len(raw) != 0 {
				test.Fatal("work reader was reused by a foreign store transaction", err)
			}
			return nil
		})
	}); err != nil {
		test.Fatal(err)
	}
	if raw, err := retained.LoadObject(fixture.ctx, owned.Hash); !errors.Is(err, domain.ErrConflict) || len(raw) != 0 {
		test.Fatal("work reader escaped its owning transaction", err)
	}
	if fixture.image(test) != before || foreign.image(test) != foreignBefore {
		test.Fatal("work reader boundary probes changed durable state")
	}
}
