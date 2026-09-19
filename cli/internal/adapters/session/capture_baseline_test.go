package session

import (
	"context"
	"github.com/wnsdy95/cxthub/cli/internal/adapters/providerfs"
	"github.com/wnsdy95/cxthub/cli/internal/ports/outbound"
	"os"
	"testing"
)

func TestMaterializersOwnCaptureBaseline(t *testing.T) {
	for _, makeWriter := range []func() outbound.SessionMaterializer{
		func() outbound.SessionMaterializer { return NewClaudeMaterializer() },
		func() outbound.SessionMaterializer { return NewCodexMaterializer() },
	} {
		writer := makeWriter()
		t.Run(string(writer.Provider()), func(t *testing.T) {
			t.Setenv("HOME", t.TempDir())
			root := t.TempDir()
			raw := []byte("{\"type\":\"user\",\"sessionId\":\"source\",\"message\":{\"role\":\"user\",\"content\":\"hello\"}}\n")
			path, _, err := writer.Materialize(context.Background(), raw, root)
			if err != nil {
				t.Fatal(err)
			}
			info, err := os.Stat(path)
			if err != nil {
				t.Fatal(err)
			}
			if !providerfs.CaptureExcluded(root, path, info.Size()) {
				t.Fatal("materialized replay can be recaptured before provider conversation grows")
			}
			if providerfs.CaptureExcluded(root, path, info.Size()+1) {
				t.Fatal("new provider conversation remains excluded")
			}
		})
	}
}
