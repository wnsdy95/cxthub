package cli

import (
	"encoding/json"
	"testing"

	"github.com/wnsdy95/cxthub-cli/internal/adapters/providerfs"
)

func writeCapturePushOrigin(t *testing.T, root, url string) {
	t.Helper()
	raw, err := json.Marshal(map[string]any{"remotes": map[string]string{"origin": url}})
	if err != nil {
		t.Fatal(err)
	}
	if err := providerfs.WriteRepoFileDurable(root, ".cxt/config", raw, 0600); err != nil {
		t.Fatal(err)
	}
}
