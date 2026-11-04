//go:build postgres

package store

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

func docNormalizationPGMaintainLease(test *testing.T, storage *PostgresStore, ctx context.Context,
	job domain.DocRangeIndexJob, lease time.Duration) func() {
	test.Helper()
	if err := storage.RenewDocRangeJob(ctx, job, time.Now().UTC(), lease); err != nil {
		test.Fatal("start fixture lease renewal", err)
	}
	renewal, cancel := context.WithCancel(ctx)
	finished := make(chan error, 1)
	go func() {
		ticker := time.NewTicker(lease / 3)
		defer ticker.Stop()
		for {
			select {
			case <-renewal.Done():
				finished <- nil
				return
			case <-ticker.C:
				if err := storage.RenewDocRangeJob(renewal, job, time.Now().UTC(), lease); err != nil {
					if errors.Is(err, context.Canceled) && renewal.Err() != nil {
						err = nil
					}
					finished <- err
					return
				}
			}
		}
	}()
	var once sync.Once
	stop := func() {
		once.Do(func() {
			cancel()
			if err := <-finished; err != nil {
				test.Error("fixture owner lost its renewable lease", err)
			}
		})
	}
	test.Cleanup(stop)
	return stop
}

func docNormalizationPGLeaseImage(before, after string) bool {
	var prior, current map[string]any
	if json.Unmarshal([]byte(before), &prior) != nil || json.Unmarshal([]byte(after), &current) != nil {
		return false
	}
	priorJob, priorOK := prior["job"].([]any)
	currentJob, currentOK := current["job"].([]any)
	if !priorOK || !currentOK || len(priorJob) != 2 || len(currentJob) != 2 {
		return false
	}
	priorFields, priorOK := priorJob[0].(map[string]any)
	currentFields, currentOK := currentJob[0].(map[string]any)
	if !priorOK || !currentOK || priorFields["lease_until"] == nil || currentFields["lease_until"] == nil {
		return false
	}
	currentFields["lease_until"], currentJob[1] = priorFields["lease_until"], priorJob[1]
	return reflect.DeepEqual(prior, current)
}

func docNormalizationPGKeepLease(test *testing.T, storage *PostgresStore, ctx context.Context,
	fixture legacyByteStagePGFixture, job domain.DocRangeIndexJob, lease time.Duration, public *string) func() error {
	test.Helper()
	nextRenewal := job.LeaseUntil.Add(-2 * lease / 3)
	return func() error {
		now := time.Now().UTC()
		if now.Before(nextRenewal) {
			return nil
		}
		if legacyByteStagePGPublicImage(test, storage, ctx, fixture) != *public {
			return errors.New("normalization changed public state before fixture lease renewal")
		}
		if err := storage.RenewDocRangeJob(ctx, job, now, lease); err != nil {
			return err
		}
		after := legacyByteStagePGPublicImage(test, storage, ctx, fixture)
		if !docNormalizationPGLeaseImage(*public, after) {
			return errors.New("fixture renewal changed more than the job lease and row version")
		}
		*public = after
		nextRenewal = now.Add(lease / 3)
		return nil
	}
}

