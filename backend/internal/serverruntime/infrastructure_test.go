package serverruntime

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/wnsdy95/cxthub/backend/internal/adapters/store"
	"github.com/wnsdy95/cxthub/backend/internal/app/documentquery"
)

func TestInfrastructureUsesExplicitConfigurationWithoutRuntimeDependencies(test *testing.T) {
	for name, value := range map[string]string{
		"CXT_AUTH":                            "invalid-auth-mode",
		"CXT_PUBLIC_URL":                      "not-an-origin",
		"CXT_POSTGRES_DSN":                    "invalid-dsn",
		"CXT_MIGRATIONS_DIR":                  "missing-migrations",
		"CXT_MEMORY_READ_REPOS":               "invalid-repository",
		"CXT_CONVERSATION_ROOT_PUBLICATION":   "true",
		"CXT_LEGACY_BYTE_RANGE_PUBLICATION":   "true",
		"CXT_LEGACY_DERIVED_BYTE_PUBLICATION": "true",
	} {
		test.Setenv(name, value)
	}
	infrastructure, err := OpenInfrastructure(context.Background(), InfrastructureConfig{
		DSN:                 " \t\n",
		DataDir:             test.TempDir(),
		MigrationsDirectory: filepath.Join(test.TempDir(), "missing"),
	})
	if err != nil {
		test.Fatal(err)
	}
	test.Cleanup(func() { _ = infrastructure.Close() })
	if _, filesystem := infrastructure.Store.(*store.FSStore); !filesystem {
		test.Fatalf("explicit development configuration selected %T", infrastructure.Store)
	}
	if infrastructure.Store != infrastructure.ownedStore || infrastructure.DocumentProfile != documentquery.LegacyCompatibility {
		test.Fatal("infrastructure changed store ownership or the development document profile")
	}
	if err := infrastructure.Close(); err != nil {
		test.Fatal(err)
	}
	if err := infrastructure.Close(); err != nil {
		test.Fatal(err)
	}
}

func TestInfrastructurePreservesLegacyPublicationErrors(test *testing.T) {
	for _, scenario := range []struct {
		name    string
		byte    bool
		derived bool
		want    string
	}{
		{name: "reader only"},
		{name: "byte requires support", byte: true, want: "legacy byte-range publication requires a supporting store"},
		{name: "derived requires byte", derived: true, want: "CXT_LEGACY_DERIVED_BYTE_PUBLICATION requires CXT_LEGACY_BYTE_RANGE_PUBLICATION"},
		{name: "derived requires support", byte: true, derived: true, want: "legacy derived byte-range publication requires a supporting store"},
	} {
		test.Run(scenario.name, func(test *testing.T) {
			infrastructure, err := OpenInfrastructure(context.Background(), InfrastructureConfig{
				DataDir:                      test.TempDir(),
				LegacyByteRangePublication:   scenario.byte,
				LegacyDerivedBytePublication: scenario.derived,
			})
			if infrastructure != nil {
				test.Cleanup(func() { _ = infrastructure.Close() })
			}
			if scenario.want == "" {
				if err != nil || infrastructure == nil {
					test.Fatal("reader-only infrastructure failed", infrastructure, err)
				}
				return
			}
			if err == nil || err.Error() != scenario.want || infrastructure != nil {
				test.Fatal("publication error changed", infrastructure, err)
			}
		})
	}
}

func TestInfrastructurePreservesStoreErrorBeforePublicationValidation(test *testing.T) {
	dataDir := filepath.Join(test.TempDir(), "not-a-directory")
	if err := os.WriteFile(dataDir, []byte("blocked storage"), 0o600); err != nil {
		test.Fatal(err)
	}
	infrastructure, err := OpenInfrastructure(context.Background(), InfrastructureConfig{
		DataDir:                      dataDir,
		LegacyDerivedBytePublication: true,
	})
	var pathError *os.PathError
	if infrastructure != nil || err == nil || !strings.HasPrefix(err.Error(), "open store: ") || !errors.As(err, &pathError) {
		test.Fatal("store error was lost or reordered", infrastructure, err)
	}
}

