package domain

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"reflect"
	"strconv"
	"strings"
	"testing"
)

func memoryReadRequirementSourceFixture() RepositoryMemorySourceGeneration {
	return RepositoryMemorySourceGeneration{
		Version: RepositoryMemorySourceVersion,
		Graph: RepositoryViewSourceCheckpoint{
			Version: RepositoryViewSourceVersion, Scope: RepositoryGraphSourceScope,
			RepoID: HashContent([]byte("requirement repository")), Epoch: "12345678-1234-4321-9876-123456789abc", Sequence: 7,
		},
		Epoch: "23456789-1234-4321-9876-123456789abc", Sequence: 11,
	}
}

func memoryReadRequirementFixture(test *testing.T) (MemoryReadTarget, MemoryReadRequirement) {
	test.Helper()
	source := memoryReadRequirementSourceFixture()
	target := MemoryReadTarget{
		Version: MemoryReadWorkVersion, Rules: MemoryReadRules, RepoID: source.Graph.RepoID,
		Selection: EffectiveMemorySelection{SnapshotID: HashContent([]byte("snapshot")), CodeCommit: strings.Repeat("a", 40)},
	}
	identifier, err := MemoryReadTargetID(context.Background(), target)
	if err != nil {
		test.Fatal(err)
	}
	return target, MemoryReadRequirement{Version: MemoryReadRequirementVersion, TargetID: identifier, Minimum: source}
}

func memoryReadRequirementStatusFixture() MemoryReadStatus {
	source := memoryReadRequirementSourceFixture()
	return MemoryReadStatus{
		Version: MemoryReadVersion, Rules: MemoryReadRules, Generation: HashContent([]byte("read generation")),
		Minimum: source, Observed: source, Applied: source, Complete: true, Freshness: "current",
	}
}

func TestCheckMemorySourceMinimumPartialOrder(test *testing.T) {
	for _, scenario := range []struct {
		name         string
		appliedGraph int64
		applied      int64
		minimumGraph int64
		minimum      int64
		want         error
	}{
		{name: "zero"},
		{name: "equal", appliedGraph: 7, applied: 11, minimumGraph: 7, minimum: 11},
		{name: "graph ahead", appliedGraph: 8, applied: 11, minimumGraph: 7, minimum: 11},
		{name: "memory ahead", appliedGraph: 7, applied: 12, minimumGraph: 7, minimum: 11},
		{name: "both ahead", appliedGraph: 8, applied: 12, minimumGraph: 7, minimum: 11},
		{name: "graph behind", appliedGraph: 6, applied: 11, minimumGraph: 7, minimum: 11, want: ErrMemoryReadNotReady},
		{name: "memory behind", appliedGraph: 7, applied: 10, minimumGraph: 7, minimum: 11, want: ErrMemoryReadNotReady},
		{name: "both behind", appliedGraph: 6, applied: 10, minimumGraph: 7, minimum: 11, want: ErrMemoryReadNotReady},
		{name: "no max scalar memory ahead", appliedGraph: 6, applied: math.MaxInt64, minimumGraph: 7, minimum: 11, want: ErrMemoryReadNotReady},
		{name: "no max scalar graph ahead", appliedGraph: math.MaxInt64, applied: 10, minimumGraph: 7, minimum: 11, want: ErrMemoryReadNotReady},
		{name: "equal maximum", appliedGraph: math.MaxInt64, applied: math.MaxInt64, minimumGraph: math.MaxInt64, minimum: math.MaxInt64},
		{name: "large graph behind", appliedGraph: 1 << 53, applied: math.MaxInt64, minimumGraph: 1<<53 + 1, minimum: 11, want: ErrMemoryReadNotReady},
		{name: "large memory behind", appliedGraph: math.MaxInt64, applied: 1 << 53, minimumGraph: 7, minimum: 1<<53 + 1, want: ErrMemoryReadNotReady},
	} {
		test.Run(scenario.name, func(test *testing.T) {
			applied, minimum := memoryReadRequirementSourceFixture(), memoryReadRequirementSourceFixture()
			applied.Graph.Sequence, applied.Sequence = scenario.appliedGraph, scenario.applied
			minimum.Graph.Sequence, minimum.Sequence = scenario.minimumGraph, scenario.minimum
			beforeApplied, beforeMinimum := applied, minimum
			if err := CheckMemorySourceMinimum(applied, minimum); !errors.Is(err, scenario.want) {
				test.Fatalf("minimum comparison = %v; want %v", err, scenario.want)
			}
			if applied != beforeApplied || minimum != beforeMinimum {
				test.Fatal("minimum comparison changed its inputs")
			}
		})
	}
}

