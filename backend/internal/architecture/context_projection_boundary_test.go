package architecture

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

func contextProjectionViolations(source, module string) ([]string, error) {
	file, err := parser.ParseFile(token.NewFileSet(), "context-projection.go", source, 0)
	if err != nil {
		return nil, err
	}
	var violations []string
	for _, imported := range file.Imports {
		path, err := strconv.Unquote(imported.Path.Value)
		if err != nil {
			return nil, err
		}
		if strings.Contains(path, ".") && path != module+"/internal/domain" {
			violations = append(violations, "context projection imports forbidden dependency "+path)
		}
	}
	ast.Inspect(file, func(node ast.Node) bool {
		switch inspected := node.(type) {
		case *ast.ChanType:
			violations = append(violations, "context projection owns admission")
		case *ast.StarExpr:
			if name, ok := inspected.X.(*ast.Ident); ok && name.Name == "Service" {
				violations = append(violations, "context projection retains Service")
			}
		case *ast.TypeAssertExpr:
			data, ok := inspected.Type.(*ast.SelectorExpr)
			if ok {
				owner, named := data.X.(*ast.Ident)
				if named && owner.Name == "domain" && data.Sel.Name == "ContentHash" {
					break
				}
			}
			violations = append(violations, "context projection discovers runtime capabilities")
		}
		return true
	})
	return violations, nil
}

func TestContextProjectionProductionBoundary(test *testing.T) {
	module, err := readGoModule(filepath.Join("..", ".."))
	if err != nil {
		test.Fatal(err)
	}
	directory := filepath.Join(module.directory, "internal", "app", "contextprojection")
	entries, err := os.ReadDir(directory)
	if err != nil {
		test.Fatal(err)
	}
	owned := map[string]bool{"PrepareBranchContext": false, "Finish": false, "ResolveBranchCodeFromIndex": false,
		"InheritedIntegrationScopes": false, "OrderedBranchSnapshots": false, "AppendContributionMerges": false, "AppendContributionOperations": false}
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".go") || strings.HasSuffix(entry.Name(), "_test.go") {
			continue
		}
		raw, err := os.ReadFile(filepath.Join(directory, entry.Name()))
		if err != nil {
			test.Fatal(err)
		}
		violations, err := contextProjectionViolations(string(raw), module.path)
		if err != nil || len(violations) != 0 {
			test.Fatal(entry.Name(), violations, err)
		}
		file, err := parser.ParseFile(token.NewFileSet(), entry.Name(), raw, 0)
		if err != nil {
			test.Fatal(err)
		}
		for _, declaration := range file.Decls {
			function, ok := declaration.(*ast.FuncDecl)
			if !ok || function.Body == nil {
				continue
			}
			if _, expected := owned[function.Name.Name]; expected {
				owned[function.Name.Name] = true
			}
		}
	}
	for function, present := range owned {
		if !present {
			test.Error("shared branch policy is not owned by context projection", function)
		}
	}
}

func TestContextProjectionBoundaryRejectsParentAndWriterDependencies(test *testing.T) {
	for _, fixture := range []struct{ code, want string }{
		{`import "github.com/wnsdy95/cxthub/backend/internal/app"`, "forbidden dependency"},
		{`import "github.com/wnsdy95/cxthub/backend/internal/ports/outbound"`, "forbidden dependency"},
		{`type Projection struct { parent *Service }`, "retains Service"},
		{`type Projection struct { gate chan struct{} }`, "owns admission"},
		{`func reader(value any) { _ = value.(interface{ Write() }) }`, "runtime capabilities"},
	} {
		violations, err := contextProjectionViolations("package contextprojection\n"+fixture.code, "github.com/wnsdy95/cxthub/backend")
		if err != nil || !strings.Contains(fmt.Sprint(violations), fixture.want) {
			test.Fatal("forbidden shared projection dependency escaped the guard", fixture.want, violations, err)
		}
	}
	violations, err := contextProjectionViolations("package contextprojection\nfunc value(raw any) { _ = raw.(domain.ContentHash) }", "github.com/wnsdy95/cxthub/backend")
	if err != nil || len(violations) != 0 {
		test.Fatal("heap data cast was mistaken for capability discovery", violations, err)
	}
}
