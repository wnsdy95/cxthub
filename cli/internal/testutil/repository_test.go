package testutil

import (
	"os"
	"path/filepath"
	"testing"
)

func TestRepositoryRootLayouts(t *testing.T) {
	for _, layout := range []struct {
		name       string
		modulePath string
		assetPath  string
	}{
		{name: "monorepo", modulePath: "cli", assetPath: "."},
		{name: "exported", modulePath: "cxthub-cli", assetPath: "cxthub-cli"},
		{name: "exported-named-cli", modulePath: "cli", assetPath: "cli"},
	} {
		t.Run(layout.name, func(t *testing.T) {
			root := t.TempDir()
			module := filepath.Join(root, layout.modulePath)
			assets := filepath.Join(root, layout.assetPath)
			packageDirectory := filepath.Join(module, "internal", "domain")
			for _, directory := range []string{packageDirectory, filepath.Join(assets, "schemas"), filepath.Join(root, "schemas")} {
				if err := os.MkdirAll(directory, 0700); err != nil {
					t.Fatal(err)
				}
			}
			if err := os.WriteFile(filepath.Join(module, "go.mod"), []byte("module github.com/wnsdy95/cxthub-cli\n"), 0600); err != nil {
				t.Fatal(err)
			}
			got, err := repositoryRoot(packageDirectory)
			if err != nil || got != assets {
				t.Fatalf("repository root = %q, %v; want %q", got, err, assets)
			}
			t.Chdir(packageDirectory)
			if got := RepositoryPath(t, "schemas", "testdata", "fixture.json"); got != filepath.Join(assets, "schemas", "testdata", "fixture.json") {
				t.Fatalf("fixture path = %q", got)
			}
		})
	}
}

func TestRepositoryRootRejectsMissingAssets(t *testing.T) {
	for _, moduleName := range []string{"cli", "cxthub-cli"} {
		t.Run(moduleName, func(t *testing.T) {
			root := t.TempDir()
			module := filepath.Join(root, moduleName)
			if err := os.MkdirAll(module, 0700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(module, "go.mod"), nil, 0600); err != nil {
				t.Fatal(err)
			}
			if moduleName != "cli" {
				if err := os.Mkdir(filepath.Join(root, "schemas"), 0700); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := repositoryRoot(module); err == nil {
				t.Fatal("accepted module without repository schemas")
			}
		})
	}
}
