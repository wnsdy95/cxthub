// Package architecture checks dependency direction in both Go modules.
package architecture

import (
	"encoding/json"
	"fmt"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// Guard executable boundaries as well as domain dependency direction. A
// shared bootstrap must not silently pull MCP back into API or bring provider
// credentials and mutation handlers into the MCP process.
func TestIndependentServerDependencies(test *testing.T) {
	backend, err := readGoModule(filepath.Join("..", ".."))
	if err != nil {
		test.Fatal(err)
	}
	for command, forbidden := range map[string][]string{
		"cxtd":       {"/internal/mcpserver", "/internal/adapters/delivery/mcp"},
		"cxt-mcp":    {"/internal/adapters/delivery/http", "/internal/adapters/githubapp", "/internal/adapters/gitevidence", "/internal/adapters/resend"},
		"cxt-worker": {"/internal/mcpserver", "/internal/adapters/delivery/http", "/internal/adapters/delivery/mcp"},
	} {
		for _, variant := range []struct {
			name string
			tags string
		}{
			{name: "default"},
			{name: "postgres", tags: "postgres"},
		} {
			test.Run(command+"/"+variant.name, func(test *testing.T) {
				cmd := exec.Command("go", "list", "-tags="+variant.tags, "-f", `{{join .Deps "\n"}}`, "./cmd/"+command)
				cmd.Dir = backend.directory
				out, err := cmd.CombinedOutput()
				if err != nil {
					test.Fatalf("dependency inspection: %v\n%s", err, out)
				}
				for _, path := range forbidden {
					for _, dependency := range strings.Fields(string(out)) {
						if withinPackage(dependency, backend.path+path) {
							test.Errorf("%s imports forbidden server dependency %s", command, dependency)
						}
					}
				}
			})
		}
	}
}

type goModule struct {
	directory string
	path      string
}

func readGoModule(directory string) (goModule, error) {
	modFile, err := filepath.Abs(filepath.Join(directory, "go.mod"))
	if err != nil {
		return goModule{}, err
	}
	cmd := exec.Command("go", "mod", "edit", "-json", modFile)
	cmd.Dir = directory
	cmd.Env = append(os.Environ(), "GOWORK=off")
	out, err := cmd.CombinedOutput()
	if err != nil {
		return goModule{}, fmt.Errorf("read %s: %w\n%s", modFile, err, out)
	}
	var metadata struct {
		Module struct {
			Path string
		}
	}
	if err := json.Unmarshal(out, &metadata); err != nil {
		return goModule{}, fmt.Errorf("decode %s: %w", modFile, err)
	}
	if metadata.Module.Path == "" {
		return goModule{}, fmt.Errorf("%s: missing module identity", modFile)
	}
	return goModule{directory: directory, path: metadata.Module.Path}, nil
}

func withinPackage(dependency, prefix string) bool {
	return dependency == prefix || strings.HasPrefix(dependency, prefix+"/")
}

func forbidden(layer, module, dependency string, modules []goModule) bool {
	for _, other := range modules {
		if other.path != module && withinPackage(dependency, other.path) {
			return true
		}
	}
	if !withinPackage(dependency, module) {
		return false
	}
	prefix := module + "/internal/"
	if withinPackage(dependency, prefix+"domain") {
		return false
	}
	if (layer == "ports" || layer == "app") && withinPackage(dependency, prefix+"ports") {
		return false
	}
	return layer != "app" || !withinPackage(dependency, prefix+"app")
}

type dependencyViolation struct {
	file       string
	layer      string
	dependency string
}

func scanDependencies(root string) ([]dependencyViolation, error) {
	var modules []goModule
	for _, directory := range []string{"backend", "cli"} {
		module, err := readGoModule(filepath.Join(root, directory))
		if err != nil {
			return nil, err
		}
		for _, existing := range modules {
			if module.path == existing.path {
				return nil, fmt.Errorf("%s and %s share module identity %s", existing.directory, module.directory, module.path)
			}
		}
		modules = append(modules, module)
	}
	var violations []dependencyViolation
	for _, module := range modules {
		for _, layer := range []string{"domain", "ports", "app"} {
			layerDirectory := filepath.Join(module.directory, "internal", layer)
			err := filepath.WalkDir(layerDirectory, func(path string, entry fs.DirEntry, err error) error {
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
						return fmt.Errorf("%s: %w", path, err)
					}
					if forbidden(layer, module.path, dependency, modules) {
						violations = append(violations, dependencyViolation{file: path, layer: layer, dependency: dependency})
					}
				}
				return nil
			})
			if err != nil {
				return nil, fmt.Errorf("scan %s: %w", layerDirectory, err)
			}
		}
	}
	return violations, nil
}

