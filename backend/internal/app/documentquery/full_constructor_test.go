package documentquery

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

type constructorPrefixFullRangeReader struct {
	DocRangeReader
}

func (ports *constructorPrefixPorts) AcquireInteractive(context.Context) (context.Context, func(), error) {
	return nil, nil, ports.called("AcquireInteractive")
}

func (ports *constructorPrefixPorts) AcquireBulk(context.Context) (context.Context, func(), error) {
	return nil, nil, ports.called("AcquireBulk")
}

func (ports *constructorPrefixPorts) InReadOnlyTransaction(context.Context) bool {
	_ = ports.called("InReadOnlyTransaction")
	return false
}

func (ports *constructorPrefixPorts) DocRangeIndex(context.Context, domain.ContentHash, domain.ContentHash) (domain.DocRangeIndex, error) {
	return domain.DocRangeIndex{}, ports.called("DocRangeIndex")
}

func (ports *constructorPrefixPorts) DocRangePage(context.Context, domain.ContentHash, domain.DocRangeIndex, int) ([]domain.DocRangeEntry, error) {
	return nil, ports.called("DocRangePage")
}

func (ports *constructorPrefixPorts) EnsureDocRangeIndex(context.Context, domain.ContentHash, domain.ContentHash) error {
	return ports.called("EnsureDocRangeIndex")
}

func (ports *constructorPrefixPorts) OpenDocRange(context.Context, domain.ContentHash, domain.ContentHash) (outbound.DocRangeRead, error) {
	return nil, ports.called("OpenDocRange")
}

func (ports *constructorPrefixPorts) OpenDocRangeGeneration(context.Context, domain.ContentHash, domain.ContentHash) (outbound.DocRangeGenerationRead, error) {
	return nil, ports.called("OpenDocRangeGeneration")
}

func (ports *constructorPrefixPorts) HasDocRangeIndex(context.Context, domain.ContentHash, domain.ContentHash) (bool, error) {
	return false, ports.called("HasDocRangeIndex")
}

func (ports *constructorPrefixPorts) EnqueueDocRangeJob(context.Context, domain.ContentHash, domain.ContentHash, time.Time) (domain.DocRangeIndexJob, error) {
	return domain.DocRangeIndexJob{}, ports.called("EnqueueDocRangeJob")
}

func (ports *constructorPrefixPorts) OpenDocVisibleGeneration(context.Context, domain.ContentHash, domain.ContentHash) (outbound.DocVisibleGenerationRead, error) {
	return nil, ports.called("OpenDocVisibleGeneration")
}

func (ports *constructorPrefixPorts) HasDocVisibleIndex(context.Context, domain.ContentHash, domain.ContentHash) (bool, error) {
	return false, ports.called("HasDocVisibleIndex")
}

func (ports *constructorPrefixPorts) EnqueueDocVisibleIndex(context.Context, domain.ContentHash, domain.ContentHash, time.Time) (domain.DocRangeIndexJob, error) {
	return domain.DocRangeIndexJob{}, ports.called("EnqueueDocVisibleIndex")
}

func (ports *constructorPrefixPorts) DocRangeBoundary(context.Context, domain.ContentHash, domain.ContentHash, domain.ContentHash) (int, error) {
	return 0, ports.called("DocRangeBoundary")
}

func (ports *constructorPrefixPorts) EnqueueDocRangeBoundary(context.Context, domain.ContentHash, domain.ContentHash, domain.ContentHash, time.Time) (domain.DocRangeIndexJob, error) {
	return domain.DocRangeIndexJob{}, ports.called("EnqueueDocRangeBoundary")
}

func (ports *constructorPrefixPorts) ReadConversationChunk(context.Context, domain.ContentHash, domain.ContentHash, int64) ([]byte, error) {
	return nil, ports.called("ReadConversationChunk")
}

