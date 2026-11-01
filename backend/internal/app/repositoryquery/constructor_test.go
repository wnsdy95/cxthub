package repositoryquery

import (
	"errors"
	"fmt"
	"reflect"
	"testing"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

type constructorProbe struct {
	Scope
	Resources
	MetadataReader
	RevisionReader
	HistoryReader
	ArchiveReader
	PendingReader
	PendingStatusReader
	GraphStatusReader
}

func constructorConfig(profile Profile) Config {
	probe := &constructorProbe{}
	return Config{Profile: profile, Scope: probe, Resources: probe, Metadata: probe, Revisions: probe,
		History: probe, Archives: probe, Pending: probe, PendingState: probe, GraphState: probe}
}

func TestConstructorRejectsMissingAndTypedNilPortsBeforeAnyCalls(test *testing.T) {
	for _, profile := range []Profile{LegacyCompatibility, PinnedSnapshots} {
		for _, name := range []string{"Scope", "Resources", "Metadata", "Revisions", "History", "Archives", "Pending", "PendingState", "GraphState"} {
			for _, typedNil := range []bool{false, true} {
				test.Run(fmt.Sprintf("%d/%s/typed=%t", profile, name, typedNil), func(test *testing.T) {
					config := constructorConfig(profile)
					field := reflect.ValueOf(&config).Elem().FieldByName(name)
					if typedNil {
						var absent *constructorProbe
						field.Set(reflect.ValueOf(absent))
					} else {
						field.SetZero()
					}
					queries, err := New(config)
					required := typedNil || name == "Scope" || name == "Resources" || name == "Metadata" || profile == PinnedSnapshots && (name == "Revisions" || name == "History")
					if required && (queries != nil || err == nil) || !required && (queries == nil || err != nil) {
						test.Fatal("constructor silently discovered or discarded a dependency", queries, err)
					}
				})
			}
		}
	}
	for _, profile := range []Profile{0, 99} {
		if queries, err := New(constructorConfig(profile)); queries != nil || !errors.Is(err, domain.ErrValidation) {
			test.Fatal("missing/unknown profile accepted", queries, err)
		}
	}
}

func TestConstructorCopiesRolloutSelectionsButKeepsExplicitPorts(test *testing.T) {
	repo := domain.HashContent([]byte("constructor-repository"))
	config := constructorConfig(PinnedSnapshots)
	config.PendingRepos, config.GraphRepos = []domain.ContentHash{repo}, []domain.ContentHash{repo}
	queries, err := New(config)
	if err != nil || queries.scope != config.Scope || queries.resources != config.Resources || queries.metadata != config.Metadata || queries.revisions != config.Revisions || queries.history != config.History || queries.archives != config.Archives || queries.pending != config.Pending || queries.pendingState != config.PendingState || queries.graphState != config.GraphState {
		test.Fatal("explicit read ports were cloned or replaced", queries, err)
	}
	config.PendingRepos[0], config.GraphRepos[0] = "corrupt", "corrupt"
	if !queries.pendingRepos[repo] || !queries.graphRepos[repo] || queries.pendingRepos["corrupt"] || queries.graphRepos["corrupt"] || !queries.atomicRead {
		test.Fatal("constructor retained mutable rollout selections or disabled atomic result")
	}
	config.PendingRepos = []domain.ContentHash{"corrupt"}
	if queries, err := New(config); queries != nil || !errors.Is(err, domain.ErrIntegrity) {
		test.Fatal("malformed rollout repository accepted", queries, err)
	}
}

func TestConstructorRequiresEnabledProjectionCapabilities(test *testing.T) {
	repo := domain.HashContent([]byte("projection-repository"))
	for _, missing := range []string{"Pending", "PendingState", "GraphState"} {
		config := constructorConfig(PinnedSnapshots)
		config.PendingRepos, config.GraphRepos = []domain.ContentHash{repo}, []domain.ContentHash{repo}
		reflect.ValueOf(&config).Elem().FieldByName(missing).SetZero()
		if queries, err := New(config); queries != nil || !errors.Is(err, domain.ErrCatalogUnsupported) {
			test.Fatal("enabled projection is missing its read dependency", missing, queries, err)
		}
	}
	config := constructorConfig(LegacyCompatibility)
	config.PendingRepos, config.PendingState = []domain.ContentHash{repo}, nil
	if queries, err := New(config); queries == nil || err != nil {
		test.Fatal("explicit legacy body-only reader was rejected", queries, err)
	}
}
