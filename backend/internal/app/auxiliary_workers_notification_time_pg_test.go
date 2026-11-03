//go:build postgres

package app

import (
	"bytes"
	"encoding/json"
	"reflect"
	"testing"
	"time"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

func TestWave41NotificationJobsEqualPreservesFieldsAndInputs(test *testing.T) {
	instant := time.Date(2026, time.November, 3, 12, 0, 0, 123456789, time.UTC)
	job := domain.NotificationJob{
		ID: "evt_original", RepositoryID: "ws_original", Kind: "ref_updated", State: "running",
		Text: "https://recipient.example/events?token=original", Attempts: 2, Version: 3,
		Reason: "http_retryable", HTTPStatus: 503, CreatedAt: instant, UpdatedAt: instant.Add(time.Second),
		NextAttempt: instant.Add(time.Minute), LeaseUntil: instant.Add(2 * time.Minute),
	}
	check := func(test *testing.T, actual, expected domain.NotificationJob, equal bool) {
		test.Helper()
		beforeActual, beforeExpected := actual, expected
		if wave41NotificationJobsEqual(actual, expected) != equal || wave41NotificationJobsEqual(expected, actual) != equal {
			test.Fatalf("notification equality: want %t, actual=%#v expected=%#v", equal, actual, expected)
		}
		if actual != beforeActual || expected != beforeExpected {
			test.Fatal("notification comparison mutated an input")
		}
	}
	for _, fixture := range []struct {
		name string
		job  domain.NotificationJob
	}{{"populated", job}, {"zero", domain.NotificationJob{}}} {
		test.Run(fixture.name, func(test *testing.T) {
			for _, location := range []*time.Location{time.UTC, time.Local, time.FixedZone("UTC-alias", 0), time.FixedZone("Seoul-alias", 9*60*60)} {
				test.Run(location.String(), func(test *testing.T) {
					alias := fixture.job
					alias.CreatedAt = alias.CreatedAt.In(location)
					alias.UpdatedAt = alias.UpdatedAt.In(location)
					alias.NextAttempt = alias.NextAttempt.In(location)
					alias.LeaseUntil = alias.LeaseUntil.In(location)
					check(test, fixture.job, alias, true)
				})
			}
			for _, change := range []struct {
				name   string
				mutate func(*domain.NotificationJob)
			}{
				{"id", func(job *domain.NotificationJob) { job.ID += "changed" }},
				{"repository_id", func(job *domain.NotificationJob) { job.RepositoryID += "changed" }},
				{"kind", func(job *domain.NotificationJob) { job.Kind += "changed" }},
				{"state", func(job *domain.NotificationJob) { job.State += "changed" }},
				{"text_recipient_url", func(job *domain.NotificationJob) { job.Text += "&recipient=changed" }},
				{"attempts", func(job *domain.NotificationJob) { job.Attempts++ }},
				{"version", func(job *domain.NotificationJob) { job.Version++ }},
				{"reason", func(job *domain.NotificationJob) { job.Reason += "changed" }},
				{"http_status", func(job *domain.NotificationJob) { job.HTTPStatus++ }},
				{"created_at_nanosecond", func(job *domain.NotificationJob) { job.CreatedAt = job.CreatedAt.Add(time.Nanosecond) }},
				{"updated_at_nanosecond", func(job *domain.NotificationJob) { job.UpdatedAt = job.UpdatedAt.Add(time.Nanosecond) }},
				{"next_attempt_nanosecond", func(job *domain.NotificationJob) { job.NextAttempt = job.NextAttempt.Add(time.Nanosecond) }},
				{"lease_until_nanosecond", func(job *domain.NotificationJob) { job.LeaseUntil = job.LeaseUntil.Add(time.Nanosecond) }},
			} {
				test.Run(change.name, func(test *testing.T) {
					changed := fixture.job
					change.mutate(&changed)
					check(test, fixture.job, changed, false)
				})
			}
		})
	}
	test.Run("monotonic_reading", func(test *testing.T) {
		monotonic := job
		monotonic.UpdatedAt = time.Now()
		monotonic.LeaseUntil = monotonic.UpdatedAt.Add(2 * time.Minute)
		wall := monotonic
		wall.UpdatedAt = wall.UpdatedAt.UTC()
		wall.LeaseUntil = wall.LeaseUntil.UTC()
		check(test, monotonic, wall, true)
	})
}

func TestPGWave41NotificationJobsEqualMatchesPersistedClaim(test *testing.T) {
	postgres, _ := wave41AuxiliaryPG(test)
	record, _ := wave41AuxiliaryRepository(test, postgres, "https://recipient.example/events?token=original")
	wave41EnqueueNotification(test, postgres, record, 0)
	claimed, err := postgres.ClaimNotification(wave40PGContext(systemTestContext(), postgres), time.Now().UTC(), 2*time.Minute)
	if err != nil {
		test.Fatal(err)
	}
	persisted := wave41NotificationState(test, postgres, record)
	beforeClaimed, beforePersisted := claimed, persisted
	claimedJSON, err := json.Marshal(claimed.Job)
	if err != nil {
		test.Fatal(err)
	}
	persistedJSON, err := json.Marshal(persisted)
	if err != nil {
		test.Fatal(err)
	}
	allInstantsEqual := persisted.CreatedAt.Equal(claimed.Job.CreatedAt) && persisted.UpdatedAt.Equal(claimed.Job.UpdatedAt) && persisted.NextAttempt.Equal(claimed.Job.NextAttempt) && persisted.LeaseUntil.Equal(claimed.Job.LeaseUntil)
	test.Logf("raw_DeepEqual=%t JSON_equal=%t all_instants_equal=%t claimed_UpdatedAt=%#v persisted_UpdatedAt=%#v claimed_LeaseUntil=%#v persisted_LeaseUntil=%#v", reflect.DeepEqual(persisted, claimed.Job), bytes.Equal(claimedJSON, persistedJSON), allInstantsEqual, claimed.Job.UpdatedAt, persisted.UpdatedAt, claimed.Job.LeaseUntil, persisted.LeaseUntil)
	if !bytes.Equal(claimedJSON, persistedJSON) || !allInstantsEqual || !wave41NotificationJobsEqual(persisted, claimed.Job) || claimed.Destination != record.WebhookURL {
		test.Fatal("durable claim changed notification fields, timestamp instants or recipient URL", persisted, claimed)
	}
	if claimed != beforeClaimed || persisted != beforePersisted {
		test.Fatal("notification comparison mutated a claim or receipt")
	}
}
