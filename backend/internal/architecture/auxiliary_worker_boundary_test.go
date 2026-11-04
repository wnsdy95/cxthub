package architecture

import (
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

func auxiliaryWorkerForbidden(module, dependency string) bool {
	return dependency == module+"/internal/app" || strings.Contains(dependency, "/internal/adapters/") ||
		withinPackage(dependency, module+"/internal/serverruntime") || withinPackage(dependency, module+"/internal/mcpserver") ||
		dependency == "database/sql" || dependency == "net/http" || withinPackage(dependency, "github.com/jackc/pgx/v5")
}

func TestAuxiliaryWorkerOwnershipBoundaries(test *testing.T) {
	module, err := readGoModule(filepath.Join("..", ".."))
	if err != nil {
		test.Fatal(err)
	}
	for _, owner := range []string{"notificationworker", "systemmaintenance"} {
		paths, err := filepath.Glob(filepath.Join(module.directory, "internal", "app", owner, "*.go"))
		if err != nil || len(paths) == 0 {
			test.Fatal("actual auxiliary worker package missing", owner, err)
		}
		for _, path := range paths {
			if strings.HasSuffix(path, "_test.go") {
				continue
			}
			file, err := parser.ParseFile(token.NewFileSet(), path, nil, 0)
			if err != nil {
				test.Fatal(err)
			}
			for _, imported := range file.Imports {
				dependency, err := strconv.Unquote(imported.Path.Value)
				if err != nil || auxiliaryWorkerForbidden(module.path, dependency) {
					test.Error("auxiliary worker imports infrastructure or command owner", owner, dependency, err)
				}
			}
			ast.Inspect(file, func(node ast.Node) bool {
				switch named := node.(type) {
				case *ast.Ident:
					if named.Name == "Service" || named.Name == "IdentityService" {
						test.Error("auxiliary worker retains broad service", owner, named.Name)
					}
				case *ast.SelectorExpr:
					if named.Sel.Name == "Service" || named.Sel.Name == "IdentityService" {
						test.Error("auxiliary worker retains qualified broad service", owner, named.Sel.Name)
					}
				}
				return true
			})
		}
	}
	for _, name := range []string{"notifications.go", "webhook.go", "auxiliary_workers.go"} {
		file, err := parser.ParseFile(token.NewFileSet(), filepath.Join(module.directory, "internal", "app", name), nil, 0)
		if err != nil {
			test.Fatal(err)
		}
		for _, imported := range file.Imports {
			dependency, _ := strconv.Unquote(imported.Path.Value)
			if dependency == "net/http" || strings.Contains(dependency, "/internal/adapters/") {
				test.Error("application still owns notification HTTP transport", name, dependency)
			}
		}
		if name != "auxiliary_workers.go" {
			continue
		}
		ast.Inspect(file, func(node ast.Node) bool {
			if _, started := node.(*ast.GoStmt); started {
				test.Error("auxiliary constructors start hidden workers")
			}
			if call, called := node.(*ast.CallExpr); called {
				if function, plain := call.Fun.(*ast.Ident); plain && (function.Name == "NewService" || function.Name == "NewIdentityService") {
					test.Error("auxiliary constructors reconstruct command services")
				}
			}
			return true
		})
	}
}

func TestAuxiliaryWorkerGuardRejectsReverseDependencies(test *testing.T) {
	const module = "github.com/wnsdy95/cxthub/backend"
	for _, dependency := range []string{module + "/internal/app", module + "/internal/adapters/store", module + "/internal/serverruntime", "net/http", "database/sql", "github.com/jackc/pgx/v5/pgxpool"} {
		if !auxiliaryWorkerForbidden(module, dependency) {
			test.Error("auxiliary guard missed a forbidden dependency", dependency)
		}
	}
	for _, dependency := range []string{"context", "time", "net/url", module + "/internal/domain", module + "/internal/ports/outbound"} {
		if auxiliaryWorkerForbidden(module, dependency) {
			test.Error("auxiliary guard rejects a required policy dependency", dependency)
		}
	}
}
