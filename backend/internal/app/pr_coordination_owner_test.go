package app

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/inbound"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

type prOwnerCallerKey struct{}
type prOwnerBoundKey struct{}

type prOwnerProbe struct {
	test                   *testing.T
	ctx                    context.Context
	job, claimed, accepted domain.PRPromotionJob
	output                 inbound.UpdateRefOutput
	listed                 []domain.PRPromotionJob
	evidence               outbound.PRJobAssociationEvidence
	numbers                []int
	calls                  []string
	failures               map[string]error
	active                 string
	delivery               bool
	since                  time.Time
}

func (probe *prOwnerProbe) touch(ctx context.Context, repo domain.ContentHash, operation, scope string) error {
	probe.test.Helper()
	actor, system := inbound.RepositoryActor(ctx)
	deadline, bounded := ctx.Deadline()
	wanted, wantBounded := probe.ctx.Deadline()
	if repo != probe.job.RepoID || ctx.Value(prOwnerCallerKey{}) != probe || actor != "ordinary-author" || system ||
		inbound.Correlation(ctx) != "pr-owner" || bounded != wantBounded || !deadline.Equal(wanted) || ctx.Done() != probe.ctx.Done() ||
		!reflect.DeepEqual(inbound.DocumentIdentities(ctx), inbound.DocumentIdentities(probe.ctx)) {
		probe.test.Fatal("coordination changed repository, caller authority, compatibility declaration, or cancellation")
	}
	if probe.active != scope || (scope != "" && ctx.Value(prOwnerBoundKey{}) != scope) || (scope == "" && ctx.Value(prOwnerBoundKey{}) != nil) {
		probe.test.Fatalf("%s escaped its bound scope: active=%q want=%q", operation, probe.active, scope)
	}
	if probe.delivery && outbound.CheckDocumentIdentityCompatibility(ctx, domain.DocumentIdentityRootV1) != nil {
		probe.test.Fatal("delivery lost supported document compatibility")
	}
	probe.calls = append(probe.calls, operation)
	if err := ctx.Err(); err != nil {
		return err
	}
	return probe.failures[operation]
}

func (probe *prOwnerProbe) within(ctx context.Context, repo domain.ContentHash, scope string, operation func(context.Context) error) error {
	if err := probe.touch(ctx, repo, scope, ""); err != nil {
		return err
	}
	probe.active = scope
	defer func() { probe.active = "" }()
	if err := operation(context.WithValue(ctx, prOwnerBoundKey{}, scope)); err != nil {
		return err
	}
	return probe.failures[scope+"-commit"]
}

func (probe *prOwnerProbe) WithinPRSubmit(ctx context.Context, repo domain.ContentHash, operation func(context.Context) (domain.PRPromotionJob, error)) (domain.PRPromotionJob, error) {
	var job domain.PRPromotionJob
	err := probe.within(ctx, repo, "submit", func(bound context.Context) error {
		var err error
		job, err = operation(bound)
		return err
	})
	if err == nil {
		probe.accepted = job
		probe.calls = append(probe.calls, "accepted")
	}
	return job, err
}

func (probe *prOwnerProbe) WithinPRRetry(ctx context.Context, repo domain.ContentHash, operation func(context.Context) error) error {
	return probe.within(ctx, repo, "retry", operation)
}

func (probe *prOwnerProbe) WithinPRRead(ctx context.Context, repo domain.ContentHash, operation func(context.Context) ([]domain.PRPromotionJob, error)) ([]domain.PRPromotionJob, error) {
	var jobs []domain.PRPromotionJob
	err := probe.within(ctx, repo, "read", func(bound context.Context) error {
		var err error
		jobs, err = operation(bound)
		return err
	})
	return jobs, err
}

func (probe *prOwnerProbe) SubmitPRPromotion(ctx context.Context, repo domain.ContentHash, request domain.PullRequestMerge) (domain.PRPromotionJob, error) {
	if request != probe.job.PR {
		probe.test.Fatal("submission changed the exact request")
	}
	return probe.job, probe.touch(ctx, repo, "submission", "submit")
}

