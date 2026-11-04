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

func TestRepositoryGraphReadOwnsMetadataAndCoherentEndpointExecution(test *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "app", "repositoryquery", "legacy_read.go"))
	if err != nil {
		test.Fatal(err)
	}
	file, err := parser.ParseFile(token.NewFileSet(), "legacy-read.go", raw, 0)
	if err != nil {
		test.Fatal(err)
	}
	reads := map[string]bool{"GetRepo": false, "ListRefs": false, "ListSnapshots": false, "ReadReflog": false, "ListHistoryEvents": false, "ListPendings": false, "ListUnsyncs": false, "ListSessionArchives": false, "RepositoryRevision": false}
	endpoints := map[string]bool{"GetRepositoryView": false, "GetPendingView": false, "QueryGraphState": false}
	for _, declaration := range file.Decls {
		method, ok := declaration.(*ast.FuncDecl)
		if !ok {
			continue
		}
		if method.Recv == nil && method.Name.Name == "LoadRepositoryView" {
			ast.Inspect(method.Body, func(node ast.Node) bool {
				if call, ok := node.(*ast.CallExpr); ok {
					if target, selected := call.Fun.(*ast.SelectorExpr); selected {
						if _, expected := reads[target.Sel.Name]; expected {
							reads[target.Sel.Name] = true
						}
					}
				}
				return true
			})
		}
		if method.Recv != nil && documentQueryReceiver(method.Recv.List[0].Type) == "LegacyGraphQueries" {
			if _, expected := endpoints[method.Name.Name]; expected {
				ast.Inspect(method.Body, func(node ast.Node) bool {
					if call, ok := node.(*ast.CallExpr); ok {
						if target, named := call.Fun.(*ast.Ident); named && target.Name == "readLegacyGraphResult" {
							endpoints[method.Name.Name] = true
						}
					}
					return true
				})
			}
		}
	}
	for operation, found := range reads {
		if !found {
			test.Error("actual repository metadata read is not query-owned", operation)
		}
	}
	for operation, found := range endpoints {
		if !found {
			test.Error("actual graph read bypasses its coherent query scope", operation)
		}
	}
	for _, name := range []string{"repository_view.go", "graph_state.go"} {
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
				test.Error("root facade still owns graph metadata assembly", name)
			}
			return true
		})
	}
	for _, name := range []string{"repository_view.go", "repository_changes.go"} {
		raw, err := os.ReadFile(filepath.Join("..", "adapters", "delivery", "http", name))
		if err != nil {
			test.Fatal(err)
		}
		for _, forbidden := range []string{"s.b.GetRepositoryView", "s.b.GetPendingView", "s.b.QueryGraphState"} {
			if strings.Contains(string(raw), forbidden) {
				test.Error("HTTP graph read still uses its broad backend", name, forbidden)
			}
		}
	}
}
