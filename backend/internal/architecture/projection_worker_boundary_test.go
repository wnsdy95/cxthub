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

func projectionWorkerForbidden(module, dependency string) bool {
	return dependency == module+"/internal/app" || strings.Contains(dependency, "/internal/adapters/") ||
		withinPackage(dependency, module+"/internal/serverruntime") || withinPackage(dependency, module+"/internal/mcpserver") ||
		dependency == "database/sql" || dependency == "net/http" || withinPackage(dependency, "github.com/jackc/pgx/v5")
}

func TestProjectionWorkerOwnerBoundaries(test *testing.T) {
	module, err := readGoModule(filepath.Join("..", ".."))
	if err != nil {
		test.Fatal(err)
	}
	paths, err := filepath.Glob(filepath.Join(module.directory, "internal", "app", "projectionworker", "*.go"))
	if err != nil || len(paths) == 0 {
		test.Fatal("projection worker implementations are missing", err)
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
			if err != nil {
				test.Fatal(err)
			}
			if projectionWorkerForbidden(module.path, dependency) {
				test.Error("projection worker retains command/infrastructure dependency", filepath.Base(path), dependency)
			}
		}
		ast.Inspect(file, func(node ast.Node) bool {
			if pointer, ok := node.(*ast.StarExpr); ok {
				switch target := pointer.X.(type) {
				case *ast.Ident:
					if target.Name == "Service" || target.Name == "IdentityService" {
						test.Error("projection worker retains command service", filepath.Base(path))
					}
				case *ast.SelectorExpr:
					if target.Sel.Name == "Service" || target.Sel.Name == "IdentityService" {
						test.Error("projection worker retains qualified command service", filepath.Base(path))
					}
				}
			}
			return true
		})
	}
	for _, name := range []string{"projection_workers.go", "projection_worker_scope.go", "graph_projection_reference.go"} {
		file, err := parser.ParseFile(token.NewFileSet(), filepath.Join(module.directory, "internal", "app", name), nil, 0)
		if err != nil {
			test.Fatal(err)
		}
		ast.Inspect(file, func(node ast.Node) bool {
			switch value := node.(type) {
			case *ast.GoStmt:
				test.Error("projection assembly implicitly starts a goroutine", name)
			case *ast.CallExpr:
				if called, ok := value.Fun.(*ast.Ident); ok && (called.Name == "NewService" || called.Name == "NewIdentityService") {
					test.Error("projection assembly reconstructs a command service", name)
				}
				if called, ok := value.Fun.(*ast.SelectorExpr); ok && (called.Sel.Name == "NewService" || called.Sel.Name == "NewIdentityService") {
					test.Error("projection assembly reconstructs a qualified command service", name)
				}
			}
			return true
		})
	}
}

func TestProjectionWorkerGuardRejectsReverseDependencies(test *testing.T) {
	const module = "github.com/wnsdy95/cxthub/backend"
	for _, forbidden := range []string{module + "/internal/app", module + "/internal/adapters/store", module + "/internal/serverruntime", module + "/internal/mcpserver", "database/sql", "net/http", "github.com/jackc/pgx/v5/pgxpool"} {
		if !projectionWorkerForbidden(module, forbidden) {
			test.Error("worker guard accepts reverse dependency", forbidden)
		}
	}
	for _, allowed := range []string{"context", "sync/atomic", module + "/internal/domain", module + "/internal/ports/outbound", module + "/internal/app/repositoryquery"} {
		if projectionWorkerForbidden(module, allowed) {
			test.Error("worker guard rejects existing narrow policy contract", allowed)
		}
	}
}
