package app

import (
	"context"
	"errors"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/inbound"
)

type branchOwnerBoundKey struct{}

type branchOwnerProbe struct {
	test       *testing.T
	repo       domain.Repo
	graph      domain.JoinGraph
	logs       []domain.RefLogEntry
	history    []domain.HistoryEvent
	births     []domain.HistoryEvent
	mutations  []domain.JoinMutation
	swapped    domain.Ref
	expected   domain.ContentHash
	failures   map[string]error
	afterErr   error
	bound      context.Context
	active     bool
	calls      []string
	scopes     []string
	actors     map[string]string
	access     []bool
	accessUser string
}

func newBranchOwnerProbe(test *testing.T) *branchOwnerProbe {
	repo, source := domain.HashContent([]byte("branch-owner-repo")), domain.HashContent([]byte("branch-owner-source"))
	return &branchOwnerProbe{test: test, repo: domain.Repo{ID: repo}, failures: map[string]error{}, actors: map[string]string{},
		graph: domain.JoinGraph{Snapshots: []domain.Snapshot{{ID: source, RepoID: repo}}}}
}

func (probe *branchOwnerProbe) touch(ctx context.Context, repo domain.ContentHash, operation string) error {
	probe.test.Helper()
	if !probe.active || ctx != probe.bound || ctx.Value(branchOwnerBoundKey{}) != probe || repo != probe.repo.ID {
		probe.test.Fatalf("%s escaped callback context: repo=%s active=%v", operation, repo, probe.active)
	}
	probe.calls = append(probe.calls, operation)
	return probe.failures[operation]
}

func branchOwnerWithin[Result any](probe *branchOwnerProbe, ctx context.Context, repo domain.ContentHash, name string, operation func(context.Context) (Result, error)) (output Result, err error) {
	probe.test.Helper()
	if repo != probe.repo.ID || (probe.active && ctx != probe.bound) {
		probe.test.Fatal("scope changed repository or nested callback context")
	}
	previous, active := probe.bound, probe.active
	probe.bound, probe.active = context.WithValue(ctx, branchOwnerBoundKey{}, probe), true
	defer func() { probe.bound, probe.active = previous, active }()
	probe.scopes = append(probe.scopes, name)
	probe.actors[name], _ = inbound.RepositoryActor(ctx)
	if err = probe.failures["scope:"+name]; err != nil {
		return output, err
	}
	output, err = operation(probe.bound)
	if err == nil {
		err = probe.afterErr
	}
	return output, err
}

func (probe *branchOwnerProbe) WithinFork(ctx context.Context, repo domain.ContentHash, operation func(context.Context) (inbound.ForkOutput, error)) (inbound.ForkOutput, error) {
	return branchOwnerWithin(probe, ctx, repo, "fork", operation)
}
func (probe *branchOwnerProbe) WithinJoin(ctx context.Context, repo domain.ContentHash, operation func(context.Context) (inbound.JoinOutput, error)) (inbound.JoinOutput, error) {
	return branchOwnerWithin(probe, ctx, repo, "join", operation)
}
func (probe *branchOwnerProbe) WithinPreview(ctx context.Context, repo domain.ContentHash, operation func(context.Context) (inbound.JoinPreviewOutput, error)) (inbound.JoinPreviewOutput, error) {
	return branchOwnerWithin(probe, ctx, repo, "preview", operation)
}
func (probe *branchOwnerProbe) WithinHistory(ctx context.Context, repo domain.ContentHash, operation func(context.Context) ([]domain.HistoryEvent, error)) ([]domain.HistoryEvent, error) {
	return branchOwnerWithin(probe, ctx, repo, "history", operation)
}
func (probe *branchOwnerProbe) WithinProtocol(ctx context.Context, repo domain.ContentHash, operation func(context.Context) error) error {
	_, err := branchOwnerWithin(probe, ctx, repo, "protocol", func(bound context.Context) (struct{}, error) { return struct{}{}, operation(bound) })
	return err
}