func (ports *constructorPrefixPorts) ReadLegacyDerivedByteChunk(context.Context, domain.ContentHash, domain.ContentHash, domain.DocReadGenerationBinding, int, domain.ConversationManifestChunk) ([]byte, error) {
	return nil, ports.called("ReadLegacyDerivedByteChunk")
}

func (ports *constructorPrefixPorts) LegacyByteRangePublicationEnabled() bool {
	_ = ports.called("LegacyByteRangePublicationEnabled")
	return false
}

func (ports *constructorPrefixPorts) HasDocByteRangeIndex(context.Context, domain.ContentHash, domain.ContentHash) (bool, error) {
	return false, ports.called("HasDocByteRangeIndex")
}

func (ports *constructorPrefixPorts) EnqueueDocByteRangeJob(context.Context, domain.ContentHash, domain.ContentHash, time.Time) (domain.DocRangeIndexJob, error) {
	return domain.DocRangeIndexJob{}, ports.called("EnqueueDocByteRangeJob")
}

func (ports *constructorPrefixPorts) ReadDocFindFields(context.Context, domain.ContentHash, domain.ContentHash, domain.ContentHash, int) (domain.DocFindFields, error) {
	return domain.DocFindFields{}, ports.called("ReadDocFindFields")
}

func (ports *constructorPrefixPorts) EnqueueDocFindFields(context.Context, domain.ContentHash, domain.ContentHash, time.Time) (domain.DocRangeIndexJob, error) {
	return domain.DocRangeIndexJob{}, ports.called("EnqueueDocFindFields")
}

func (ports *constructorPrefixPorts) ReadDocDisplayGeneration(context.Context, domain.DocDisplayTarget) (domain.DocDisplayStringGeneration, error) {
	return domain.DocDisplayStringGeneration{}, ports.called("ReadDocDisplayGeneration")
}

func (ports *constructorPrefixPorts) ReadDocDisplayPage(context.Context, domain.DocDisplayTarget, int) (domain.DocDisplayPage, error) {
	return domain.DocDisplayPage{}, ports.called("ReadDocDisplayPage")
}

func (ports *constructorPrefixPorts) EnqueueDocDisplay(context.Context, domain.DocDisplayTarget, time.Time) (domain.DocDisplayJob, error) {
	return domain.DocDisplayJob{}, ports.called("EnqueueDocDisplay")
}

func (ports *constructorPrefixPorts) ReadDocEventDisplayGeneration(context.Context, domain.DocDisplayTarget) (domain.DocDisplayProjectionGeneration, error) {
	return domain.DocDisplayProjectionGeneration{}, ports.called("ReadDocEventDisplayGeneration")
}

func (ports *constructorPrefixPorts) ReadDocEventDisplayPage(context.Context, domain.DocDisplayTarget, int) (domain.DocDisplayPage, error) {
	return domain.DocDisplayPage{}, ports.called("ReadDocEventDisplayPage")
}

func constructorPrefixFullConfig(test *testing.T, profile Profile) Config {
	test.Helper()
	config := constructorPrefixConfig(test, profile)
	log := config.Legacy.(*constructorPrefixPorts).log
	port := func(name string) *constructorPrefixPorts {
		return &constructorPrefixPorts{name: name, log: log}
	}
	config.View = &ViewDependencies{
		Range:              port("view-range"),
		RangeSnapshots:     port("view-range-snapshots"),
		RangeGenerations:   port("view-range-generations"),
		RangeReadiness:     port("view-range-readiness"),
		RangeJobs:          port("view-range-jobs"),
		VisibleGenerations: port("view-visible-generations"),
		VisiblePreparation: port("view-visible-preparation"),
		Boundaries:         port("view-boundaries"),
		Chunks:             port("view-chunks"),
		DerivedChunks:      port("view-derived-chunks"),
		Bytes:              port("view-bytes"),
		Resources:          port("view-resources"),
	}
	config.Find = &FindDependencies{
		Fields:       port("find-fields"),
		DisplayRead:  port("find-display-read"),
		Display:      port("find-display"),
		EventDisplay: port("find-event-display"),
	}
	if profile == LegacyCompatibility {
		config.View.LegacyPreparation = port("view-legacy-preparation")
	}
	return config
}

