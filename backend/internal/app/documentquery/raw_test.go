package documentquery

import (
	"bytes"
	"context"
	"errors"
	"reflect"
	"testing"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

func rawOwnerFixture(test *testing.T, root bool) (*RawReader, *indexOwnerPorts) {
	test.Helper()
	_, ports := indexOwnerFixture(test)
	if root {
		manifest, bodies, err := domain.ConversationManifestForCIR(ports.document.CIR)
		if err != nil {
			test.Fatal(err)
		}
		hash, err := domain.ConversationManifestHash(manifest)
		if err != nil {
			test.Fatal(err)
		}
		var verifier domain.CanonicalDocVerifier
		proof, err := verifier.VerifyConversationManifestDoc(ports.ctx, hash, manifest, func(_ context.Context, chunk domain.ContentHash) ([]byte, error) {
			return bodies[chunk], nil
		})
		if err != nil {
			test.Fatal(err)
		}
		ports.hash = hash
		ports.document.Hash = hash
		ports.document.Identity = domain.DocumentIdentityRootV1
		ports.snapshot = domain.Snapshot{ID: hash, RepoID: ports.repo, DocHash: hash, DocIdentity: domain.DocumentIdentityRootV1}
		ports.verified = proof
	}
	reader, err := NewRawReader(RawConfig{Profile: LegacyCompatibility, Snapshots: ports, Legacy: ports, Index: ports, Verified: ports})
	if err != nil {
		test.Fatal(err)
	}
	return reader, ports
}

func TestRawMaterializationUsesCallerScopeAndExactIdentity(test *testing.T) {
	for _, root := range []bool{false, true} {
		test.Run(map[bool]string{false: "legacy", true: "root"}[root], func(test *testing.T) {
			reader, ports := rawOwnerFixture(test, root)
			document, err := reader.MaterializedDocumentRead(ports.ctx, ports.repo, ports.hash)
			wantCalls := []string{"metadata", "document"}
			if root {
				wantCalls[1] = "verified"
				if domain.HashContent(ports.verified.Bytes()) == ports.hash {
					test.Fatal("root fixture accidentally has legacy byte identity")
				}
			}
			actual, actualError := domain.CanonicalBytes(document.CIR)
			expected, expectedError := domain.CanonicalBytes(ports.document.CIR)
			if err != nil || actualError != nil || expectedError != nil || document.DocumentRef() != ports.document.DocumentRef() || !bytes.Equal(actual, expected) || !reflect.DeepEqual(ports.calls, wantCalls) {
				test.Fatalf("raw identity, bytes, or scope changed: %+v calls=%v error=%v", document, ports.calls, err)
			}
			if reader.source.scope != nil {
				test.Fatal("raw reader acquired its own authorization or admission scope")
			}
		})
	}
}

func TestRawMaterializationMissingLegacySnapshotAndIdentityFence(test *testing.T) {
	reader, ports := rawOwnerFixture(test, false)
	ports.failStage, ports.failure = "metadata", domain.ErrNotFound
	document, err := reader.MaterializedDocumentRead(ports.ctx, ports.repo, ports.hash)
	if err != nil || !reflect.DeepEqual(document, ports.document) {
		test.Fatal("missing legacy metadata changed compatibility", err)
	}
	ports.document.Identity = domain.DocumentIdentityRootV1
	if _, err := reader.MaterializedDocumentRead(ports.ctx, ports.repo, ports.hash); !errors.Is(err, domain.ErrIntegrity) {
		test.Fatal("root document was accepted as legacy", err)
	}
	ports.document.Identity = domain.DocumentIdentityLegacy
	ports.document.Hash = domain.HashContent([]byte("wrong document"))
	if _, err := reader.MaterializedDocumentRead(ports.ctx, ports.repo, ports.hash); !errors.Is(err, domain.ErrIntegrity) {
		test.Fatal("wrong legacy document was accepted", err)
	}
}

func TestRawMaterializationFailsClosedWithoutLegacyFallback(test *testing.T) {
	for _, stage := range []string{"metadata", "verified"} {
		for _, failure := range []error{domain.ErrIntegrity, domain.ErrForbidden, domain.ErrResourceOverloaded, context.Canceled, context.DeadlineExceeded} {
			test.Run(stage+"/"+failure.Error(), func(test *testing.T) {
				reader, ports := rawOwnerFixture(test, true)
				ports.failStage, ports.failure = stage, failure
				document, err := reader.MaterializedDocumentRead(ports.ctx, ports.repo, ports.hash)
				if !errors.Is(err, failure) || !reflect.DeepEqual(document, domain.SessionDoc{}) {
					test.Fatalf("raw failure changed: %+v %v", document, err)
				}
				for _, call := range ports.calls {
					if call == "document" || call == "index" {
						test.Fatal("root failure attempted legacy fallback", ports.calls)
					}
				}
			})
		}
	}
	reader, ports := rawOwnerFixture(test, true)
	ports.verified = domain.VerifiedSessionDoc{}
	if _, err := reader.MaterializedDocumentRead(ports.ctx, ports.repo, ports.hash); !errors.Is(err, domain.ErrIntegrity) {
		test.Fatal("unbound verified proof was accepted", err)
	}
	reader.source.verified = nil
	if _, err := reader.MaterializedDocumentRead(ports.ctx, ports.repo, ports.hash); !errors.Is(err, domain.ErrUnsupportedDocumentIdentity) {
		test.Fatal("verified capability was rediscovered from another port", err)
	}
}

func TestRawMaterializationCancellationBeforePorts(test *testing.T) {
	reader, ports := rawOwnerFixture(test, true)
	ctx, cancel := context.WithCancel(ports.ctx)
	cancel()
	if _, err := reader.MaterializedDocumentRead(ctx, ports.repo, ports.hash); !errors.Is(err, context.Canceled) || len(ports.calls) != 0 {
		test.Fatal("canceled raw read reached storage", err, ports.calls)
	}
}

func TestRawConstructorValidatesExplicitReadDependencies(test *testing.T) {
	for _, profile := range []Profile{LegacyCompatibility, VerifiedRoots} {
		for _, dependency := range []string{"snapshots", "legacy", "index", "verified"} {
			for _, typedNil := range []bool{false, true} {
				test.Run(string(rune('0'+profile))+"/"+dependency+map[bool]string{false: "/nil", true: "/typed-nil"}[typedNil], func(test *testing.T) {
					original := constructorPrefixConfig(test, profile)
					config := RawConfig{Profile: profile, Snapshots: original.Snapshots, Legacy: original.Legacy, Index: original.Index, Verified: original.Verified}
					var missing *constructorPrefixPorts
					switch dependency {
					case "snapshots":
						config.Snapshots = nil
						if typedNil {
							config.Snapshots = missing
						}
					case "legacy":
						config.Legacy = nil
						if typedNil {
							config.Legacy = missing
						}
					case "index":
						config.Index = nil
						if typedNil {
							config.Index = missing
						}
					case "verified":
						config.Verified = nil
						if typedNil {
							config.Verified = missing
						}
					}
					reader, err := NewRawReader(config)
					optional := dependency == "index" || dependency == "verified"
					if optional && !typedNil && profile == LegacyCompatibility {
						if err != nil || reader == nil {
							test.Fatal("explicit compatibility omission was rejected", err)
						}
						return
					}
					want := domain.ErrValidation
					if optional && !typedNil {
						want = domain.ErrUnsupportedDocumentIdentity
					}
					if reader != nil || !errors.Is(err, want) {
						test.Fatalf("invalid raw assembly accepted: %p %v", reader, err)
					}
				})
			}
		}
	}
	if reader, err := NewRawReader(RawConfig{}); reader != nil || !errors.Is(err, domain.ErrValidation) {
		test.Fatal("undeclared raw profile was accepted", err)
	}
}