func (probe *branchOwnerProbe) GetRepo(ctx context.Context, repo domain.ContentHash) (domain.Repo, error) {
	return probe.repo, probe.touch(ctx, repo, "repo")
}
func (probe *branchOwnerProbe) GetSnapshot(ctx context.Context, repo, target domain.ContentHash) (domain.Snapshot, error) {
	if err := probe.touch(ctx, repo, "snapshot"); err != nil {
		return domain.Snapshot{}, err
	}
	for _, snapshot := range probe.graph.Snapshots {
		if snapshot.ID == target {
			return snapshot, nil
		}
	}
	return domain.Snapshot{}, domain.ErrNotFound
}
func (probe *branchOwnerProbe) GetRef(ctx context.Context, repo domain.ContentHash, kind domain.RefKind, name string) (domain.Ref, error) {
	if err := probe.touch(ctx, repo, "ref"); err != nil {
		return domain.Ref{}, err
	}
	for _, ref := range probe.graph.Refs {
		if ref.Kind == kind && ref.Name == name {
			return ref, nil
		}
	}
	return domain.Ref{}, domain.ErrNotFound
}
func (probe *branchOwnerProbe) CompareAndSwapRef(ctx context.Context, repo domain.ContentHash, next domain.Ref, expected domain.ContentHash) error {
	probe.swapped, probe.expected = next, expected
	return probe.touch(ctx, repo, "cas")
}
func (probe *branchOwnerProbe) ListSnapshots(ctx context.Context, repo domain.ContentHash, branch string) ([]domain.Snapshot, error) {
	if branch != "" {
		probe.test.Fatalf("join graph was branch-filtered: %q", branch)
	}
	return probe.graph.Snapshots, probe.touch(ctx, repo, "snapshots")
}
func (probe *branchOwnerProbe) ListRefs(ctx context.Context, repo domain.ContentHash) ([]domain.Ref, error) {
	return probe.graph.Refs, probe.touch(ctx, repo, "refs")
}
func (probe *branchOwnerProbe) ListPendings(ctx context.Context, repo domain.ContentHash) ([]domain.Pending, error) {
	return probe.graph.Pendings, probe.touch(ctx, repo, "pendings")
}
func (probe *branchOwnerProbe) ReadReflog(ctx context.Context, repo domain.ContentHash) ([]domain.RefLogEntry, error) {
	return probe.logs, probe.touch(ctx, repo, "reflog")
}
func (probe *branchOwnerProbe) ApplyJoin(ctx context.Context, mutation domain.JoinMutation) error {
	probe.mutations = append(probe.mutations, mutation)
	return probe.touch(ctx, mutation.RepoID, "apply")
}
func (probe *branchOwnerProbe) ListHistoryEvents(ctx context.Context, repo domain.ContentHash) ([]domain.HistoryEvent, error) {
	return probe.history, probe.touch(ctx, repo, "history")
}
func (probe *branchOwnerProbe) RecordHistory(ctx context.Context, event domain.HistoryEvent) error {
	probe.births = append(probe.births, event)
	return probe.touch(ctx, domain.ContentHash(event.RepoID), "record")
}
func (probe *branchOwnerProbe) EnableContextProtocol(ctx context.Context, repo domain.ContentHash) error {
	return probe.touch(ctx, repo, "protocol")
}
func (probe *branchOwnerProbe) AuthorizeJoin(ctx context.Context, repo domain.ContentHash, actor string, lock bool) error {
	probe.access, probe.accessUser = append(probe.access, lock), actor
	return probe.touch(ctx, repo, "access")
}
func (probe *branchOwnerProbe) ReconcileSharedPending(ctx context.Context, repo domain.ContentHash) error {
	return probe.touch(ctx, repo, "pending")
}
func (probe *branchOwnerProbe) config(profile ContextBranchProfile) ContextBranchOperationsConfig {
	return ContextBranchOperationsConfig{Profile: profile, Store: probe, Scope: probe, History: probe, Recorder: probe, Protocol: probe, Access: probe, Pending: probe}
}
func branchOwnerCommands(test *testing.T, config ContextBranchOperationsConfig) *ContextBranchOperations {
	test.Helper()
	commands, err := NewContextBranchOperations(config)
	if err != nil {
		test.Fatal(err)
	}
	return commands
}
func branchOwnerCalls(test *testing.T, probe *branchOwnerProbe, want ...string) {
	test.Helper()
	if !reflect.DeepEqual(probe.calls, want) {
		test.Fatalf("calls=%v want=%v", probe.calls, want)
	}
}
func branchOwnerJoinFixture(test *testing.T) (*branchOwnerProbe, inbound.JoinPreviewInput, map[string]domain.ContentHash) {
	test.Helper()
	_, storage, input, nodes := joinPreviewFixture(test)
	probe, ctx := newBranchOwnerProbe(test), systemTestContext()
	var err error
	probe.repo, err = storage.GetRepo(ctx, input.RepoID)
	if err == nil {
		probe.graph.Snapshots, err = storage.ListSnapshots(ctx, input.RepoID, "")
	}
	if err == nil {
		probe.graph.Refs, err = storage.ListRefs(ctx, input.RepoID)
	}
	if err == nil {
		probe.graph.Pendings, err = storage.ListPendings(ctx, input.RepoID)
	}
	if err == nil {
		probe.logs, err = storage.ReadReflog(ctx, input.RepoID)
	}
	if err == nil {
		probe.graph.Memberships, err = domain.ProjectSnapshotBranchMemberships(ctx, probe.graph.Snapshots, probe.logs)
	}
	if err != nil {
		test.Fatal(err)
	}
	return probe, input, nodes
}

