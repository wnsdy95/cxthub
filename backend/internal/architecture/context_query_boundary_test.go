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

func TestContextQueryOwnsScopedSelectionAndActualDelivery(test *testing.T) {
	owned, err := os.ReadFile(filepath.Join("..", "app", "repositoryquery", "context_read.go"))
	if err != nil {
		test.Fatal(err)
	}
	file, err := parser.ParseFile(token.NewFileSet(), "context-read.go", owned, 0)
	if err != nil {
		test.Fatal(err)
	}
	required := map[string]bool{"ReadContext": false, "LoadRepositoryView": false, "SelectContext": false,
		"NewInputs": false, "PrepareBranchContext": false, "VerifiedPR": false, "VerifiedGit": false, "Finish": false}
	ast.Inspect(file, func(node ast.Node) bool {
		call, ok := node.(*ast.CallExpr)
		if !ok {
			return true
		}
		var name string
		switch target := call.Fun.(type) {
		case *ast.Ident:
			name = target.Name
		case *ast.SelectorExpr:
			name = target.Sel.Name
		}
		if _, expected := required[name]; expected {
			required[name] = true
		}
		return true
	})
	for operation, found := range required {
		if !found {
			test.Error("actual context execution escaped its query owner", operation)
		}
	}
	root, err := os.ReadFile(filepath.Join("..", "app", "context_query.go"))
	if err != nil {
		test.Fatal(err)
	}
	rootFile, err := parser.ParseFile(token.NewFileSet(), "context-facade.go", root, 0)
	if err != nil {
		test.Fatal(err)
	}
	ast.Inspect(rootFile, func(node ast.Node) bool {
		switch node.(type) {
		case *ast.ForStmt, *ast.RangeStmt, *ast.TypeAssertExpr, *ast.MapType:
			test.Error("context facade retained selection, capability or cache logic")
		}
		return true
	})
	for _, name := range []string{"context_segment.go", "context_segment_cache.go"} {
		if _, err := os.Stat(filepath.Join("..", "app", name)); !os.IsNotExist(err) {
			test.Error("old context implementation remains beside the new owner", name, err)
		}
	}
	for _, path := range []string{
		filepath.Join("..", "adapters", "delivery", "http", "repository_view.go"),
		filepath.Join("..", "adapters", "delivery", "mcp", "pages.go"),
		filepath.Join("..", "adapters", "delivery", "mcp", "context_segment.go"),
	} {
		raw, err := os.ReadFile(path)
		if err != nil {
			test.Fatal(err)
		}
		for _, forbidden := range []string{"s.b.QueryContext", "s.context.QueryContext"} {
			if strings.Contains(string(raw), forbidden) {
				test.Error("context delivery rediscovered a broad backend", path, forbidden)
			}
		}
	}
}