func constructorPrefixFullAssemble(test *testing.T, config Config) *Queries {
	test.Helper()
	queries, err := New(config)
	if err != nil || queries == nil {
		test.Fatalf("explicit full constructor failed: queries=%p error=%v", queries, err)
	}
	var expectedView ViewDependencies
	var expectedFind FindDependencies
	if config.View != nil {
		expectedView = *config.View
	}
	if config.Find != nil {
		expectedFind = *config.Find
	}
	if queries.view != expectedView || queries.find != expectedFind {
		test.Fatal("constructor inferred, dropped, copied ports, or rewired explicit view/find dependencies")
	}
	if queries.scope != config.Scope || queries.metadata != config.Snapshots || queries.legacy != config.Legacy ||
		queries.index != config.Index || queries.verified != config.Verified || queries.rootEvents != config.RootEvents || queries.atomicRead != config.AtomicRead {
		test.Fatal("full assembly changed an explicit core dependency or atomic-read policy")
	}
	return queries
}

func constructorPrefixFullClearField(test *testing.T, dependencies any, name string, typedNil bool) {
	test.Helper()
	field := reflect.ValueOf(dependencies).Elem().FieldByName(name)
	if !field.IsValid() || field.Kind() != reflect.Interface || !field.CanSet() {
		test.Fatalf("fixture does not name a configurable dependency: %T.%s", dependencies, name)
	}
	if typedNil {
		var missing *constructorPrefixPorts
		field.Set(reflect.ValueOf(missing))
	} else {
		field.SetZero()
	}
}

func TestFullConstructorCopiesDependencyValuesButSharesEveryExplicitPort(test *testing.T) {
	for _, candidate := range []struct {
		name    string
		profile Profile
	}{
		{"legacy", LegacyCompatibility},
		{"verified-roots", VerifiedRoots},
	} {
		test.Run(candidate.name, func(test *testing.T) {
			for _, policy := range []struct {
				name   string
				atomic bool
			}{
				{"non-atomic", false},
				{"atomic", true},
			} {
				test.Run(policy.name, func(test *testing.T) {
					config := constructorPrefixFullConfig(test, candidate.profile)
					config.AtomicRead = policy.atomic
					expectedView, expectedFind := *config.View, *config.Find
					queries := constructorPrefixFullAssemble(test, config)
					*config.View, *config.Find = ViewDependencies{}, FindDependencies{}
					if queries.view != expectedView || queries.find != expectedFind {
						test.Fatal("caller clearing dependency structs changed the owned dependencies")
					}
					replacement := constructorPrefixFullConfig(test, candidate.profile)
					*config.View, *config.Find = *replacement.View, *replacement.Find
					if queries.view != expectedView || queries.find != expectedFind {
						test.Fatal("caller replacing dependency fields rewired the owned ports")
					}
					config.View, config.Find = nil, nil
					if queries.view != expectedView || queries.find != expectedFind {
						test.Fatal("caller discarding configuration pointers changed constructed queries")
					}
				})
			}
		})
	}
}

func TestFullConstructorLegacyEventOnlyDoesNotDiscoverViewOrFind(test *testing.T) {
	config := constructorPrefixConfig(test, LegacyCompatibility)
	config.Index, config.Verified, config.RootEvents = nil, nil, nil
	constructorPrefixFullAssemble(test, config)
}

