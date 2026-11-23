package architecture

import (
	"go/ast"
	"go/token"
	"path/filepath"
	"strings"
	"testing"
)

type rac1AssetCallVisitor struct {
	visitor wave45PRCallVisitor
	origins map[string]string
	visit   func(*ast.CallExpr, string)
}

func rac1AssetCallOrigin(walker rac1AssetCallVisitor, expression ast.Expr) string {
	switch value := expression.(type) {
	case *ast.Ident:
		if origin := walker.origins[value.Name]; origin != "" {
			return origin
		}
		if kind := walker.visitor.bindings[value.Name]; kind != "" && kind != "?" {
			return kind
		}
		return value.Name
	case *ast.SelectorExpr:
		return rac1AssetCallOrigin(walker, value.X) + "." + value.Sel.Name
	case *ast.ParenExpr:
		return rac1AssetCallOrigin(walker, value.X)
	case *ast.IndexExpr:
		return rac1AssetCallOrigin(walker, value.X)
	case *ast.IndexListExpr:
		return rac1AssetCallOrigin(walker, value.X)
	}
	return ""
}

func rac1AssetCallTarget(visitor wave45PRCallVisitor, expression ast.Expr) string {
	if target := visitor.call(expression); target != "" {
		return target
	}
	switch value := expression.(type) {
	case *ast.Ident:
		if visitor.bindings[value.Name] != "" {
			return ""
		}
	case *ast.SelectorExpr:
		if owner := wave44PreparationExpressionType(visitor.set, value.X, visitor.bindings); owner != "" && owner != "?" {
			return owner + "." + value.Sel.Name
		}
	case *ast.IndexExpr:
		return rac1AssetCallTarget(visitor, value.X)
	case *ast.IndexListExpr:
		return rac1AssetCallTarget(visitor, value.X)
	case *ast.ParenExpr:
		return rac1AssetCallTarget(visitor, value.X)
	}
	return workerSelectorPath(expression)
}

func (walker rac1AssetCallVisitor) Visit(node ast.Node) ast.Visitor {
	if node == nil {
		return nil
	}
	switch node.(type) {
	case *ast.BlockStmt, *ast.FuncLit:
		origins := map[string]string{}
		for name, origin := range walker.origins {
			origins[name] = origin
		}
		walker.origins = origins
	}
	walker.visitor = walker.visitor.Visit(node).(wave45PRCallVisitor)
	if literal, anonymous := node.(*ast.FuncLit); anonymous {
		for _, fields := range []*ast.FieldList{literal.Type.Params, literal.Type.Results} {
			if fields != nil {
				for _, field := range fields.List {
					for _, name := range field.Names {
						delete(walker.origins, name.Name)
					}
				}
			}
		}
	}
	bind := func(name string, expression ast.Expr) {
		walker.origins[name] = rac1AssetCallOrigin(walker, expression)
		if target := rac1AssetCallTarget(walker.visitor, expression); target != "" {
			walker.visitor.functions[name] = target
		}
	}
	switch value := node.(type) {
	case *ast.AssignStmt:
		for index, expression := range value.Lhs {
			if name, named := expression.(*ast.Ident); named && index < len(value.Rhs) {
				bind(name.Name, value.Rhs[index])
			}
		}
	case *ast.ValueSpec:
		for index, name := range value.Names {
			if index < len(value.Values) {
				bind(name.Name, value.Values[index])
			}
		}
	case *ast.CallExpr:
		target := rac1AssetCallTarget(walker.visitor, value.Fun)
		if walker.visitor.set.types["Server"] != nil && strings.HasPrefix(target, "inbound.RepositoryAssetCommands.") && !strings.HasPrefix(rac1AssetCallOrigin(walker, value.Fun), "Server.repositoryAssets.") {
			target = "unbound." + target
		}
		walker.visit(value, target)
	}
	return walker
}

func rac1AssetCalls(set wave42LegacyGitSourceSet, function *ast.FuncDecl, node ast.Node, visit func(*ast.CallExpr, string)) map[string]int {
	counts := map[string]int{}
	if function == nil || node == nil {
		return counts
	}
	visitor := wave45PRCallVisitor{set: set, bindings: map[string]string{}, functions: map[string]string{}, visit: func(string) {}}
	for _, fields := range []*ast.FieldList{function.Recv, function.Type.Params, function.Type.Results} {
		if fields != nil {
			for _, field := range fields.List {
				for _, name := range field.Names {
					visitor.bind(name.Name, nil, wave44PreparationNamedType(field.Type))
				}
			}
		}
	}
	ast.Walk(rac1AssetCallVisitor{visitor: visitor, origins: map[string]string{}, visit: func(call *ast.CallExpr, target string) {
		if node != function.Body && (call.Pos() < node.Pos() || call.End() > node.End()) {
			return
		}
		counts[target]++
		if visit != nil {
			visit(call, target)
		}
	}}, function.Body)
	return counts
}