func (probe *prOwnerProbe) GetPRJob(ctx context.Context, repo domain.ContentHash, id string) (domain.PRPromotionJob, error) {
	probe.test.Fatal("coordination unexpectedly reloaded acceptance", id)
	return domain.PRPromotionJob{}, domain.ErrIntegrity
}

func (probe *prOwnerProbe) ListPRJobs(ctx context.Context, repo domain.ContentHash) ([]domain.PRPromotionJob, error) {
	return probe.listed, probe.touch(ctx, repo, "list", "read")
}

func (probe *prOwnerProbe) ClaimPRJob(ctx context.Context, repo domain.ContentHash, id string, now time.Time, lease time.Duration) (domain.PRPromotionJob, error) {
	probe.execution(ctx)
	if id != probe.job.ID || lease != prJobLease || now.Before(probe.since) || now.After(time.Now()) || now.Location() != time.UTC {
		probe.test.Fatal("claim changed the accepted job, UTC timestamp, or lease")
	}
	return probe.claimed, probe.touch(ctx, repo, "claim", "")
}

func (probe *prOwnerProbe) RetryPRJob(ctx context.Context, repo domain.ContentHash, id string, now time.Time) error {
	if id != probe.job.ID || now.Before(probe.since) || now.After(time.Now()) || now.Location() != time.UTC {
		probe.test.Fatal("retry changed exact identity or UTC time")
	}
	return probe.touch(ctx, repo, "retry-job", "retry")
}

func (probe *prOwnerProbe) execution(ctx context.Context) {
	probe.test.Helper()
	if probe.delivery {
		permit, _ := ctx.Value(resourcePermitKey{}).(*resourcePermit)
		if !reflect.DeepEqual(probe.accepted, probe.job) || permit == nil || !permit.live.Load() || permit.class != ResourceBulk {
			probe.test.Fatal("execution preceded exact committed acceptance or Bulk admission")
		}
	}
}

func (probe *prOwnerProbe) PromoteBoundPR(ctx context.Context, repo domain.ContentHash, request domain.PullRequestMerge, base string) (inbound.UpdateRefOutput, error) {
	probe.execution(ctx)
	wanted := ""
	if probe.delivery {
		wanted = probe.job.BaseBranchID
	}
	if request != probe.job.PR || base != wanted {
		probe.test.Fatal("promotion changed request or accepted base identity")
	}
	return probe.output, probe.touch(ctx, repo, "promote", "")
}

func (probe *prOwnerProbe) RunPRJob(ctx context.Context, job domain.PRPromotionJob) (inbound.UpdateRefOutput, error) {
	probe.execution(ctx)
	if !reflect.DeepEqual(job, probe.claimed) {
		probe.test.Fatal("executor did not receive the exact leased job and diagnostics")
	}
	return probe.output, probe.touch(ctx, job.RepoID, "run", "")
}

func (probe *prOwnerProbe) ReadPRJobAssociationEvidence(ctx context.Context, repo domain.ContentHash, numbers []int) (outbound.PRJobAssociationEvidence, error) {
	probe.numbers = append([]int(nil), numbers...)
	return probe.evidence, probe.touch(ctx, repo, "associations", "read")
}

var (
	_ *prCoordinationCommands        = (*PRPromotionCommands)(nil)
	_ inbound.PRPromotionCommands    = (*PRPromotionCommands)(nil)
	_ PRPromotionScope               = (*prOwnerProbe)(nil)
	_ PRPromotionJobControl          = (*prOwnerProbe)(nil)
	_ PRPromotionSubmission          = (*prOwnerProbe)(nil)
	_ PRPromotionExecutor            = (*prOwnerProbe)(nil)
	_ outbound.PRJobAssociationStore = (*prOwnerProbe)(nil)
	_ PRPromotionSubmission          = (*prPublicationCommands)(nil)
	_ PRPromotionExecutor            = (*PRPromotionWorker)(nil)
)