func TestCheckMemorySourceMinimumValidationAndIdentity(test *testing.T) {
	for _, scenario := range []struct {
		name   string
		mutate func(*RepositoryMemorySourceGeneration)
		want   error
	}{
		{name: "zero value", mutate: func(source *RepositoryMemorySourceGeneration) { *source = RepositoryMemorySourceGeneration{} }, want: ErrCatalogResetRequired},
		{name: "missing version", mutate: func(source *RepositoryMemorySourceGeneration) { source.Version = 0 }, want: ErrCatalogResetRequired},
		{name: "future version", mutate: func(source *RepositoryMemorySourceGeneration) { source.Version++ }, want: ErrCatalogResetRequired},
		{name: "missing graph version", mutate: func(source *RepositoryMemorySourceGeneration) { source.Graph.Version = 0 }, want: ErrValidation},
		{name: "future graph version", mutate: func(source *RepositoryMemorySourceGeneration) { source.Graph.Version++ }, want: ErrValidation},
		{name: "view scope", mutate: func(source *RepositoryMemorySourceGeneration) { source.Graph.Scope = RepositoryViewSourceScope }, want: ErrValidation},
		{name: "unknown scope", mutate: func(source *RepositoryMemorySourceGeneration) { source.Graph.Scope = "foreign" }, want: ErrValidation},
		{name: "missing repository", mutate: func(source *RepositoryMemorySourceGeneration) { source.Graph.RepoID = "" }, want: ErrValidation},
		{name: "invalid repository", mutate: func(source *RepositoryMemorySourceGeneration) { source.Graph.RepoID = "foreign" }, want: ErrValidation},
		{name: "foreign repository", mutate: func(source *RepositoryMemorySourceGeneration) { source.Graph.RepoID = HashContent([]byte("foreign")) }, want: ErrValidation},
		{name: "missing graph epoch", mutate: func(source *RepositoryMemorySourceGeneration) { source.Graph.Epoch = "" }, want: ErrValidation},
		{name: "invalid graph epoch", mutate: func(source *RepositoryMemorySourceGeneration) { source.Graph.Epoch = "foreign" }, want: ErrValidation},
		{name: "zero graph epoch", mutate: func(source *RepositoryMemorySourceGeneration) {
			source.Graph.Epoch = "00000000-0000-0000-0000-000000000000"
		}, want: ErrValidation},
		{name: "missing memory epoch", mutate: func(source *RepositoryMemorySourceGeneration) { source.Epoch = "" }, want: ErrValidation},
		{name: "invalid memory epoch", mutate: func(source *RepositoryMemorySourceGeneration) { source.Epoch = "foreign" }, want: ErrValidation},
		{name: "zero memory epoch", mutate: func(source *RepositoryMemorySourceGeneration) { source.Epoch = "00000000-0000-0000-0000-000000000000" }, want: ErrValidation},
		{name: "negative graph sequence", mutate: func(source *RepositoryMemorySourceGeneration) { source.Graph.Sequence = -1 }, want: ErrValidation},
		{name: "negative memory sequence", mutate: func(source *RepositoryMemorySourceGeneration) { source.Sequence = -1 }, want: ErrValidation},
		{name: "graph reset", mutate: func(source *RepositoryMemorySourceGeneration) { source.Graph.Epoch = source.Epoch }, want: ErrCatalogResetRequired},
		{name: "memory reset", mutate: func(source *RepositoryMemorySourceGeneration) { source.Epoch = source.Graph.Epoch }, want: ErrCatalogResetRequired},
		{name: "reset before not ready", mutate: func(source *RepositoryMemorySourceGeneration) {
			source.Epoch = source.Graph.Epoch
			source.Graph.Sequence, source.Sequence = 6, 12
		}, want: ErrCatalogResetRequired},
		{name: "foreign identity before reset", mutate: func(source *RepositoryMemorySourceGeneration) {
			source.Graph.RepoID = HashContent([]byte("foreign"))
			source.Epoch = source.Graph.Epoch
		}, want: ErrValidation},
	} {
		for _, operand := range []string{"applied", "minimum"} {
			test.Run(scenario.name+"/"+operand, func(test *testing.T) {
				applied, minimum := memoryReadRequirementSourceFixture(), memoryReadRequirementSourceFixture()
				if operand == "applied" {
					scenario.mutate(&applied)
				} else {
					scenario.mutate(&minimum)
				}
				if err := CheckMemorySourceMinimum(applied, minimum); !errors.Is(err, scenario.want) {
					test.Fatalf("minimum comparison = %v; want %v", err, scenario.want)
				}
			})
		}
	}
}

