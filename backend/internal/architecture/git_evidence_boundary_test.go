package architecture

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func gitEvidenceShapeViolations(source string) ([]string, error) {
	file, err := parser.ParseFile(token.NewFileSet(), "git-evidence.go", source, 0)
	if err != nil {
		return nil, err
	}
	var violations []string
	ast.Inspect(file, func(node ast.Node) bool {
		declaration, ok := node.(*ast.TypeSpec)
		if !ok || declaration.Name.Name != "GitEvidenceConfig" && declaration.Name.Name != "GitEvidence" {
			return true
		}
		shape, ok := declaration.Type.(*ast.StructType)
		if !ok {
			violations = append(violations, "Git evidence owner must be concrete")
			return true
		}
		for _, field := range shape.Fields.List {
			if _, callback := field.Type.(*ast.FuncType); callback {
				violations = append(violations, "Git evidence retains a parent callback")
			}
			if declaration.Name.Name == "GitEvidence" {
				for _, name := range field.Names {
					if name.IsExported() {
						violations = append(violations, "Git evidence exposes mutable owner state")
					}
				}
				continue
			}
			dependency, named := field.Type.(*ast.Ident)
			allowed := named && (dependency.Name == "string" || dependency.Name == "bool" || dependency.Name == "GitTreeReader" || dependency.Name == "GitDeltaReader")
			if selected, scoped := field.Type.(*ast.SelectorExpr); scoped {
				owner, local := selected.X.(*ast.Ident)
				allowed = local && owner.Name == "domain" && selected.Sel.Name == "ContentHash"
			}
			if !allowed {
				violations = append(violations, "Git evidence config has an undeclared dependency")
			}
		}
		return true
	})
	return violations, nil
}

func TestGitEvidenceOwnsActualReaderAndMutableState(test *testing.T) {
	directory := filepath.Join("..", "app", "evidencequery")
	raw, err := os.ReadFile(filepath.Join(directory, "git_evidence.go"))
	if err != nil {
		test.Fatal(err)
	}
	violations, err := gitEvidenceShapeViolations(string(raw))
	if err != nil || len(violations) != 0 {
		test.Fatal(violations, err)
	}
	file, err := parser.ParseFile(token.NewFileSet(), "git-evidence.go", raw, 0)
	if err != nil {
		test.Fatal(err)
	}
	owned := map[string]bool{"GetGitCommitTree": false, "GetGitTreeNode": false, "GetGitDelta": false}
	for _, declaration := range file.Decls {
		method, ok := declaration.(*ast.FuncDecl)
		if !ok || method.Recv == nil || documentQueryReceiver(method.Recv.List[0].Type) != "GitEvidence" {
			continue
		}
		if _, relevant := owned[method.Name.Name]; !relevant {
			continue
		}
		ast.Inspect(method.Body, func(node ast.Node) bool {
			if call, ok := node.(*ast.CallExpr); ok {
				if operation, selected := call.Fun.(*ast.SelectorExpr); selected && operation.Sel.Name == method.Name.Name {
					owned[method.Name.Name] = true
				}
			}
			return true
		})
	}
	for method, present := range owned {
		if !present {
			test.Error("Git evidence has no actual reader implementation", method)
		}
	}
	adapter, err := os.ReadFile(filepath.Join("..", "app", "code_evidence.go"))
	if err != nil {
		test.Fatal(err)
	}
	root, err := parser.ParseFile(token.NewFileSet(), "code-evidence.go", adapter, 0)
	if err != nil {
		test.Fatal(err)
	}
	ast.Inspect(root, func(node ast.Node) bool {
		switch inspected := node.(type) {
		case *ast.MapType, *ast.ForStmt, *ast.RangeStmt:
			test.Error("root adapter still owns the Git cache or traversal algorithm")
		case *ast.TypeSpec:
			if inspected.Name.Name != "codeEvidence" {
				break
			}
			shape, ok := inspected.Type.(*ast.StructType)
			if !ok {
				test.Error("root evidence adapter is not concrete")
				break
			}
			for _, field := range shape.Fields.List {
				for _, name := range field.Names {
					switch name.Name {
					case "reads", "limited", "traversalLimited", "commits", "nodes", "changes", "closures":
						test.Error("root adapter duplicates mutable Git owner state", name.Name)
					}
				}
			}
		}
		return true
	})
}

func TestGitEvidenceShapeRejectsCallbacksAndMutableExposure(test *testing.T) {
	for _, fixture := range []struct{ source, want string }{
		{`type GitEvidenceConfig struct { reader func() error }`, "parent callback"},
		{`type GitEvidenceConfig struct { store MetadataStore }`, "undeclared dependency"},
		{`type GitEvidence struct { Reads int }`, "mutable owner state"},
	} {
		violations, err := gitEvidenceShapeViolations("package evidencequery\n" + fixture.source)
		if err != nil || !strings.Contains(fmt.Sprint(violations), fixture.want) {
			test.Fatal("Git evidence shape guard missed forbidden ownership", violations, err)
		}
	}
}