var _ inbound.ContextBranchOperations = (*ContextBranchOperations)(nil)
var _ *contextBranchOperations = (*ContextBranchOperations)(nil)

func TestContextBranchOwnerConstructorNoIO(test *testing.T) {
	for _, profile := range []ContextBranchProfile{ContextBranchComplete, ContextBranchCompatibility} {
		for _, name := range []string{"Store", "Scope", "History", "Recorder", "Protocol", "Access", "Pending"} {
			for _, typed := range []bool{false, true} {
				probe := newBranchOwnerProbe(test)
				config := probe.config(profile)
				field := reflect.ValueOf(&config).Elem().FieldByName(name)
				field.Set(reflect.Zero(field.Type()))
				want := error(nil)
				if profile == ContextBranchComplete {
					want = domain.ErrCatalogUnsupported
				}
				if name == "Store" || name == "Scope" {
					want = domain.ErrValidation
				}
				if typed {
					field.Set(reflect.ValueOf((*branchOwnerProbe)(nil)))
					want = domain.ErrValidation
					if name != "History" {
						config.History = nil
					}
				}
				commands, err := NewContextBranchOperations(config)
				if !errors.Is(err, want) || (commands == nil) != (want != nil) {
					test.Fatalf("profile=%s port=%s typed=%v owner=%v error=%v want=%v", profile, name, typed, commands, err, want)
				}
				if len(probe.calls)+len(probe.scopes) != 0 {
					test.Fatal("constructor performed IO")
				}
			}
		}
	}
	probe := newBranchOwnerProbe(test)
	branchOwnerCommands(test, probe.config(ContextBranchComplete))
	config := probe.config(ContextBranchCompatibility)
	config.History, config.Recorder, config.Protocol, config.Access, config.Pending = nil, nil, nil, nil, nil
	branchOwnerCommands(test, config)
	for _, profile := range []ContextBranchProfile{"", "future"} {
		config.Profile = profile
		if commands, err := NewContextBranchOperations(config); commands != nil || !errors.Is(err, domain.ErrValidation) {
			test.Fatalf("invalid profile accepted: %v %v", commands, err)
		}
	}
	if len(probe.calls)+len(probe.scopes) != 0 {
		test.Fatal("constructor performed IO")
	}
}