func newPROwnerFixture(test *testing.T) (*prOwnerProbe, PRPromotionCommandsConfig) {
	test.Helper()
	ctx, _ := admissionTestContext(time.Minute)
	ctx, cancel := context.WithTimeout(ctx, time.Minute)
	test.Cleanup(cancel)
	probe := &prOwnerProbe{test: test, failures: map[string]error{}, since: time.Now().UTC()}
	probe.ctx = inbound.WithDocumentIdentities(inbound.WithCorrelation(inbound.WithRepositoryActor(context.WithValue(ctx, prOwnerCallerKey{}, probe), "ordinary-author"), "pr-owner"), []domain.DocumentIdentity{domain.DocumentIdentityRootV1})
	probe.job = domain.PRPromotionJob{RepoID: hh(test.Name()), PR: domain.PullRequestMerge{Number: 7, BaseBranch: "main", HeadBranch: "feature/pr", HeadSHA: strings.Repeat("a", 40), MergeSHA: strings.Repeat("b", 40)}, BaseBranchID: "accepted-base-not-current-name", GitOrigin: "github.com/acme/repo", State: "waiting", CreatedAt: probe.since}
	probe.job.ID = domain.PRPromotionID(probe.job.RepoID, probe.job.PR.Number)
	probe.job = domain.AppendPRJobDiagnostic(domain.PRPromotionJob{}, probe.job, "queued", probe.since)
	probe.claimed = probe.job
	probe.claimed.State, probe.claimed.Version, probe.claimed.Attempts = "running", 9, 3
	probe.claimed = domain.AppendPRJobDiagnostic(probe.job, probe.claimed, "claimed", probe.since)
	probe.output = inbound.UpdateRefOutput{Ref: domain.Ref{RepoID: probe.job.RepoID, BranchID: probe.job.BaseBranchID, Target: hh("promoted")}}
	return probe, PRPromotionCommandsConfig{Profile: PRCoordinationComplete, Scope: probe, Jobs: probe, Submission: probe, Executor: probe, Associations: probe, SupportedIdentities: []domain.DocumentIdentity{domain.DocumentIdentityLegacy, domain.DocumentIdentityRootV1}}
}

func prOwnerCommands(test *testing.T, config PRPromotionCommandsConfig) *PRPromotionCommands {
	test.Helper()
	owner, err := NewPRPromotionCommands(config)
	if err != nil || owner == nil {
		test.Fatalf("construct actual coordinator: owner=%v err=%v", owner, err)
	}
	return owner
}

func TestPRCoordinationOwnerConstructor(test *testing.T) {
	probe, base := newPROwnerFixture(test)
	for _, profile := range []PRCoordinationProfile{PRCoordinationCompatibility, PRCoordinationComplete, "", "invalid"} {
		config := base
		config.Profile = profile
		if profile == "" || profile == "invalid" {
			config.Jobs = nil
			if owner, err := NewPRPromotionCommands(config); owner != nil || !errors.Is(err, domain.ErrValidation) {
				test.Fatalf("profile precedence: owner=%v err=%v", owner, err)
			}
			continue
		}
		owner := prOwnerCommands(test, config)
		config.SupportedIdentities[0] = "mutated"
		if owner.supported[0] != domain.DocumentIdentityLegacy || owner.profile != profile {
			test.Fatal("constructor retained identity input or changed profile")
		}
		config.SupportedIdentities[0] = domain.DocumentIdentityLegacy
		for _, field := range []string{"Scope", "Jobs", "Submission", "Executor", "Associations"} {
			for _, typed := range []bool{false, true} {
				candidate := config
				port := reflect.ValueOf(&candidate).Elem().FieldByName(field)
				port.Set(reflect.Zero(port.Type()))
				var want error
				if field == "Scope" || typed {
					want = domain.ErrValidation
				} else if profile == PRCoordinationComplete && field != "Associations" {
					want = domain.ErrCatalogUnsupported
				}
				if typed {
					candidate.Jobs, candidate.Submission, candidate.Executor = nil, nil, nil
					port.Set(reflect.ValueOf((*prOwnerProbe)(nil)))
				}
				actual, err := NewPRPromotionCommands(candidate)
				if !errors.Is(err, want) || (actual == nil) != (want != nil) {
					test.Fatalf("%s/%s typed=%v: owner=%v err=%v want=%v", profile, field, typed, actual, err, want)
				}
			}
		}
	}
	work := &repositoryWork{}
	scope, submission, executor := &prCoordinationScope{work: work}, &prPublicationCommands{work: work}, &PRPromotionWorker{work: work}
	base.Scope, base.Submission, base.Executor = scope, submission, executor
	owner := prOwnerCommands(test, base)
	if owner.scope != scope || owner.submission != submission || owner.executor != executor || scope.work != submission.work || scope.work != executor.work || len(probe.calls) != 0 {
		test.Fatal("constructor replaced original shared work/ports or performed IO")
	}
}