func TestMemoryReadRequirementValidate(test *testing.T) {
	target, valid := memoryReadRequirementFixture(test)
	if MemoryReadRequirementVersion != 1 {
		test.Fatal("unexpected requirement version")
	}
	if err := valid.Validate(target); err != nil {
		test.Fatal(err)
	}
	for _, scenario := range []struct {
		name   string
		mutate func(*MemoryReadRequirement, *MemoryReadTarget)
		want   error
	}{
		{name: "missing version", mutate: func(requirement *MemoryReadRequirement, target *MemoryReadTarget) { requirement.Version = 0 }, want: ErrValidation},
		{name: "future version", mutate: func(requirement *MemoryReadRequirement, target *MemoryReadTarget) { requirement.Version++ }, want: ErrValidation},
		{name: "missing target id", mutate: func(requirement *MemoryReadRequirement, target *MemoryReadTarget) { requirement.TargetID = "" }, want: ErrValidation},
		{name: "invalid target id", mutate: func(requirement *MemoryReadRequirement, target *MemoryReadTarget) { requirement.TargetID = "target" }, want: ErrValidation},
		{name: "foreign target id", mutate: func(requirement *MemoryReadRequirement, target *MemoryReadTarget) {
			requirement.TargetID = HashContent([]byte("other target"))
		}, want: ErrValidation},
		{name: "target version", mutate: func(requirement *MemoryReadRequirement, target *MemoryReadTarget) { target.Version++ }, want: ErrValidation},
		{name: "target rules", mutate: func(requirement *MemoryReadRequirement, target *MemoryReadTarget) { target.Rules++ }, want: ErrValidation},
		{name: "target repository", mutate: func(requirement *MemoryReadRequirement, target *MemoryReadTarget) {
			target.RepoID = HashContent([]byte("other repository"))
		}, want: ErrValidation},
		{name: "target snapshot", mutate: func(requirement *MemoryReadRequirement, target *MemoryReadTarget) {
			target.Selection.SnapshotID = HashContent([]byte("other snapshot"))
		}, want: ErrValidation},
		{name: "target commit", mutate: func(requirement *MemoryReadRequirement, target *MemoryReadTarget) {
			target.Selection.CodeCommit = strings.Repeat("b", 40)
		}, want: ErrValidation},
		{name: "target branch", mutate: func(requirement *MemoryReadRequirement, target *MemoryReadTarget) { target.Selection.Branch = "main" }, want: ErrValidation},
		{name: "target memory", mutate: func(requirement *MemoryReadRequirement, target *MemoryReadTarget) {
			target.Selection.MemoryHash = HashContent([]byte("memory"))
		}, want: ErrValidation},
		{name: "target content", mutate: func(requirement *MemoryReadRequirement, target *MemoryReadTarget) { target.Content = "claims" }, want: ErrValidation},
		{name: "invalid selection", mutate: func(requirement *MemoryReadRequirement, target *MemoryReadTarget) { target.Selection.CodeCommit = "" }, want: ErrValidation},
		{name: "minimum version", mutate: func(requirement *MemoryReadRequirement, target *MemoryReadTarget) { requirement.Minimum.Version++ }, want: ErrCatalogResetRequired},
		{name: "minimum graph version", mutate: func(requirement *MemoryReadRequirement, target *MemoryReadTarget) {
			requirement.Minimum.Graph.Version++
		}, want: ErrValidation},
		{name: "minimum scope", mutate: func(requirement *MemoryReadRequirement, target *MemoryReadTarget) {
			requirement.Minimum.Graph.Scope = RepositoryViewSourceScope
		}, want: ErrValidation},
		{name: "minimum repository", mutate: func(requirement *MemoryReadRequirement, target *MemoryReadTarget) {
			requirement.Minimum.Graph.RepoID = HashContent([]byte("other repository"))
		}, want: ErrValidation},
		{name: "minimum graph epoch", mutate: func(requirement *MemoryReadRequirement, target *MemoryReadTarget) {
			requirement.Minimum.Graph.Epoch = ""
		}, want: ErrValidation},
		{name: "minimum memory epoch", mutate: func(requirement *MemoryReadRequirement, target *MemoryReadTarget) { requirement.Minimum.Epoch = "" }, want: ErrValidation},
		{name: "minimum graph sequence", mutate: func(requirement *MemoryReadRequirement, target *MemoryReadTarget) {
			requirement.Minimum.Graph.Sequence = -1
		}, want: ErrValidation},
		{name: "minimum memory sequence", mutate: func(requirement *MemoryReadRequirement, target *MemoryReadTarget) { requirement.Minimum.Sequence = -1 }, want: ErrValidation},
	} {
		test.Run(scenario.name, func(test *testing.T) {
			requirement, selected := valid, target
			scenario.mutate(&requirement, &selected)
			if err := requirement.Validate(selected); !errors.Is(err, scenario.want) {
				test.Fatalf("requirement validation = %v; want %v", err, scenario.want)
			}
		})
	}
}

