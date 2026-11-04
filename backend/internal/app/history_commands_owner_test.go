package app

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/wnsdy95/cxthub/backend/internal/adapters/store"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/inbound"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

type historyOwnerCallerKey struct{}
type historyOwnerScopeKey struct{}
type historyOwnerProofKey struct{}

type historyOwnerProbe struct {
	test         *testing.T
	ctx          context.Context
	repository   domain.Repo
	snapshot     func(context.Context, domain.ContentHash, domain.ContentHash) (domain.Snapshot, error)
	memory       func(context.Context, domain.ContentHash, domain.ContentHash) (domain.MemoryDigest, error)
	verify       func(context.Context, domain.ContentHash, domain.Snapshot) error
	accepted     []domain.HistoryEvent
	written      []domain.HistoryEvent
	queued       []domain.HistoryEvent
	captured     []domain.Snapshot
	notification domain.Ref
	calls        []string
	failures     map[string]error
	active       string
	preparing    bool
	nilProof     bool
}

func (probe *historyOwnerProbe) caller(ctx context.Context) {
	probe.test.Helper()
	actor, system := inbound.RepositoryActor(ctx)
	wantActor, wantSystem := inbound.RepositoryActor(probe.ctx)
	deadline, hasDeadline := ctx.Deadline()
	wantDeadline, wantHasDeadline := probe.ctx.Deadline()
	if ctx.Value(historyOwnerCallerKey{}) != probe || actor != wantActor || system != wantSystem ||
		inbound.Correlation(ctx) != "history-owner" || hasDeadline != wantHasDeadline || !deadline.Equal(wantDeadline) {
		probe.test.Fatal("history owner discarded caller context")
	}
	if probe.preparing && ctx.Value(historyOwnerProofKey{}) != probe {
		probe.test.Fatal("history owner discarded document-proof context")
	}
}

func (probe *historyOwnerProbe) touch(ctx context.Context, repo domain.ContentHash, operation string) error {
	probe.test.Helper()
	probe.caller(ctx)
	if repo != probe.repository.ID || probe.active == "" || ctx.Value(historyOwnerScopeKey{}) != probe.active {
		probe.test.Fatalf("%s escaped bound repository scope %q", operation, probe.active)
	}
	probe.calls = append(probe.calls, operation)
	if err := ctx.Err(); err != nil {
		return err
	}
	return probe.failures[operation]
}

func (probe *historyOwnerProbe) count(operation string) int {
	count := 0
	for _, actual := range probe.calls {
		if actual == operation {
			count++
		}
	}
	return count
}

func (probe *historyOwnerProbe) require(condition bool, message string) {
	probe.test.Helper()
	if !condition {
		probe.test.Fatalf("%s: calls=%v", message, probe.calls)
	}
}

func (probe *historyOwnerProbe) result(actual, wanted error) {
	probe.test.Helper()
	if !errors.Is(actual, wanted) {
		probe.test.Fatalf("err=%v want=%v calls=%v", actual, wanted, probe.calls)
	}
}

func (probe *historyOwnerProbe) GetRepo(ctx context.Context, repo domain.ContentHash) (domain.Repo, error) {
	return probe.repository, probe.touch(ctx, repo, "repo")
}

func (probe *historyOwnerProbe) GetSnapshot(ctx context.Context, repo, hash domain.ContentHash) (domain.Snapshot, error) {
	if err := probe.touch(ctx, repo, "snapshot"); err != nil {
		return domain.Snapshot{}, err
	}
	return probe.snapshot(ctx, repo, hash)
}

func (probe *historyOwnerProbe) ListHistoryEvents(ctx context.Context, repo domain.ContentHash) ([]domain.HistoryEvent, error) {
	return append([]domain.HistoryEvent(nil), probe.accepted...), probe.touch(ctx, repo, "history")
}

func (probe *historyOwnerProbe) ApplyHistoryEvent(ctx context.Context, event domain.HistoryEvent) error {
	if probe.active != "write" {
		probe.test.Fatal("history mutation ran outside final write")
	}
	if err := probe.touch(ctx, domain.ContentHash(event.RepoID), "apply"); err != nil {
		return err
	}
	probe.written = append(probe.written, event)
	probe.accepted = append(probe.accepted, event)
	return nil
}

func (probe *historyOwnerProbe) GetMemory(ctx context.Context, repo, hash domain.ContentHash) (domain.MemoryDigest, error) {
	probe.require(probe.active == "write", "memory read escaped final write")
	if err := probe.touch(ctx, repo, "memory"); err != nil {
		return domain.MemoryDigest{}, err
	}
	return probe.memory(ctx, repo, hash)
}

