package domain

import (
	"bytes"
	"encoding/json"
	"errors"
	"reflect"
	"testing"
	"time"
)

const (
	docRangeJobLifetimeOriginalEpoch    = "3bea11e5-4357-4627-bc88-84849d142db5"
	docRangeJobLifetimeReplacementEpoch = "954c040f-1112-48b0-930e-5d50dbd06cf7"
)

type docRangeJobLifetimeFixture struct {
	name string
	job  DocRangeIndexJob
}

func docRangeJobLifetimeFixtures() []docRangeJobLifetimeFixture {
	now := time.Date(2026, 10, 29, 0, 0, 0, 0, time.UTC)
	job := DocRangeIndexJob{
		RepoID:      HashContent([]byte("range lifetime native repository")),
		DocHash:     HashContent([]byte("range lifetime native document")),
		State:       "running",
		Reason:      "temporary_failure",
		Attempts:    2,
		Version:     7,
		CreatedAt:   now,
		NextAttempt: now.Add(time.Second),
		LeaseUntil:  now.Add(time.Minute),
	}
	boundary := job
	boundary.BaseHash = HashContent([]byte("range lifetime native base"))
	return []docRangeJobLifetimeFixture{
		{name: "range", job: job},
		{name: "boundary", job: boundary},
	}
}

func TestDocRangeJobLifetimeJSONOmitsAndIgnoresEpoch(test *testing.T) {
	for _, fixture := range docRangeJobLifetimeFixtures() {
		test.Run(fixture.name, func(test *testing.T) {
			bound := fixture.job
			bound.RepositoryEpoch = docRangeJobLifetimeOriginalEpoch
			raw, err := json.Marshal(bound)
			if err != nil {
				test.Fatal(err)
			}
			for _, value := range []string{"RepositoryEpoch", "repository_epoch", "repositoryEpoch", "REPOSITORY_EPOCH", bound.RepositoryEpoch} {
				if bytes.Contains(raw, []byte(value)) {
					test.Fatal("JSON exposed repository epoch metadata", value, string(raw))
				}
			}
			payloads := []struct {
				name string
				raw  []byte
			}{{name: "round-trip", raw: raw}}
			for _, field := range []string{"RepositoryEpoch", "repository_epoch", "repositoryEpoch", "REPOSITORY_EPOCH"} {
				injected := append(bytes.Clone(raw[:len(raw)-1]), []byte(`,"`+field+`":"`+bound.RepositoryEpoch+`"}`)...)
				payloads = append(payloads, struct {
					name string
					raw  []byte
				}{name: "ignored-" + field, raw: injected})
			}
			for _, payload := range payloads {
				test.Run(payload.name, func(test *testing.T) {
					var decoded DocRangeIndexJob
					if err := json.Unmarshal(payload.raw, &decoded); err != nil {
						test.Fatal("decode unchanged range job JSON", err)
					}
					if decoded.RepositoryEpoch != "" || !reflect.DeepEqual(decoded, fixture.job) {
						test.Fatal("JSON populated repository epoch metadata or changed existing job fields", decoded)
					}
					if err := decoded.Validate(); err != nil {
						test.Fatal("unbound JSON job lost native metadata validity", err)
					}
					encoded, err := json.Marshal(decoded)
					if err != nil || !bytes.Equal(encoded, raw) {
						test.Fatal("round-trip or ignored epoch extension changed existing wire bytes", err)
					}
				})
			}
		})
	}
}