func TestMemoryReadStatusValidate(test *testing.T) {
	for _, scenario := range []struct {
		name   string
		mutate func(*MemoryReadStatus)
		want   error
	}{
		{name: "current at minimum", mutate: func(status *MemoryReadStatus) {}},
		{name: "current above minimum", mutate: func(status *MemoryReadStatus) {
			status.Applied.Graph.Sequence++
			status.Applied.Sequence++
			status.Observed = status.Applied
		}},
		{name: "graph catching up", mutate: func(status *MemoryReadStatus) { status.Observed.Graph.Sequence++; status.Freshness = "catching_up" }},
		{name: "memory catching up", mutate: func(status *MemoryReadStatus) { status.Observed.Sequence++; status.Freshness = "catching_up" }},
		{name: "both catching up", mutate: func(status *MemoryReadStatus) {
			status.Observed.Graph.Sequence++
			status.Observed.Sequence++
			status.Freshness = "catching_up"
		}},
		{name: "missing version", mutate: func(status *MemoryReadStatus) { status.Version = 0 }, want: ErrValidation},
		{name: "future version", mutate: func(status *MemoryReadStatus) { status.Version++ }, want: ErrValidation},
		{name: "missing rules", mutate: func(status *MemoryReadStatus) { status.Rules = 0 }, want: ErrValidation},
		{name: "future rules", mutate: func(status *MemoryReadStatus) { status.Rules++ }, want: ErrValidation},
		{name: "missing generation", mutate: func(status *MemoryReadStatus) { status.Generation = "" }, want: ErrValidation},
		{name: "invalid generation", mutate: func(status *MemoryReadStatus) { status.Generation = "generation" }, want: ErrValidation},
		{name: "uppercase generation", mutate: func(status *MemoryReadStatus) { status.Generation = ContentHash("sha256:" + strings.Repeat("A", 64)) }, want: ErrValidation},
		{name: "incomplete", mutate: func(status *MemoryReadStatus) { status.Complete = false }, want: ErrValidation},
		{name: "missing freshness", mutate: func(status *MemoryReadStatus) { status.Freshness = "" }, want: ErrValidation},
		{name: "unknown freshness", mutate: func(status *MemoryReadStatus) { status.Freshness = "stale" }, want: ErrValidation},
		{name: "uppercase freshness", mutate: func(status *MemoryReadStatus) { status.Freshness = "Current" }, want: ErrValidation},
		{name: "current with graph lag", mutate: func(status *MemoryReadStatus) { status.Observed.Graph.Sequence++ }, want: ErrValidation},
		{name: "current with memory lag", mutate: func(status *MemoryReadStatus) { status.Observed.Sequence++ }, want: ErrValidation},
		{name: "catching up without lag", mutate: func(status *MemoryReadStatus) { status.Freshness = "catching_up" }, want: ErrValidation},
		{name: "applied graph below minimum", mutate: func(status *MemoryReadStatus) { status.Applied.Graph.Sequence-- }, want: ErrMemoryReadNotReady},
		{name: "applied memory below minimum", mutate: func(status *MemoryReadStatus) { status.Applied.Sequence-- }, want: ErrMemoryReadNotReady},
		{name: "applied mixed graph behind", mutate: func(status *MemoryReadStatus) {
			status.Applied.Graph.Sequence--
			status.Applied.Sequence = math.MaxInt64
		}, want: ErrMemoryReadNotReady},
		{name: "applied mixed memory behind", mutate: func(status *MemoryReadStatus) {
			status.Applied.Graph.Sequence = math.MaxInt64
			status.Applied.Sequence--
		}, want: ErrMemoryReadNotReady},
		{name: "observed graph below applied", mutate: func(status *MemoryReadStatus) { status.Observed.Graph.Sequence-- }, want: ErrMemoryReadNotReady},
		{name: "observed memory below applied", mutate: func(status *MemoryReadStatus) { status.Observed.Sequence-- }, want: ErrMemoryReadNotReady},
		{name: "observed mixed graph behind", mutate: func(status *MemoryReadStatus) {
			status.Observed.Graph.Sequence--
			status.Observed.Sequence = math.MaxInt64
		}, want: ErrMemoryReadNotReady},
		{name: "observed mixed memory behind", mutate: func(status *MemoryReadStatus) {
			status.Observed.Graph.Sequence = math.MaxInt64
			status.Observed.Sequence--
		}, want: ErrMemoryReadNotReady},
	} {
		test.Run(scenario.name, func(test *testing.T) {
			status := memoryReadRequirementStatusFixture()
			scenario.mutate(&status)
			before := status
			if err := status.Validate(); !errors.Is(err, scenario.want) {
				test.Fatalf("status validation = %v; want %v", err, scenario.want)
			}
			if status != before {
				test.Fatal("status validation changed its input")
			}
		})
	}
}