func TestPRCoordinationOwnerSubmitAndRetry(test *testing.T) {
	for _, profile := range []PRCoordinationProfile{PRCoordinationCompatibility, PRCoordinationComplete} {
		for _, failure := range []string{"", "submit", "submission", "submit-commit", "retry-success", "retry", "retry-job", "retry-commit"} {
			test.Run(string(profile)+"/"+failure, func(test *testing.T) {
				probe, config := newPROwnerFixture(test)
				config.Profile = profile
				injected := errors.New("native port failure")
				probe.failures[failure] = injected
				owner := prOwnerCommands(test, config)
				if strings.HasPrefix(failure, "retry") {
					var wantErr error = injected
					if failure == "retry-success" {
						wantErr = nil
					}
					err := owner.RetryPRPromotion(probe.ctx, probe.job.RepoID, probe.job.ID)
					if !errors.Is(err, wantErr) || strings.Join(probe.calls, ",") != map[bool]string{true: "retry", false: "retry,retry-job"}[failure == "retry"] {
						test.Fatal("retry scope/error changed", probe.calls, err)
					}
					return
				}
				diagnostics := *probe.job.Diagnostics
				diagnostics.Events = append([]domain.PRJobDiagnostic(nil), diagnostics.Events...)
				job, err := owner.SubmitPRPromotion(probe.ctx, probe.job.RepoID, probe.job.PR)
				wanted, calls := probe.job, "submit,submission"
				var wantErr error
				if failure == "" {
					calls += ",accepted"
				} else {
					wantErr = injected
					if profile == PRCoordinationComplete || failure == "submit" {
						wanted = domain.PRPromotionJob{}
					}
					if failure == "submit" {
						calls = "submit"
					}
				}
				if !errors.Is(err, wantErr) || !reflect.DeepEqual(job, wanted) || !reflect.DeepEqual(probe.job.Diagnostics, &diagnostics) || strings.Join(probe.calls, ",") != calls {
					test.Fatal("submission lost scope, exact diagnostics, or error profile", job, err, probe.calls)
				}
			})
		}
	}
}