func TestInfrastructureRejectsRequiredPostgresWithoutDSN(test *testing.T) {
	dataDir := filepath.Join(test.TempDir(), "must-not-be-created")
	infrastructure, err := OpenInfrastructure(context.Background(), InfrastructureConfig{
		DSN:                          " \t\n",
		DataDir:                      dataDir,
		RequirePostgres:              true,
		LegacyDerivedBytePublication: true,
	})
	if infrastructure != nil || err == nil || !strings.HasPrefix(err.Error(), "open store: ") || !strings.Contains(err.Error(), "PostgreSQL") {
		test.Fatal("required PostgreSQL fell back to development storage or reordered validation", infrastructure, err)
	}
	if _, err := os.Stat(dataDir); !errors.Is(err, os.ErrNotExist) {
		test.Fatal("missing PostgreSQL configuration touched storage", err)
	}
}

type infrastructureCloseStore struct {
	store.Store
	closeCalls int
}

func (storage *infrastructureCloseStore) Close() {
	storage.closeCalls++
}

type infrastructureErrorCloseStore struct {
	store.Store
	closeCalls int
	closeErr   error
}

func (storage *infrastructureErrorCloseStore) Close() error {
	storage.closeCalls++
	return storage.closeErr
}

func TestInfrastructureClosesOnlyOwnedStoreOnce(test *testing.T) {
	owned := &infrastructureCloseStore{}
	borrowed := &infrastructureCloseStore{}
	infrastructure := &Infrastructure{Store: borrowed, ownedStore: owned}
	for attempt := 0; attempt < 3; attempt++ {
		if err := infrastructure.Close(); err != nil {
			test.Fatal(err)
		}
	}
	if owned.closeCalls != 1 || borrowed.closeCalls != 0 {
		test.Fatal("close lost original ownership", owned.closeCalls, borrowed.closeCalls)
	}
}

func TestInfrastructureRetainsOwnedCloseError(test *testing.T) {
	want := errors.New("owned store close failed")
	owned := &infrastructureErrorCloseStore{closeErr: want}
	infrastructure := &Infrastructure{Store: owned, ownedStore: owned}
	for attempt := 0; attempt < 3; attempt++ {
		if err := infrastructure.Close(); !errors.Is(err, want) {
			test.Fatal("close error changed", err)
		}
	}
	if owned.closeCalls != 1 {
		test.Fatal("error close repeated", owned.closeCalls)
	}
}

func TestInfrastructureCloseWithoutOwnedStore(test *testing.T) {
	var absent *Infrastructure
	borrowed := &infrastructureCloseStore{}
	for _, infrastructure := range []*Infrastructure{absent, {}, {Store: borrowed}} {
		if err := infrastructure.Close(); err != nil {
			test.Fatal(err)
		}
	}
	if borrowed.closeCalls != 0 {
		test.Fatal("unowned store closed", borrowed.closeCalls)
	}
}

func TestRuntimeCloseUsesInfrastructureOwnerAndLegacyFallback(test *testing.T) {
	owned := &infrastructureCloseStore{}
	borrowed := &infrastructureCloseStore{}
	infrastructure := &Infrastructure{Store: owned, ownedStore: owned}
	runtime := &Runtime{Store: borrowed, infrastructure: infrastructure}
	runtime.Close()
	runtime.Close()
	if owned.closeCalls != 1 || borrowed.closeCalls != 0 {
		test.Fatal("runtime did not use its infrastructure owner", owned.closeCalls, borrowed.closeCalls)
	}
	legacyStore := &infrastructureCloseStore{}
	legacy := &Runtime{Store: legacyStore}
	legacy.Close()
	if legacyStore.closeCalls != 1 {
		test.Fatal("manually constructed runtime lost its legacy closer", legacyStore.closeCalls)
	}
	(&Runtime{}).Close()
}

