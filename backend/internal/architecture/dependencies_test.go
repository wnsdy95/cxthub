// Package architecture checks dependency direction in both Go modules.
package architecture

import (
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

func forbidden(layer, module, dependency string) bool {
	prefix := "github.com/wnsdy95/cxthub/" + module + "/internal/"
	if !strings.HasPrefix(dependency, prefix) {
		return strings.HasPrefix(dependency, "github.com/wnsdy95/cxthub/")
	}
	dest := strings.TrimPrefix(dependency, prefix)
	if layer == "domain" {
		return dest != "domain" && !strings.HasPrefix(dest, "domain/")
	}
	return dest != "domain" && !strings.HasPrefix(dest, "domain/") && !strings.HasPrefix(dest, "ports/") && !(layer == "app" && (dest == "app" || strings.HasPrefix(dest, "app/")))
}
func TestHexagonalDependencyDirection(t *testing.T) {
	root := filepath.Join("..", "..", "..")
	for _, module := range []string{"backend", "cli"} {
		for _, layer := range []string{"domain", "ports", "app"} {
			err := filepath.WalkDir(filepath.Join(root, module, "internal", layer), func(path string, entry fs.DirEntry, err error) error {
				if err != nil {
					return err
				}
				if entry.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
					return nil
				}
				file, err := parser.ParseFile(token.NewFileSet(), path, nil, parser.ImportsOnly)
				if err != nil {
					return err
				}
				for _, imp := range file.Imports {
					dependency, err := strconv.Unquote(imp.Path.Value)
					if err != nil {
						return err
					}
					if forbidden(layer, module, dependency) {
						t.Errorf("%s: %s layer must not import %s", path, layer, dependency)
					}
				}
				return nil
			})
			if err != nil {
				t.Fatal(err)
			}
		}
	}
}
func TestDependencyGuardDetectsForbiddenEdges(t *testing.T) {
	for _, row := range []struct {
		layer, dep string
		bad        bool
	}{
		{"domain", "backend/internal/adapters/store", true},
		{"domain", "backend/internal/app", true},
		{"ports", "backend/internal/app", true},
		{"app", "backend/internal/adapters/store", true},
		{"app", "cli/internal/domain", true},
		{"app", "backend/internal/domain", false},
		{"app", "backend/internal/ports/outbound", false},
	} {
		if forbidden(row.layer, "backend", "github.com/wnsdy95/cxthub/"+row.dep) != row.bad {
			t.Fatalf("missed %+v", row)
		}
	}
}
