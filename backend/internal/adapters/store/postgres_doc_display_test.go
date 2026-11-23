//go:build postgres

package store

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

type docDisplayPGFixture struct {
	storage *PostgresStore
	ctx     context.Context
	repo    domain.ContentHash
	doc     rootReaderFixture
	targets []domain.DocDisplayTarget
	stream  []byte
}

func (fixture *docDisplayPGFixture) refreshTargets(t *testing.T) {
	t.Helper()
	if err := fixture.storage.WithinReadSnapshot(fixture.ctx, func(read context.Context) error {
		generation, err := fixture.storage.OpenDocRangeGeneration(read, fixture.repo, fixture.doc.hash)
		if err != nil {
			return err
		}
		entries, err := generation.ReadPage(read, 0)
		if err != nil {
			return err
		}
		fixture.targets = nil
		for _, entry := range entries {
			fixture.targets = append(fixture.targets, domain.DocDisplayTarget{RepoID: fixture.repo, DocHash: fixture.doc.hash,
				Binding: generation.Binding(), Event: entry.DocEventIndex})
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func (fixture docDisplayPGFixture) enqueue(t *testing.T, index int) domain.DocDisplayJob {
	t.Helper()
	job, err := fixture.storage.EnqueueDocDisplay(fixture.ctx, fixture.targets[index], time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	return job
}

func (fixture docDisplayPGFixture) claim(t *testing.T) domain.DocDisplayJob {
	t.Helper()
	job, err := fixture.storage.ClaimDocDisplay(fixture.ctx, time.Now().UTC(), time.Minute)
	if err != nil || job.RepoID != fixture.repo {
		t.Fatalf("claim: %+v %v", job, err)
	}
	return job
}

func (fixture docDisplayPGFixture) source(t *testing.T, index int) domain.DocDisplayStringSource {
	t.Helper()
	location := fixture.targets[index].Event
	raw := fixture.stream[location.Offset : location.Offset+location.Length]
	source, err := domain.BuildDocDisplayStringSourceReader(fixture.ctx, location.Hash, location.Length, bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	return source
}

func (fixture docDisplayPGFixture) initialize(t *testing.T, job domain.DocDisplayJob) domain.DocDisplayPreparation {
	t.Helper()
	prepared, err := fixture.storage.InitializeDocDisplay(fixture.ctx, job, fixture.source(t, job.Target.Event.Index))
	if err != nil {
		t.Fatal(err)
	}
	return prepared
}

func (fixture docDisplayPGFixture) step(t *testing.T, prepared domain.DocDisplayPreparation) domain.DocDisplayStringStep {
	t.Helper()
	var location domain.DocEventIndex
	for _, target := range fixture.targets {
		if target.Event.Hash == prepared.Source.EventHash {
			location = target.Event
		}
	}
	step, err := domain.StepDocDisplayString(fixture.ctx, *prepared.Source, prepared.State, func(_ context.Context, offset, length int) ([]byte, error) {
		return fixture.stream[location.Offset+offset : location.Offset+offset+length], nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return step
}

func (fixture docDisplayPGFixture) done(t *testing.T, job domain.DocDisplayJob) domain.DocDisplayPreparation {
	t.Helper()
	prepared := fixture.initialize(t, job)
	for !prepared.State.Done {
		step := fixture.step(t, prepared)
		var err error
		prepared, err = fixture.storage.SaveDocDisplayStep(fixture.ctx, job, prepared.Checkpoint, step)
		if err != nil {
			t.Fatal(err)
		}
	}
	return prepared
}

func (fixture docDisplayPGFixture) image(t *testing.T) string {
	t.Helper()
	var image string
	if err := fixture.storage.pool.QueryRow(fixture.ctx, `SELECT jsonb_build_object(
 'jobs',(SELECT jsonb_agg(to_jsonb(job) ORDER BY event_index) FROM doc_display_jobs_v1 job WHERE repo_id=$1),
 'sources',(SELECT jsonb_agg(to_jsonb(source) ORDER BY event_hash) FROM doc_display_sources_v1 source WHERE event_hash IN ($2,$3)),
 'objects',(SELECT jsonb_agg(to_jsonb(object) ORDER BY hash) FROM doc_display_objects_v1 object WHERE source_hash IN
 (SELECT source_hash FROM doc_display_sources_v1 WHERE event_hash IN ($2,$3))),
 'generations',(SELECT jsonb_agg(to_jsonb(generation) ORDER BY event_hash) FROM doc_display_generations_v1 generation WHERE event_hash IN ($2,$3)))::text`,
		fixture.repo, fixture.targets[0].Event.Hash, fixture.targets[1].Event.Hash).Scan(&image); err != nil {
		t.Fatal(err)
	}
	return image
}

func (fixture docDisplayPGFixture) read(t *testing.T, target domain.DocDisplayTarget, want error) {
	t.Helper()
	before := fixture.image(t)
	if err := fixture.storage.WithinReadSnapshot(fixture.ctx, func(read context.Context) error {
		generation, err := fixture.storage.ReadDocDisplayGeneration(read, target)
		if !errors.Is(err, want) {
			t.Fatalf("read generation: %+v %v, want %v", generation, err, want)
		}
		if err == nil {
			var text strings.Builder
			for index := range generation.Pages.Count {
				page, err := fixture.storage.ReadDocDisplayPage(read, target, index)
				if err != nil {
					return err
				}
				text.WriteString(page.Text)
			}
			if text.String() != fixture.doc.cir.Events[target.Event.Index].Output {
				t.Fatal("display text differs from source")
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if after := fixture.image(t); after != before {
		t.Fatal("reader modified durable state")
	}
}
