package domain

import (
	"bytes"
	"encoding/json"
	"reflect"
	"testing"
	"time"
)

const (
	docJobLifetimeOriginalEpoch    = "3bea11e5-4357-4627-bc88-84849d142db5"
	docJobLifetimeReplacementEpoch = "954c040f-1112-48b0-930e-5d50dbd06cf7"
)

type docJobLifetimeFixture struct {
	name string
	job  DocFinalizationJob
}

func docJobLifetimeFixtures(test *testing.T) []docJobLifetimeFixture {
	test.Helper()
	roots, legacy := publicationVectors(test)
	var fixtures []docJobLifetimeFixture
	for _, variant := range []struct {
		name    string
		vectors []publicationVector
	}{{"native-root", roots}, {"legacy", legacy}} {
		for _, vector := range variant.vectors {
			job, err := NewDocFinalizationJobForRepresentation(vector.Repo, publicationDoc(test, vector), time.Date(2026, 10, 28, 0, 0, 0, 0, time.UTC))
			if err != nil || job.ID != vector.ID || job.RepositoryEpoch != "" {
				test.Fatal("construct unchanged golden job without native repository lifetime evidence", vector.Name, job, err)
			}
			fixtures = append(fixtures, docJobLifetimeFixture{name: variant.name + "/" + vector.Name, job: job})
		}
	}
	return fixtures
}

func TestDocJobLifetimeFencesRejectDifferentEpochs(test *testing.T) {
	for _, fixture := range docJobLifetimeFixtures(test) {
		test.Run(fixture.name, func(test *testing.T) {
			now := fixture.job.CreatedAt
			original := fixture.job
			original.RepositoryEpoch = docJobLifetimeOriginalEpoch
			oldClaim := original.Claim(now, time.Minute)
			replacement := fixture.job
			replacement.RepositoryEpoch = docJobLifetimeReplacementEpoch
			current := replacement.Claim(now, time.Minute)
			withoutNewEpoch := current
			withoutNewEpoch.RepositoryEpoch = oldClaim.RepositoryEpoch
			if !reflect.DeepEqual(withoutNewEpoch, oldClaim) || current.Version != 1 || oldClaim.Version != 1 {
				test.Fatal("ABA fixture must differ only in repository epoch, retaining identical job ID, version, creation time, and live lease")
			}
			if err := current.Validate(); err != nil {
				test.Fatal("replacement lifetime changed document validity", err)
			}
			if !oldClaim.Fences(oldClaim, now) || !current.Fences(current, now) {
				test.Fatal("matching native lifetimes must retain valid self fences")
			}
			if current.Fences(oldClaim, now) || oldClaim.Fences(current, now) {
				test.Fatal("identical job ID/version and lease admitted a claim from a different repository lifetime")
			}
			unbound := current
			unbound.RepositoryEpoch = ""
			if current.Fences(unbound, now) || unbound.Fences(current, now) {
				test.Fatal("one empty epoch must not substitute for native lifetime evidence in either fence direction")
			}
		})
	}
}

func TestDocJobLifetimeClaimAndRenewPreserveEpoch(test *testing.T) {
	for _, fixture := range docJobLifetimeFixtures(test) {
		test.Run(fixture.name, func(test *testing.T) {
			waiting := fixture.job
			waiting.RepositoryEpoch = docJobLifetimeOriginalEpoch
			before := waiting
			now := waiting.CreatedAt
			claim := waiting.Claim(now, time.Minute)
			if !reflect.DeepEqual(waiting, before) {
				test.Fatal("claim mutated its waiting value receiver")
			}
			if claim.RepositoryEpoch != waiting.RepositoryEpoch || claim.ID != waiting.ID || claim.DocumentRef() != waiting.DocumentRef() || claim.Version != waiting.Version+1 || claim.Attempts != waiting.Attempts+1 || claim.State != "running" || !claim.UpdatedAt.Equal(now) || !claim.LeaseUntil.Equal(now.Add(time.Minute)) {
				test.Fatal("claim lost its lifetime evidence or changed native claim semantics", claim)
			}
			if err := claim.Validate(); err != nil || !claim.Fences(claim, now) {
				test.Fatal("same-epoch claim is not valid and fenced", err)
			}
			renewed := claim
			renewed.UpdatedAt = now.Add(30 * time.Second)
			renewed.LeaseUntil = renewed.UpdatedAt.Add(time.Minute)
			if renewed.RepositoryEpoch != claim.RepositoryEpoch || renewed.Version != claim.Version || !renewed.Fences(claim, renewed.UpdatedAt) || !renewed.Fences(claim, claim.LeaseUntil) {
				test.Fatal("same-epoch authoritative lease renewal must retain the original claim, including after its caller-side lease expires")
			}
			if renewed.Fences(claim, renewed.LeaseUntil) {
				test.Fatal("repository epoch weakened the strict authoritative lease deadline")
			}
			reclaimed := renewed.Claim(renewed.LeaseUntil, time.Minute)
			if reclaimed.RepositoryEpoch != renewed.RepositoryEpoch || reclaimed.Version != renewed.Version+1 || reclaimed.Attempts != renewed.Attempts+1 || !reclaimed.Fences(reclaimed, reclaimed.UpdatedAt) || reclaimed.Fences(claim, reclaimed.UpdatedAt) {
				test.Fatal("reclaim must retain epoch evidence without admitting an older claim version", reclaimed)
			}
		})
	}
}