func (probe *historyOwnerProbe) VerifySnapshotDocument(ctx context.Context, repo domain.ContentHash, snapshot domain.Snapshot) error {
	wantScope := "write"
	if probe.preparing && !probe.nilProof {
		wantScope = "read"
	}
	if probe.active != wantScope {
		probe.test.Fatalf("document verification ran in %q, want %q", probe.active, wantScope)
	}
	if err := probe.touch(ctx, repo, "verify"); err != nil {
		return err
	}
	return probe.verify(ctx, repo, snapshot)
}

func (probe *historyOwnerProbe) within(ctx context.Context, repo domain.ContentHash, kind string, operation func(context.Context) error) error {
	probe.caller(ctx)
	if probe.active != "" || repo != probe.repository.ID {
		probe.test.Fatal("history owner nested or changed repository scope")
	}
	probe.calls = append(probe.calls, kind)
	if err := probe.failures[kind]; err != nil {
		return err
	}
	probe.active = kind
	defer func() { probe.active = "" }()
	return operation(context.WithValue(ctx, historyOwnerScopeKey{}, kind))
}

func (probe *historyOwnerProbe) WithinHistoryWrite(ctx context.Context, repo domain.ContentHash, operation func(context.Context) error) error {
	return probe.within(ctx, repo, "write", operation)
}

func (probe *historyOwnerProbe) WithinHistoryObservation(ctx context.Context, repo domain.ContentHash, operation func(context.Context) error) error {
	return probe.within(ctx, repo, "observation", operation)
}

func (probe *historyOwnerProbe) WithinHistoryRead(ctx context.Context, repo domain.ContentHash, operation func(context.Context) error) error {
	return probe.within(ctx, repo, "read", operation)
}

func (probe *historyOwnerProbe) NotifyRefUpdate(ctx context.Context, repo domain.ContentHash, ref domain.Ref, forced, created bool) error {
	probe.require(probe.active == "write", "notification escaped final write")
	if !forced || created || ref.RepoID != repo || ref.Kind != domain.RefBranch {
		probe.test.Fatal("history advance changed notification semantics")
	}
	probe.notification = ref
	return probe.touch(ctx, repo, "notify")
}

func (probe *historyOwnerProbe) QueueHistoryGitScan(ctx context.Context, event domain.HistoryEvent) error {
	probe.require(probe.active == "write", "Git scan escaped final write")
	probe.queued = append(probe.queued, event)
	return probe.touch(ctx, domain.ContentHash(event.RepoID), "scan")
}

func (probe *historyOwnerProbe) WakePRSourceJobs(ctx context.Context, repo domain.ContentHash, when time.Time) error {
	probe.require(probe.active == "write", "PR wake escaped final write")
	if when.IsZero() {
		probe.test.Fatal("publication wake lost its timestamp")
	}
	return probe.touch(ctx, repo, "wake")
}

func (probe *historyOwnerProbe) WithinHistoryDocumentProof(ctx context.Context, repo domain.ContentHash, operation func(context.Context, outbound.HistoryDocumentProof) error) error {
	probe.caller(ctx)
	if repo != probe.repository.ID || probe.active != "" {
		probe.test.Fatal("proof entered inside a repository scope")
	}
	probe.calls = append(probe.calls, "proof")
	probe.preparing = true
	defer func() { probe.preparing = false }()
	var proof outbound.HistoryDocumentProof = probe
	if probe.nilProof {
		proof = nil
	}
	return operation(context.WithValue(ctx, historyOwnerProofKey{}, probe), proof)
}

func (probe *historyOwnerProbe) Capture(ctx context.Context, snapshots []domain.Snapshot) error {
	if probe.active != "read" {
		probe.test.Fatal("document proof captured outside readonly preparation")
	}
	probe.captured = append([]domain.Snapshot(nil), snapshots...)
	return probe.touch(ctx, probe.repository.ID, "capture")
}

func (probe *historyOwnerProbe) Pin(ctx context.Context) error {
	if probe.active != "write" || len(probe.captured) == 0 {
		probe.test.Fatal("document proof pinned without capture or final write")
	}
	return probe.touch(ctx, probe.repository.ID, "pin")
}

