package remotecfg

import (
	"context"

	"github.com/wnsdy95/cxthub-cli/internal/domain"
	"testing"
)

func TestCaptureIdentityMalformedCannotSelectRoot(t *testing.T) {
	for _, value := range []any{map[string]any{"version": 1, "identity": "unknown"}, map[string]any{"version": 2, "identity": string(domain.DocumentIdentityRootV1)}, map[string]any{"version": 1, "identity": string(domain.DocumentIdentityRootV1), "repo_id": "invalid"}, "root"} {
		root := t.TempDir()
		ctx := context.Background()
		if err := setField(ctx, root, "capture_document", value); err != nil {
			t.Fatal(err)
		}
		if _, err := CaptureIdentity(ctx, root); err == nil {
			t.Fatalf("accepted malformed preference %#v", value)
		}
	}
}
