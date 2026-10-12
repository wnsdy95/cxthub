package testutil

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

func RepositoryPath(t testing.TB, elements ...string) string {
	t.Helper()
	directory, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	root, err := repositoryRoot(directory)
	if err != nil {
		t.Fatal(err)
	}
	return filepath.Join(append([]string{root}, elements...)...)
}

func repositoryRoot(directory string) (string, error) {
	for {
		if _, err := os.Stat(filepath.Join(directory, "go.mod")); err == nil {
			candidates := []string{directory}
			if filepath.Base(directory) == "cli" {
				candidates = append(candidates, filepath.Dir(directory))
			}
			for _, candidate := range candidates {
				info, err := os.Stat(filepath.Join(candidate, "schemas"))
				if err == nil && info.IsDir() {
					return candidate, nil
				}
				if err != nil && !os.IsNotExist(err) {
					return "", err
				}
			}
			return "", fmt.Errorf("repository schemas missing for module %s", directory)
		} else if !os.IsNotExist(err) {
			return "", err
		}
		parent := filepath.Dir(directory)
		if parent == directory {
			return "", fmt.Errorf("cannot locate CLI module go.mod")
		}
		directory = parent
	}
}