func TestContextBranchOwnerForkProtocolRouting(test *testing.T) {
	for _, scenario := range []string{"legacy", "birth", "source missing", "invalid name", "cas failure", "recorder failure"} {
		test.Run(scenario, func(test *testing.T) {
			probe := newBranchOwnerProbe(test)
			input := inbound.ForkInput{RepoID: probe.repo.ID, FromSnapshot: probe.graph.Snapshots[0].ID, NewBranch: "topic"}
			wantErr := error(nil)
			if scenario == "birth" || scenario == "recorder failure" {
				probe.repo.ContextProtocol = 1
				birth := domain.HistoryEvent{ID: strings.Repeat("1", 32), RepoID: string(probe.repo.ID), BranchID: "retired", Branch: "topic", Kind: "birth"}
				archive := domain.HistoryEvent{ID: strings.Repeat("2", 32), RepoID: birth.RepoID, BranchID: birth.BranchID, Branch: birth.Branch, Kind: "archive", BindingParent: birth.ID}
				probe.history = []domain.HistoryEvent{archive, birth}
			}
			switch scenario {
			case "source missing":
				input.FromSnapshot, wantErr = domain.HashContent([]byte("missing")), domain.ErrNotFound
			case "invalid name":
				input.NewBranch, wantErr = "../topic", domain.ErrValidation
			case "cas failure":
				probe.failures["cas"], wantErr = domain.ErrRefConflict, domain.ErrRefConflict
			case "recorder failure":
				probe.failures["record"], wantErr = domain.ErrIntegrity, domain.ErrIntegrity
			}
			output, err := branchOwnerCommands(test, probe.config(ContextBranchComplete)).Fork(inbound.WithRepositoryActor(context.Background(), "transport"), input)
			if !errors.Is(err, wantErr) {
				test.Fatalf("fork=%+v error=%v want=%v", output, err, wantErr)
			}
			if wantErr != nil {
				if output != (inbound.ForkOutput{}) {
					test.Fatalf("failure exposed success: %+v", output)
				}
			} else if output != (inbound.ForkOutput{Branch: "topic", Head: input.FromSnapshot}) {
				test.Fatalf("fork=%+v", output)
			}
			if len(probe.scopes) != 0 && probe.actors["fork"] != "transport" {
				test.Fatal("fork lost authenticated actor")
			}
			switch scenario {
			case "legacy", "cas failure":
				branchOwnerCalls(test, probe, "snapshot", "repo", "cas")
				if probe.expected != "" || probe.swapped != (domain.Ref{RepoID: input.RepoID, Kind: domain.RefBranch, Name: "topic", Target: input.FromSnapshot}) {
					test.Fatalf("CAS=%+v expected=%s", probe.swapped, probe.expected)
				}
			case "birth", "recorder failure":
				branchOwnerCalls(test, probe, "snapshot", "repo", "history", "record")
				if len(probe.births) != 1 {
					test.Fatalf("births=%+v", probe.births)
				}
				event := probe.births[0]
				if len(event.ID) != 32 || event.ID != event.BranchID || event.BranchID == "retired" || event.Kind != "birth" || event.Branch != "topic" || event.RepoID != string(input.RepoID) || event.Source != input.FromSnapshot || event.Target != input.FromSnapshot || event.BindingParent != strings.Repeat("2", 32) || event.CreatedAt.IsZero() {
					test.Fatalf("birth=%+v", event)
				}
				if err := domain.ValidateHistoryEvent(event); err != nil {
					test.Fatalf("invalid immutable birth: %v", err)
				}
			case "source missing":
				branchOwnerCalls(test, probe, "snapshot")
			case "invalid name":
				branchOwnerCalls(test, probe)
			}
		})
	}
}

