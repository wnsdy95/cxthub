package architecture

import (
	"go/ast"
)

type wave45PRCallVisitor struct {
	set       wave42LegacyGitSourceSet
	bindings  map[string]string
	functions map[string]string
	visit     func(string)
}

func (visitor wave45PRCallVisitor) call(expression ast.Expr) string {
	if identifier, named := expression.(*ast.Ident); named {
		if target := visitor.functions[identifier.Name]; target != "" {
			return target
		}
	}
	return wave44PreparationCall(visitor.set, expression, visitor.bindings)
}

func (visitor wave45PRCallVisitor) bind(name string, expression ast.Expr, kind string) {
	target := visitor.call(expression)
	if kind == "" {
		kind = wave44PreparationExpressionType(visitor.set, expression, visitor.bindings)
	}
	if kind == "" {
		kind = "?"
	}
	visitor.bindings[name] = kind
	delete(visitor.functions, name)
	if target != "" {
		visitor.functions[name] = target
	}
}

func (visitor wave45PRCallVisitor) Visit(node ast.Node) ast.Visitor {
	if node == nil {
		return nil
	}
	switch value := node.(type) {
	case *ast.BlockStmt, *ast.FuncLit:
		bindings, functions := map[string]string{}, map[string]string{}
		for name, kind := range visitor.bindings {
			bindings[name] = kind
		}
		for name, target := range visitor.functions {
			functions[name] = target
		}
		visitor.bindings, visitor.functions = bindings, functions
		if literal, ok := node.(*ast.FuncLit); ok {
			for _, fields := range []*ast.FieldList{literal.Type.Params, literal.Type.Results} {
				if fields != nil {
					for _, field := range fields.List {
						for _, name := range field.Names {
							visitor.bind(name.Name, nil, wave44PreparationNamedType(field.Type))
						}
					}
				}
			}
		}
	case *ast.AssignStmt:
		for index, left := range value.Lhs {
			if name, ok := left.(*ast.Ident); ok && index < len(value.Rhs) {
				visitor.bind(name.Name, value.Rhs[index], "")
			}
		}
	case *ast.ValueSpec:
		for index, name := range value.Names {
			var expression ast.Expr
			if index < len(value.Values) {
				expression = value.Values[index]
			}
			visitor.bind(name.Name, expression, wave44PreparationNamedType(value.Type))
		}
	case *ast.CallExpr:
		visitor.visit(visitor.call(value.Fun))
	}
	return visitor
}

func wave45PRReachable(set wave42LegacyGitSourceSet, entry string) map[string]*ast.FuncDecl {
	visited := map[string]*ast.FuncDecl{}
	var visit func(string)
	visit = func(key string) {
		function := set.methods[key]
		if function == nil || visited[key] != nil {
			return
		}
		visited[key] = function
		visitor := wave45PRCallVisitor{set: set, bindings: map[string]string{}, functions: map[string]string{}, visit: visit}
		for _, fields := range []*ast.FieldList{function.Recv, function.Type.Params, function.Type.Results} {
			if fields != nil {
				for _, field := range fields.List {
					for _, name := range field.Names {
						visitor.bind(name.Name, nil, wave44PreparationNamedType(field.Type))
					}
				}
			}
		}
		ast.Walk(visitor, function.Body)
	}
	visit(entry)
	return visited
}