func TestMemoryReadStatusValidatesEverySource(test *testing.T) {
	for _, scenario := range []struct {
		name   string
		mutate func(*RepositoryMemorySourceGeneration)
		want   error
	}{
		{name: "version", mutate: func(source *RepositoryMemorySourceGeneration) { source.Version++ }, want: ErrCatalogResetRequired},
		{name: "graph version", mutate: func(source *RepositoryMemorySourceGeneration) { source.Graph.Version++ }, want: ErrValidation},
		{name: "scope", mutate: func(source *RepositoryMemorySourceGeneration) { source.Graph.Scope = RepositoryViewSourceScope }, want: ErrValidation},
		{name: "foreign repository", mutate: func(source *RepositoryMemorySourceGeneration) { source.Graph.RepoID = HashContent([]byte("foreign")) }, want: ErrValidation},
		{name: "invalid repository", mutate: func(source *RepositoryMemorySourceGeneration) { source.Graph.RepoID = "" }, want: ErrValidation},
		{name: "invalid graph epoch", mutate: func(source *RepositoryMemorySourceGeneration) { source.Graph.Epoch = "" }, want: ErrValidation},
		{name: "invalid memory epoch", mutate: func(source *RepositoryMemorySourceGeneration) { source.Epoch = "" }, want: ErrValidation},
		{name: "negative graph sequence", mutate: func(source *RepositoryMemorySourceGeneration) { source.Graph.Sequence = -1 }, want: ErrValidation},
		{name: "negative memory sequence", mutate: func(source *RepositoryMemorySourceGeneration) { source.Sequence = -1 }, want: ErrValidation},
		{name: "graph reset", mutate: func(source *RepositoryMemorySourceGeneration) { source.Graph.Epoch = source.Epoch }, want: ErrCatalogResetRequired},
		{name: "memory reset", mutate: func(source *RepositoryMemorySourceGeneration) { source.Epoch = source.Graph.Epoch }, want: ErrCatalogResetRequired},
	} {
		for _, field := range []string{"minimum", "observed", "applied"} {
			test.Run(scenario.name+"/"+field, func(test *testing.T) {
				status := memoryReadRequirementStatusFixture()
				switch field {
				case "minimum":
					scenario.mutate(&status.Minimum)
				case "observed":
					scenario.mutate(&status.Observed)
				case "applied":
					scenario.mutate(&status.Applied)
				}
				if err := status.Validate(); !errors.Is(err, scenario.want) {
					test.Fatalf("status validation = %v; want %v", err, scenario.want)
				}
			})
		}
	}
}