func TestDocRangeJobLifetimeEpochPreservesLegacyWireAndIdentity(test *testing.T) {
	for _, fixture := range docRangeJobLifetimeFixtures() {
		test.Run(fixture.name, func(test *testing.T) {
			baseline := fixture.job
			legacy := struct {
				RepoID      ContentHash
				DocHash     ContentHash
				BaseHash    ContentHash
				State       string
				Reason      string
				Attempts    int
				Version     int64
				CreatedAt   time.Time
				NextAttempt time.Time
				LeaseUntil  time.Time
			}{
				RepoID:      baseline.RepoID,
				DocHash:     baseline.DocHash,
				BaseHash:    baseline.BaseHash,
				State:       baseline.State,
				Reason:      baseline.Reason,
				Attempts:    baseline.Attempts,
				Version:     baseline.Version,
				CreatedAt:   baseline.CreatedAt,
				NextAttempt: baseline.NextAttempt,
				LeaseUntil:  baseline.LeaseUntil,
			}
			legacyWire, err := json.Marshal(legacy)
			if err != nil {
				test.Fatal(err)
			}
			identity := [3]ContentHash{baseline.RepoID, baseline.DocHash, baseline.BaseHash}
			for _, epoch := range []string{"", docRangeJobLifetimeOriginalEpoch, docRangeJobLifetimeReplacementEpoch} {
				job := baseline
				job.RepositoryEpoch = epoch
				if [3]ContentHash{job.RepoID, job.DocHash, job.BaseHash} != identity {
					test.Fatal("repository epoch metadata changed the existing range job key", epoch)
				}
				wire, err := json.Marshal(job)
				if err != nil || !bytes.Equal(wire, legacyWire) {
					test.Fatal("repository epoch metadata changed pre-epoch JSON shape or exact wire bytes", epoch, err)
				}
				job.RepositoryEpoch = ""
				if !reflect.DeepEqual(job, baseline) {
					test.Fatal("repository epoch metadata changed existing job identity or delivery fields", epoch)
				}
			}
		})
	}
}

func TestDocRangeJobLifetimeEpochPreservesNativeFilesystemValidation(test *testing.T) {
	scenarios := []struct {
		name   string
		change func(*DocRangeIndexJob)
		want   error
	}{
		{name: "unchanged"},
		{name: "waiting", change: func(job *DocRangeIndexJob) { job.State = "waiting" }},
		{name: "retrying", change: func(job *DocRangeIndexJob) { job.State = "retrying" }},
		{name: "completed", change: func(job *DocRangeIndexJob) { job.State = "completed" }},
		{name: "rejected", change: func(job *DocRangeIndexJob) { job.State = "rejected" }},
		{name: "empty-lease", change: func(job *DocRangeIndexJob) { job.LeaseUntil = time.Time{} }},
		{name: "expired-lease", change: func(job *DocRangeIndexJob) { job.LeaseUntil = job.CreatedAt.Add(-time.Second) }},
		{name: "zero-value", change: func(job *DocRangeIndexJob) { *job = DocRangeIndexJob{} }, want: ErrIntegrity},
		{name: "invalid-repository", change: func(job *DocRangeIndexJob) { job.RepoID = "invalid" }, want: ErrIntegrity},
		{name: "invalid-document", change: func(job *DocRangeIndexJob) { job.DocHash = "invalid" }, want: ErrIntegrity},
		{name: "invalid-base", change: func(job *DocRangeIndexJob) { job.BaseHash = "invalid" }, want: ErrIntegrity},
		{name: "negative-attempts", change: func(job *DocRangeIndexJob) { job.Attempts = -1 }, want: ErrValidation},
		{name: "negative-version", change: func(job *DocRangeIndexJob) { job.Version = -1 }, want: ErrValidation},
		{name: "zero-created-at", change: func(job *DocRangeIndexJob) { job.CreatedAt = time.Time{} }, want: ErrValidation},
		{name: "zero-next-attempt", change: func(job *DocRangeIndexJob) { job.NextAttempt = time.Time{} }, want: ErrValidation},
		{name: "empty-state", change: func(job *DocRangeIndexJob) { job.State = "" }, want: ErrValidation},
		{name: "unknown-state", change: func(job *DocRangeIndexJob) { job.State = "future" }, want: ErrValidation},
	}
	for _, fixture := range docRangeJobLifetimeFixtures() {
		test.Run(fixture.name, func(test *testing.T) {
			for _, scenario := range scenarios {
				test.Run(scenario.name, func(test *testing.T) {
					baseline := fixture.job
					if scenario.change != nil {
						scenario.change(&baseline)
					}
					if baseline.RepositoryEpoch != "" {
						test.Fatal("native compatibility fixture unexpectedly contains repository epoch metadata")
					}
					if err := baseline.Validate(); !errors.Is(err, scenario.want) {
						test.Fatal("empty epoch changed existing native or filesystem-compatible metadata validation", err, scenario.want)
					}
					for _, epoch := range []string{"", docRangeJobLifetimeOriginalEpoch, docRangeJobLifetimeReplacementEpoch, "opaque-native-metadata"} {
						job := baseline
						job.RepositoryEpoch = epoch
						if err := job.Validate(); !errors.Is(err, scenario.want) {
							test.Fatal("repository epoch metadata changed existing validation rules", epoch, err, scenario.want)
						}
					}
				})
			}
		})
	}
}