func TestContextBranchOwnerForkUsesActualHistoryRecorder(test *testing.T) {
	history, recorderConfig, native := newHistoryOwnerFixture(test)
	probe := newBranchOwnerProbe(test)
	probe.repo = history.repository
	probe.repo.ContextProtocol = 1
	snapshot, err := native.st.GetSnapshot(native.ctx, native.repo, native.before.Source)
	if err != nil {
		test.Fatal(err)
	}
	probe.graph.Snapshots = []domain.Snapshot{snapshot}
	birth := domain.HistoryEvent{ID: strings.Repeat("3", 32), RepoID: string(native.repo), BranchID: "retired", Branch: "topic", Kind: "birth", Source: snapshot.ID, Target: snapshot.ID}
	archive := birth
	archive.ID, archive.Kind, archive.BindingParent = strings.Repeat("4", 32), "archive", birth.ID
	probe.history, history.accepted = []domain.HistoryEvent{archive, birth}, []domain.HistoryEvent{archive, birth}
	config := probe.config(ContextBranchComplete)
	config.Recorder = historyOwnerCommand(test, recorderConfig)
	output, err := branchOwnerCommands(test, config).Fork(history.ctx, inbound.ForkInput{RepoID: native.repo, FromSnapshot: snapshot.ID, NewBranch: "topic"})
	if err != nil || output.Head != snapshot.ID || len(history.written) != 1 || history.count("verify") != 1 {
		test.Fatalf("fork=%+v recorder=%+v calls=%v error=%v", output, history.written, history.calls, err)
	}
	if history.written[0].BindingParent != archive.ID || history.written[0].Kind != "birth" || history.written[0].BranchID == birth.BranchID {
		test.Fatalf("recorded birth=%+v", history.written[0])
	}
	branchOwnerCalls(test, probe, "snapshot", "repo", "history")
}

func TestContextBranchOwnerPreviewUsesDomainPlan(test *testing.T) {
	probe, input, nodes := branchOwnerJoinFixture(test)
	secondParent := domain.HashContent([]byte("second natural parent"))
	probe.graph.Snapshots = append(probe.graph.Snapshots, domain.Snapshot{RepoID: input.RepoID, ID: secondParent, Branch: "main"})
	for index := range probe.graph.Snapshots {
		if probe.graph.Snapshots[index].ID == nodes["H"] {
			probe.graph.Snapshots[index].Parents = []domain.ContentHash{nodes["P"], secondParent}
		}
	}
	commands := branchOwnerCommands(test, probe.config(ContextBranchComplete))
	plan, planErr := domain.PlanJoin(probe.graph, domain.JoinRequest{RepoID: input.RepoID, Branch: "main", Source: input.Snapshot})
	if planErr != nil {
		test.Fatal(planErr)
	}
	whole := plan
	whole.NewHead, whole.RemainingTip = nodes["T"], ""
	onlyRevision, allRevision := plan.Revision(), whole.Revision()
	output, err := commands.PreviewJoin(context.Background(), input)
	if err != nil || output.Branch != plan.Branch || output.BranchID != plan.BranchID || output.ExpectedHead != plan.ExpectedHead || output.Tip != nodes["T"] || output.Descendants != 1 || output.OnlyRevision != onlyRevision || output.AllRevision != allRevision || !reflect.DeepEqual(output.DropTargets, []domain.ContentHash{nodes["H"], nodes["P"]}) {
		test.Fatalf("preview=%+v plan=%+v error=%v", output, plan, err)
	}
	branchOwnerCalls(test, probe, "access", "snapshots", "refs", "pendings", "reflog")
	if !reflect.DeepEqual(probe.access, []bool{false}) || probe.accessUser != input.ActorID || !reflect.DeepEqual(probe.scopes, []string{"preview"}) {
		test.Fatal("preview did not use read scope and explicit actor without access lock")
	}
	probe.calls = nil
	for _, name := range []string{"zeta", "alpha"} {
		probe.graph.Refs = append(probe.graph.Refs, domain.Ref{RepoID: input.RepoID, Kind: domain.RefBranch, Name: name, BranchID: name, Target: nodes["P"]})
		probe.logs = append(probe.logs, domain.RefLogEntry{Kind: domain.RefBranch, Name: name, Old: input.Snapshot, New: nodes["P"]})
	}
	output, err = commands.PreviewJoin(context.Background(), input)
	wantOptions := []inbound.JoinBranchOption{{Branch: "alpha", BranchID: "alpha"}, {Branch: "main", BranchID: plan.BranchID}, {Branch: "zeta", BranchID: "zeta"}}
	if err != nil || output.Reason != "branch_required" || !reflect.DeepEqual(output.Branches, wantOptions) {
		test.Fatalf("branch choice=%+v error=%v", output, err)
	}
	input.Branch, input.Snapshot = "main", nodes["P"]
	output, err = commands.PreviewJoin(context.Background(), input)
	if err != nil || output.Reason != "natural_history" || output.Branch != "main" || output.OnlyRevision != "" || output.AllRevision != "" {
		test.Fatalf("denial=%+v error=%v", output, err)
	}
	if len(probe.mutations)+len(probe.births) != 0 || probe.swapped.Target != "" {
		test.Fatal("preview mutated state")
	}
	probe.calls, probe.failures["access"] = nil, domain.ErrForbidden
	output, err = commands.PreviewJoin(context.Background(), input)
	if !errors.Is(err, domain.ErrForbidden) || !reflect.DeepEqual(output, inbound.JoinPreviewOutput{}) {
		test.Fatalf("access denial=%+v error=%v", output, err)
	}
	branchOwnerCalls(test, probe, "access")
}