func TestHexagonalDependencyDirection(test *testing.T) {
	violations, err := scanDependencies(filepath.Join("..", "..", ".."))
	if err != nil {
		test.Fatal(err)
	}
	for _, violation := range violations {
		test.Errorf("%s: %s layer must not import %s", violation.file, violation.layer, violation.dependency)
	}
}

func TestDependencyGuardDetectsForbiddenEdges(test *testing.T) {
	modules := []goModule{
		{path: "github.com/wnsdy95/cxthub/backend"},
		{path: "github.com/wnsdy95/cxthub-cli"},
	}
	for _, module := range modules {
		for _, layer := range []string{"domain", "ports", "app"} {
			test.Run(module.path+"/"+layer, func(test *testing.T) {
				for _, row := range []struct {
					path    string
					allowed bool
				}{
					{"", false},
					{"/internal", false},
					{"/internal/domain", true},
					{"/internal/domain/values", true},
					{"/internal/ports", layer != "domain"},
					{"/internal/ports/inbound", layer != "domain"},
					{"/internal/ports/outbound", layer != "domain"},
					{"/internal/app", layer == "app"},
					{"/internal/app/service", layer == "app"},
					{"/internal/adapters/store", false},
					{"/internal/platform", false},
					{"/pkg/shared", false},
					{"/internal/domainextra", false},
					{"/internal/portsextra/outbound", false},
					{"/internal/application", false},
					{"/internal-extra/domain", false},
				} {
					dependency := module.path + row.path
					if got := forbidden(layer, module.path, dependency, modules); got != !row.allowed {
						test.Errorf("forbidden(%q) = %v, want %v", dependency, got, !row.allowed)
					}
				}
				for _, other := range modules {
					if other.path == module.path {
						continue
					}
					for _, suffix := range []string{"", "/internal/domain", "/internal/domain/values", "/internal/ports/outbound", "/internal/app", "/internal/app/service", "/pkg/shared"} {
						dependency := other.path + suffix
						if !forbidden(layer, module.path, dependency, modules) {
							test.Errorf("allowed cross-module dependency %s", dependency)
						}
					}
				}
				for _, dependency := range []string{
					"context",
					"net/http",
					"example.com/external/internal/app",
					"github.com/wnsdy95/cxthub/backend-extra/internal/app",
					"github.com/wnsdy95/cxthub-cli-extra/internal/app",
				} {
					if forbidden(layer, module.path, dependency, modules) {
						test.Errorf("rejected external dependency %s", dependency)
					}
				}
			})
		}
	}
}

func TestPackagePrefixRequiresSegmentBoundary(test *testing.T) {
	for _, prefix := range []string{
		"github.com/wnsdy95/cxthub/backend",
		"github.com/wnsdy95/cxthub-cli",
		"example.com/backend/internal/mcpserver",
		"example.com/backend/internal/adapters/delivery/http",
	} {
		for _, suffix := range []string{"", "/nested", "extra", "-extra/nested"} {
			want := suffix == "" || strings.HasPrefix(suffix, "/")
			if got := withinPackage(prefix+suffix, prefix); got != want {
				test.Errorf("withinPackage(%q, %q) = %v, want %v", prefix+suffix, prefix, got, want)
			}
		}
	}
}

func writeDependencyFixture(test *testing.T, root, relativePath, content string) string {
	test.Helper()
	path := filepath.Join(root, filepath.FromSlash(relativePath))
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		test.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		test.Fatal(err)
	}
	return path
}

func dependencyFixture(test *testing.T, modulePaths []string) string {
	test.Helper()
	root := test.TempDir()
	for index, directory := range []string{"backend", "cli"} {
		writeDependencyFixture(test, root, directory+"/go.mod", fmt.Sprintf("module %q\n", modulePaths[index]))
		for _, layer := range []string{"domain", "ports", "app"} {
			writeDependencyFixture(test, root, directory+"/internal/"+layer+"/valid.go", "package "+layer+"\nimport _ \"context\"\n")
		}
	}
	return root
}