func TestFullConstructorExplicitMinimalViewAndFindAssembly(test *testing.T) {
	for _, candidate := range []struct {
		name    string
		profile Profile
	}{
		{"legacy", LegacyCompatibility},
		{"verified-roots", VerifiedRoots},
	} {
		test.Run(candidate.name, func(test *testing.T) {
			for _, shape := range []string{"view-only", "empty-find"} {
				test.Run(shape, func(test *testing.T) {
					config := constructorPrefixFullConfig(test, candidate.profile)
					if candidate.profile == LegacyCompatibility {
						config.View = &ViewDependencies{Resources: config.View.Resources}
					} else {
						config.View.RangeSnapshots, config.View.DerivedChunks, config.View.Bytes = nil, nil, nil
					}
					config.Find = nil
					if shape == "empty-find" {
						config.Find = &FindDependencies{}
					}
					constructorPrefixFullAssemble(test, config)
				})
			}
		})
	}
}

func TestFullConstructorViewNilAndTypedNilPerFieldBeforePortCalls(test *testing.T) {
	for _, candidate := range []struct {
		name    string
		profile Profile
	}{
		{"legacy", LegacyCompatibility},
		{"verified-roots", VerifiedRoots},
	} {
		test.Run(candidate.name, func(test *testing.T) {
			for _, dependency := range []struct {
				name             string
				verifiedRequired bool
			}{
				{"Range", true},
				{"LegacyPreparation", false},
				{"RangeSnapshots", false},
				{"RangeGenerations", true},
				{"RangeReadiness", true},
				{"RangeJobs", true},
				{"VisibleGenerations", true},
				{"VisiblePreparation", true},
				{"Boundaries", true},
				{"Chunks", true},
				{"DerivedChunks", false},
				{"Bytes", false},
				{"Resources", true},
			} {
				test.Run(dependency.name, func(test *testing.T) {
					for _, shape := range []struct {
						name     string
						typedNil bool
					}{
						{"nil", false},
						{"typed-nil", true},
					} {
						test.Run(shape.name, func(test *testing.T) {
							config := constructorPrefixFullConfig(test, candidate.profile)
							constructorPrefixFullClearField(test, config.View, dependency.name, shape.typedNil)
							if shape.typedNil || dependency.name == "Resources" {
								constructorPrefixAssertRejected(test, config, domain.ErrValidation)
							} else if candidate.profile == VerifiedRoots && dependency.verifiedRequired {
								constructorPrefixAssertRejected(test, config, domain.ErrUnsupportedDocumentIdentity)
							} else {
								constructorPrefixFullAssemble(test, config)
							}
						})
					}
				})
			}
		})
	}
}

func TestFullConstructorFindNilAndTypedNilPerFieldBeforePortCalls(test *testing.T) {
	for _, candidate := range []struct {
		name    string
		profile Profile
	}{
		{"legacy", LegacyCompatibility},
		{"verified-roots", VerifiedRoots},
	} {
		test.Run(candidate.name, func(test *testing.T) {
			for _, dependency := range []string{"Fields", "DisplayRead", "Display", "EventDisplay"} {
				test.Run(dependency, func(test *testing.T) {
					for _, shape := range []struct {
						name     string
						typedNil bool
					}{
						{"nil", false},
						{"typed-nil", true},
					} {
						test.Run(shape.name, func(test *testing.T) {
							config := constructorPrefixFullConfig(test, candidate.profile)
							constructorPrefixFullClearField(test, config.Find, dependency, shape.typedNil)
							if shape.typedNil {
								constructorPrefixAssertRejected(test, config, domain.ErrValidation)
							} else {
								constructorPrefixFullAssemble(test, config)
							}
						})
					}
				})
			}
		})
	}
}

func TestFullConstructorLegacyPreparationIsExplicitAndCompatibilityOnly(test *testing.T) {
	for _, candidate := range []struct {
		name    string
		profile Profile
	}{
		{"legacy", LegacyCompatibility},
		{"verified-roots", VerifiedRoots},
	} {
		test.Run(candidate.name, func(test *testing.T) {
			config := constructorPrefixFullConfig(test, candidate.profile)
			config.View.LegacyPreparation = config.Legacy.(*constructorPrefixPorts)
			if candidate.profile == VerifiedRoots {
				constructorPrefixAssertRejected(test, config, domain.ErrValidation)
			} else {
				constructorPrefixFullAssemble(test, config)
			}
		})
	}
}

