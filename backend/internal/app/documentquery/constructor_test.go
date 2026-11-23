package documentquery

import (
	"context"
	"errors"
	"testing"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

var constructorPrefixUnexpectedCall = errors.New("document query constructor invoked a port")

type constructorPrefixCallLog struct {
	methods []string
}

type constructorPrefixPorts struct {
	name string
	log  *constructorPrefixCallLog
}

func (ports *constructorPrefixPorts) called(method string) error {
	if ports == nil {
		panic("document query constructor invoked a typed-nil port: " + method)
	}
	ports.log.methods = append(ports.log.methods, ports.name+"."+method)
	return constructorPrefixUnexpectedCall
}

func (ports *constructorPrefixPorts) AuthorizeDocumentRead(ctx context.Context, _ domain.ContentHash) (context.Context, error) {
	return ctx, ports.called("AuthorizeDocumentRead")
}

func (ports *constructorPrefixPorts) AuthorizeDocumentPreparation(ctx context.Context, _ domain.ContentHash) (context.Context, error) {
	return ctx, ports.called("AuthorizeDocumentPreparation")
}

func (ports *constructorPrefixPorts) WithinDocumentRead(context.Context, domain.ContentHash, func(context.Context) error) error {
	return ports.called("WithinDocumentRead")
}

func (ports *constructorPrefixPorts) WithinDocumentSnapshot(context.Context, domain.ContentHash, func(context.Context) error) error {
	return ports.called("WithinDocumentSnapshot")
}

func (ports *constructorPrefixPorts) GetSnapshot(context.Context, domain.ContentHash, domain.ContentHash) (domain.Snapshot, error) {
	return domain.Snapshot{}, ports.called("GetSnapshot")
}

func (ports *constructorPrefixPorts) GetDoc(context.Context, domain.ContentHash, domain.ContentHash) (domain.SessionDoc, error) {
	return domain.SessionDoc{}, ports.called("GetDoc")
}

func (ports *constructorPrefixPorts) GetDocManifest(context.Context, domain.ContentHash, domain.ContentHash) (domain.DocChunkManifest, error) {
	return domain.DocChunkManifest{}, ports.called("GetDocManifest")
}

func (ports *constructorPrefixPorts) GetChunk(context.Context, domain.ContentHash, domain.ContentHash) ([]byte, error) {
	return nil, ports.called("GetChunk")
}

func (ports *constructorPrefixPorts) DocReadIndex(context.Context, domain.ContentHash, domain.ContentHash) (domain.DocReadIndex, error) {
	return domain.DocReadIndex{}, ports.called("DocReadIndex")
}

func (ports *constructorPrefixPorts) SearchDocEvents(context.Context, domain.ContentHash, domain.ContentHash, string, int, int) ([]domain.DocEventIndex, error) {
	return nil, ports.called("SearchDocEvents")
}

func (ports *constructorPrefixPorts) ReadVerifiedDoc(context.Context, domain.ContentHash, domain.ContentHash) (domain.VerifiedSessionDoc, error) {
	return domain.VerifiedSessionDoc{}, ports.called("ReadVerifiedDoc")
}

func (ports *constructorPrefixPorts) ReadVerifiedRootEvents(context.Context, domain.ContentHash, domain.ContentHash, domain.RootEventSelection) (domain.VerifiedRootEvents, error) {
	return domain.VerifiedRootEvents{}, ports.called("ReadVerifiedRootEvents")
}

func constructorPrefixConfig(test *testing.T, profile Profile) Config {
	test.Helper()
	log := &constructorPrefixCallLog{}
	test.Cleanup(func() {
		if len(log.methods) != 0 {
			test.Errorf("constructor called ports before startup completed: %v", log.methods)
		}
	})
	return Config{
		Profile:    profile,
		Scope:      &constructorPrefixPorts{name: "scope", log: log},
		Snapshots:  &constructorPrefixPorts{name: "snapshots", log: log},
		Legacy:     &constructorPrefixPorts{name: "legacy", log: log},
		Index:      &constructorPrefixPorts{name: "index", log: log},
		Verified:   &constructorPrefixPorts{name: "verified", log: log},
		RootEvents: &constructorPrefixPorts{name: "root-events", log: log},
	}
}

func constructorPrefixAssertRejected(test *testing.T, config Config, expected error) {
	test.Helper()
	queries, err := New(config)
	if queries != nil || !errors.Is(err, expected) {
		test.Fatalf("invalid constructor returned queries=%p error=%v; want nil queries and %v", queries, err, expected)
	}
}

func TestConstructorRejectsUndeclaredProfilesBeforePortCalls(test *testing.T) {
	for _, candidate := range []struct {
		name    string
		profile Profile
	}{
		{"zero", 0},
		{"outside-declared-profiles", VerifiedRoots + 1},
		{"maximum-profile-value", Profile(255)},
	} {
		test.Run(candidate.name, func(test *testing.T) {
			config := constructorPrefixConfig(test, candidate.profile)
			constructorPrefixAssertRejected(test, config, domain.ErrValidation)
		})
	}
	test.Run("zero-config", func(test *testing.T) {
		constructorPrefixAssertRejected(test, Config{}, domain.ErrValidation)
	})
}

func TestConstructorRequiresExplicitRequiredPortsForBothProfiles(test *testing.T) {
	for _, candidate := range []struct {
		name    string
		profile Profile
	}{
		{"legacy", LegacyCompatibility},
		{"verified-roots", VerifiedRoots},
	} {
		test.Run(candidate.name, func(test *testing.T) {
			for _, missing := range []string{"scope", "snapshots", "legacy"} {
				test.Run(missing, func(test *testing.T) {
					for _, shape := range []string{"nil", "typed-nil"} {
						test.Run(shape, func(test *testing.T) {
							config := constructorPrefixConfig(test, candidate.profile)
							var absent *constructorPrefixPorts
							switch missing {
							case "scope":
								config.Scope = nil
								if shape == "typed-nil" {
									config.Scope = absent
								}
							case "snapshots":
								config.Snapshots = nil
								if shape == "typed-nil" {
									config.Snapshots = absent
								}
							case "legacy":
								config.Legacy = nil
								if shape == "typed-nil" {
									config.Legacy = absent
								}
							}
							constructorPrefixAssertRejected(test, config, domain.ErrValidation)
						})
					}
				})
			}
		})
	}
}

func TestConstructorRejectsTypedNilOptionalPortsForBothProfiles(test *testing.T) {
	for _, candidate := range []struct {
		name    string
		profile Profile
	}{
		{"legacy", LegacyCompatibility},
		{"verified-roots", VerifiedRoots},
	} {
		test.Run(candidate.name, func(test *testing.T) {
			for _, missing := range []string{"index", "verified", "root-events"} {
				test.Run(missing, func(test *testing.T) {
					for _, wiring := range []string{"otherwise-nil", "otherwise-populated"} {
						test.Run(wiring, func(test *testing.T) {
							config := constructorPrefixConfig(test, candidate.profile)
							if wiring == "otherwise-nil" {
								config.Index, config.Verified, config.RootEvents = nil, nil, nil
							}
							var absent *constructorPrefixPorts
							switch missing {
							case "index":
								config.Index = absent
							case "verified":
								config.Verified = absent
							case "root-events":
								config.RootEvents = absent
							}
							constructorPrefixAssertRejected(test, config, domain.ErrValidation)
						})
					}
				})
			}
		})
	}
}

func TestConstructorVerifiedRootsDoesNotInferOmittedPorts(test *testing.T) {
	for _, missing := range []string{"index", "verified", "root-events", "all"} {
		test.Run(missing, func(test *testing.T) {
			config := constructorPrefixConfig(test, VerifiedRoots)
			switch missing {
			case "index":
				config.Index = nil
			case "verified":
				config.Verified = nil
			case "root-events":
				config.RootEvents = nil
			case "all":
				config.Index, config.Verified, config.RootEvents = nil, nil, nil
			}
			constructorPrefixAssertRejected(test, config, domain.ErrUnsupportedDocumentIdentity)
		})
	}
}