func TestDependencyScannerDetectsForbiddenSources(test *testing.T) {
	for _, row := range []struct {
		name        string
		modulePaths []string
	}{
		{"real_modules", []string{"github.com/wnsdy95/cxthub/backend", "github.com/wnsdy95/cxthub-cli"}},
		{"renamed_modules", []string{"example.com/server", "example.net/client"}},
	} {
		test.Run(row.name, func(test *testing.T) {
			root := dependencyFixture(test, row.modulePaths)
			for index, directory := range []string{"backend", "cli"} {
				for _, layer := range []string{"domain", "ports", "app"} {
					imports := []string{
						row.modulePaths[index] + "/internal/domain",
						row.modulePaths[index] + "-extra/internal/app",
						row.modulePaths[1-index] + "-extra/internal/app",
					}
					if layer != "domain" {
						imports = append(imports, row.modulePaths[index]+"/internal/ports", row.modulePaths[index]+"/internal/ports/outbound")
					}
					if layer == "app" {
						imports = append(imports, row.modulePaths[index]+"/internal/app/service")
					}
					source := "package " + layer + "\n"
					for _, dependency := range imports {
						source += fmt.Sprintf("import _ %q\n", dependency)
					}
					writeDependencyFixture(test, root, directory+"/internal/"+layer+"/nested/valid.go", source)
					writeDependencyFixture(test, root, directory+"/internal/"+layer+"/ignored_test.go", "not production Go source")
					writeDependencyFixture(test, root, directory+"/internal/"+layer+"/ignored.txt", "not Go source")
				}
			}
			if violations, err := scanDependencies(root); err != nil || len(violations) != 0 {
				test.Fatalf("valid fixture: violations=%+v, error=%v", violations, err)
			}
			want := make(map[dependencyViolation]bool)
			for index, directory := range []string{"backend", "cli"} {
				for _, layer := range []string{"domain", "ports", "app"} {
					for _, edge := range []struct {
						name       string
						dependency string
					}{
						{"adapter", row.modulePaths[index] + "/internal/adapters/store"},
						{"cross_domain", row.modulePaths[1-index] + "/internal/domain"},
						{"cross_app", row.modulePaths[1-index] + "/internal/app"},
						{"lookalike", row.modulePaths[index] + "/internal/domainextra"},
					} {
						source := fmt.Sprintf("//go:build architecture_fixture\n\npackage %s\nimport _ %q\n", layer, edge.dependency)
						path := writeDependencyFixture(test, root, directory+"/internal/"+layer+"/nested/"+edge.name+".go", source)
						want[dependencyViolation{file: path, layer: layer, dependency: edge.dependency}] = true
					}
				}
			}
			violations, err := scanDependencies(root)
			if err != nil {
				test.Fatal(err)
			}
			for _, violation := range violations {
				if !want[violation] {
					test.Errorf("unexpected or duplicate violation: %+v", violation)
				}
				delete(want, violation)
			}
			for violation := range want {
				test.Errorf("missed forbidden source: %+v", violation)
			}
		})
	}
}

func TestDependencyScannerRejectsInvalidInputs(test *testing.T) {
	for _, directory := range []string{"backend", "cli"} {
		for _, problem := range []string{"missing_module", "malformed_module", "missing_identity", "duplicate_identity", "missing_layer", "malformed_source", "unreadable_source"} {
			test.Run(directory+"/"+problem, func(test *testing.T) {
				root := dependencyFixture(test, []string{"example.com/server", "example.net/client"})
				relativePath := directory + "/go.mod"
				switch problem {
				case "missing_module":
					if err := os.Remove(filepath.Join(root, relativePath)); err != nil {
						test.Fatal(err)
					}
				case "malformed_module":
					writeDependencyFixture(test, root, relativePath, "module example.com/broken\ninvalid directive\n")
				case "missing_identity":
					writeDependencyFixture(test, root, relativePath, "go 1.26.0\n")
				case "duplicate_identity":
					writeDependencyFixture(test, root, "backend/go.mod", "module example.com/shared\n")
					writeDependencyFixture(test, root, "cli/go.mod", "module example.com/shared\n")
				case "missing_layer":
					relativePath = directory + "/internal/ports"
					if err := os.RemoveAll(filepath.Join(root, relativePath)); err != nil {
						test.Fatal(err)
					}
				case "malformed_source":
					relativePath = directory + "/internal/app/broken.go"
					writeDependencyFixture(test, root, relativePath, "package app\nimport (\n")
				case "unreadable_source":
					relativePath = directory + "/internal/domain/unreadable.go"
					if err := os.Symlink(filepath.Join(root, "absent.go"), filepath.Join(root, relativePath)); err != nil {
						test.Fatal(err)
					}
				}
				_, err := scanDependencies(root)
				if err == nil {
					test.Fatal("invalid fixture silently passed")
				}
				if !strings.Contains(err.Error(), filepath.Join(root, relativePath)) && problem != "duplicate_identity" {
					test.Errorf("error does not identify failing input %s: %v", relativePath, err)
				}
			})
		}
	}
}