func TestContextBranchOwnerConfirmOrderingAndFailures(test *testing.T) {
	for _, scenario := range []string{"partial", "whole", "missing approval", "missing head", "stale revision", "stale head", "stale identity", "changed choice", "planner denial", "revoked access", "allocation failure", "apply failure"} {
		test.Run(scenario, func(test *testing.T) {
			probe, input, nodes := branchOwnerJoinFixture(test)
			commands := branchOwnerCommands(test, probe.config(ContextBranchComplete))
			preview, err := commands.PreviewJoin(context.Background(), input)
			if err != nil {
				test.Fatal(err)
			}
			confirmation := confirmPreview(input, preview, scenario == "whole")
			wantErr := error(nil)
			switch scenario {
			case "missing approval":
				confirmation.PlanRevision, wantErr = "", domain.ErrJoinPreviewChanged
			case "missing head":
				confirmation.ExpectedHead, wantErr = "", domain.ErrJoinPreviewChanged
			case "stale revision":
				confirmation.PlanRevision, wantErr = domain.HashContent([]byte("stale")), domain.ErrJoinPreviewChanged
			case "stale head":
				confirmation.ExpectedHead, wantErr = nodes["P"], domain.ErrJoinPreviewChanged
			case "stale identity":
				confirmation.BranchID, wantErr = "rebound-branch", domain.ErrJoinPreviewChanged
			case "changed choice":
				confirmation.IncludeDescendants, wantErr = true, domain.ErrJoinPreviewChanged
			case "planner denial":
				for index := range probe.graph.Refs {
					if probe.graph.Refs[index].Kind == domain.RefBranch && probe.graph.Refs[index].Name == "main" {
						probe.graph.Refs[index].Target = input.Snapshot
					}
				}
				wantErr = domain.ErrJoinPreviewChanged
			case "revoked access":
				probe.failures["access"], wantErr = domain.ErrForbidden, domain.ErrForbidden
			case "allocation failure":
				probe.failures["ref"], wantErr = domain.ErrIntegrity, domain.ErrIntegrity
			case "apply failure":
				probe.failures["apply"], wantErr = domain.ErrConflict, domain.ErrConflict
			}
			probe.calls, probe.scopes, probe.access = nil, nil, nil
			output, err := commands.ConfirmJoin(inbound.WithRepositoryActor(context.Background(), "wrong-actor"), confirmation)
			if !errors.Is(err, wantErr) {
				test.Fatalf("confirmation=%+v error=%v want=%v", output, err, wantErr)
			}
			if scenario == "missing approval" || scenario == "missing head" {
				if len(probe.scopes)+len(probe.calls) != 0 {
					test.Fatal("missing approval reached write scope")
				}
			} else if !reflect.DeepEqual(probe.scopes, []string{"join"}) || probe.actors["join"] != input.ActorID || probe.accessUser != input.ActorID || !reflect.DeepEqual(probe.access, []bool{true}) {
				test.Fatal("confirmation did not bind actor and reauthorize inside write scope")
			}
			if wantErr != nil {
				if output != (inbound.JoinOutput{}) || slices.Contains(probe.calls, "pending") {
					test.Fatalf("failure output=%+v calls=%v", output, probe.calls)
				}
				if scenario != "apply failure" && len(probe.mutations) != 0 {
					test.Fatal("rejected plan reached ApplyJoin")
				}
				return
			}
			plan, err := domain.PlanJoin(probe.graph, domain.JoinRequest{RepoID: input.RepoID, Branch: "main", BranchID: confirmation.BranchID, Source: input.Snapshot, IncludeDescendants: confirmation.IncludeDescendants})
			if err != nil {
				test.Fatal(err)
			}
			mutation, err := plan.Mutation(output.ForkBranch)
			if err != nil || !reflect.DeepEqual(probe.mutations, []domain.JoinMutation{mutation}) || output.Head != plan.NewHead || output.Branch != plan.Branch {
				test.Fatalf("output=%+v mutation=%+v error=%v", output, probe.mutations, err)
			}
			wantCalls := []string{"access", "snapshots", "refs", "pendings", "reflog"}
			if scenario == "partial" {
				wantCalls = append(wantCalls, "ref")
				if output.ForkBranch != domain.SessionRefPrefix("main")+strings.TrimPrefix(string(nodes["T"]), "sha256:")[:10] {
					test.Fatalf("residual ref=%q", output.ForkBranch)
				}
			}
			branchOwnerCalls(test, probe, append(wantCalls, "apply", "pending")...)
			for _, snapshot := range probe.graph.Snapshots {
				if snapshot.ID == input.Snapshot && !reflect.DeepEqual(snapshot.Parents, []domain.ContentHash{nodes["P"]}) {
					test.Fatal("join changed natural parents")
				}
			}
		})
	}
}