func TestPRCoordinationOwnerDeliveryAndDirectPromotion(test *testing.T) {
	for _, profile := range []PRCoordinationProfile{PRCoordinationCompatibility, PRCoordinationComplete} {
		for _, mode := range []string{"run", "completed", "queued", "claim-error", "run-error", "replay-error", "submit-error", "direct", "direct-error"} {
			test.Run(string(profile)+"/"+mode, func(test *testing.T) {
				probe, config := newPROwnerFixture(test)
				config.Profile, probe.delivery = profile, !strings.HasPrefix(mode, "direct")
				wantErr, calls := error(nil), "submit,submission,accepted,claim,run"
				if mode == "completed" || mode == "replay-error" {
					probe.job.State, calls = "completed", "submit,submission,accepted,promote"
				}
				if strings.HasPrefix(mode, "direct") {
					calls = "promote"
				}
				switch mode {
				case "queued":
					probe.failures["claim"], wantErr, calls = domain.ErrNotFound, domain.ErrConflict, "submit,submission,accepted,claim"
				case "claim-error":
					probe.failures["claim"], wantErr, calls = domain.ErrForbidden, domain.ErrForbidden, "submit,submission,accepted,claim"
				case "run-error":
					probe.failures["run"], wantErr = domain.ErrIntegrity, domain.ErrIntegrity
				case "replay-error", "direct-error":
					probe.failures["promote"], wantErr = domain.ErrConflict, domain.ErrConflict
				case "submit-error":
					probe.failures["submission"], wantErr, calls = domain.ErrForbidden, domain.ErrForbidden, "submit,submission"
				}
				owner := prOwnerCommands(test, config)
				operation := owner.DeliverPRPromotion
				if !probe.delivery {
					operation = owner.PromoteRepositoryPR
				}
				diagnostics := *probe.job.Diagnostics
				diagnostics.Events = append([]domain.PRJobDiagnostic(nil), diagnostics.Events...)
				out, err := operation(probe.ctx, probe.job.RepoID, probe.job.PR)
				wanted := probe.output
				if wantErr != nil && (profile == PRCoordinationComplete || mode == "queued" || mode == "claim-error" || mode == "submit-error") {
					wanted = inbound.UpdateRefOutput{}
				}
				if !errors.Is(err, wantErr) || !reflect.DeepEqual(out, wanted) || !reflect.DeepEqual(probe.job.Diagnostics, &diagnostics) || strings.Join(probe.calls, ",") != calls {
					test.Fatal("delivery changed acceptance/replay/lease/executor ordering or error output", out, err, probe.calls)
				}
				if mode == "queued" && !strings.Contains(err.Error(), "durably queued") {
					test.Fatal("queued conflict lost its diagnostic", err)
				}
			})
		}
	}
}

func TestPRCoordinationOwnerCancellationAfterAcceptanceBeforeBulk(test *testing.T) {
	probe, config := newPROwnerFixture(test)
	probe.delivery = true
	gate := probe.ctx.Value(resourceAdmissionKey{}).(*resourceAdmission)
	_, release, err := AcquireResources(probe.ctx, ResourceBulk)
	if err != nil {
		test.Fatal(err)
	}
	defer release()
	ctx, cancel := context.WithCancel(probe.ctx)
	probe.ctx = ctx
	defer cancel()
	owner := prOwnerCommands(test, config)
	done := make(chan error, 1)
	go func() {
		out, err := owner.DeliverPRPromotion(probe.ctx, probe.job.RepoID, probe.job.PR)
		if !reflect.DeepEqual(out, inbound.UpdateRefOutput{}) {
			err = errors.New("canceled delivery returned apparent success")
		}
		done <- err
	}()
	waitResourceQueue(test, gate, ResourceBulk, 1)
	if !reflect.DeepEqual(probe.accepted, probe.job) || probe.active != "" || strings.Join(probe.calls, ",") != "submit,submission,accepted" {
		test.Fatal("Bulk wait preceded exact committed acceptance or retained submit scope", probe.calls)
	}
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) || !reflect.DeepEqual(probe.accepted, probe.job) || len(probe.calls) != 3 {
			test.Fatal("cancellation lost acceptance or claimed work", err, probe.calls)
		}
	case <-time.After(5 * time.Second):
		test.Fatal("canceled delivery did not leave Bulk wait")
	}
	release()
	assertResourcesReleased(test, gate)
}

