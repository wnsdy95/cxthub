package architecture

import (
	"go/ast"
	"strings"
)

func wave44PreparationNamedType(expression ast.Expr) string {
	return strings.TrimPrefix(wave42LegacyGitType(expression), "*")
}

func wave44PreparationMember(set wave42LegacyGitSourceSet, owner, name string, method bool, seen map[string]bool) string {
	if owner == "" || seen[owner] {
		return ""
	}
	seen[owner] = true
	if method && set.methods[owner+"."+name] != nil {
		return owner + "." + name
	}
	declaration := set.types[owner]
	if declaration == nil {
		return ""
	}
	shape, concrete := declaration.Type.(*ast.StructType)
	if !concrete {
		return wave44PreparationMember(set, wave44PreparationNamedType(declaration.Type), name, method, seen)
	}
	for _, field := range shape.Fields.List {
		for _, identifier := range field.Names {
			if !method && identifier.Name == name {
				return wave44PreparationNamedType(field.Type)
			}
		}
		if len(field.Names) == 0 {
			embedded := wave44PreparationNamedType(field.Type)
			if !method && embedded == name {
				return embedded
			}
			if selected := wave44PreparationMember(set, embedded, name, method, seen); selected != "" {
				return selected
			}
		}
	}
	return ""
}

func wave44PreparationExpressionType(set wave42LegacyGitSourceSet, expression ast.Expr, bindings map[string]string) string {
	switch value := expression.(type) {
	case *ast.Ident:
		if bound := bindings[value.Name]; bound != "" {
			return bound
		}
		if set.types[value.Name] != nil {
			return value.Name
		}
	case *ast.UnaryExpr:
		return wave44PreparationExpressionType(set, value.X, bindings)
	case *ast.StarExpr:
		return wave44PreparationExpressionType(set, value.X, bindings)
	case *ast.ParenExpr:
		return wave44PreparationExpressionType(set, value.X, bindings)
	case *ast.CompositeLit:
		return wave44PreparationNamedType(value.Type)
	case *ast.TypeAssertExpr:
		return wave44PreparationNamedType(value.Type)
	case *ast.SelectorExpr:
		owner := wave44PreparationExpressionType(set, value.X, bindings)
		return wave44PreparationMember(set, owner, value.Sel.Name, false, map[string]bool{})
	case *ast.CallExpr:
		key := wave44PreparationCall(set, value.Fun, bindings)
		if function := set.methods[key]; function != nil && function.Type.Results != nil && len(function.Type.Results.List) > 0 {
			return wave44PreparationNamedType(function.Type.Results.List[0].Type)
		}
		if len(value.Args) == 1 && wave42LegacyGitFunctionName(value.Fun) == "new" {
			return wave44PreparationNamedType(value.Args[0])
		}
	}
	return ""
}

func wave44PreparationCall(set wave42LegacyGitSourceSet, expression ast.Expr, bindings map[string]string) string {
	switch value := expression.(type) {
	case *ast.Ident:
		if bindings[value.Name] == "" && set.methods[value.Name] != nil {
			return value.Name
		}
	case *ast.SelectorExpr:
		owner := wave44PreparationExpressionType(set, value.X, bindings)
		return wave44PreparationMember(set, owner, value.Sel.Name, true, map[string]bool{})
	case *ast.IndexExpr:
		return wave44PreparationCall(set, value.X, bindings)
	case *ast.IndexListExpr:
		return wave44PreparationCall(set, value.X, bindings)
	case *ast.ParenExpr:
		return wave44PreparationCall(set, value.X, bindings)
	}
	return ""
}