var (
	_ *historyRecorder                   = (*HistoryCommands)(nil)
	_ HistoryMetadata                    = (*historyOwnerProbe)(nil)
	_ HistoryEventStore                  = (*historyOwnerProbe)(nil)
	_ HistoryMemoryReader                = (*historyOwnerProbe)(nil)
	_ HistoryDocumentSources             = (*historyOwnerProbe)(nil)
	_ HistoryWriteScope                  = (*historyOwnerProbe)(nil)
	_ HistoryFollowups                   = (*historyOwnerProbe)(nil)
	_ outbound.HistoryDocumentProofStore = (*historyOwnerProbe)(nil)
)

func newHistoryOwnerFixture(test *testing.T) (*historyOwnerProbe, HistoryCommandsConfig, historyMemorySelectionFixture) {
	test.Helper()
	storage := store.NewFSStore(test.TempDir())
	native := newHistoryMemorySelectionFixture(test, nil, storage, hh(test.Name()))
	repository, err := storage.GetRepo(native.ctx, native.repo)
	if err != nil {
		test.Fatal(err)
	}
	probe := &historyOwnerProbe{test: test, repository: repository, snapshot: storage.GetSnapshot, memory: storage.GetMemory, failures: make(map[string]error)}
	ctx := inbound.WithCorrelation(context.WithValue(native.ctx, historyOwnerCallerKey{}, probe), "history-owner")
	bound, cancel := context.WithTimeout(ctx, time.Minute)
	probe.ctx = bound
	test.Cleanup(cancel)
	identities := []domain.DocumentIdentity{domain.DocumentIdentityLegacy, domain.DocumentIdentityRootV1}
	probe.verify = documentPublicationSourcesFromStores(storage, storage, nil, identities).VerifySnapshotDocument
	config := HistoryCommandsConfig{Profile: HistoryComplete, Metadata: probe, Events: probe, Memories: probe,
		Sources: probe, Scope: probe, Followups: probe, SupportedIdentities: identities}
	return probe, config, native
}

func historyOwnerCommand(test *testing.T, config HistoryCommandsConfig) *HistoryCommands {
	test.Helper()
	owner, err := NewHistoryCommands(config)
	if err != nil || owner == nil {
		test.Fatalf("construct actual history owner: owner=%v err=%v", owner, err)
	}
	return owner
}

func TestHistoryOwnerConstructorContract(test *testing.T) {
	probe, base, _ := newHistoryOwnerFixture(test)
	for _, profile := range []HistoryProfile{HistoryCompatibility, HistoryComplete} {
		config := base
		config.Profile, config.Preparation = profile, probe
		owner := historyOwnerCommand(test, config)
		config.SupportedIdentities[0] = domain.DocumentIdentity("mutated")
		if owner.supported[0] != domain.DocumentIdentityLegacy {
			test.Fatal("constructor retained caller-owned identity slice")
		}
		config.SupportedIdentities[0] = domain.DocumentIdentityLegacy
	}
	var missing *historyOwnerProbe
	for _, field := range []string{"Metadata", "Scope", "Events", "Memories", "Sources", "Followups", "Preparation"} {
		for _, profile := range []HistoryProfile{HistoryCompatibility, HistoryComplete} {
			for _, typed := range []bool{false, true} {
				test.Run(field+"/"+string(profile)+map[bool]string{false: "/absent", true: "/typed-nil"}[typed], func(test *testing.T) {
					config := base
					config.Profile = profile
					port := reflect.ValueOf(&config).Elem().FieldByName(field)
					port.Set(reflect.Zero(port.Type()))
					if typed {
						config.Events, config.Memories, config.Sources, config.Followups = nil, nil, nil, nil
						port.Set(reflect.ValueOf(missing))
					}
					var want error
					if typed || field == "Metadata" || field == "Scope" {
						want = domain.ErrValidation
					} else if profile == HistoryComplete && field != "Preparation" {
						want = domain.ErrCatalogUnsupported
					}
					owner, err := NewHistoryCommands(config)
					if !errors.Is(err, want) || (want == nil) != (owner != nil) {
						test.Fatalf("constructor owner=%v err=%v, want %v", owner, err, want)
					}
				})
			}
		}
	}
	for _, profile := range []HistoryProfile{HistoryCompatibility, HistoryComplete, HistoryProfile("")} {
		config := base
		config.Profile = profile
		config.Events, config.Memories, config.Sources, config.Followups = nil, nil, nil, nil
		wanted := map[HistoryProfile]error{HistoryCompatibility: nil, HistoryComplete: domain.ErrCatalogUnsupported, HistoryProfile(""): domain.ErrValidation}
		_, err := NewHistoryCommands(config)
		probe.result(err, wanted[profile])
	}
	for _, field := range []string{"Events", "Sources"} {
		config := base
		config.Profile, config.Preparation = HistoryCompatibility, probe
		port := reflect.ValueOf(&config).Elem().FieldByName(field)
		port.Set(reflect.Zero(port.Type()))
		_, err := NewHistoryCommands(config)
		probe.result(err, domain.ErrCatalogUnsupported)
		port.Set(reflect.ValueOf(missing))
		_, err = NewHistoryCommands(config)
		probe.result(err, domain.ErrValidation)
	}
	probe.require(len(probe.calls) == 0, "constructor performed IO")
}

