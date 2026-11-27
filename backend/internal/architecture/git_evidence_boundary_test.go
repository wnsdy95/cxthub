package architecture

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
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