func TestContextBranchOwnerHistoryAndProtocolScopes(test *testing.T) {
	for _, events := range [][]domain.HistoryEvent{nil, {}, {{ID: "preserve", BranchID: "identity", Kind: "advance"}}} {
		probe := newBranchOwnerProbe(test)
		probe.history = events
		commands := branchOwnerCommands(test, probe.config(ContextBranchComplete))
		output, err := commands.ListHistory(context.Background(), probe.repo.ID)
		if err != nil || !reflect.DeepEqual(output, probe.history) || !reflect.DeepEqual(probe.scopes, []string{"history"}) {
			test.Fatalf("history=%+v scopes=%v error=%v", output, probe.scopes, err)
		}
		branchOwnerCalls(test, probe, "history")
	}
	for _, failure := range []string{"", "repo", "protocol", "scope:protocol"} {
		probe := newBranchOwnerProbe(test)
		if failure != "" {
			probe.failures[failure] = domain.ErrForbidden
		}
		err := branchOwnerCommands(test, probe.config(ContextBranchComplete)).EnableContextProtocol(context.Background(), probe.repo.ID)
		if (failure == "" && err != nil) || (failure != "" && !errors.Is(err, domain.ErrForbidden)) {
			test.Fatalf("protocol failure=%s error=%v", failure, err)
		}
		if !reflect.DeepEqual(probe.scopes, []string{"protocol"}) {
			test.Fatalf("protocol scopes=%v", probe.scopes)
		}
		switch failure {
		case "scope:protocol":
			branchOwnerCalls(test, probe)
		case "repo":
			branchOwnerCalls(test, probe, "repo")
		default:
			branchOwnerCalls(test, probe, "repo", "protocol")
		}
	}
}

func TestContextBranchOwnerCompleteClearsAllErrorOutputs(test *testing.T) {
	failure := errors.New("post-scope failure")
	for _, profile := range []ContextBranchProfile{ContextBranchComplete, ContextBranchCompatibility} {
		for _, operation := range []string{"fork", "preview", "join", "history", "history read", "pending"} {
			probe, input, _ := branchOwnerJoinFixture(test)
			probe.history = []domain.HistoryEvent{{ID: "diagnostic"}}
			commands := branchOwnerCommands(test, probe.config(profile))
			preview, err := commands.PreviewJoin(context.Background(), input)
			if err != nil {
				test.Fatal(err)
			}
			switch operation {
			case "pending":
				probe.failures["pending"] = failure
			case "history read":
				probe.failures["history"] = failure
			default:
				probe.afterErr = failure
			}
			var output, zero any
			switch operation {
			case "fork":
				output, err = commands.Fork(context.Background(), inbound.ForkInput{RepoID: input.RepoID, FromSnapshot: input.Snapshot, NewBranch: "topic"})
				zero = inbound.ForkOutput{}
			case "preview":
				output, err = commands.PreviewJoin(context.Background(), input)
				zero = inbound.JoinPreviewOutput{}
			case "join", "pending":
				output, err = commands.ConfirmJoin(context.Background(), confirmPreview(input, preview, true))
				zero = inbound.JoinOutput{}
			case "history", "history read":
				output, err = commands.ListHistory(context.Background(), input.RepoID)
				zero = []domain.HistoryEvent(nil)
			}
			if !errors.Is(err, failure) || reflect.DeepEqual(output, zero) != (profile == ContextBranchComplete) {
				test.Fatalf("profile=%s operation=%s output=%+v error=%v", profile, operation, output, err)
			}
		}
	}
}