func TestPRCoordinationOwnerListActualAssociationProjection(test *testing.T) {
	for _, mode := range []string{"absent", "unavailable", "unknown", "recorded", "partial", "list-error", "scope-error"} {
		test.Run(mode, func(test *testing.T) {
			probe, config := newPROwnerFixture(test)
			probe.job.Association = &domain.PRJobAssociation{ProviderHistory: "stored-sentinel"}
			other := probe.job
			other.PR.Number, other.GitOrigin = 8, ""
			probe.listed = []domain.PRPromotionJob{probe.job, other}
			before := append([]domain.PRPromotionJob(nil), probe.listed...)
			checkpoint := domain.PRHistoryCheckpoint{RepoID: probe.job.RepoID, GitOrigin: "https://github.com/acme/repo", PR: probe.job.PR, Complete: mode != "partial"}
			proof := domain.PRParticipation{ID: "proof", RepoID: probe.job.RepoID, GitOrigin: checkpoint.GitOrigin, Number: 7, MergeSHA: probe.job.PR.MergeSHA, PublicationID: "publication"}
			record := domain.PRContribution{ID: "record", RepoID: probe.job.RepoID, GitOrigin: checkpoint.GitOrigin, PR: probe.job.PR, DestinationBranchID: probe.job.BaseBranchID}
			wrongBase, wrongRepo := record, record
			wrongBase.ID, wrongBase.DestinationBranchID, wrongRepo.ID, wrongRepo.RepoID = "wrong-base", "reused-base", "wrong-repo", hh("other-repo")
			complete := mode == "recorded" || mode == "partial"
			probe.evidence = outbound.PRJobAssociationEvidence{Checkpoints: []domain.PRHistoryCheckpoint{checkpoint}, CheckpointsComplete: complete, Participations: []domain.PRParticipation{proof, proof}, ParticipationsComplete: complete, Contributions: []domain.PRContribution{record, record, wrongBase, wrongRepo}, ContributionsComplete: complete}
			if mode == "absent" {
				config.Associations = nil
			}
			if mode == "unavailable" {
				probe.failures["associations"] = errors.New("private-provider-detail")
			}
			if mode == "list-error" {
				probe.failures["list"] = domain.ErrForbidden
			}
			if mode == "scope-error" {
				probe.failures["read-commit"] = domain.ErrForbidden
			}
			jobs, err := prOwnerCommands(test, config).ListPRPromotions(probe.ctx, probe.job.RepoID)
			if strings.HasSuffix(mode, "-error") {
				if !errors.Is(err, domain.ErrForbidden) || jobs != nil {
					test.Fatal("read error retained apparent results", jobs, err)
				}
				return
			}
			if err != nil || len(jobs) != 2 || !reflect.DeepEqual(before, probe.listed) {
				test.Fatal("listing mutated source jobs or lost results", jobs, err)
			}
			status := jobs[0].Association
			wanted := &domain.PRJobAssociation{ProviderHistory: "unknown", Participation: "unknown", Contribution: "unknown", CurrentApplicability: "not_evaluated"}
			if mode == "absent" || mode == "unavailable" {
				wanted.ProviderHistory, wanted.Participation, wanted.Contribution = "unavailable", "unavailable", "unavailable"
			}
			otherWanted := *wanted
			if complete {
				count := 1
				wanted.ProviderHistory, wanted.Participation, wanted.Contribution = "recorded_complete", "recorded", "recorded"
				if mode == "partial" {
					wanted.ProviderHistory = "recorded_partial"
				}
				wanted.ProviderCheckpointCount, wanted.ParticipationCount, wanted.PublicationCount, wanted.ContributionCount = &count, &count, &count, &count
			}
			calls := "read,list,associations"
			if mode == "absent" {
				calls = "read,list"
			}
			if !reflect.DeepEqual(status, wanted) || !reflect.DeepEqual(jobs[1].Association, &otherWanted) || strings.Join(probe.calls, ",") != calls || (mode != "absent" && !reflect.DeepEqual(probe.numbers, []int{7, 8})) {
				test.Fatal("association projection invented completeness/applicability or lost batching", status, probe.calls)
			}
			jobs[0].State, status.ProviderHistory = "changed", "changed"
			if !reflect.DeepEqual(before, probe.listed) {
				test.Fatal("listing retained mutable source aliases")
			}
		})
	}
}