func TestMemoryReadRequirementAndPageJSONSequenceStrings(test *testing.T) {
	target, requirement := memoryReadRequirementFixture(test)
	requirement.Minimum.Graph.Sequence, requirement.Minimum.Sequence = 1<<53+1, 1<<53+3
	status := memoryReadRequirementStatusFixture()
	status.Minimum = requirement.Minimum
	status.Applied = status.Minimum
	status.Applied.Graph.Sequence++
	status.Applied.Sequence++
	status.Observed = status.Applied
	status.Observed.Graph.Sequence++
	status.Observed.Sequence = math.MaxInt64
	status.Freshness = "catching_up"
	page := MemoryReadPage{
		Status: status,
		Page: EffectiveMemoryPage{
			Selection: target.Selection, Items: []EffectiveMemoryItem{}, Total: 0, NextCursor: "next",
		},
	}
	if err := requirement.Validate(target); err != nil {
		test.Fatal(err)
	}
	if err := status.Validate(); err != nil {
		test.Fatal(err)
	}
	encodedRequirement, err := json.Marshal(requirement)
	if err != nil {
		test.Fatal(err)
	}
	encodedPage, err := json.Marshal(page)
	if err != nil {
		test.Fatal(err)
	}
	var requirementWire map[string]json.RawMessage
	var pageWire map[string]json.RawMessage
	var statusWire map[string]json.RawMessage
	if err := json.Unmarshal(encodedRequirement, &requirementWire); err != nil {
		test.Fatal(err)
	}
	if err := json.Unmarshal(encodedPage, &pageWire); err != nil {
		test.Fatal(err)
	}
	if err := json.Unmarshal(pageWire["status"], &statusWire); err != nil {
		test.Fatal(err)
	}
	if len(requirementWire) != 3 || string(requirementWire["version"]) != "1" || requirementWire["target_id"] == nil ||
		len(pageWire) != 2 || pageWire["page"] == nil || len(statusWire) != 8 ||
		string(statusWire["schema_version"]) != "1" || string(statusWire["algorithm_version"]) != "1" ||
		statusWire["generation"] == nil || string(statusWire["complete"]) != "true" || string(statusWire["freshness"]) != `"catching_up"` {
		test.Fatalf("unexpected wire fields: %s %s", encodedRequirement, encodedPage)
	}
	for _, source := range []struct {
		encoded json.RawMessage
		want    RepositoryMemorySourceGeneration
	}{
		{encoded: requirementWire["minimum_epoch"], want: requirement.Minimum},
		{encoded: statusWire["minimum_epoch"], want: status.Minimum},
		{encoded: statusWire["source_epoch"], want: status.Observed},
		{encoded: statusWire["applied_epoch"], want: status.Applied},
	} {
		var wire struct {
			Sequence string `json:"sequence"`
			Graph    struct {
				Sequence string `json:"sequence"`
			} `json:"graph"`
		}
		if err := json.Unmarshal(source.encoded, &wire); err != nil {
			test.Fatal(err)
		}
		if wire.Sequence != strconv.FormatInt(source.want.Sequence, 10) || wire.Graph.Sequence != strconv.FormatInt(source.want.Graph.Sequence, 10) {
			test.Fatalf("sequence precision changed: %s", source.encoded)
		}
	}
	var decodedRequirement MemoryReadRequirement
	if err := json.Unmarshal(encodedRequirement, &decodedRequirement); err != nil || decodedRequirement != requirement {
		test.Fatalf("requirement round trip = %+v, %v", decodedRequirement, err)
	}
	var decodedPage MemoryReadPage
	if err := json.Unmarshal(encodedPage, &decodedPage); err != nil || !reflect.DeepEqual(decodedPage, page) {
		test.Fatalf("page round trip = %+v, %v", decodedPage, err)
	}
}