func TestHistoryOwnerActualAdmission(test *testing.T) {
	for _, mode := range []string{"position", "birth", "unproven-origin", "publish", "unproven-publish", "pr-spoof", "memory", "missing-predecessor", "corrupt-memory", "advance", "protected-advance", "protected-rename", "protected-archive", "missing-body"} {
		test.Run(mode, func(test *testing.T) {
			probe, config, native := newHistoryOwnerFixture(test)
			event, want := native.before, error(nil)
			switch mode {
			case "birth", "unproven-origin":
				origin := native.before
				origin.BranchID = "retained-origin"
				event.Kind, event.Branch, event.BranchID = "birth", "feature/new", "new-context"
				event.Creation = &domain.GitCreation{Evidence: "process-argv", Command: []string{"git", "switch", "-c", event.Branch, "main"}, StartRef: "main", StartCommit: event.GitAfter, OriginBranch: "main", OriginBranchID: origin.BranchID}
				if mode == "birth" {
					probe.accepted, event.ID = []domain.HistoryEvent{origin}, strings.Repeat("3", 32)
				} else {
					want = domain.ErrConflict
				}
			case "publish", "unproven-publish":
				event = prPublication(native.before)
				if mode == "publish" {
					probe.accepted = []domain.HistoryEvent{native.before}
				} else {
					want = domain.ErrConflict
				}
			case "pr-spoof":
				event.Kind, event.SourceBranchID = "pr-merge", "source-context"
				event.PR = &domain.PullRequestMerge{Number: 61, BaseBranch: "main", HeadBranch: "feature/x", HeadSHA: strings.Repeat("a", 40), MergeSHA: strings.Repeat("b", 40)}
				want = domain.ErrForbidden
			case "memory", "missing-predecessor", "corrupt-memory":
				event, probe.accepted = native.after, []domain.HistoryEvent{native.before}
				if mode == "missing-predecessor" {
					probe.accepted, want = nil, domain.ErrConflict
				}
				if mode == "corrupt-memory" {
					read := probe.memory
					probe.memory = func(ctx context.Context, repo, hash domain.ContentHash) (domain.MemoryDigest, error) {
						memory, err := read(ctx, repo, hash)
						memory.Summary = "tampered"
						return memory, err
					}
					want = domain.ErrIntegrity
				}
			case "advance", "protected-advance", "protected-rename", "protected-archive":
				event.Kind = strings.TrimPrefix(mode, "protected-")
				if event.Kind == "advance" {
					event.Target = native.other
				}
				if event.Kind == "rename" {
					event.PreviousBranch, event.Branch = "main", "renamed"
				}
				if mode != "advance" {
					probe.repository.ProtectDefault, want = true, domain.ErrForbidden
				}
			case "missing-body":
				probe.failures["verify"], want = domain.ErrNotFound, domain.ErrNotFound
			}
			probe.result(domain.ValidateHistoryEvent(event), nil)
			probe.result(historyOwnerCommand(test, config).RecordHistory(probe.ctx, event), want)
			probe.require(probe.count("write") == 1, "admission did not use one write scope")
			if want != nil {
				probe.require(len(probe.written) == 0 && len(probe.queued) == 0 && probe.count("notify") == 0 && probe.count("wake") == 0, "rejection wrote or emitted followups")
				return
			}
			probe.require(len(probe.written) == 1 && reflect.DeepEqual(probe.written[0], event), "owner lost exact event or duplicated write")
			probe.require(len(probe.queued) == 1 && reflect.DeepEqual(probe.queued[0], event), "Git scan lost exact event")
			probe.require((probe.count("wake") == 1) == (event.Kind == "publish") && (probe.count("notify") == 1) == (event.Kind == "advance"), "wrong followups")
			if event.Kind == "advance" {
				probe.require(probe.notification.Target == event.Target && probe.notification.Name == event.Branch, "advance notification lost ref")
			}
		})
	}
}