func TestFullConstructorFindPresenceRequiresAnExplicitView(test *testing.T) {
	for _, candidate := range []struct {
		name    string
		profile Profile
	}{
		{"legacy", LegacyCompatibility},
		{"verified-roots", VerifiedRoots},
	} {
		test.Run(candidate.name, func(test *testing.T) {
			for _, shape := range []string{"full-find", "empty-find"} {
				test.Run(shape, func(test *testing.T) {
					config := constructorPrefixFullConfig(test, candidate.profile)
					config.View = nil
					if shape == "empty-find" {
						config.Find = &FindDependencies{}
					}
					constructorPrefixAssertRejected(test, config, domain.ErrValidation)
				})
			}
		})
	}
}

func TestFullConstructorAcceptsRangeReaderWithoutLegacyPreparationCapability(test *testing.T) {
	for _, candidate := range []struct {
		name    string
		profile Profile
	}{
		{"legacy", LegacyCompatibility},
		{"verified-roots", VerifiedRoots},
	} {
		test.Run(candidate.name, func(test *testing.T) {
			config := constructorPrefixFullConfig(test, candidate.profile)
			config.View.Range = constructorPrefixFullRangeReader{DocRangeReader: config.View.Range}
			config.View.LegacyPreparation = nil
			constructorPrefixFullAssemble(test, config)
		})
	}
}

func TestFullConstructorCoreOnlyViewsAndFindFailWithoutPanic(test *testing.T) {
	queries, err := New(constructorPrefixConfig(test, LegacyCompatibility))
	if err != nil {
		test.Fatal(err)
	}
	repo, hash := domain.HashContent([]byte("repo")), domain.HashContent([]byte("doc"))
	ctx := context.Background()
	operations := map[string]func() error{
		"view events":   func() error { _, err := queries.ReadDocViewEvents(ctx, repo, hash, "", 0, 1, "chat"); return err },
		"view fragment": func() error { _, err := queries.ReadDocViewFragment(ctx, repo, hash, 0, 0, 4); return err },
		"visible": func() error {
			_, err := queries.ReadDocVisiblePage(ctx, repo, hash, domain.DocVisiblePageRequest{View: "chat", Limit: 1})
			return err
		},
		"find": func() error {
			_, err := queries.FindDocPage(ctx, repo, hash, domain.DocFindPageRequest{Legacy: true, Total: 1, To: 1, Query: "needle"})
			return err
		},
		"find fragment": func() error {
			_, err := queries.ReadDocFindFragment(ctx, repo, hash, domain.DocFindFragmentRequest{Legacy: true, Total: 1, Budget: 4})
			return err
		},
	}
	for name, operation := range operations {
		test.Run(name, func(test *testing.T) {
			if err := operation(); !errors.Is(err, domain.ErrDocRangeIndexUnavailable) {
				test.Fatal("core-only query did not fail closed before touching view dependencies", err)
			}
		})
	}
}

func TestFullConstructorViewWithoutFindStillRequiresResources(test *testing.T) {
	for _, candidate := range []struct {
		name    string
		profile Profile
	}{
		{"legacy", LegacyCompatibility},
		{"verified-roots", VerifiedRoots},
	} {
		test.Run(candidate.name, func(test *testing.T) {
			for _, shape := range []struct {
				name     string
				typedNil bool
			}{
				{"nil", false},
				{"typed-nil", true},
			} {
				test.Run(shape.name, func(test *testing.T) {
					config := constructorPrefixFullConfig(test, candidate.profile)
					config.Find = nil
					constructorPrefixFullClearField(test, config.View, "Resources", shape.typedNil)
					constructorPrefixAssertRejected(test, config, domain.ErrValidation)
				})
			}
		})
	}
}
