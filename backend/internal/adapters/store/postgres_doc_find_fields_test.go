//go:build postgres

package store

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

type docFindFieldsPGFixture struct {
	storage *PostgresStore
	ctx     context.Context
	repo    domain.ContentHash
	doc     domain.ContentHash
}

func (fixture docFindFieldsPGFixture) enqueue(t *testing.T) domain.DocRangeIndexJob {
	t.Helper()
	job, err := fixture.storage.EnqueueDocFindFields(fixture.ctx, fixture.repo, fixture.doc, time.Now().UTC())
	if err != nil || job.BaseHash != "" {
		t.Fatalf("enqueue: %+v %v", job, err)
	}
	return job
}

func (fixture docFindFieldsPGFixture) claim(t *testing.T) domain.DocRangeIndexJob {
	t.Helper()
	job, err := fixture.storage.ClaimDocFindFields(fixture.ctx, time.Now().UTC(), time.Minute)
	if err != nil || job.RepoID != fixture.repo || job.DocHash != fixture.doc || job.BaseHash != "" || job.State != "running" {
		t.Fatalf("claim: %+v %v", job, err)
	}
	return job
}

func (fixture docFindFieldsPGFixture) finish(t *testing.T, job domain.DocRangeIndexJob, state string, want error) {
	t.Helper()
	now := time.Now().UTC()
	job.State, job.NextAttempt = state, now
	if err := fixture.storage.FinishDocFindFields(fixture.ctx, job, now); !errors.Is(err, want) {
		t.Fatalf("finish %s: %v, want %v", state, err, want)
	}
}

func (fixture docFindFieldsPGFixture) read(t *testing.T, repo domain.ContentHash, fields domain.DocFindFields, want error) {
	t.Helper()
	err := fixture.storage.WithinReadSnapshot(fixture.ctx, func(read context.Context) error {
		got, err := fixture.storage.ReadDocFindFields(read, repo, fixture.doc, fields.EventHash, fields.EventBytes)
		if !errors.Is(err, want) {
			t.Fatalf("read: %+v %v, want %v", got, err, want)
		}
		if err == nil && !reflect.DeepEqual(got, fields) {
			t.Fatalf("read fields: %+v, want %+v", got, fields)
		}
		if err != nil && !reflect.DeepEqual(got, domain.DocFindFields{}) {
			t.Fatalf("failed read returned fields: %+v", got)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func (fixture docFindFieldsPGFixture) changeSource(t *testing.T) {
	t.Helper()
	if _, err := fixture.storage.pool.Exec(fixture.ctx, `UPDATE blobs SET bytes=bytes WHERE hash=$1`, fixture.doc); err != nil {
		t.Fatal(err)
	}
	if err := fixture.storage.WithinRepository(fixture.ctx, fixture.repo, func(bound context.Context) error {
		return fixture.storage.ensureRangeGeneration(bound, fixture.repo, fixture.doc)
	}); err != nil {
		t.Fatal(err)
	}
}
