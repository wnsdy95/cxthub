//go:build postgres

package app

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

type docDisplayJobProbe struct {
	outbound.DocDisplayStore
	initialized     int
	discovered      int
	verified        int
	discoveryFrom   []int
	saved           int
	completed       int
	beforeSave      func() error
	beforeDiscovery func() error
}

func newDocDisplayPGFixture(test *testing.T) docViewPGFixture {
	test.Helper()
	fixture := newDocViewPGFixture(test)
	test.Cleanup(func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if _, err := fixture.pool.Exec(cleanup, `DELETE FROM doc_display_jobs_v1 WHERE repo_id=$1`, fixture.repo); err != nil {
			test.Error(err)
		}
	})
	return fixture
}

func (probe *docDisplayJobProbe) InitializeDocDisplay(ctx context.Context, job domain.DocDisplayJob, source domain.DocDisplayStringSource) (domain.DocDisplayPreparation, error) {
	probe.initialized++
	return probe.DocDisplayStore.InitializeDocDisplay(ctx, job, source)
}

func (probe *docDisplayJobProbe) SaveDocDisplayDiscovery(ctx context.Context, job domain.DocDisplayJob, step domain.DocFieldDiscoveryStep) (domain.DocDisplayPreparation, error) {
	if probe.beforeDiscovery != nil {
		if err := probe.beforeDiscovery(); err != nil {
			return domain.DocDisplayPreparation{}, err
		}
	}
	progress, err := probe.DocDisplayStore.SaveDocDisplayDiscovery(ctx, job, step)
	if err == nil {
		probe.discovered++
		probe.discoveryFrom = append(probe.discoveryFrom, step.From())
		if progress.Source != nil {
			probe.verified++
		}
	}
	return progress, err
}

func (probe *docDisplayJobProbe) SaveDocDisplayStep(ctx context.Context, job domain.DocDisplayJob, checkpoint domain.ContentHash, step domain.DocDisplayStringStep) (domain.DocDisplayPreparation, error) {
	if probe.beforeSave != nil {
		if err := probe.beforeSave(); err != nil {
			return domain.DocDisplayPreparation{}, err
		}
	}
	progress, err := probe.DocDisplayStore.SaveDocDisplayStep(ctx, job, checkpoint, step)
	if err == nil {
		probe.saved++
	}
	return progress, err
}

func (probe *docDisplayJobProbe) CompleteDocDisplay(ctx context.Context, job domain.DocDisplayJob, checkpoint domain.ContentHash, now time.Time) error {
	err := probe.DocDisplayStore.CompleteDocDisplay(ctx, job, checkpoint, now)
	if err == nil {
		probe.completed++
	}
	return err
}

func docDisplayTargetPG(test *testing.T, fixture docViewPGFixture, hash domain.ContentHash, index int) domain.DocDisplayTarget {
	test.Helper()
	target, err := repositoryReadForRepo(fixture.ctx, fixture.service, fixture.repo, func(read context.Context) (domain.DocDisplayTarget, error) {
		source, legacy, err := fixture.service.legacyGenerationSource(read, fixture.repo, hash)
		if err != nil {
			return domain.DocDisplayTarget{}, err
		}
		if !legacy {
			source, err = fixture.service.docViewSource(read, fixture.repo, hash, false)
			if err != nil {
				return domain.DocDisplayTarget{}, err
			}
		}
		entry, err := source.location(index)
		return domain.DocDisplayTarget{RepoID: fixture.repo, DocHash: hash, Binding: source.binding, Event: entry}, err
	})
	if err != nil || target.Validate() != nil {
		test.Fatal(target, err)
	}
	return target
}

func readDocDisplayGenerationPG(fixture docViewPGFixture, target domain.DocDisplayTarget) (domain.DocDisplayStringGeneration, error) {
	return repositoryReadForRepo(fixture.ctx, fixture.service, fixture.repo, func(read context.Context) (domain.DocDisplayStringGeneration, error) {
		return fixture.storage.ReadDocDisplayGeneration(read, target)
	})
}