func TestHistoryOwnerAcceptedReplaySkipsEvidenceAndPolicy(test *testing.T) {
	for _, kind := range []string{"position", "advance", "publish"} {
		for _, prepared := range []bool{false, true} {
			test.Run(kind+map[bool]string{false: "/fallback", true: "/prepared"}[prepared], func(test *testing.T) {
				probe, config, native := newHistoryOwnerFixture(test)
				event := native.after
				if kind == "advance" {
					event = native.before
					event.Kind = kind
				}
				if kind == "publish" {
					event = prPublication(native.before)
				}
				probe.accepted, probe.repository.ProtectDefault = []domain.HistoryEvent{event}, true
				for _, operation := range []string{"repo", "snapshot", "verify", "memory"} {
					probe.failures[operation] = domain.ErrNotFound
				}
				if prepared {
					config.Preparation = probe
				}
				owner := historyOwnerCommand(test, config)
				probe.result(owner.RecordHistory(probe.ctx, event), nil)
				probe.require(probe.count("write") == 1 && probe.count("scan") == 1 && (probe.count("wake") == 1) == (kind == "publish"), "replay changed followups")
				for _, operation := range []string{"repo", "snapshot", "verify", "memory", "apply", "notify", "capture", "pin", "read"} {
					probe.require(probe.count(operation) == 0, "accepted replay repeated "+operation)
				}
				event.CreatedAt = event.CreatedAt.Add(time.Nanosecond)
				probe.result(owner.RecordHistory(probe.ctx, event), domain.ErrRefConflict)
				probe.require(probe.count("scan") == 1 && len(probe.written) == 0, "altered same-ID event emitted work")
			})
		}
	}
}

func TestHistoryOwnerPreparationBoundariesAndFailures(test *testing.T) {
	for _, stage := range []string{"success", "nil-proof", "verify", "capture", "pin", "write"} {
		test.Run(stage, func(test *testing.T) {
			probe, config, native := newHistoryOwnerFixture(test)
			config.Preparation, probe.nilProof = probe, stage == "nil-proof"
			failure := errors.New("history preparation failure")
			var want error
			if stage != "success" && stage != "nil-proof" {
				probe.failures[stage], want = failure, failure
			}
			probe.result(historyOwnerCommand(test, config).RecordHistory(probe.ctx, native.before), want)
			probe.require(probe.count("write") == 1 && probe.active == "", "preparation changed final write scope")
			if stage == "nil-proof" {
				probe.require(probe.count("observation") == 0 && probe.count("read") == 0 && probe.count("pin") == 0, "nil proof fabricated preparation")
			} else {
				probe.require(probe.count("observation") == 1 && probe.count("read") == 1, "preflight lost separate bound scopes")
			}
			if want != nil {
				probe.require(probe.count("apply") == 0 && len(probe.queued) == 0, "failed proof mutated history")
				return
			}
			probe.require(probe.count("verify") == 1 && probe.count("apply") == 1 && len(probe.written) == 1, "duplicate roots caused duplicate verification/write")
			if stage == "success" {
				probe.require(probe.count("pin") == 1 && len(probe.captured) == 1 && probe.captured[0].ID == native.before.Target, "proof captured/pinned wrong root set")
			}
		})
	}
}

func TestHistoryOwnerWriteAndFollowupErrors(test *testing.T) {
	for _, stage := range []string{"apply", "notify", "scan", "wake"} {
		test.Run(stage, func(test *testing.T) {
			probe, config, native := newHistoryOwnerFixture(test)
			event := native.before
			if stage == "notify" {
				event.Kind, event.Target = "advance", native.other
			}
			if stage == "wake" {
				event, probe.accepted = prPublication(native.before), []domain.HistoryEvent{native.before}
			}
			failure := errors.New("history write/followup failure")
			probe.failures[stage] = failure
			probe.result(historyOwnerCommand(test, config).RecordHistory(probe.ctx, event), failure)
			probe.require(probe.count("apply") == 1 && probe.count("write") == 1, "write/followup failure retried")
			if stage == "apply" || stage == "notify" {
				probe.require(probe.count("scan") == 0, "followups continued after earlier failure")
			}
			if stage != "wake" {
				probe.require(probe.count("wake") == 0, "unrelated event woke PR source jobs")
			}
		})
	}
}
