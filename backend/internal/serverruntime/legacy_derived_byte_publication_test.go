package serverruntime

import (
	"context"
	"strings"
	"testing"

	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

func TestLegacyDerivedBytePublicationRuntimeConfiguration(test *testing.T) {
	for _, scenario := range []struct {
		name               string
		bytePublication    string
		derivedPublication string
		wantError          string
	}{
		{name: "reader-only default"},
		{name: "reader-only explicitly disabled", bytePublication: "false", derivedPublication: "false"},
		{name: "requires enabled byte publication", derivedPublication: "true", wantError: "CXT_LEGACY_DERIVED_BYTE_PUBLICATION requires CXT_LEGACY_BYTE_RANGE_PUBLICATION"},
		{name: "rejects disabled byte publication", bytePublication: "false", derivedPublication: "true", wantError: "CXT_LEGACY_DERIVED_BYTE_PUBLICATION requires CXT_LEGACY_BYTE_RANGE_PUBLICATION"},
		{name: "rejects unsupported store", bytePublication: "true", derivedPublication: "true", wantError: "legacy derived byte-range publication requires a supporting store"},
		{name: "accepts boolean aliases before capability check", bytePublication: " ON ", derivedPublication: " YES ", wantError: "legacy derived byte-range publication requires a supporting store"},
	} {
		test.Run(scenario.name, func(test *testing.T) {
			test.Setenv("CXT_AUTH", "dev")
			test.Setenv("CXT_POSTGRES_DSN", "")
			test.Setenv("CXT_MEMORY_READ_REPOS", "")
			test.Setenv("CXT_CONVERSATION_ROOT_PUBLICATION", "false")
			test.Setenv("CXT_LEGACY_BYTE_RANGE_PUBLICATION", scenario.bytePublication)
			test.Setenv("CXT_LEGACY_DERIVED_BYTE_PUBLICATION", scenario.derivedPublication)
			test.Setenv("CXT_PUBLIC_URL", "http://127.0.0.1:18999")
			runtime, err := Open(context.Background(), "127.0.0.1:18999", test.TempDir(), false)
			if runtime != nil {
				test.Cleanup(runtime.Close)
			}
			if scenario.wantError != "" {
				if err == nil || !strings.Contains(err.Error(), scenario.wantError) || runtime != nil {
					test.Fatal("invalid derived publication configuration accepted", runtime, err)
				}
				return
			}
			if err != nil || runtime == nil || runtime.Context == nil || runtime.Identity == nil {
				test.Fatal("reader-only configuration stopped the existing runtime", runtime, err)
			}
			if _, supported := runtime.Store.(outbound.LegacyDerivedByteRangePublicationStore); supported {
				test.Fatal("filesystem store unexpectedly supports derived byte publication")
			}
		})
	}
}