func TestPGDocDisplayWorkerResumesBoundedBatchesWithoutPublishingStaging(test *testing.T) {
	for _, legacy := range []bool{false, true} {
		name := "root"
		if legacy {
			name = "legacy"
		}
		test.Run(name, func(test *testing.T) {
			fixture := newDocDisplayPGFixture(test)
			output := string(fixture.repo) + strings.Repeat("a🙂<&>\n", 100_000)
			document := domain.CIRDocument{Envelope: fixture.cir.Envelope, Events: []domain.CIREvent{{Kind: domain.EventToolResult, Output: output}}}
			var hash domain.ContentHash
			if legacy {
				hash = seedLegacyFindPGDocument(test, fixture, document.Events).Hash
				prepareLegacyFindGenerationPG(test, fixture, hash)
			} else {
				hash = seedDocViewBoundaryPG(test, fixture, document)
			}
			target := docDisplayTargetPG(test, fixture, hash, 0)
			queued, err := fixture.storage.EnqueueDocDisplay(fixture.ctx, target, time.Now().UTC())
			if err != nil || queued.State != "waiting" {
				test.Fatal(queued, err)
			}
			probe := &docDisplayJobProbe{DocDisplayStore: fixture.storage}
			fixture.service.ConfigureDocumentDisplay(probe)
			if err := fixture.service.ProcessDocDisplayJobs(fixture.ctx, 1); err != nil {
				test.Fatal(err)
			}
			if probe.discovered != docDisplayJobSteps || probe.saved != 0 || probe.initialized != 0 || probe.verified != 0 || probe.completed != 0 {
				test.Fatal("preparation did not yield at bounded checkpoint", probe)
			}
			if _, err := readDocDisplayGenerationPG(fixture, target); !errors.Is(err, domain.ErrNotFound) {
				test.Fatal("staging became readable before completion", err)
			}
			for attempt := 0; attempt < 400; attempt++ {
				restarted := NewService(fixture.storage, fixture.storage, nil, nil, fixture.storage)
				restarted.ConfigureDocumentDisplay(probe)
				if err := restarted.ProcessDocDisplayJobs(fixture.ctx, 1); err != nil {
					test.Fatal("new process could not resume persisted work", err)
				}
				if probe.completed == 1 {
					break
				}
			}
			generation, err := readDocDisplayGenerationPG(fixture, target)
			if err != nil || generation.TextBytes != len(output) || probe.completed != 1 || probe.initialized != 0 || probe.verified != 1 || probe.discovered != (target.Event.Length+domain.DocFieldDiscoveryStepBytes-1)/domain.DocFieldDiscoveryStepBytes {
				test.Fatal("prepared generation lost content or repeated discovery", generation, probe, err)
			}
			for index, from := range probe.discoveryFrom {
				if from != index*domain.DocFieldDiscoveryStepBytes {
					test.Fatal("discovery restarted or skipped a source range", index, from)
				}
			}
			var text strings.Builder
			for index := 0; index < generation.Pages.Count; index++ {
				page, err := repositoryReadForRepo(fixture.ctx, fixture.service, fixture.repo, func(read context.Context) (domain.DocDisplayPage, error) {
					return fixture.storage.ReadDocDisplayPage(read, target, index)
				})
				if err != nil || page.From != text.Len() {
					test.Fatal("selected page did not continue exact display text", page, err)
				}
				_, _ = text.WriteString(page.Text)
			}
			if text.String() != output {
				test.Fatal("prepared pages differ from original displayed output")
			}
			if err := fixture.service.ProcessDocDisplayJobs(fixture.ctx, 2); err != nil || probe.completed != 1 {
				test.Fatal("completed work repeated", err, probe.completed)
			}
			test.Logf("pages=%d render_steps=%d discovery_steps=%d source_verifications=%d output_bytes=%d", generation.Pages.Count, probe.saved, probe.discovered, probe.verified, generation.TextBytes)
		})
	}
}

