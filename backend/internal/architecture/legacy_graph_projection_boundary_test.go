package architecture

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLegacyGraphProjectionOwnsActualAlgorithmAndSharedCache(test *testing.T) {
	directory := filepath.Join("..", "app", "repositoryquery")
	entries, err := os.ReadDir(directory)
	if err != nil {
		test.Fatal(err)
	}
	wanted := map[string]map[string]bool{
		"Project":          {"projectGraphBase": false, "ResolveBranchCodeFromIndex": false, "PrepareBranchContext": false, "ApplyGraphIntegrations": false},
		"projectGraphBase": {"ProjectGraphState": false},
		"Cached":           {"AcquireBuild": false, "Encode": false, "DecodeGraphProjection": false},
	}
	for _, entry := range entries {
		if !strings.HasSuffix(entry.Name(), ".go") || strings.HasSuffix(entry.Name(), "_test.go") {
			continue
		}
		raw, err := os.ReadFile(filepath.Join(directory, entry.Name()))
		if err != nil {
			test.Fatal(err)
		}
		file, err := parser.ParseFile(token.NewFileSet(), entry.Name(), raw, 0)
		if err != nil {
			test.Fatal(err)
		}
		for _, declaration := range file.Decls {
			method, ok := declaration.(*ast.FuncDecl)
			if !ok || method.Recv == nil && method.Name.Name != "projectGraphBase" || method.Recv != nil && documentQueryReceiver(method.Recv.List[0].Type) != "LegacyProjection" {
				continue
			}
			operations, relevant := wanted[method.Name.Name]
			if !relevant {
				continue
			}
			ast.Inspect(method.Body, func(node ast.Node) bool {
				if call, ok := node.(*ast.CallExpr); ok {
					switch target := call.Fun.(type) {
					case *ast.SelectorExpr:
						if _, expected := operations[target.Sel.Name]; expected {
							operations[target.Sel.Name] = true
						}
					case *ast.Ident:
						if _, expected := operations[target.Name]; expected {
							operations[target.Name] = true
						}
					}
				}
				return true
			})
		}
	}
	for method, operations := range wanted {
		for operation, present := range operations {
			if !present {
				test.Error("actual projection operation is not owned by the query", method, operation)
			}
		}
	}
	for _, name := range []string{"graph_projection_cache.go", "branch_projection_cache.go"} {
		raw, err := os.ReadFile(filepath.Join("..", "app", name))
		if err != nil {
			test.Fatal(err)
		}
		file, err := parser.ParseFile(token.NewFileSet(), name, raw, 0)
		if err != nil {
			test.Fatal(err)
		}
		ast.Inspect(file, func(node ast.Node) bool {
			switch node.(type) {
			case *ast.ForStmt, *ast.RangeStmt, *ast.MapType:
				test.Error("root still owns projection cache or traversal", name)
			}
			return true
		})
	}
	if _, err := os.Stat(filepath.Join("..", "app", "graph_projection_encoding.go")); !os.IsNotExist(err) {
		test.Fatal("production root still owns a duplicate graph codec", err)
	}
}