func TestDocJobLifetimeEmptyEpochPreservesCompatibility(test *testing.T) {
	for _, fixture := range docJobLifetimeFixtures(test) {
		test.Run(fixture.name, func(test *testing.T) {
			now := fixture.job.CreatedAt
			claim := fixture.job.Claim(now, time.Minute)
			if claim.RepositoryEpoch != "" || !claim.Fences(claim, now) || !claim.Fences(claim, claim.LeaseUntil.Add(-time.Nanosecond)) {
				test.Fatal("both-empty epochs must preserve native root and filesystem legacy fence compatibility")
			}
			if claim.Fences(claim, claim.LeaseUntil) || claim.Fences(claim, claim.LeaseUntil.Add(time.Nanosecond)) {
				test.Fatal("both-empty epochs weakened the original strict lease expiry")
			}
			for _, mutation := range []struct {
				name   string
				change func(*DocFinalizationJob)
			}{
				{"job-id", func(job *DocFinalizationJob) { job.ID = string(HashContent([]byte("other lifetime test job"))) }},
				{"repository-id", func(job *DocFinalizationJob) { job.RepoID = HashContent([]byte("other lifetime test repository")) }},
				{"version", func(job *DocFinalizationJob) { job.Version++ }},
				{"waiting", func(job *DocFinalizationJob) { job.State = "waiting" }},
				{"retrying", func(job *DocFinalizationJob) { job.State = "retrying" }},
				{"completed", func(job *DocFinalizationJob) { job.State = "completed" }},
				{"rejected", func(job *DocFinalizationJob) { job.State = "rejected" }},
				{"expired", func(job *DocFinalizationJob) { job.LeaseUntil = now }},
			} {
				test.Run(mutation.name, func(test *testing.T) {
					current := claim
					mutation.change(&current)
					if current.Fences(claim, now) {
						test.Fatal("both-empty epoch compatibility bypassed an existing fence invariant")
					}
				})
			}
			renewed := claim
			renewed.UpdatedAt = now.Add(30 * time.Second)
			renewed.LeaseUntil = renewed.UpdatedAt.Add(time.Minute)
			if renewed.RepositoryEpoch != "" || !renewed.Fences(claim, claim.LeaseUntil) {
				test.Fatal("filesystem-compatible renewal must not require native lifetime evidence or caller lease equality")
			}
		})
	}
}

