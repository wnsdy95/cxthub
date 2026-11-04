package serverruntime

import (
	"context"
	"strings"
	"testing"
)

func TestLegacyBytePublicationDefaultsToReaderOnlyAndRejectsUnsupportedStore(test *testing.T) {
	for _, value := range []string{"", "false", "true"} {
		test.Run("configured="+value, func(test *testing.T) {
			test.Setenv("CXT_AUTH", "dev")
			test.Setenv("CXT_POSTGRES_DSN", "")
			test.Setenv("CXT_MEMORY_READ_REPOS", "")
			test.Setenv("CXT_CONVERSATION_ROOT_PUBLICATION", "false")
			test.Setenv("CXT_LEGACY_BYTE_RANGE_PUBLICATION", value)
			test.Setenv("CXT_LEGACY_DERIVED_BYTE_PUBLICATION", "")
			test.Setenv("CXT_PUBLIC_URL", "http://127.0.0.1:18999")
			runtime, err := Open(context.Background(), "127.0.0.1:18999", test.TempDir(), false)
			if value == "true" {
				if err == nil || !strings.Contains(err.Error(), "legacy byte-range publication requires a supporting store") || runtime != nil {
					test.Fatal("unsupported publication silently enabled", runtime, err)
				}
				return
			}
			if err != nil {
				test.Fatal("reader-only default stopped the existing store", err)
			}
			runtime.Close()
		})
	}
}