func TestPGDocDisplayWorkerRecoversTransientFailureWithoutRepeatingDiscovery(test *testing.T) {
	fixture := newDocDisplayPGFixture(test)
	output := string(fixture.repo) + strings.Repeat("x", 90_000)
	hash := seedDocViewBoundaryPG(test, fixture, domain.CIRDocument{Envelope: fixture.cir.Envelope, Events: []domain.CIREvent{{Kind: domain.EventToolResult, Output: output}}})
	target := docDisplayTargetPG(test, fixture, hash, 0)
	if _, err := fixture.storage.EnqueueDocDisplay(fixture.ctx, target, time.Now().UTC()); err != nil {
		test.Fatal(err)
	}
	interrupted := errors.New("synthetic persistence interruption")
	probe := &docDisplayJobProbe{DocDisplayStore: fixture.storage}
	probe.beforeSave = func() error {
		if probe.saved == 1 {
			return interrupted
		}
		return nil
	}
	fixture.service.ConfigureDocumentDisplay(probe)
	if err := fixture.service.ProcessDocDisplayJobs(fixture.ctx, 1); !errors.Is(err, interrupted) {
		test.Fatal(err)
	}
	if _, err := fixture.pool.Exec(fixture.ctx, `UPDATE doc_display_jobs_v1 SET next_attempt=clock_timestamp() WHERE repo_id=$1 AND doc_hash=$2 AND event_index=$3`, fixture.repo, hash, target.Event.Index); err != nil {
		test.Fatal(err)
	}
	job, err := fixture.storage.ClaimDocDisplay(fixture.ctx, time.Now().UTC(), time.Minute)
	if err != nil {
		test.Fatal(err)
	}
	progress, err := fixture.storage.LoadDocDisplayJob(fixture.ctx, job)
	if err != nil || progress.Source == nil || progress.State.Directory.Count != 1 || progress.Checkpoint == "" {
		test.Fatal("last successful checkpoint was not retained", progress, err)
	}
	probe.beforeSave = nil
	restarted := NewService(fixture.storage, fixture.storage, nil, nil, fixture.storage)
	restarted.ConfigureDocumentDisplay(probe)
	if err := restarted.runDocDisplayJob(fixture.ctx, job); err != nil {
		test.Fatal(err)
	}
	generation, err := readDocDisplayGenerationPG(fixture, target)
	if err != nil || generation.TextBytes != len(output) || probe.initialized != 0 || probe.verified != 1 || probe.completed != 1 {
		test.Fatal(generation, err, probe)
	}
}

func TestPGDocDisplayWorkerHandlesEmptyAndUnsupportedWithoutRawFallback(test *testing.T) {
	for _, empty := range []bool{false, true} {
		fixture := newDocDisplayPGFixture(test)
		var output any = map[string]any{"opaque": string(fixture.repo)}
		if empty {
			output = strings.Repeat(" \t\n", 60_000)
		}
		hash := seedDocViewBoundaryPG(test, fixture, domain.CIRDocument{Envelope: fixture.cir.Envelope, Events: []domain.CIREvent{{Kind: domain.EventToolResult, Output: output}}})
		target := docDisplayTargetPG(test, fixture, hash, 0)
		if _, err := fixture.storage.EnqueueDocDisplay(fixture.ctx, target, time.Now().UTC()); err != nil {
			test.Fatal(err)
		}
		probe := &docDisplayJobProbe{DocDisplayStore: fixture.storage}
		fixture.service.ConfigureDocumentDisplay(probe)
		err := fixture.service.ProcessDocDisplayJobs(fixture.ctx, 1)
		if empty {
			generation, readErr := readDocDisplayGenerationPG(fixture, target)
			if err != nil || readErr != nil || generation.TextBytes != 0 || generation.Pages.Count != 0 || probe.saved != 0 || probe.completed != 1 {
				test.Fatal(generation, err, readErr, probe)
			}
		} else {
			if !errors.Is(err, domain.ErrDocFindDisplayUnavailable) || probe.initialized != 0 || probe.saved != 0 || probe.completed != 0 {
				test.Fatal("unsupported tool formatting was reinterpreted", err, probe)
			}
			if _, err := readDocDisplayGenerationPG(fixture, target); !errors.Is(err, domain.ErrNotFound) {
				test.Fatal("unsupported raw source became searchable", err)
			}
		}
	}
}

