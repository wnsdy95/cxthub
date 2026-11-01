package architecture

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"testing"
)

func TestMCPCompositionDoesNotRediscoverCommandService(test *testing.T) {
	module, err := readGoModule(filepath.Join("..", ".."))
	if err != nil {
		test.Fatal(err)
	}
	for _, relative := range []string{"internal/app/read_services.go", "internal/mcpserver/server.go"} {
		path := filepath.Join(module.directory, relative)
		file, err := parser.ParseFile(token.NewFileSet(), path, nil, 0)
		if err != nil {
			test.Fatal(err)
		}
		ast.Inspect(file, func(node ast.Node) bool {
			switch value := node.(type) {
			case *ast.CallExpr:
				if name, ok := value.Fun.(*ast.Ident); ok && name.Name == "NewService" {
					test.Error(relative, "constructs the command service")
				}
				if name, ok := value.Fun.(*ast.SelectorExpr); ok && name.Sel.Name == "NewService" {
					test.Error(relative, "constructs a qualified command service")
				}
			case *ast.StarExpr:
				if name, ok := value.X.(*ast.Ident); ok && name.Name == "Service" {
					test.Error(relative, "retains the command service")
				}
				if name, ok := value.X.(*ast.SelectorExpr); ok && name.Sel.Name == "Service" {
					test.Error(relative, "retains a qualified command service")
				}
			}
			return true
		})
	}
	path := filepath.Join(module.directory, "cmd", "cxt-mcp", "main.go")
	raw, err := os.ReadFile(path)
	if err != nil {
		test.Fatal(err)
	}
	file, err := parser.ParseFile(token.NewFileSet(), path, raw, 0)
	if err != nil {
		test.Fatal(err)
	}
	minimal := false
	ast.Inspect(file, func(node ast.Node) bool {
		call, ok := node.(*ast.CallExpr)
		if !ok {
			return true
		}
		selector, ok := call.Fun.(*ast.SelectorExpr)
		if !ok {
			return true
		}
		owner, ok := selector.X.(*ast.Ident)
		if ok && owner.Name == "serverruntime" {
			if selector.Sel.Name == "Open" {
				test.Error("MCP starts the API command runtime")
			}
			minimal = minimal || selector.Sel.Name == "OpenMCP"
		}
		return true
	})
	if !minimal {
		test.Fatal("MCP does not start its minimal query runtime")
	}
}