func TestDocJobLifetimeJSONCannotCarryOrForgeEpoch(test *testing.T) {
	for _, fixture := range docJobLifetimeFixtures(test) {
		test.Run(fixture.name, func(test *testing.T) {
			now := fixture.job.CreatedAt
			bound := fixture.job.Claim(now, time.Minute)
			bound.RepositoryEpoch = docJobLifetimeOriginalEpoch
			raw, err := json.Marshal(bound)
			if err != nil {
				test.Fatal(err)
			}
			if bytes.Contains(raw, []byte("RepositoryEpoch")) || bytes.Contains(raw, []byte("repository_epoch")) || bytes.Contains(raw, []byte(bound.RepositoryEpoch)) {
				test.Fatal("JSON leaked native repository lifetime evidence", string(raw))
			}
			payloads := []struct {
				name string
				raw  []byte
			}{{"round-trip", raw}}
			for _, field := range []string{"repository_epoch", "RepositoryEpoch", "repositoryEpoch", "REPOSITORY_EPOCH"} {
				injected := append(bytes.Clone(raw[:len(raw)-1]), []byte(`,"`+field+`":"`+bound.RepositoryEpoch+`"}`)...)
				payloads = append(payloads, struct {
					name string
					raw  []byte
				}{"injected-" + field, injected})
			}
			for _, payload := range payloads {
				test.Run(payload.name, func(test *testing.T) {
					for _, receiverEpoch := range []string{"", docJobLifetimeReplacementEpoch} {
						decoded := bound
						decoded.RepositoryEpoch = receiverEpoch
						if err := json.Unmarshal(payload.raw, &decoded); err != nil {
							test.Fatal("decode compatible job JSON with ignored epoch extension", err)
						}
						expected := bound
						expected.RepositoryEpoch = ""
						if decoded.RepositoryEpoch != "" || !reflect.DeepEqual(decoded, expected) {
							test.Fatal("JSON carried, forged, or retained repository lifetime evidence", receiverEpoch, decoded)
						}
						if err := decoded.Validate(); err != nil {
							test.Fatal("dropping native lifetime evidence changed document validity", err)
						}
						if bound.Fences(decoded, now) || decoded.Fences(bound, now) {
							test.Fatal("unhydrated wire job satisfied a native lifetime fence")
						}
						decoded.RepositoryEpoch = bound.RepositoryEpoch
						if !bound.Fences(decoded, now) || !decoded.Fences(bound, now) {
							test.Fatal("restoring native lifetime evidence failed to recover the unchanged claim")
						}
						encoded, err := json.Marshal(decoded)
						if err != nil || !bytes.Equal(encoded, raw) {
							test.Fatal("round-trip or ignored epoch injection changed job wire bytes", err)
						}
					}
				})
			}
		})
	}
}

func TestDocJobLifetimeEpochDoesNotChangeIdentityOrWire(test *testing.T) {
	for _, fixture := range docJobLifetimeFixtures(test) {
		test.Run(fixture.name, func(test *testing.T) {
			baseline := fixture.job
			baselineWire, err := json.Marshal(baseline)
			if err != nil {
				test.Fatal(err)
			}
			baselineCanonical, err := canonicalJSON(baseline)
			if err != nil {
				test.Fatal(err)
			}
			baselineRepresentation, err := baseline.Representation()
			if err != nil {
				test.Fatal(err)
			}
			baselineDocumentWire, err := canonicalJSON(baselineRepresentation)
			if err != nil {
				test.Fatal(err)
			}
			for _, epoch := range []string{"", docJobLifetimeOriginalEpoch, docJobLifetimeReplacementEpoch} {
				job := baseline
				job.RepositoryEpoch = epoch
				if err := job.Validate(); err != nil {
					test.Fatal("native epoch changed otherwise valid job metadata", epoch, err)
				}
				if job.ID != baseline.ID || job.identity() != baseline.ID || job.DocumentRef() != baseline.DocumentRef() {
					test.Fatal("repository lifetime evidence changed golden job identity or document reference", epoch)
				}
				representation, err := job.Representation()
				if err != nil || !reflect.DeepEqual(representation, baselineRepresentation) {
					test.Fatal("repository lifetime evidence changed document representation", epoch, err)
				}
				if job.DocIdentity == DocumentIdentityRootV1 {
					identity, err := RootDocFinalizationID(job.RepoID, representation)
					if err != nil || identity != baseline.ID {
						test.Fatal("repository lifetime evidence changed canonical root finalization identity", epoch, identity, err)
					}
				}
				wire, err := json.Marshal(job)
				if err != nil || !bytes.Equal(wire, baselineWire) {
					test.Fatal("repository lifetime evidence changed exact job wire bytes", epoch, err)
				}
				canonical, err := canonicalJSON(job)
				if err != nil || !bytes.Equal(canonical, baselineCanonical) {
					test.Fatal("repository lifetime evidence changed canonical job wire bytes", epoch, err)
				}
				documentWire, err := canonicalJSON(representation)
				if err != nil || !bytes.Equal(documentWire, baselineDocumentWire) {
					test.Fatal("repository lifetime evidence changed canonical document wire bytes", epoch, err)
				}
			}
		})
	}
}