func TestPGDocDisplayWorkerFencesSourceReplacementBetweenReadAndSave(test *testing.T) {
	fixture := newDocDisplayPGFixture(test)
	output := string(fixture.repo) + strings.Repeat("z", 80_000)
	hash := seedDocViewBoundaryPG(test, fixture, domain.CIRDocument{Envelope: fixture.cir.Envelope, Events: []domain.CIREvent{{Kind: domain.EventToolResult, Output: output}}})
	target := docDisplayTargetPG(test, fixture, hash, 0)
	if _, err := fixture.storage.EnqueueDocDisplay(fixture.ctx, target, time.Now().UTC()); err != nil {
		test.Fatal(err)
	}
	probe := &docDisplayJobProbe{DocDisplayStore: fixture.storage}
	probe.beforeSave = func() error {
		probe.beforeSave = nil
		_, err := fixture.pool.Exec(fixture.ctx, `UPDATE blobs SET bytes=bytes WHERE hash=$1`, hash)
		return err
	}
	fixture.service.ConfigureDocumentDisplay(probe)
	if err := fixture.service.ProcessDocDisplayJobs(fixture.ctx, 1); !errors.Is(err, domain.ErrConflict) {
		test.Fatal("source replaced after bounded read was accepted", err, "initializations", probe.initialized, "saved", probe.saved, "completed", probe.completed)
	}
	if probe.saved != 0 || probe.completed != 0 {
		test.Fatal("stale work was committed", probe)
	}
	if _, err := fixture.storage.EnqueueDocRangeJob(fixture.ctx, fixture.repo, hash, time.Now().UTC()); err != nil {
		test.Fatal(err)
	}
	if err := fixture.service.ProcessDocRangeJobs(fixture.ctx, 1); err != nil {
		test.Fatal(err)
	}
	current := docDisplayTargetPG(test, fixture, hash, 0)
	if current.Binding.SourceID == target.Binding.SourceID {
		test.Fatal("source replacement fixture did not change the incarnation")
	}
	if _, err := readDocDisplayGenerationPG(fixture, current); !errors.Is(err, domain.ErrNotFound) {
		test.Fatal("stale work was published", err)
	}
	if _, err := fixture.storage.EnqueueDocDisplay(fixture.ctx, current, time.Now().UTC()); err != nil {
		test.Fatal("new incarnation could not prepare after stale work rejection", err)
	}
	if err := fixture.service.ProcessDocDisplayJobs(fixture.ctx, 1); err != nil {
		test.Fatal(err)
	}
	generation, err := readDocDisplayGenerationPG(fixture, current)
	if err != nil || generation.TextBytes != len(output) || probe.completed != 1 {
		test.Fatal(generation, err, probe)
	}
}

func TestPGDocDisplayWorkerResumesDiscoveryAfterPersistenceFailure(test *testing.T) {
	fixture := newDocDisplayPGFixture(test)
	output := string(fixture.repo) + strings.Repeat("resume-source-", 80_000)
	hash := seedDocViewBoundaryPG(test, fixture, domain.CIRDocument{Envelope: fixture.cir.Envelope, Events: []domain.CIREvent{{Kind: domain.EventToolResult, Output: output}}})
	target := docDisplayTargetPG(test, fixture, hash, 0)
	if _, err := fixture.storage.EnqueueDocDisplay(fixture.ctx, target, time.Now().UTC()); err != nil {
		test.Fatal(err)
	}
	interrupted := errors.New("discovery persistence interrupted")
	probe := &docDisplayJobProbe{DocDisplayStore: fixture.storage}
	probe.beforeDiscovery = func() error {
		if probe.discovered == 1 {
			return interrupted
		}
		return nil
	}
	fixture.service.ConfigureDocumentDisplay(probe)
	if err := fixture.service.ProcessDocDisplayJobs(fixture.ctx, 1); !errors.Is(err, interrupted) {
		test.Fatal("discovery save failure was lost", err)
	}
	if _, err := fixture.pool.Exec(fixture.ctx, `UPDATE doc_display_jobs_v1 SET next_attempt=clock_timestamp() WHERE repo_id=$1`, fixture.repo); err != nil {
		test.Fatal(err)
	}
	job, err := fixture.storage.ClaimDocDisplay(fixture.ctx, time.Now().UTC(), time.Minute)
	if err != nil {
		test.Fatal(err)
	}
	progress, err := fixture.storage.LoadDocDisplayJob(fixture.ctx, job)
	if err != nil || progress.Source != nil || progress.Checkpoint != "" || progress.Discovery == nil || progress.Discovery.Next != domain.DocFieldDiscoveryStepBytes || progress.DiscoveryCheckpoint == "" {
		test.Fatal("interrupted source discovery was lost or prematurely published", progress, err)
	}
	probe.beforeDiscovery = nil
	restarted := NewService(fixture.storage, fixture.storage, nil, nil, fixture.storage)
	restarted.ConfigureDocumentDisplay(probe)
	if err := restarted.runDocDisplayJob(fixture.ctx, job); err != nil {
		test.Fatal(err)
	}
	for attempt := 0; attempt < 30 && probe.completed == 0; attempt++ {
		if err := restarted.ProcessDocDisplayJobs(fixture.ctx, 1); err != nil {
			test.Fatal(err)
		}
	}
	generation, err := readDocDisplayGenerationPG(fixture, target)
	if err != nil || generation.TextBytes != len(output) || probe.completed != 1 || probe.verified != 1 || probe.initialized != 0 || probe.discovered != (target.Event.Length+domain.DocFieldDiscoveryStepBytes-1)/domain.DocFieldDiscoveryStepBytes {
		test.Fatal("restarted discovery did not converge exactly once", generation, err, probe)
	}
	for index, from := range probe.discoveryFrom {
		if from != index*domain.DocFieldDiscoveryStepBytes {
			test.Fatal("restarted source was rescanned or skipped", index, from)
		}
	}
}