func infrastructureRuntimeEnvironment(test *testing.T) {
	test.Helper()
	test.Setenv("CXT_POSTGRES_DSN", "")
	test.Setenv("CXT_AUTH", "dev")
	test.Setenv("CXT_FIREBASE_PROJECT", "")
	test.Setenv("CXT_PUBLIC_URL", "https://cxthub.example")
	test.Setenv("CXT_MEMORY_READ_REPOS", "")
	test.Setenv("CXT_CONVERSATION_ROOT_PUBLICATION", "false")
	test.Setenv("CXT_LEGACY_BYTE_RANGE_PUBLICATION", "false")
	test.Setenv("CXT_LEGACY_DERIVED_BYTE_PUBLICATION", "false")
}

func TestRuntimeCallersReuseInfrastructureStore(test *testing.T) {
	for name, open := range map[string]func(context.Context, string, string, bool) (*Runtime, error){"API": Open, "MCP": OpenMCP} {
		test.Run(name, func(test *testing.T) {
			infrastructureRuntimeEnvironment(test)
			runtime, err := open(context.Background(), "127.0.0.1:0", test.TempDir(), false)
			if err != nil {
				test.Fatal(err)
			}
			test.Cleanup(runtime.Close)
			if runtime.infrastructure == nil || runtime.Store != runtime.infrastructure.Store || runtime.Store != runtime.infrastructure.ownedStore {
				test.Fatal("runtime reopened or replaced its infrastructure store")
			}
			if runtime.DocumentQueryProfile != runtime.infrastructure.DocumentProfile || runtime.Identity == nil {
				test.Fatal("runtime lost its infrastructure profile or identity assembly")
			}
			if (runtime.Context != nil) != (name == "API") {
				test.Fatal("command service crossed the API/MCP boundary")
			}
		})
	}
}

func TestRuntimeValidationStillPrecedesInfrastructure(test *testing.T) {
	for name, open := range map[string]func(context.Context, string, string, bool) (*Runtime, error){"API": Open, "MCP": OpenMCP} {
		test.Run(name, func(test *testing.T) {
			for _, scenario := range []struct {
				name      string
				prepared  string
				auth      string
				publicURL string
				want      string
			}{
				{name: "prepared scope first", prepared: "invalid", auth: "invalid", publicURL: "invalid", want: "CXT_MEMORY_READ_REPOS requires comma-separated repository hashes"},
				{name: "auth before public origin", auth: "invalid", publicURL: "invalid", want: "CXT_AUTH must be firebase or dev"},
				{name: "firebase before public origin", auth: "firebase", publicURL: "invalid", want: "CXT_FIREBASE_PROJECT is required for Firebase authentication"},
				{name: "public origin before store", auth: "dev", publicURL: "invalid", want: "CXT_PUBLIC_URL must be an absolute origin without credentials, path, query or fragment"},
			} {
				test.Run(scenario.name, func(test *testing.T) {
					infrastructureRuntimeEnvironment(test)
					test.Setenv("CXT_MEMORY_READ_REPOS", scenario.prepared)
					test.Setenv("CXT_AUTH", scenario.auth)
					test.Setenv("CXT_PUBLIC_URL", scenario.publicURL)
					dataDir := filepath.Join(test.TempDir(), "must-not-be-created")
					runtime, err := open(context.Background(), "127.0.0.1:0", dataDir, true)
					if runtime != nil {
						test.Cleanup(runtime.Close)
					}
					if runtime != nil || err == nil || err.Error() != scenario.want {
						test.Fatal("pre-storage error precedence changed", runtime, err)
					}
					if _, err := os.Stat(dataDir); !errors.Is(err, os.ErrNotExist) {
						test.Fatal("invalid runtime configuration touched storage", err)
					}
				})
			}
		})
	}
}