func TestContextBranchOwnerCompatibilityDefersMissingPorts(test *testing.T) {
	for _, missing := range []string{"History", "Recorder", "Protocol"} {
		probe := newBranchOwnerProbe(test)
		config := probe.config(ContextBranchCompatibility)
		field := reflect.ValueOf(&config).Elem().FieldByName(missing)
		field.Set(reflect.Zero(field.Type()))
		commands := branchOwnerCommands(test, config)
		if len(probe.calls)+len(probe.scopes) != 0 {
			test.Fatal("missing optional port triggered constructor IO")
		}
		var err error
		want := "context history storage unavailable"
		switch missing {
		case "History":
			_, err = commands.ListHistory(context.Background(), probe.repo.ID)
		case "Recorder":
			probe.repo.ContextProtocol = 1
			_, err = commands.Fork(context.Background(), inbound.ForkInput{RepoID: probe.repo.ID, FromSnapshot: probe.graph.Snapshots[0].ID, NewBranch: "topic"})
		case "Protocol":
			err = commands.EnableContextProtocol(context.Background(), probe.repo.ID)
			want = "context protocol storage unavailable"
		}
		if err == nil || err.Error() != want {
			test.Fatalf("missing %s error=%v want=%q", missing, err, want)
		}
		if len(probe.births)+len(probe.mutations) != 0 || probe.swapped.Target != "" {
			test.Fatal("missing port reached mutation")
		}
	}
}

func TestContextBranchOwnerValidationBeforeMissingScope(test *testing.T) {
	probe := newBranchOwnerProbe(test)
	configured := branchOwnerCommands(test, probe.config(ContextBranchComplete))
	if output, err := configured.PreviewJoin(context.Background(), inbound.JoinPreviewInput{RepoID: probe.repo.ID, Snapshot: "invalid"}); !errors.Is(err, domain.ErrIntegrity) || !reflect.DeepEqual(output, inbound.JoinPreviewOutput{}) {
		test.Fatalf("invalid preview=%+v error=%v", output, err)
	}
	if len(probe.scopes)+len(probe.calls) != 0 {
		test.Fatal("invalid preview entered read scope")
	}
	if output, err := configured.ConfirmJoin(context.Background(), inbound.ConfirmJoinInput{}); !errors.Is(err, domain.ErrJoinPreviewChanged) || output != (inbound.JoinOutput{}) {
		test.Fatalf("missing approval=%+v error=%v", output, err)
	}
	probe.repo.ID = "invalid"
	if err := configured.EnableContextProtocol(context.Background(), probe.repo.ID); !errors.Is(err, domain.ErrIntegrity) {
		test.Fatalf("invalid protocol repository: %v", err)
	}
	branchOwnerCalls(test, probe)
	var commands ContextBranchOperations
	if output, err := commands.PreviewJoin(context.Background(), inbound.JoinPreviewInput{Snapshot: "invalid"}); !errors.Is(err, domain.ErrValidation) || !reflect.DeepEqual(output, inbound.JoinPreviewOutput{}) {
		test.Fatalf("invalid preview=%+v error=%v", output, err)
	}
	if output, err := commands.ConfirmJoin(context.Background(), inbound.ConfirmJoinInput{}); !errors.Is(err, domain.ErrValidation) || output != (inbound.JoinOutput{}) {
		test.Fatalf("unconfigured confirmation=%+v error=%v", output, err)
	}
}