func TestPGDocDisplayWorkerSharesDiscoveryAndRenderClaimBudget(test *testing.T) {
	fixture := newDocDisplayPGFixture(test)
	output := string(fixture.repo) + strings.Repeat("x", 300_000)
	hash := seedDocViewBoundaryPG(test, fixture, domain.CIRDocument{Envelope: fixture.cir.Envelope, Events: []domain.CIREvent{{Kind: domain.EventToolResult, Output: output}}})
	target := docDisplayTargetPG(test, fixture, hash, 0)
	if _, err := fixture.storage.EnqueueDocDisplay(fixture.ctx, target, time.Now().UTC()); err != nil {
		test.Fatal(err)
	}
	probe := &docDisplayJobProbe{DocDisplayStore: fixture.storage}
	fixture.service.ConfigureDocumentDisplay(probe)
	if err := fixture.service.ProcessDocDisplayJobs(fixture.ctx, 1); err != nil {
		test.Fatal(err)
	}
	if probe.discovered != 5 || probe.saved != 3 || probe.verified != 1 || probe.completed != 0 {
		test.Fatal("phase transition exceeded the shared claim budget", probe)
	}
	if _, err := readDocDisplayGenerationPG(fixture, target); !errors.Is(err, domain.ErrNotFound) {
		test.Fatal("partly rendered generation became public", err)
	}
	if err := fixture.service.ProcessDocDisplayJobs(fixture.ctx, 2); err != nil {
		test.Fatal(err)
	}
	generation, err := readDocDisplayGenerationPG(fixture, target)
	if err != nil || generation.TextBytes != len(output) || probe.completed != 1 || probe.verified != 1 {
		test.Fatal(generation, err, probe)
	}
}

func TestPGDocDisplayWorkerFencesSourceReplacementDuringDiscovery(test *testing.T) {
	fixture := newDocDisplayPGFixture(test)
	output := string(fixture.repo) + strings.Repeat("x", 90_000)
	hash := seedDocViewBoundaryPG(test, fixture, domain.CIRDocument{Envelope: fixture.cir.Envelope, Events: []domain.CIREvent{{Kind: domain.EventToolResult, Output: output}}})
	target := docDisplayTargetPG(test, fixture, hash, 0)
	if _, err := fixture.storage.EnqueueDocDisplay(fixture.ctx, target, time.Now().UTC()); err != nil {
		test.Fatal(err)
	}
	probe := &docDisplayJobProbe{DocDisplayStore: fixture.storage}
	probe.beforeDiscovery = func() error {
		probe.beforeDiscovery = nil
		_, err := fixture.pool.Exec(fixture.ctx, `UPDATE blobs SET bytes=bytes WHERE hash=$1`, hash)
		return err
	}
	fixture.service.ConfigureDocumentDisplay(probe)
	if err := fixture.service.ProcessDocDisplayJobs(fixture.ctx, 1); !errors.Is(err, domain.ErrConflict) {
		test.Fatal("source replacement between discovery read and commit was accepted", err)
	}
	if probe.discovered != 0 || probe.verified != 0 || probe.saved != 0 || probe.completed != 0 {
		test.Fatal("stale discovery produced evidence or output", probe)
	}
}