func TestDocNormalizationPGFixtureLeaseRenewal(test *testing.T) {
	storage, ctx := chunkReusePGContext(test, test.Context())
	fixture := legacyByteStagePGSource(test, storage, ctx, 1024)
	storage.ConfigureLegacyByteRangePublication(true)
	if _, err := storage.EnqueueDocByteRangeJob(ctx, fixture.repo, fixture.hash, time.Now().UTC()); err != nil {
		test.Fatal(err)
	}
	const lease = 3 * time.Second
	job, err := storage.ClaimDocRangeJob(ctx, time.Now().UTC(), lease)
	if err != nil || job.RepoID != fixture.repo || job.DocHash != fixture.hash {
		test.Fatal("claim fixture lease", job, err)
	}
	public := legacyByteStagePGPublicImage(test, storage, ctx, fixture)
	renew := docNormalizationPGKeepLease(test, storage, ctx, fixture, job, lease, &public)
	initial := public
	if err := renew(); err != nil || public != initial {
		test.Fatal("fresh fixture lease was renewed early", err)
	}
	ticker := time.NewTicker(lease / 3)
	defer ticker.Stop()
	wait := func() {
		select {
		case <-ticker.C:
		case <-ctx.Done():
			test.Fatal(ctx.Err())
		}
	}
	for time.Now().Before(job.LeaseUntil) {
		wait()
		if err := renew(); err != nil {
			test.Fatal("valid fixture owner lost its renewable lease", err)
		}
	}
	current := docRangeJobPGGet(test, storage, ctx, fixture.repo, fixture.hash)
	if public == initial || !current.LeaseUntil.After(time.Now()) || !current.LeaseUntil.After(job.LeaseUntil) || current.Version != job.Version {
		test.Fatal("fixture did not outlive its initial lease with the same owner", job, current)
	}
	legacyBytePrepareBoundaryWrite(test, storage, ctx, fixture, "lease expiry")
	public = legacyByteStagePGPublicImage(test, storage, ctx, fixture)
	for time.Now().Before(current.LeaseUntil) {
		wait()
	}
	if err := renew(); !errors.Is(err, domain.ErrConflict) {
		test.Fatal("fixture renewal revived an intentionally expired lease", err)
	}
	if after := legacyByteStagePGPublicImage(test, storage, ctx, fixture); after != public {
		test.Fatal("rejected fixture renewal changed fenced public state")
	}
}

func TestDocNormalizationFixtureLeaseImage(test *testing.T) {
	before := `{"job":[{"lease_until":"before","version":1},"xmin-before"],"source":"original"}`
	for _, scenario := range []struct {
		name  string
		after string
		valid bool
	}{
		{"renewal", `{"job":[{"lease_until":"after","version":1},"xmin-after"],"source":"original"}`, true},
		{"job fence", `{"job":[{"lease_until":"after","version":2},"xmin-after"],"source":"original"}`, false},
		{"source", `{"job":[{"lease_until":"after","version":1},"xmin-after"],"source":"changed"}`, false},
		{"missing job", `{"job":null,"source":"original"}`, false},
	} {
		test.Run(scenario.name, func(test *testing.T) {
			if docNormalizationPGLeaseImage(before, scenario.after) != scenario.valid {
				test.Fatal("fixture renewal hid an unrelated public mutation", scenario.after)
			}
		})
	}
}

func TestDocNormalizationPGMaintainedLeaseStopsBeforeFence(test *testing.T) {
	storage, ctx := chunkReusePGContext(test, test.Context())
	fixture := legacyByteStagePGSource(test, storage, ctx, 1024)
	storage.ConfigureLegacyByteRangePublication(true)
	if _, err := storage.EnqueueDocByteRangeJob(ctx, fixture.repo, fixture.hash, time.Now().UTC()); err != nil {
		test.Fatal(err)
	}
	const lease = 3 * time.Second
	job, err := storage.ClaimDocRangeJob(ctx, time.Now().UTC(), lease)
	if err != nil || job.RepoID != fixture.repo || job.DocHash != fixture.hash {
		test.Fatal("claim fixture lease", job, err)
	}
	stop := docNormalizationPGMaintainLease(test, storage, ctx, job, lease)
	timer := time.NewTimer(time.Until(job.LeaseUntil))
	defer timer.Stop()
	select {
	case <-timer.C:
	case <-ctx.Done():
		test.Fatal(ctx.Err())
	}
	stop()
	stop()
	current := docRangeJobPGGet(test, storage, ctx, fixture.repo, fixture.hash)
	if !current.LeaseUntil.After(time.Now()) || !current.LeaseUntil.After(job.LeaseUntil) || current.Version != job.Version {
		test.Fatal("background fixture renewal lost the original owner", job, current)
	}
	legacyBytePrepareBoundaryWrite(test, storage, ctx, fixture, "lease expiry")
	public := legacyByteStagePGPublicImage(test, storage, ctx, fixture)
	timer.Reset(lease / 3)
	select {
	case <-timer.C:
	case <-ctx.Done():
		test.Fatal(ctx.Err())
	}
	if after := legacyByteStagePGPublicImage(test, storage, ctx, fixture); after != public {
		test.Fatal("joined fixture renewal changed an intentionally expired lease")
	}
	if err := storage.RenewDocRangeJob(ctx, job, time.Now().UTC(), lease); !errors.Is(err, domain.ErrConflict) {
		test.Fatal("stopped fixture lease did not preserve its real expiry fence", err)
	}
}