func rac1AssetExecutionCalls(set wave42LegacyGitSourceSet, entry string, active map[string]bool) map[string]int {
	counts := map[string]int{}
	function := set.methods[entry]
	if function == nil || active[entry] {
		return counts
	}
	active[entry] = true
	defer delete(active, entry)
	for target, count := range rac1AssetCalls(set, function, function.Body, nil) {
		counts[target] += count
		for nested, occurrences := range rac1AssetExecutionCalls(set, target, active) {
			counts[nested] += count * occurrences
		}
	}
	return counts
}

func rac1AssetFields(set wave42LegacyGitSourceSet, owner string, allowed, required []string) []string {
	declaration := set.types[owner]
	if declaration == nil {
		return []string{"missing concrete asset dependency owner: " + owner}
	}
	shape, concrete := declaration.Type.(*ast.StructType)
	if !concrete || declaration.Assign.IsValid() {
		return []string{"asset owner is aliased or not concrete: " + owner}
	}
	var violations []string
	observed := map[string]bool{}
	for _, field := range shape.Fields.List {
		kind := wave42LegacyGitType(field.Type)
		accepted := false
		for _, candidate := range allowed {
			accepted = accepted || candidate == kind
		}
		if !accepted || len(field.Names) == 0 {
			violations = append(violations, "broad/service/cache/worker/callback asset dependency: "+owner+" "+kind)
		}
		observed[kind] = true
	}
	for _, kind := range required {
		if !observed[kind] {
			violations = append(violations, "missing retained asset dependency: "+owner+" "+kind)
		}
	}
	return violations
}

func rac1AssetRejects(block *ast.BlockStmt) bool {
	if block == nil || len(block.List) == 0 {
		return false
	}
	returned, returns := block.List[len(block.List)-1].(*ast.ReturnStmt)
	return returns && len(returned.Results) > 0 && workerSelectorPath(returned.Results[len(returned.Results)-1]) != "nil"
}

func rac1AssetAbsent(expression ast.Expr, argument string) bool {
	negated, negative := expression.(*ast.UnaryExpr)
	if !negative || negated.Op != token.NOT {
		return false
	}
	call, called := negated.X.(*ast.CallExpr)
	return called && wave42LegacyGitFunctionName(call.Fun) == "rootAdapterPresent" && len(call.Args) == 1 && workerSelectorPath(call.Args[0]) == argument
}

func rac1AssetInitializes(node ast.Node) bool {
	found := false
	ast.Inspect(node, func(nested ast.Node) bool {
		switch value := nested.(type) {
		case *ast.Ident:
			switch value.Name {
			case "NewRepositoryAssetCommands", "ConfigureRepositoryAssetCommands", "RepositoryAssets", "repositoryAssets", "RepositoryAssetCommands":
				found = true
			}
		case *ast.SelectorExpr:
			if value.Sel.Name == "RepositoryAssets" || value.Sel.Name == "RepositoryAssetCommands" || value.Sel.Name == "ConfigureRepositoryAssetCommands" || value.Sel.Name == "NewRepositoryAssetCommands" {
				found = true
			}
		}
		return true
	})
	return found
}

func rac1AssetRuntimeViolations(set wave42LegacyGitSourceSet) []string {
	var violations []string
	for entry, query := range map[string]string{"Open": "false", "OpenMCP": "true"} {
		function := set.methods[entry]
		correct := false
		if function != nil {
			rac1AssetCalls(set, function, function.Body, func(call *ast.CallExpr, target string) {
				correct = correct || target == "openRuntime" && len(call.Args) == 5 && workerSelectorPath(call.Args[4]) == query
			})
		}
		if !correct {
			violations = append(violations, "API/MCP runtime entry lost explicit query-only selection: "+entry)
		}
	}
	function := set.methods["openRuntime"]
	if function == nil || function.Body == nil {
		return append(violations, "missing shared runtime composition")
	}
	boundary := -1
	for index, statement := range function.Body.List {
		conditional, condition := statement.(*ast.IfStmt)
		if !condition || workerSelectorPath(conditional.Cond) != "queriesOnly" || conditional.Else != nil || len(conditional.Body.List) == 0 {
			continue
		}
		calls := rac1AssetCalls(set, function, conditional.Body, nil)
		_, returned := conditional.Body.List[len(conditional.Body.List)-1].(*ast.ReturnStmt)
		if returned && calls["Runtime.configureMCPReads"] == 1 {
			boundary = index
		}
	}
	configured := false
	for index, statement := range function.Body.List {
		calls := rac1AssetCalls(set, function, statement, nil)
		configured = configured || index > boundary && calls["Runtime.ConfigureRepositoryAssetCommands"] == 1
		if boundary < 0 || index > boundary {
			continue
		}
		if rac1AssetInitializes(statement) {
			violations = append(violations, "asset commands initialized before/inside MCP early return")
		}
		for target := range calls {
			for key, reachable := range wave45PRReachable(set, target) {
				if rac1AssetInitializes(reachable.Body) {
					violations = append(violations, "MCP prefix indirectly initializes asset commands: "+key)
				}
			}
		}
	}
	if boundary < 0 || !configured {
		violations = append(violations, "API asset configuration is not after the MCP early return")
	}
	configuration := set.methods["Runtime.ConfigureRepositoryAssetCommands"]
	output, bound := "", false
	if configuration != nil {
		ast.Inspect(configuration.Body, func(node ast.Node) bool {
			assignment, assigned := node.(*ast.AssignStmt)
			if !assigned || len(assignment.Rhs) != 1 || len(assignment.Lhs) == 0 {
				return true
			}
			if call, called := assignment.Rhs[0].(*ast.CallExpr); called && wave42LegacyGitFunctionName(call.Fun) == "RepositoryAssetCommands" {
				output = workerSelectorPath(assignment.Lhs[0])
			}
			if selected, selector := assignment.Lhs[0].(*ast.SelectorExpr); selector && selected.Sel.Name == "RepositoryAssets" && output != "" && workerSelectorPath(assignment.Rhs[0]) == output {
				bound = true
			}
			return true
		})
	}
	if !bound {
		violations = append(violations, "runtime does not bind the actual asset owner")
	}
	return violations
}

