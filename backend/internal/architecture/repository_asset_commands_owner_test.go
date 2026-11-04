package architecture

import (
	"go/ast"
	"go/token"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

func rac1AssetMethods() []string {
	return []string{"UpdateAbout", "UpdateRepoConfig", "PutSettings", "PutSettingsObject", "SaveSecrets", "PatchRepoProfile"}
}

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

func rac1AssetRejectCondition(expression ast.Expr, predicate func(ast.Expr) bool) bool {
	if predicate(expression) {
		return true
	}
	switch value := expression.(type) {
	case *ast.ParenExpr:
		return rac1AssetRejectCondition(value.X, predicate)
	case *ast.BinaryExpr:
		if value.Op == token.LOR {
			return rac1AssetRejectCondition(value.X, predicate) || rac1AssetRejectCondition(value.Y, predicate)
		}
	}
	return false
}

func rac1AssetAbsent(expression ast.Expr, argument string) bool {
	negated, negative := expression.(*ast.UnaryExpr)
	if !negative || negated.Op != token.NOT {
		return false
	}
	call, called := negated.X.(*ast.CallExpr)
	return called && wave42LegacyGitFunctionName(call.Fun) == "rootAdapterPresent" && len(call.Args) == 1 && workerSelectorPath(call.Args[0]) == argument
}

func rac1AssetConstructorViolations(set wave42LegacyGitSourceSet) []string {
	constructor := set.methods["NewRepositoryAssetCommands"]
	if constructor == nil || constructor.Body == nil || constructor.Type.Params == nil || len(constructor.Type.Params.List) == 0 || len(constructor.Type.Params.List[0].Names) == 0 {
		return []string{"missing asset constructor"}
	}
	config := constructor.Type.Params.List[0].Names[0].Name
	var violations []string
	allocation := constructor.Body.End()
	allocated := false
	ast.Inspect(constructor.Body, func(node ast.Node) bool {
		if literal, composite := node.(*ast.CompositeLit); composite && wave42LegacyGitType(literal.Type) == "RepositoryAssetCommands" && literal.Pos() < allocation {
			allocation = literal.Pos()
			allocated = true
		}
		return true
	})
	if !allocated {
		violations = append(violations, "asset constructor does not construct its concrete owner")
	}
	for _, dependency := range []string{"Store", "Scope"} {
		guarded := false
		for _, statement := range constructor.Body.List {
			if conditional, condition := statement.(*ast.IfStmt); condition && conditional.End() < allocation && rac1AssetRejects(conditional.Body) {
				guarded = guarded || rac1AssetRejectCondition(conditional.Cond, func(expression ast.Expr) bool {
					return rac1AssetAbsent(expression, config+"."+dependency)
				})
			}
		}
		if !guarded {
			violations = append(violations, "asset constructor does not reject missing/typed-nil "+dependency)
		}
	}
	checked := map[string]bool{}
	ast.Inspect(constructor.Body, func(node ast.Node) bool {
		loop, ranged := node.(*ast.RangeStmt)
		if !ranged {
			return true
		}
		dependency, named := loop.Value.(*ast.Ident)
		values, listed := loop.X.(*ast.CompositeLit)
		if !named || !listed || loop.End() >= allocation {
			return true
		}
		typedNil, missing := false, false
		for _, statement := range loop.Body.List {
			conditional, condition := statement.(*ast.IfStmt)
			if !condition || !rac1AssetRejects(conditional.Body) {
				continue
			}
			joined, conjunction := conditional.Cond.(*ast.BinaryExpr)
			if !conjunction || joined.Op != token.LAND {
				continue
			}
			nonNil := func(expression ast.Expr) bool {
				comparison, compared := expression.(*ast.BinaryExpr)
				return compared && comparison.Op == token.NEQ && workerSelectorPath(comparison.X) == dependency.Name && workerSelectorPath(comparison.Y) == "nil"
			}
			typedNil = typedNil || nonNil(joined.X) && rac1AssetAbsent(joined.Y, dependency.Name) || nonNil(joined.Y) && rac1AssetAbsent(joined.X, dependency.Name)
			complete := func(expression ast.Expr) bool {
				comparison, compared := expression.(*ast.BinaryExpr)
				return compared && comparison.Op == token.EQL && workerSelectorPath(comparison.X) == config+".Profile" && workerSelectorPath(comparison.Y) == "RepositoryAssetsComplete"
			}
			absent := func(expression ast.Expr) bool {
				comparison, compared := expression.(*ast.BinaryExpr)
				return compared && comparison.Op == token.EQL && workerSelectorPath(comparison.X) == dependency.Name && workerSelectorPath(comparison.Y) == "nil"
			}
			missing = missing || complete(joined.X) && absent(joined.Y) || complete(joined.Y) && absent(joined.X)
		}
		if typedNil && missing {
			for _, expression := range values.Elts {
				checked[workerSelectorPath(expression)] = true
			}
		}
		return true
	})
	for _, dependency := range []string{"Repositories", "SecretsCAS", "Notifications"} {
		if !checked[config+"."+dependency] {
			violations = append(violations, "complete asset constructor does not reject missing/typed-nil "+dependency)
		}
	}
	presence := set.methods["rootAdapterPresent"]
	if presence == nil || !wave42LegacyGitCalls(presence.Body)["ValueOf"] || !wave42LegacyGitCalls(presence.Body)["IsNil"] || !wave42LegacyGitCalls(presence.Body)["Kind"] {
		violations = append(violations, "asset constructor lost typed-nil-aware adapter validation")
	} else {
		nilRejected, typedNilRejected := false, false
		kinds := map[string]bool{}
		ast.Inspect(presence.Body, func(node ast.Node) bool {
			switch value := node.(type) {
			case *ast.IfStmt:
				comparison, compared := value.Cond.(*ast.BinaryExpr)
				if compared && comparison.Op == token.EQL && workerSelectorPath(comparison.Y) == "nil" && len(value.Body.List) == 1 {
					if returned, returns := value.Body.List[0].(*ast.ReturnStmt); returns && len(returned.Results) == 1 && workerSelectorPath(returned.Results[0]) == "false" {
						nilRejected = true
					}
				}
			case *ast.CaseClause:
				for _, kind := range value.List {
					kinds[workerSelectorPath(kind)] = true
				}
				for _, statement := range value.Body {
					if returned, returns := statement.(*ast.ReturnStmt); returns && len(returned.Results) == 1 {
						if negated, negative := returned.Results[0].(*ast.UnaryExpr); negative && negated.Op == token.NOT && wave42LegacyGitCalls(negated.X)["IsNil"] {
							typedNilRejected = true
						}
					}
				}
			}
			return true
		})
		for _, kind := range []string{"Pointer", "Interface", "Map", "Slice", "Func", "Chan"} {
			if !kinds["reflect."+kind] {
				typedNilRejected = false
			}
		}
		if !nilRejected || !typedNilRejected {
			violations = append(violations, "adapter presence check accepts nil/typed-nil dependencies")
		}
	}
	return violations
}

func rac1AssetOwnerViolations(set wave42LegacyGitSourceSet) []string {
	ports := []string{"RepositoryAssetStore", "RepositoryAssetWriteScope", "RepositoryAssetRepositoryReader", "outbound.SecretsCASStore", "RepositoryNotificationQueue"}
	violations := rac1AssetFields(set, "RepositoryAssetCommands", ports, ports)
	violations = append(violations, rac1AssetFields(set, "RepositoryAssetCommandsConfig", append(append([]string{}, ports...), "RepositoryAssetProfile"), ports)...)
	violations = append(violations, rac1AssetFields(set, "repositoryAssetScope", []string{"*repositoryWork", "bool", "outbound.RepositoryDocumentIdentityStore"}, []string{"*repositoryWork"})...)
	violations = append(violations, rac1AssetConstructorViolations(set)...)
	for path, file := range set.files {
		owned := false
		for _, declaration := range file.Decls {
			if function, callable := declaration.(*ast.FuncDecl); callable {
				owned = owned || function.Name.Name == "NewRepositoryAssetCommands"
				if function.Recv != nil {
					owner := wave44PreparationNamedType(function.Recv.List[0].Type)
					owned = owned || owner == "RepositoryAssetCommands" || owner == "repositoryAssetScope"
				}
			}
		}
		if !owned {
			continue
		}
		for _, imported := range file.Imports {
			dependency, err := strconv.Unquote(imported.Path.Value)
			if err != nil || dependency == "database/sql" || dependency == "sync" || strings.Contains(dependency, "/pgx") || strings.Contains(dependency, "/semaphore") || strings.HasSuffix(dependency, "/adapters/store") || strings.Contains(dependency, "/app/notificationworker") || strings.Contains(dependency, "/app/projectionworker") {
				violations = append(violations, "asset owner/scope imports independent DB/gate/cache/worker infrastructure: "+path+" "+dependency)
			}
		}
	}
	store := set.types["RepositoryAssetStore"]
	if store == nil {
		violations = append(violations, "missing narrow repository asset store")
	} else if shape, narrow := store.Type.(*ast.InterfaceType); !narrow || store.Assign.IsValid() {
		violations = append(violations, "asset store aliases an aggregate")
	} else {
		allowed := map[string]bool{"GetRepo": true, "UpdateRepoAbout": true, "UpdateRepoConfig": true, "PutSettingsBundle": true, "PutSettingsObject": true, "GetSecretsEnvelope": true}
		for _, method := range shape.Methods.List {
			if len(method.Names) == 0 {
				violations = append(violations, "asset store embeds an aggregate")
			}
			for _, name := range method.Names {
				if !allowed[name.Name] {
					violations = append(violations, "unrelated aggregate capability on asset store: "+name.Name)
				}
			}
		}
	}
	for entry, operations := range map[string][]string{
		"UpdateAbout":       {"RepositoryAssetWriteScope.WithinAssetWrite", "domain.ValidateContentHash", "normalizeAboutWebsite", "strings.TrimSpace", "RepositoryAssetStore.UpdateRepoAbout"},
		"UpdateRepoConfig":  {"RepositoryAssetWriteScope.WithinAssetWrite", "domain.ValidateContentHash", "domain.ValidateBranchName", "RepositoryAssetStore.UpdateRepoConfig"},
		"PutSettings":       {"RepositoryAssetWriteScope.WithinAssetWrite", "domain.ValidateContentHash", "domain.ValidateSettingsBundle", "time.Now", "RepositoryAssetStore.PutSettingsBundle"},
		"PutSettingsObject": {"RepositoryAssetWriteScope.WithinAssetWrite", "validateHashes", "domain.ValidateSettingsBundle", "RepositoryAssetStore.PutSettingsObject"},
		"SaveSecrets":       {"inbound.WithRepositoryActor", "domain.ValidateContentHash", "RepositoryAssetWriteScope.WithinAssetSecretsWrite", "RepositoryAssetWriteScope.AuthorizeAssetSecrets", "RepositoryAssetStore.GetSecretsEnvelope", "domain.ValidateSecretsEdit", "domain.WithSecretsRevision", "outbound.SecretsCASStore.CompareAndSwapSecrets", "domain.SecretsRevision", "enqueueRepositoryNotificationWithQueue"},
		"PatchRepoProfile":  {"RepositoryAssetWriteScope.WithinAssetProfileWrite", "RepositoryAssetStore.GetRepo", "RepositoryAssetWriteScope.RequireAssetDocumentIdentity", "domain.ValidateBranchName", "normalizeAboutWebsite", "RepositoryAssetCommands.updateConfig", "RepositoryAssetCommands.updateAbout"},
	} {
		root := "RepositoryAssetCommands." + entry
		if set.methods[root] == nil {
			violations = append(violations, "missing actual asset command: "+root)
		}
		calls := map[string]int{}
		for key, function := range wave45PRReachable(set, root) {
			if strings.HasPrefix(key, "RepositoryAssetCommands.") || key == "normalizeAboutWebsite" {
				for target, count := range rac1AssetCalls(set, function, function.Body, nil) {
					calls[target] += count
				}
			}
		}
		for _, operation := range operations {
			if calls[operation] == 0 {
				violations = append(violations, "policy left actual asset owner: "+root+" "+operation)
			}
		}
		if entry == "PatchRepoProfile" && (calls["RepositoryAssetWriteScope.WithinAssetProfileWrite"] != 1 || calls["RepositoryAssetCommands.UpdateAbout"] != 0 || calls["RepositoryAssetCommands.UpdateRepoConfig"] != 0) {
			violations = append(violations, "compound asset profile patch split its single scope")
		}
	}
	for root := range set.methods {
		if root != "NewRepositoryAssetCommands" && !strings.HasPrefix(root, "RepositoryAssetCommands.") && !strings.HasPrefix(root, "repositoryAssetScope.") {
			continue
		}
		for key, function := range wave45PRReachable(set, root) {
			if strings.HasPrefix(key, "Service.") || strings.HasPrefix(key, "IdentityService.") || wave43ForbiddenOwner(function) {
				violations = append(violations, "asset reverse dependency: "+root+" -> "+key)
			}
			ast.Inspect(function.Body, func(node ast.Node) bool {
				if _, started := node.(*ast.GoStmt); started {
					violations = append(violations, "asset construction/commands start hidden work: "+key)
				}
				if literal, composite := node.(*ast.CompositeLit); composite && wave42LegacyGitType(literal.Type) == "repositoryWork" {
					violations = append(violations, "asset operation reconstructs common repository work: "+key)
				}
				return true
			})
			for target := range rac1AssetCalls(set, function, function.Body, nil) {
				switch target {
				case "newRepositoryWork", "sql.Open", "sql.OpenDB", "store.Open", "store.OpenFSStore", "pgx.Connect", "pgxpool.New", "AcquireResources", "NewGate", "NewCache":
					violations = append(violations, "asset operation creates independent DB/gate/cache/work: "+key+" "+target)
				}
				if strings.HasSuffix(target, ".WithinRepository") && key != "repositoryWriteWithWork" {
					violations = append(violations, "asset scope bypasses common repository transaction: "+key)
				}
			}
		}
	}
	for entry, shared := range map[string]string{
		"WithinAssetWrite": "repositoryWriteErrorWithWork", "WithinAssetSecretsWrite": "repositoryWriteWithWork", "WithinAssetProfileWrite": "repositoryWriteWithWork",
	} {
		reachable := wave45PRReachable(set, "repositoryAssetScope."+entry)
		for _, dependency := range []string{shared, "repositoryWriteWithWork", "repositoryWork.authorizeRepositoryWrite", "repositoryWork.checkDocumentIdentity", "repositoryWork.revisionWrite", "repositoryWork.auditRepositoryWrite"} {
			if reachable[dependency] == nil {
				violations = append(violations, "asset scope lost existing common repository work: "+entry+" "+dependency)
			}
		}
	}
	assembly := set.methods["Service.repositoryAssets"]
	if assembly == nil || wave45PRReachable(set, "Service.repositoryAssets")["Service.repositoryWork"] == nil {
		violations = append(violations, "asset compatibility assembly no longer retains existing repositoryWork")
	} else if calls := rac1AssetCalls(set, assembly, assembly.Body, nil); calls["Service.repositoryWork"] != 1 || calls["NewRepositoryAssetCommands"] != 1 || calls["newRepositoryWork"] != 0 {
		violations = append(violations, "asset assembly creates independent work instead of retaining the common helper")
	}
	factory := set.methods["Service.RepositoryAssetCommands"]
	complete := false
	if factory != nil {
		rac1AssetCalls(set, factory, factory.Body, func(call *ast.CallExpr, target string) {
			complete = complete || target == "Service.repositoryAssets" && len(call.Args) == 1 && workerSelectorPath(call.Args[0]) == "RepositoryAssetsComplete"
		})
	}
	if !complete || wave45PRReachable(set, "Service.RepositoryAssetCommands")["NewRepositoryAssetCommands"] == nil {
		violations = append(violations, "public asset binding does not construct the complete actual owner")
	}
	if set.methods["normalizeAboutWebsite"] == nil || set.methods["Service.normalizeAboutWebsite"] != nil {
		violations = append(violations, "website normalizer is not an application-owned function")
	}
	return violations
}

func rac1AssetFacadeViolations(set wave42LegacyGitSourceSet) []string {
	var violations []string
	for _, method := range rac1AssetMethods() {
		entry := "Service." + method
		function := set.methods[entry]
		if function == nil || function.Body == nil {
			violations = append(violations, "missing asset compatibility facade: "+entry)
			continue
		}
		calls := rac1AssetCalls(set, function, function.Body, nil)
		assemblyError := ""
		ast.Inspect(function.Body, func(node ast.Node) bool {
			assignment, assigned := node.(*ast.AssignStmt)
			if !assigned || len(assignment.Lhs) != 2 || len(assignment.Rhs) != 1 {
				return true
			}
			if call, called := assignment.Rhs[0].(*ast.CallExpr); called && wave42LegacyGitFunctionName(call.Fun) == "repositoryAssets" {
				assemblyError = workerSelectorPath(assignment.Lhs[1])
			}
			return true
		})
		if calls["Service.repositoryAssets"] != 1 || calls["RepositoryAssetCommands."+method] != 1 {
			violations = append(violations, "asset facade does not assemble/delegate its actual owner: "+entry)
		}
		for target := range calls {
			if target != "Service.repositoryAssets" && target != "RepositoryAssetCommands."+method {
				violations = append(violations, "asset facade retained command policy: "+entry+" "+target)
			}
		}
		ast.Inspect(function.Body, func(node ast.Node) bool {
			switch value := node.(type) {
			case *ast.ForStmt, *ast.RangeStmt, *ast.SwitchStmt, *ast.TypeSwitchStmt, *ast.FuncLit, *ast.GoStmt, *ast.DeferStmt:
				violations = append(violations, "algorithm retained in asset facade: "+entry)
			case *ast.IfStmt:
				comparison, compared := value.Cond.(*ast.BinaryExpr)
				if !compared || assemblyError == "" || comparison.Op != token.NEQ || workerSelectorPath(comparison.X) != assemblyError || workerSelectorPath(comparison.Y) != "nil" || !rac1AssetRejects(value.Body) || value.Else != nil {
					violations = append(violations, "policy branch retained in asset facade: "+entry)
				}
			case *ast.SelectorExpr:
				if value.Sel.Name == "meta" || value.Sel.Name == "store" || value.Sel.Name == "repositories" {
					violations = append(violations, "asset facade accesses command storage: "+entry)
				}
			}
			return true
		})
	}
	for _, method := range []string{"updateAboutCommand", "updateRepoConfigCommand", "putSettingsCommand", "putSettingsObjectCommand", "authorizeSecretsEdit", "notifySecretsChanged"} {
		if set.methods["Service."+method] != nil || set.methods[method] != nil {
			violations = append(violations, "removed private asset algorithm returned to Service: "+method)
		}
	}
	return violations
}

func rac1AssetHTTPViolations(set wave42LegacyGitSourceSet) []string {
	var violations []string
	for route, expected := range map[string][]string{
		"PATCH /api/v1/repos/{repoID}/about":                 {"patchAbout", "PatchRepoProfile"},
		"PUT /api/v1/repos/{repoID}/settings/{kind}":         {"putSettings", "PutSettings"},
		"PUT /api/v1/repos/{repoID}/secrets":                 {"putSecrets", "SaveSecrets"},
		"PUT /api/v1/repos/{repoID}/settings-objects/{hash}": {"putSettingsObject", "PutSettingsObject"},
	} {
		registered := 0
		for _, function := range wave45PRReachable(set, "Server.Handler") {
			rac1AssetCalls(set, function, function.Body, func(call *ast.CallExpr, target string) {
				if !strings.HasSuffix(target, ".HandleFunc") || len(call.Args) != 2 {
					return
				}
				literal, quoted := call.Args[0].(*ast.BasicLit)
				if !quoted || literal.Kind != token.STRING {
					return
				}
				pattern, err := strconv.Unquote(literal.Value)
				if err != nil || pattern != route {
					return
				}
				ast.Inspect(call.Args[1], func(node ast.Node) bool {
					if selected, selector := node.(*ast.SelectorExpr); selector && selected.Sel.Name == expected[0] && function.Recv != nil {
						for _, receiver := range function.Recv.List {
							for _, name := range receiver.Names {
								if wave44PreparationNamedType(receiver.Type) == "Server" && workerSelectorPath(selected.X) == name.Name {
									registered++
								}
							}
						}
					}
					return true
				})
			})
		}
		if registered != 1 {
			violations = append(violations, "missing/misdirected actual asset HTTP route: "+route)
		}
		entry := "Server." + expected[0]
		calls := rac1AssetExecutionCalls(set, entry, map[string]bool{})
		if calls["inbound.RepositoryAssetCommands."+expected[1]] != 1 {
			violations = append(violations, "HTTP entry does not invoke injected repositoryAssets exactly once: "+entry)
		}
		for target := range calls {
			for _, method := range rac1AssetMethods() {
				if strings.HasSuffix(target, "."+method) && target != "inbound.RepositoryAssetCommands."+expected[1] {
					violations = append(violations, "HTTP asset route split/bypassed its injected command: "+entry+" "+target)
				}
			}
		}
	}
	server := set.types["Server"]
	bound := false
	if server != nil {
		if shape, concrete := server.Type.(*ast.StructType); concrete {
			for _, field := range shape.Fields.List {
				for _, name := range field.Names {
					bound = bound || name.Name == "repositoryAssets" && wave42LegacyGitType(field.Type) == "inbound.RepositoryAssetCommands"
				}
			}
		}
	}
	setter := set.methods["Server.SetRepositoryAssetCommands"]
	if !bound || setter == nil || !wave42LegacyGitCalls(setter.Body)["IsNil"] {
		violations = append(violations, "HTTP injection lost narrow binding or typed-nil rejection")
	}
	return violations
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

func TestRAC1RepositoryAssetCommandsOwnPolicyAndRetainSharedScope(test *testing.T) {
	set := wave48Package(test, filepath.Join("..", "app"))
	for _, violation := range append(rac1AssetOwnerViolations(set), rac1AssetFacadeViolations(set)...) {
		test.Error(violation)
	}
}

func TestRAC1RepositoryAssetHTTPRoutesUseInjectedOwner(test *testing.T) {
	for _, violation := range rac1AssetHTTPViolations(wave48Package(test, filepath.Join("..", "adapters", "delivery", "http"))) {
		test.Error(violation)
	}
}

func TestRAC1RepositoryAssetRuntimeIsAPIOnlyAndExplicitlyBound(test *testing.T) {
	runtime := wave48Package(test, filepath.Join("..", "serverruntime"))
	for _, violation := range rac1AssetRuntimeViolations(runtime) {
		test.Error(violation)
	}
	for _, source := range []struct {
		directory string
		entry     string
	}{
		{filepath.Join("..", "serverruntime"), "Runtime.configureMCPReads"},
		{filepath.Join("..", "app"), "NewReadServices"},
	} {
		set := wave48Package(test, source.directory)
		if set.methods[source.entry] == nil {
			test.Fatal("missing MCP read-only composition", source.entry)
		}
		for key, function := range wave45PRReachable(set, source.entry) {
			if rac1AssetInitializes(function.Body) {
				test.Error("read-only composition initializes repository asset commands", key)
			}
		}
	}
	mcp := wave48Package(test, filepath.Join("..", "..", "cmd", "cxt-mcp"))
	for path, file := range mcp.files {
		if rac1AssetInitializes(file) {
			test.Error("MCP process initializes repository asset commands", path)
		}
	}
	api := wave48Package(test, filepath.Join("..", "..", "cmd", "cxtd"))
	configured, bound := token.NoPos, token.NoPos
	configurationOwner := ""
	for _, function := range wave45PRReachable(api, "serve") {
		rac1AssetCalls(api, function, function.Body, func(call *ast.CallExpr, target string) {
			if strings.HasSuffix(target, ".ConfigureRepositoryAssetCommands") {
				configured = call.Pos()
				configurationOwner = strings.TrimSuffix(workerSelectorPath(call.Fun), ".ConfigureRepositoryAssetCommands")
			}
			if strings.HasSuffix(target, ".SetRepositoryAssetCommands") && len(call.Args) == 1 && workerSelectorPath(call.Args[0]) == configurationOwner+".RepositoryAssets" {
				bound = call.Pos()
			}
		})
	}
	if !configured.IsValid() || !bound.IsValid() || configured >= bound {
		test.Error("cxtd does not explicitly configure and Set-bind runtime.RepositoryAssets")
	}
}

func rac1AssetFixtureBody(test *testing.T, source string) *ast.BlockStmt {
	test.Helper()
	set, err := wave42LegacyGitParseSources(map[string]string{"fixture.go": "package fixture\nfunc fixture() { " + source + " }"})
	if err != nil {
		test.Fatal(err)
	}
	return set.methods["fixture"].Body
}

func TestRAC1AssetOwnerGateRejectsPolicyReturnReverseDelegatesAndBroadFields(test *testing.T) {
	sources := wave42LegacyGitReadSources(test, filepath.Join("..", "app"))
	for name, mutate := range map[string]func(wave42LegacyGitSourceSet){
		"service field": func(set wave42LegacyGitSourceSet) {
			shape := set.types["RepositoryAssetCommands"].Type.(*ast.StructType)
			shape.Fields.List = append(shape.Fields.List, &ast.Field{Names: []*ast.Ident{ast.NewIdent("legacy")}, Type: &ast.StarExpr{X: ast.NewIdent("Service")}})
		},
		"aggregate store": func(set wave42LegacyGitSourceSet) {
			shape := set.types["RepositoryAssetCommandsConfig"].Type.(*ast.StructType)
			shape.Fields.List = append(shape.Fields.List, &ast.Field{Names: []*ast.Ident{ast.NewIdent("aggregate")}, Type: &ast.SelectorExpr{X: ast.NewIdent("outbound"), Sel: ast.NewIdent("MetadataStore")}})
		},
		"policy moved back": func(set wave42LegacyGitSourceSet) {
			function := set.methods["RepositoryAssetCommands.updateAbout"]
			function.Recv.List[0].Type = &ast.StarExpr{X: ast.NewIdent("Service")}
			set.methods["Service.updateAbout"] = function
			delete(set.methods, "RepositoryAssetCommands.updateAbout")
		},
		"aliased reverse delegate": func(set wave42LegacyGitSourceSet) {
			set.methods["RepositoryAssetCommands.UpdateAbout"].Body = rac1AssetFixtureBody(test, "legacy := &Service{}; alias := legacy; execute := alias.UpdateAbout; execute()")
		},
		"same-name free reverse delegate": func(set wave42LegacyGitSourceSet) {
			set.methods["RepositoryAssetCommands.UpdateAbout"].Body = rac1AssetFixtureBody(test, "UpdateAbout()")
			set.methods["UpdateAbout"] = &ast.FuncDecl{Name: ast.NewIdent("UpdateAbout"), Type: &ast.FuncType{}, Body: rac1AssetFixtureBody(test, "NewService()")}
		},
		"facade policy": func(set wave42LegacyGitSourceSet) {
			function := set.methods["Service.PutSettings"]
			function.Body.List = append(rac1AssetFixtureBody(test, "domain.ValidateSettingsBundle()").List, function.Body.List...)
		},
		"missing required validation": func(set wave42LegacyGitSourceSet) {
			function := set.methods["NewRepositoryAssetCommands"]
			for _, statement := range function.Body.List {
				if conditional, condition := statement.(*ast.IfStmt); condition && wave42LegacyGitCalls(conditional.Cond)["rootAdapterPresent"] {
					conditional.Cond = ast.NewIdent("false")
				}
			}
		},
		"missing complete typed-nil validation": func(set wave42LegacyGitSourceSet) {
			function := set.methods["NewRepositoryAssetCommands"]
			for _, statement := range function.Body.List {
				if loop, ranged := statement.(*ast.RangeStmt); ranged {
					ast.Inspect(loop.Body, func(node ast.Node) bool {
						if conditional, condition := node.(*ast.IfStmt); condition && wave42LegacyGitCalls(conditional.Cond)["rootAdapterPresent"] {
							conditional.Cond = ast.NewIdent("false")
						}
						return true
					})
				}
			}
		},
		"independent scope": func(set wave42LegacyGitSourceSet) {
			set.methods["repositoryAssetScope.WithinAssetWrite"].Body = rac1AssetFixtureBody(test, "work := newRepositoryWork(); return repositoryWriteErrorWithWork(ctx, work, repo, write)")
		},
		"split compound patch": func(set wave42LegacyGitSourceSet) {
			function := set.methods["RepositoryAssetCommands.PatchRepoProfile"]
			function.Body.List = append(rac1AssetFixtureBody(test, "commands.UpdateAbout(); commands.UpdateRepoConfig()").List, function.Body.List...)
		},
	} {
		test.Run(name, func(test *testing.T) {
			set, err := wave42LegacyGitParseSources(sources)
			if err != nil {
				test.Fatal(err)
			}
			if violations := append(rac1AssetOwnerViolations(set), rac1AssetFacadeViolations(set)...); len(violations) != 0 {
				test.Fatal("valid owner fixture rejected", violations)
			}
			mutate(set)
			if violations := append(rac1AssetOwnerViolations(set), rac1AssetFacadeViolations(set)...); len(violations) == 0 {
				test.Fatal("asset architecture gate accepted regression", name)
			}
		})
	}
}

func TestRAC1AssetHTTPGateRejectsMissingRoutesBackendFallbackAndSplitPatch(test *testing.T) {
	sources := wave42LegacyGitReadSources(test, filepath.Join("..", "adapters", "delivery", "http"))
	aliased, err := wave42LegacyGitParseSources(sources)
	if err != nil {
		test.Fatal(err)
	}
	aliased.methods["Server.patchAbout"].Body = rac1AssetFixtureBody(test, "commands := s.repositoryAssets; execute := commands.PatchRepoProfile; execute()")
	if violations := rac1AssetHTTPViolations(aliased); len(violations) != 0 {
		test.Fatal("valid HTTP receiver and method aliases rejected", violations)
	}
	for _, regression := range []string{"missing route", "backend fallback", "split patch", "double patch"} {
		test.Run(regression, func(test *testing.T) {
			set, err := wave42LegacyGitParseSources(sources)
			if err != nil {
				test.Fatal(err)
			}
			if violations := rac1AssetHTTPViolations(set); len(violations) != 0 {
				test.Fatal("valid HTTP fixture rejected", violations)
			}
			switch regression {
			case "missing route":
				ast.Inspect(set.methods["Server.Handler"].Body, func(node ast.Node) bool {
					if literal, quoted := node.(*ast.BasicLit); quoted && literal.Value == strconv.Quote("PUT /api/v1/repos/{repoID}/secrets") {
						literal.Value = strconv.Quote("PUT /removed")
					}
					return true
				})
			case "backend fallback":
				set.methods["Server.patchAbout"].Body = rac1AssetFixtureBody(test, "s.b.PatchRepoProfile()")
			case "split patch":
				set.methods["Server.patchAbout"].Body = rac1AssetFixtureBody(test, "s.repositoryAssets.UpdateAbout(); s.repositoryAssets.UpdateRepoConfig()")
			case "double patch":
				set.methods["Server.patchAbout"].Body = rac1AssetFixtureBody(test, "commands := s.repositoryAssets; execute := commands.PatchRepoProfile; execute(); execute()")
			}
			if violations := rac1AssetHTTPViolations(set); len(violations) == 0 {
				test.Fatal("HTTP asset gate accepted regression", regression)
			}
		})
	}
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