func rac1AssetFixtureBody(test *testing.T, source string) *ast.BlockStmt {
	test.Helper()
	set, err := wave42LegacyGitParseSources(map[string]string{"fixture.go": "package fixture\nfunc fixture() { " + source + " }"})
	if err != nil {
		test.Fatal(err)
	}
	return set.methods["fixture"].Body
}

func TestRAC1AssetCallGraphKeepsReceiversFreeFunctionsAndAliasesDistinct(test *testing.T) {
	const source = `package app
type RepositoryAssetCommands struct{}
type Service struct{}
func (commands *RepositoryAssetCommands) UpdateAbout() { alias := commands; execute := alias.updateAbout; execute() }
func (commands *RepositoryAssetCommands) updateAbout() { assetPolicy[int]() }
func assetPolicy[Result any]() {}
func (service *Service) updateAbout() { NewService() }
func updateAbout() { NewService() }
`
	set, err := wave42LegacyGitParseSources(map[string]string{"renamed.go": source})
	if err != nil {
		test.Fatal(err)
	}
	reachable := wave45PRReachable(set, "RepositoryAssetCommands.UpdateAbout")
	if reachable["RepositoryAssetCommands.updateAbout"] == nil || reachable["assetPolicy"] == nil || reachable["Service.updateAbout"] != nil || reachable["updateAbout"] != nil {
		test.Fatal("asset gate confused exact receiver, free-function, generic-helper, or method-alias ownership")
	}
	calls := rac1AssetExecutionCalls(set, "RepositoryAssetCommands.UpdateAbout", map[string]bool{})
	if calls["RepositoryAssetCommands.updateAbout"] != 1 || calls["assetPolicy"] != 1 || calls["NewService"] != 0 {
		test.Fatal("asset call tracing lost aliases or followed an unrelated same-name method", calls)
	}
	shadowed := strings.Replace(source, "alias := commands;", "updateAbout := func() {}; updateAbout(); alias := commands;", 1)
	set, err = wave42LegacyGitParseSources(map[string]string{"shadowed.go": shadowed})
	if err != nil {
		test.Fatal(err)
	}
	if calls := rac1AssetExecutionCalls(set, "RepositoryAssetCommands.UpdateAbout", map[string]bool{}); calls["NewService"] != 0 || calls["assetPolicy"] != 1 {
		test.Fatal("asset call tracing resolved a shadowed local function as package policy", calls)
	}
}

func TestRAC1AssetRuntimeGateRejectsMCPInitializationAndWrongEntryProfile(test *testing.T) {
	sources := wave42LegacyGitReadSources(test, filepath.Join("..", "serverruntime"))
	for _, regression := range []string{"MCP initializes commands", "configuration before early return", "MCP selects API profile"} {
		test.Run(regression, func(test *testing.T) {
			set, err := wave42LegacyGitParseSources(sources)
			if err != nil {
				test.Fatal(err)
			}
			if violations := rac1AssetRuntimeViolations(set); len(violations) != 0 {
				test.Fatal("valid runtime fixture rejected", violations)
			}
			switch regression {
			case "MCP initializes commands":
				function := set.methods["Runtime.configureMCPReads"]
				function.Body.List = append(rac1AssetFixtureBody(test, "runtime.ConfigureRepositoryAssetCommands()").List, function.Body.List...)
			case "configuration before early return":
				function := set.methods["openRuntime"]
				for index, statement := range function.Body.List {
					if wave42LegacyGitCalls(statement)["ConfigureRepositoryAssetCommands"] {
						function.Body.List = append([]ast.Stmt{statement}, append(function.Body.List[:index], function.Body.List[index+1:]...)...)
						break
					}
				}
			case "MCP selects API profile":
				function := set.methods["OpenMCP"]
				ast.Inspect(function.Body, func(node ast.Node) bool {
					if call, called := node.(*ast.CallExpr); called && wave42LegacyGitFunctionName(call.Fun) == "openRuntime" && len(call.Args) == 5 {
						call.Args[4] = ast.NewIdent("false")
					}
					return true
				})
			}
			if violations := rac1AssetRuntimeViolations(set); len(violations) == 0 {
				test.Fatal("runtime asset gate accepted regression", regression)
			}
		})
	}
}
