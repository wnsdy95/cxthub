package architecture

import (
	"go/ast"
	"go/token"
	"path/filepath"
	"strings"
	"testing"
)

func rwa1AuthorityCalls(set wave42LegacyGitSourceSet, function *ast.FuncDecl, closures bool) map[string]int {
	counts := map[string]int{}
	if function == nil || function.Body == nil {
		return counts
	}
	var literals []*ast.FuncLit
	if !closures {
		ast.Inspect(function.Body, func(node ast.Node) bool {
			if literal, anonymous := node.(*ast.FuncLit); anonymous {
				literals = append(literals, literal)
				return false
			}
			return true
		})
	}
	rac1AssetCalls(set, function, function.Body, func(call *ast.CallExpr, target string) {
		for _, literal := range literals {
			if call.Pos() >= literal.Pos() && call.End() <= literal.End() {
				return
			}
		}
		counts[target]++
	})
	return counts
}

func rwa1AuthorityReachable(set wave42LegacyGitSourceSet, entry string) map[string]*ast.FuncDecl {
	visited := map[string]*ast.FuncDecl{}
	var visit func(string)
	visit = func(target string) {
		function := set.methods[target]
		if function == nil || visited[target] != nil {
			return
		}
		visited[target] = function
		for nested := range rwa1AuthorityCalls(set, function, false) {
			visit(nested)
		}
	}
	visit(entry)
	return visited
}

func rwa1AuthorityOperations(set wave42LegacyGitSourceSet, entry string) map[string]int {
	operations := map[string]int{}
	for _, function := range rwa1AuthorityReachable(set, entry) {
		for target, count := range rwa1AuthorityCalls(set, function, false) {
			operations[target] += count
		}
	}
	return operations
}

func rwa1AuthorityFieldViolations(set wave42LegacyGitSourceSet) []string {
	var violations []string
	for owner, allowed := range map[string][]string{
		"RepositoryAuthority":       {"RepositoryAuthorityProfile", "RepositoryAuthorityMetadata", "RepositoryAuthorityRepositories", "RepositoryAuthorityTeams", "RepositoryAuthorityOrganizations", "RepositoryAuthorityAudit", "outbound.RepositoryAccessLocker", "bool", "repositoryRoleSources"},
		"RepositoryAuthorityConfig": {"RepositoryAuthorityProfile", "RepositoryAuthorityMetadata", "RepositoryAuthorityRepositories", "RepositoryAuthorityTeams", "RepositoryAuthorityOrganizations", "RepositoryAuthorityAudit", "outbound.RepositoryAccessLocker", "bool"},
		"repositoryRoleSources":     {"RepositoryAuthorityRepositories", "RepositoryAuthorityTeams", "RepositoryAuthorityOrganizations"},
	} {
		declaration := set.types[owner]
		if declaration == nil {
			violations = append(violations, "missing-owner: "+owner)
			continue
		}
		shape, concrete := declaration.Type.(*ast.StructType)
		if !concrete || declaration.Assign.IsValid() {
			violations = append(violations, "broad-field: nonconcrete or aliased "+owner)
			continue
		}
		for _, field := range shape.Fields.List {
			kind := wave42LegacyGitType(field.Type)
			accepted := false
			for _, candidate := range allowed {
				accepted = accepted || kind == candidate
			}
			if !accepted || len(field.Names) == 0 {
				violations = append(violations, "broad-field: "+owner+" "+kind)
			}
		}
	}
	for _, violation := range wave47GitHubStructViolations(set, "RepositoryAuthorityConfig", map[string]string{
		"Profile": "RepositoryAuthorityProfile", "Metadata": "RepositoryAuthorityMetadata", "Repositories": "RepositoryAuthorityRepositories",
		"Teams": "RepositoryAuthorityTeams", "Organizations": "RepositoryAuthorityOrganizations", "Locker": "outbound.RepositoryAccessLocker",
		"Audit": "RepositoryAuthorityAudit", "Transactional": "bool",
	}) {
		violations = append(violations, "config-contract: "+violation)
	}
	for owner, expected := range map[string]map[string]string{
		"RepositoryAuthorityMetadata":      {"GetRepo": "context.Context,domain.ContentHash)(domain.Repo,error"},
		"RepositoryAuthorityRepositories":  {"GetRepository": "context.Context,string)(domain.Repository,error", "ListMembers": "context.Context,string)([]domain.Membership,error"},
		"RepositoryAuthorityTeams":         {"RepositoryTeamAccess": "context.Context,string,string)([]domain.TeamRepositoryAccess,error"},
		"RepositoryAuthorityOrganizations": {"RepositoryOrganizationAccess": "context.Context,string,string)(domain.OrganizationRepositoryAccess,error"},
		"RepositoryAuthorityAudit":         {"GetNamespace": "context.Context,string)(domain.Namespace,error", "AppendOrganizationAudit": "context.Context,domain.OrganizationAuditEvent)(error"},
	} {
		declaration := set.types[owner]
		if declaration == nil {
			violations = append(violations, "narrow-port: missing "+owner)
			continue
		}
		shape, narrow := declaration.Type.(*ast.InterfaceType)
		if !narrow || declaration.Assign.IsValid() || len(shape.Methods.List) != len(expected) {
			violations = append(violations, "narrow-port: broad, aliased, or missing methods "+owner)
			continue
		}
		observed := map[string]bool{}
		for _, field := range shape.Methods.List {
			signature, callable := field.Type.(*ast.FuncType)
			if !callable || len(field.Names) != 1 {
				violations = append(violations, "narrow-port: embedded capability "+owner)
				continue
			}
			name := field.Names[0].Name
			actual := wave46EmailFieldTypes(signature.Params) + ")(" + wave46EmailFieldTypes(signature.Results)
			if observed[name] || expected[name] != actual {
				violations = append(violations, "narrow-port: changed signature "+owner+"."+name)
			}
			observed[name] = true
		}
	}
	return violations
}

func rwa1AuthorityRuntimeViolations(set wave42LegacyGitSourceSet, entry string) []string {
	var violations []string
	for target, function := range rwa1AuthorityReachable(set, entry) {
		if strings.HasPrefix(target, "Service.") || strings.HasPrefix(target, "IdentityService.") || target == "repositoryRoleFor" || target == "appendRepositoryAudit" {
			violations = append(violations, "parent-backdelegate: "+entry+" -> "+target)
		}
		if target == "repositoryAuthorityFromStores" || target == "newRepositoryWork" || target == "NewRepositoryAuthority" {
			violations = append(violations, "runtime-discovery: "+entry+" -> "+target)
		}
		for called := range rwa1AuthorityCalls(set, function, false) {
			if strings.HasPrefix(called, "reflect.") || called == "repositoryRoleSourcesFromRepositories" {
				violations = append(violations, "runtime-discovery: "+entry+" -> "+called)
			}
		}
		ast.Inspect(function, func(node ast.Node) bool {
			switch value := node.(type) {
			case *ast.Ident:
				switch value.Name {
				case "Service", "IdentityService", "NewService", "NewIdentityService", "MetadataStore", "RepositoryStore", "TeamStore", "OrganizationStore":
					violations = append(violations, "parent-backdelegate: "+entry+" retains "+value.Name)
				}
			case *ast.TypeAssertExpr:
				kind := wave42LegacyGitType(value.Type)
				call, called := value.X.(*ast.CallExpr)
				var selection *ast.SelectorExpr
				selected := false
				if called {
					selection, selected = call.Fun.(*ast.SelectorExpr)
				}
				contextValue := selected && selection.Sel.Name == "Value" && (kind == "string" || kind == "*afterCommitActions")
				if !contextValue {
					violations = append(violations, "runtime-assertion: "+target+" "+kind)
				}
			case *ast.GoStmt:
				violations = append(violations, "runtime-background-work: "+target)
			}
			return true
		})
	}
	return violations
}

func rwa1AuthorityOwnerViolations(set wave42LegacyGitSourceSet) []string {
	violations := rwa1AuthorityFieldViolations(set)
	for method, parameters := range map[string]string{
		"AuthorizeWrite": "context.Context,domain.ContentHash", "AuthorizeSecrets": "context.Context,domain.ContentHash,string", "AuditWrite": "context.Context,domain.ContentHash",
	} {
		entry := "RepositoryAuthority." + method
		function := set.methods[entry]
		if function == nil || function.Body == nil || wave46EmailFieldTypes(function.Type.Params) != parameters || wave46EmailFieldTypes(function.Type.Results) != "error" {
			violations = append(violations, "owner-signature: "+entry)
		}
		violations = append(violations, rwa1AuthorityRuntimeViolations(set, entry)...)
	}
	for entry, expected := range map[string][]string{
		"RepositoryAuthority.AuthorizeWrite":   {"inbound.RepositoryActor", "RepositoryAuthorityMetadata.GetRepo", "outbound.RepositoryAccessLocker.LockRepositoryAccess", "RepositoryAuthorityRepositories.GetRepository", "repositoryRoleSources.roleFor"},
		"RepositoryAuthority.AuthorizeSecrets": {"RepositoryAuthorityMetadata.GetRepo", "outbound.RepositoryAccessLocker.LockRepositoryAccess", "RepositoryAuthorityRepositories.GetRepository", "repositoryRoleSources.roleFor", "domain.CanEditSecrets"},
		"RepositoryAuthority.AuditWrite":       {"inbound.RepositoryActor", "RepositoryAuthorityMetadata.GetRepo", "appendRepositoryAuditFromSources"},
		"repositoryRoleSources.roleFor":        {"RepositoryAuthorityRepositories.ListMembers", "RepositoryAuthorityTeams.RepositoryTeamAccess", "RepositoryAuthorityOrganizations.RepositoryOrganizationAccess", "domain.EffectiveRepositoryRole"},
		"appendRepositoryAuditFromSources":     {"RepositoryAuthorityRepositories.GetRepository", "RepositoryAuthorityAudit.GetNamespace", "RepositoryAuthorityAudit.AppendOrganizationAudit", "organizationAudit"},
	} {
		calls := rwa1AuthorityOperations(set, entry)
		for _, operation := range expected {
			if calls[operation] != 1 {
				violations = append(violations, "owner-shell: "+entry+" must execute exactly one "+operation)
			}
		}
	}
	for entry, owner := range map[string]string{"repositoryRoleFor": "repositoryRoleSources.roleFor", "appendRepositoryAudit": "appendRepositoryAuditFromSources"} {
		if rwa1AuthorityReachable(set, entry)[owner] == nil {
			violations = append(violations, "shared-policy: legacy "+entry+" bypasses "+owner)
		}
		for target := range rwa1AuthorityCalls(set, set.methods[entry], false) {
			for _, operation := range []string{"ListMembers", "RepositoryTeamAccess", "RepositoryOrganizationAccess", "GetRepository", "GetNamespace", "AppendOrganizationAudit"} {
				if strings.HasSuffix(target, "."+operation) {
					violations = append(violations, "shared-policy: legacy wrapper retains source execution "+entry+" -> "+target)
				}
			}
		}
	}
	for entry, function := range set.methods {
		if entry != "repositoryRoleSources.roleFor" && rwa1AuthorityCalls(set, function, false)["domain.EffectiveRepositoryRole"] > 0 {
			violations = append(violations, "shared-policy: duplicated effective role algorithm "+entry)
		}
	}
	for entry, owner := range map[string]string{
		"repositoryWork.authorizeRepositoryWrite":    "RepositoryAuthority.AuthorizeWrite",
		"repositoryWork.auditRepositoryWrite":        "RepositoryAuthority.AuditWrite",
		"repositoryAssetScope.AuthorizeAssetSecrets": "RepositoryAuthority.AuthorizeSecrets",
	} {
		calls := rwa1AuthorityCalls(set, set.methods[entry], false)
		if calls[owner] != 1 {
			violations = append(violations, "direct-delegation: "+entry+" must call "+owner+" exactly once")
		}
		for target := range calls {
			if target != owner {
				violations = append(violations, "direct-delegation: facade retains execution "+entry+" -> "+target)
			}
		}
	}
	return violations
}

func rwa1AuthorityErrorGuard(set wave42LegacyGitSourceSet, function *ast.FuncDecl, statement *ast.IfStmt) string {
	assignment, assigned := statement.Init.(*ast.AssignStmt)
	comparison, compared := statement.Cond.(*ast.BinaryExpr)
	if !assigned || len(assignment.Lhs) != 1 || len(assignment.Rhs) != 1 || !compared || comparison.Op != token.NEQ {
		return ""
	}
	name, named := assignment.Lhs[0].(*ast.Ident)
	call, called := assignment.Rhs[0].(*ast.CallExpr)
	if !named || !called || workerSelectorPath(comparison.X) != name.Name || workerSelectorPath(comparison.Y) != "nil" || len(statement.Body.List) == 0 {
		return ""
	}
	returned, returns := statement.Body.List[len(statement.Body.List)-1].(*ast.ReturnStmt)
	if !returns || len(returned.Results) == 0 || workerSelectorPath(returned.Results[len(returned.Results)-1]) != name.Name {
		return ""
	}
	target := ""
	rac1AssetCalls(set, function, call, func(actual *ast.CallExpr, resolved string) {
		if actual == call {
			target = resolved
		}
	})
	return target
}

func rwa1AuthorityComposite(expression ast.Expr, owner string) *ast.CompositeLit {
	if address, addressed := expression.(*ast.UnaryExpr); addressed && address.Op == token.AND {
		expression = address.X
	}
	if literal, composite := expression.(*ast.CompositeLit); composite && wave42LegacyGitType(literal.Type) == owner {
		return literal
	}
	return nil
}

func rwa1AuthorityField(literal *ast.CompositeLit, name string) ast.Expr {
	if literal != nil {
		for _, element := range literal.Elts {
			if field, keyed := element.(*ast.KeyValueExpr); keyed && workerSelectorPath(field.Key) == name {
				return field.Value
			}
		}
	}
	return nil
}

func rwa1AuthorityAssemblyWork(function *ast.FuncDecl, constructor, config, scope string) (string, token.Pos) {
	for _, statement := range function.Body.List {
		returned, returns := statement.(*ast.ReturnStmt)
		if !returns || len(returned.Results) != 1 {
			continue
		}
		call, called := returned.Results[0].(*ast.CallExpr)
		if !called || workerSelectorPath(call.Fun) != constructor || len(call.Args) != 1 {
			continue
		}
		configuration := rwa1AuthorityComposite(call.Args[0], config)
		binding := rwa1AuthorityComposite(rwa1AuthorityField(configuration, "Scope"), scope)
		return workerSelectorPath(rwa1AuthorityField(binding, "work")), call.Pos()
	}
	return "", token.NoPos
}

func rwa1AuthorityCompleteViolations(set wave42LegacyGitSourceSet) []string {
	var violations []string
	shared := ""
	for entry, contract := range map[string][]string{
		"Service.repositoryAssets":     {"RepositoryAssetsComplete", "NewRepositoryAssetCommands", "RepositoryAssetCommandsConfig", "repositoryAssetScope"},
		"Service.documentPublications": {"DocumentPublicationComplete", "NewDocumentPublicationCommands", "DocumentPublicationCommandsConfig", "documentPublicationScope"},
	} {
		function := set.methods[entry]
		if function == nil || function.Body == nil || function.Type.Params == nil || len(function.Type.Params.List) == 0 || len(function.Type.Params.List[0].Names) != 1 {
			violations = append(violations, "complete-validation: missing "+entry)
			continue
		}
		profileName := function.Type.Params.List[0].Names[0].Name
		profile := contract[0]
		boundWork, assembly := rwa1AuthorityAssemblyWork(function, contract[1], contract[2], contract[3])
		validator := ""
		for _, statement := range function.Body.List {
			conditional, condition := statement.(*ast.IfStmt)
			if !condition {
				continue
			}
			comparison, compared := conditional.Cond.(*ast.BinaryExpr)
			if !compared || comparison.Op != token.EQL || !((workerSelectorPath(comparison.X) == profileName && workerSelectorPath(comparison.Y) == profile) || (workerSelectorPath(comparison.Y) == profileName && workerSelectorPath(comparison.X) == profile)) {
				continue
			}
			for _, nested := range conditional.Body.List {
				guard, guarded := nested.(*ast.IfStmt)
				if !guarded {
					continue
				}
				target := rwa1AuthorityErrorGuard(set, function, guard)
				if target == "repositoryWork.validateCompleteAuthority" && boundWork != "" && guard.End() < assembly {
					assignment := guard.Init.(*ast.AssignStmt)
					call := assignment.Rhs[0].(*ast.CallExpr)
					selection, selected := call.Fun.(*ast.SelectorExpr)
					if selected && workerSelectorPath(selection.X) == boundWork {
						validator = target
					}
				}
			}
		}
		if validator == "" {
			violations = append(violations, "complete-validation: disconnected or unchecked "+entry)
			continue
		}
		if shared != "" && validator != shared {
			violations = append(violations, "complete-validation: asset/document factories do not share validation")
		}
		shared = validator
		calls := rwa1AuthorityOperations(set, validator)
		complete := false
		for _, helper := range rwa1AuthorityReachable(set, validator) {
			rac1AssetCalls(set, helper, helper.Body, func(call *ast.CallExpr, target string) {
				if target == "NewRepositoryAuthority" && len(call.Args) == 1 {
					configuration := rwa1AuthorityComposite(call.Args[0], "RepositoryAuthorityConfig")
					complete = complete || workerSelectorPath(rwa1AuthorityField(configuration, "Profile")) == "RepositoryAuthorityComplete"
				}
			})
		}
		if calls["NewRepositoryAuthority"] == 0 || !complete {
			violations = append(violations, "complete-validation: shared validator does not construct complete authority")
		}
	}
	for entry, contract := range map[string][]string{
		"Service.RepositoryAssetCommands":     {"Service.repositoryAssets", "RepositoryAssetsComplete"},
		"Service.DocumentPublicationCommands": {"Service.documentPublications", "DocumentPublicationComplete"},
	} {
		function := set.methods[entry]
		complete := false
		if function != nil {
			rac1AssetCalls(set, function, function.Body, func(call *ast.CallExpr, target string) {
				complete = complete || target == contract[0] && len(call.Args) == 1 && workerSelectorPath(call.Args[0]) == contract[1]
			})
		}
		if !complete {
			violations = append(violations, "complete-validation: public factory does not select Complete "+entry)
		}
	}
	return violations
}

func rwa1AuthorityCaptureViolations(set wave42LegacyGitSourceSet) []string {
	var violations []string
	fields := map[string]bool{}
	fieldTypes := map[string]string{}
	if declaration := set.types["repositoryWork"]; declaration != nil {
		if shape, concrete := declaration.Type.(*ast.StructType); concrete && !declaration.Assign.IsValid() {
			for _, field := range shape.Fields.List {
				fields[wave42LegacyGitType(field.Type)] = true
				for _, name := range field.Names {
					fieldTypes[name.Name] = wave42LegacyGitType(field.Type)
				}
			}
		}
	}
	factory := rwa1AuthorityReachable(set, "newRepositoryWork")
	captured := map[string]string{}
	retained := map[string]bool{}
	authorityName := ""
	if function := set.methods["newRepositoryWork"]; function != nil && function.Body != nil {
		for _, statement := range function.Body.List {
			assignment, assigned := statement.(*ast.AssignStmt)
			if !assigned || len(assignment.Lhs) != 2 || len(assignment.Rhs) != 1 {
				continue
			}
			name, named := assignment.Lhs[0].(*ast.Ident)
			if !named {
				continue
			}
			if assertion, asserted := assignment.Rhs[0].(*ast.TypeAssertExpr); asserted {
				captured[name.Name] = wave42LegacyGitType(assertion.Type)
			}
			if call, called := assignment.Rhs[0].(*ast.CallExpr); called && workerSelectorPath(call.Fun) == "repositoryAuthorityFromStores" && len(call.Args) > 0 && workerSelectorPath(call.Args[len(call.Args)-1]) == "RepositoryAuthorityCompatibility" {
				authorityName = name.Name
			}
		}
		for _, statement := range function.Body.List {
			returned, returns := statement.(*ast.ReturnStmt)
			if !returns || len(returned.Results) != 1 {
				continue
			}
			literal := rwa1AuthorityComposite(returned.Results[0], "repositoryWork")
			if literal == nil {
				continue
			}
			for _, element := range literal.Elts {
				field, keyed := element.(*ast.KeyValueExpr)
				if !keyed {
					continue
				}
				kind := fieldTypes[workerSelectorPath(field.Key)]
				name := workerSelectorPath(field.Value)
				retained[kind] = retained[kind] || kind != "" && captured[name] == kind
				if kind == "*RepositoryAuthority" && name != "" && name == authorityName {
					retained[kind] = true
				}
			}
		}
	}
	for _, capability := range []string{"outbound.RepositoryTransactions", "outbound.RepositoryRevisions", "outbound.EvidenceRevisions"} {
		if !fields[capability] || !retained[capability] {
			violations = append(violations, "factory-capture: missing retained capability "+capability)
		}
	}
	if !retained["*RepositoryAuthority"] || factory["NewRepositoryAuthority"] == nil || factory["repositoryAuthorityFromStores"] == nil {
		violations = append(violations, "factory-capture: repositoryWork does not retain its constructed common authority")
	}
	legacyCapabilities := map[string]bool{}
	for _, helper := range rwa1AuthorityReachable(set, "repositoryAuthorityFromStores") {
		ast.Inspect(helper.Body, func(node ast.Node) bool {
			if assertion, asserted := node.(*ast.TypeAssertExpr); asserted {
				legacyCapabilities[wave42LegacyGitType(assertion.Type)] = true
			}
			return true
		})
	}
	for _, capability := range []string{"outbound.TeamStore", "outbound.OrganizationStore", "outbound.RepositoryOrganizationAccess", "outbound.RepositoryAccessLocker"} {
		if !legacyCapabilities[capability] {
			violations = append(violations, "factory-capture: legacy adapter changed its explicit optional capability "+capability)
		}
	}
	for _, entry := range []string{"repositoryWriteWithWork", "repositoryReadWithWork", "repositoryWork.revisionWrite", "repositoryWork.authorizeRepositoryWrite", "repositoryWork.auditRepositoryWrite", "repositoryAssetScope.AuthorizeAssetSecrets"} {
		violations = append(violations, rwa1AuthorityRuntimeViolations(set, entry)...)
	}
	return violations
}

func rwa1AuthorityWorkAlgorithmViolations(set wave42LegacyGitSourceSet) []string {
	function := set.methods["repositoryWriteWithWork"]
	if function == nil || function.Body == nil {
		return []string{"work-algorithm: missing repositoryWriteWithWork"}
	}
	var violations []string
	calls := rwa1AuthorityCalls(set, function, true)
	for _, target := range []string{"repositoryWork.authorizeRepositoryWrite", "repositoryWork.checkDocumentIdentity", "outbound.RepositoryTransactions.WithinRepository", "repositoryWork.revisionWrite", "repositoryWork.auditRepositoryWrite", "context.WithValue", "errors.As", "time.NewTimer"} {
		if calls[target] == 0 {
			violations = append(violations, "work-algorithm: missing "+target)
		}
	}
	guardedOrder, auditOrder, afterCommit, retries, cancellation := false, 0, false, false, false
	states := map[string]bool{}
	ast.Inspect(function.Body, func(node ast.Node) bool {
		switch value := node.(type) {
		case *ast.FuncLit:
			positions := map[string]token.Pos{}
			rac1AssetCalls(set, function, value.Body, func(call *ast.CallExpr, target string) {
				if positions[target] == token.NoPos {
					positions[target] = call.Pos()
				}
			})
			authorize, identity, operation := positions["repositoryWork.authorizeRepositoryWrite"], positions["repositoryWork.checkDocumentIdentity"], token.NoPos
			ast.Inspect(value.Body, func(nested ast.Node) bool {
				if call, called := nested.(*ast.CallExpr); called && workerSelectorPath(call.Fun) == "original" {
					operation = call.Pos()
				}
				return true
			})
			guardedOrder = guardedOrder || authorize != token.NoPos && identity > authorize && operation > identity
		case *ast.BlockStmt:
			for index, statement := range value.List {
				assignment, assigned := statement.(*ast.AssignStmt)
				if !assigned || len(assignment.Rhs) != 1 || index+1 >= len(value.List) {
					continue
				}
				call, called := assignment.Rhs[0].(*ast.CallExpr)
				if !called || wave42LegacyGitFunctionName(call.Fun) != "revisionWrite" {
					continue
				}
				guard, guarded := value.List[index+1].(*ast.IfStmt)
				if guarded {
					comparison, compared := guard.Cond.(*ast.BinaryExpr)
					if compared && comparison.Op == token.EQL && workerSelectorPath(comparison.X) == "err" && workerSelectorPath(comparison.Y) == "nil" && rac1AssetCalls(set, function, guard.Body, nil)["repositoryWork.auditRepositoryWrite"] == 1 {
						auditOrder++
					}
				}
			}
		case *ast.ForStmt:
			comparison, compared := value.Cond.(*ast.BinaryExpr)
			if compared && comparison.Op == token.LSS {
				limit, literal := comparison.Y.(*ast.BasicLit)
				retries = retries || literal && limit.Value == "3"
			}
		case *ast.IfStmt:
			comparison, compared := value.Cond.(*ast.BinaryExpr)
			if compared && comparison.Op == token.EQL && workerSelectorPath(comparison.X) == "err" && workerSelectorPath(comparison.Y) == "nil" {
				ast.Inspect(value.Body, func(nested ast.Node) bool {
					if loop, ranged := nested.(*ast.RangeStmt); ranged && strings.HasSuffix(workerSelectorPath(loop.X), ".actions") {
						afterCommit = true
					}
					return true
				})
			}
		case *ast.SelectStmt:
			cancellation = wave42LegacyGitCalls(value.Body)["Done"] && wave42LegacyGitCalls(value.Body)["Stop"] && wave42LegacyGitCalls(value.Body)["Err"]
		case *ast.BasicLit:
			if value.Kind == token.STRING {
				states[value.Value] = true
			}
		}
		return true
	})
	if !guardedOrder || auditOrder != 2 || !afterCommit || !retries || !cancellation || !states[`"40001"`] || !states[`"40P01"`] || !wave42LegacyGitMentions(function.Body, "afterCommitKey") {
		violations = append(violations, "work-algorithm: changed authorization/identity, revision-before-audit, retry, cancellation, or after-commit structure")
	}
	return violations
}

func rwa1AuthorityAccessViolations(set wave42LegacyGitSourceSet) []string {
	function := set.methods["RepositoryAuthority.loadAccess"]
	if function == nil || function.Body == nil {
		return []string{"access-order: missing shared authority-owned access load"}
	}
	var violations []string
	order := map[string]int{}
	sequence := 0
	rac1AssetCalls(set, function, function.Body, func(call *ast.CallExpr, target string) {
		sequence++
		order[target] = sequence
	})
	previous := 0
	for _, operation := range []string{"RepositoryAuthorityMetadata.GetRepo", "outbound.RepositoryAccessLocker.LockRepositoryAccess", "RepositoryAuthorityRepositories.GetRepository", "repositoryRoleSources.roleFor"} {
		if order[operation] <= previous {
			violations = append(violations, "access-order: current metadata, lock, repository, then role required: "+operation)
		}
		previous = order[operation]
	}
	for entry, missingLock := range map[string]string{
		"RepositoryAuthority.AuthorizeWrite": "domain.ErrForbidden", "RepositoryAuthority.AuthorizeSecrets": "domain.ErrSecretsConsistency",
	} {
		function := set.methods[entry]
		matched := false
		if function != nil {
			rac1AssetCalls(set, function, function.Body, func(call *ast.CallExpr, target string) {
				matched = matched || target == "RepositoryAuthority.loadAccess" && len(call.Args) == 4 && workerSelectorPath(call.Args[3]) == missingLock
			})
		}
		if !matched {
			violations = append(violations, "access-order: lost explicit missing-lock distinction "+entry)
		}
	}
	return violations
}

func TestRWA1RepositoryAuthorityActualOwnerAndSharedSources(test *testing.T) {
	set, err := wave42LegacyGitParseSources(wave42LegacyGitReadSources(test, filepath.Join("..", "app")))
	if err != nil {
		test.Fatal(err)
	}
	for _, inspect := range []func(wave42LegacyGitSourceSet) []string{rwa1AuthorityOwnerViolations, rwa1AuthorityCompleteViolations, rwa1AuthorityCaptureViolations, rwa1AuthorityWorkAlgorithmViolations, rwa1AuthorityAccessViolations} {
		for _, violation := range inspect(set) {
			test.Error(violation)
		}
	}
	constructor := set.methods["NewRepositoryAuthority"]
	if constructor == nil || wave46EmailFieldTypes(constructor.Type.Params) != "RepositoryAuthorityConfig" || wave46EmailFieldTypes(constructor.Type.Results) != "*RepositoryAuthority,error" {
		test.Error("authority constructor must expose its concrete narrow config and error contract")
	}
	for _, helper := range rwa1AuthorityReachable(set, "NewRepositoryAuthority") {
		ast.Inspect(helper.Body, func(node ast.Node) bool {
			if _, started := node.(*ast.GoStmt); started {
				test.Error("authority constructor starts hidden background work")
			}
			return true
		})
		for target := range rwa1AuthorityCalls(set, helper, false) {
			for _, operation := range []string{"GetRepo", "GetRepository", "ListMembers", "RepositoryTeamAccess", "RepositoryOrganizationAccess", "GetNamespace", "AppendOrganizationAudit", "LockRepositoryAccess", "WithinRepository", "WithinReadSnapshot"} {
				if strings.HasSuffix(target, "."+operation) {
					test.Error("authority constructor performs IO", target)
				}
			}
		}
	}
	domainSet, err := wave42LegacyGitParseSources(wave42LegacyGitReadSources(test, filepath.Join("..", "domain")))
	if err != nil {
		test.Fatal(err)
	}
	pureWriteDecision := false
	for target := range rwa1AuthorityCalls(set, set.methods["RepositoryAuthority.AuthorizeWrite"], false) {
		if !strings.HasPrefix(target, "domain.") {
			continue
		}
		policy := domainSet.methods[strings.TrimPrefix(target, "domain.")]
		if policy != nil && strings.Contains(wave46EmailFieldTypes(policy.Type.Params), "MemberRole") && strings.Contains(wave46EmailFieldTypes(policy.Type.Params), "string") && (wave46EmailFieldTypes(policy.Type.Results) == "bool" || wave46EmailFieldTypes(policy.Type.Results) == "error") {
			pureWriteDecision = true
		}
	}
	if !pureWriteDecision {
		test.Error("actual authority must invoke the domain-owned role/action context-write decision")
	}
}

func TestRWA1AuthorityScopeGuardsRejectActualASTAlgorithmMutations(test *testing.T) {
	sources := wave42LegacyGitReadSources(test, filepath.Join("..", "app"))
	for _, fixture := range []struct {
		name    string
		inspect func(wave42LegacyGitSourceSet) []string
		mutate  func(wave42LegacyGitSourceSet) bool
	}{
		{"role reads before lock", rwa1AuthorityAccessViolations, func(set wave42LegacyGitSourceSet) bool {
			function := set.methods["RepositoryAuthority.loadAccess"]
			lockIndex, repositoryIndex := -1, -1
			for index, statement := range function.Body.List {
				calls := rac1AssetCalls(set, function, statement, nil)
				if calls["outbound.RepositoryAccessLocker.LockRepositoryAccess"] > 0 {
					lockIndex = index
				}
				if calls["RepositoryAuthorityRepositories.GetRepository"] > 0 {
					repositoryIndex = index
				}
			}
			if lockIndex < 0 || repositoryIndex < 0 {
				return false
			}
			function.Body.List[lockIndex], function.Body.List[repositoryIndex] = function.Body.List[repositoryIndex], function.Body.List[lockIndex]
			return true
		}},
		{"revision audit order", rwa1AuthorityWorkAlgorithmViolations, func(set wave42LegacyGitSourceSet) bool {
			changed := false
			ast.Inspect(set.methods["repositoryWriteWithWork"].Body, func(node ast.Node) bool {
				if selected, selector := node.(*ast.SelectorExpr); selector {
					switch selected.Sel.Name {
					case "revisionWrite":
						selected.Sel.Name = "auditRepositoryWrite"
						changed = true
					case "auditRepositoryWrite":
						selected.Sel.Name = "revisionWrite"
						changed = true
					}
				}
				return true
			})
			return changed
		}},
		{"changed retry bound", rwa1AuthorityWorkAlgorithmViolations, func(set wave42LegacyGitSourceSet) bool {
			changed := false
			ast.Inspect(set.methods["repositoryWriteWithWork"].Body, func(node ast.Node) bool {
				if loop, repeated := node.(*ast.ForStmt); repeated {
					if comparison, compared := loop.Cond.(*ast.BinaryExpr); compared && comparison.Op == token.LSS {
						comparison.Y = &ast.BasicLit{Kind: token.INT, Value: "1"}
						changed = true
					}
				}
				return true
			})
			return changed
		}},
	} {
		test.Run(fixture.name, func(test *testing.T) {
			set, err := wave42LegacyGitParseSources(sources)
			if err != nil {
				test.Fatal(err)
			}
			if violations := fixture.inspect(set); len(violations) != 0 {
				test.Fatal("actual positive control rejected before AST mutation", violations)
			}
			if !fixture.mutate(set) || len(fixture.inspect(set)) == 0 {
				test.Fatal("guard did not reject changed scope algorithm")
			}
		})
	}
}

const rwa1AuthorityFixture = `package app
type RepositoryAuthorityProfile string
type RepositoryAuthorityMetadata interface { GetRepo(context.Context, domain.ContentHash) (domain.Repo, error) }
type RepositoryAuthorityRepositories interface { GetRepository(context.Context, string) (domain.Repository, error); ListMembers(context.Context, string) ([]domain.Membership, error) }
type RepositoryAuthorityTeams interface { RepositoryTeamAccess(context.Context, string, string) ([]domain.TeamRepositoryAccess, error) }
type RepositoryAuthorityOrganizations interface { RepositoryOrganizationAccess(context.Context, string, string) (domain.OrganizationRepositoryAccess, error) }
type RepositoryAuthorityAudit interface { GetNamespace(context.Context, string) (domain.Namespace, error); AppendOrganizationAudit(context.Context, domain.OrganizationAuditEvent) error }
type RepositoryAuthorityConfig struct { Profile RepositoryAuthorityProfile; Metadata RepositoryAuthorityMetadata; Repositories RepositoryAuthorityRepositories; Teams RepositoryAuthorityTeams; Organizations RepositoryAuthorityOrganizations; Locker outbound.RepositoryAccessLocker; Audit RepositoryAuthorityAudit; Transactional bool }
type RepositoryAuthority struct { metadata RepositoryAuthorityMetadata; repositories RepositoryAuthorityRepositories; roles repositoryRoleSources; locker outbound.RepositoryAccessLocker; audit RepositoryAuthorityAudit; transactional bool }
type repositoryRoleSources struct { repositories RepositoryAuthorityRepositories; teams RepositoryAuthorityTeams; organizations RepositoryAuthorityOrganizations }
type repositoryWork struct { authority *RepositoryAuthority; transactions outbound.RepositoryTransactions; revisions outbound.RepositoryRevisions; evidence outbound.EvidenceRevisions }
type repositoryAssetScope struct { work *repositoryWork }
type documentPublicationScope struct { work *repositoryWork }
type Service struct { work *repositoryWork }
func NewRepositoryAuthority(config RepositoryAuthorityConfig) (*RepositoryAuthority, error) { return &RepositoryAuthority{}, nil }
func (authority *RepositoryAuthority) AuthorizeWrite(ctx context.Context, repo domain.ContentHash) error {
 inbound.RepositoryActor(ctx); authority.metadata.GetRepo(ctx, repo); authority.locker.LockRepositoryAccess(ctx, "repository", "actor")
 authority.repositories.GetRepository(ctx, "repository"); sources := authority.roles; resolve := sources.roleFor; resolve(ctx); return nil
}
func (authority *RepositoryAuthority) AuthorizeSecrets(ctx context.Context, repo domain.ContentHash, actor string) error {
 authority.metadata.GetRepo(ctx, repo); authority.locker.LockRepositoryAccess(ctx, "repository", actor)
 authority.repositories.GetRepository(ctx, "repository"); authority.roles.roleFor(ctx); domain.CanEditSecrets(); return nil
}
func (authority *RepositoryAuthority) AuditWrite(ctx context.Context, repo domain.ContentHash) error {
 inbound.RepositoryActor(ctx); authority.metadata.GetRepo(ctx, repo); return appendRepositoryAuditFromSources(ctx, authority.repositories, authority.audit)
}
func (sources repositoryRoleSources) roleFor(ctx context.Context) { sources.repositories.ListMembers(ctx, "repository"); sources.teams.RepositoryTeamAccess(ctx, "repository", "actor"); sources.organizations.RepositoryOrganizationAccess(ctx, "repository", "actor"); domain.EffectiveRepositoryRole() }
func appendRepositoryAuditFromSources(ctx context.Context, repositories RepositoryAuthorityRepositories, audit RepositoryAuthorityAudit) error { repositories.GetRepository(ctx, "repository"); audit.GetNamespace(ctx, "namespace"); return audit.AppendOrganizationAudit(ctx, organizationAudit()) }
func organizationAudit() {}
func repositoryRoleFor(ctx context.Context, repositories outbound.RepositoryStore) { sources := repositoryRoleSources{}; sources.roleFor(ctx) }
func appendRepositoryAudit(ctx context.Context, repositories outbound.RepositoryStore) error { return appendRepositoryAuditFromSources(ctx, repositories, nil) }
func (work *repositoryWork) authorizeRepositoryWrite(ctx context.Context, repo domain.ContentHash) error { return work.authority.AuthorizeWrite(ctx, repo) }
func (work *repositoryWork) auditRepositoryWrite(ctx context.Context, repo domain.ContentHash) error { return work.authority.AuditWrite(ctx, repo) }
func (scope *repositoryAssetScope) AuthorizeAssetSecrets(ctx context.Context, input inbound.SaveSecretsInput) error { return scope.work.authority.AuthorizeSecrets(ctx, input.RepoID, input.ActorID) }
func (service *Service) repositoryAssets(profile RepositoryAssetProfile) (*RepositoryAssetCommands, error) {
 if profile == RepositoryAssetsComplete { if err := service.work.validateCompleteAuthority(); err != nil { return nil, err } }
 return NewRepositoryAssetCommands(RepositoryAssetCommandsConfig{Scope: &repositoryAssetScope{work: service.work}})
}
func (service *Service) documentPublications(profile DocumentPublicationProfile) (*DocumentPublicationCommands, error) {
 if profile == DocumentPublicationComplete { if err := service.work.validateCompleteAuthority(); err != nil { return nil, err } }
 return NewDocumentPublicationCommands(DocumentPublicationCommandsConfig{Scope: &documentPublicationScope{work: service.work}})
}
func (service *Service) RepositoryAssetCommands() { service.repositoryAssets(RepositoryAssetsComplete) }
func (service *Service) DocumentPublicationCommands() { service.documentPublications(DocumentPublicationComplete) }
func (work *repositoryWork) validateCompleteAuthority() error { _, err := NewRepositoryAuthority(RepositoryAuthorityConfig{Profile: RepositoryAuthorityComplete}); return err }
func repositoryAuthorityFromStores(repositories outbound.RepositoryStore, profile RepositoryAuthorityProfile) (*RepositoryAuthority, error) { teams, _ := repositories.(outbound.TeamStore); audit, _ := repositories.(outbound.OrganizationStore); organizations, _ := repositories.(outbound.RepositoryOrganizationAccess); locker, _ := repositories.(outbound.RepositoryAccessLocker); return NewRepositoryAuthority(RepositoryAuthorityConfig{Profile: profile, Teams: teams, Audit: audit, Organizations: organizations, Locker: locker}) }
func newRepositoryWork(metadata outbound.MetadataStore) *repositoryWork { transactions, _ := metadata.(outbound.RepositoryTransactions); revisions, _ := metadata.(outbound.RepositoryRevisions); evidence, _ := metadata.(outbound.EvidenceRevisions); authority, _ := repositoryAuthorityFromStores(nil, RepositoryAuthorityCompatibility); return &repositoryWork{authority: authority, transactions: transactions, revisions: revisions, evidence: evidence} }
func (service *Service) AuthorizeWrite() { NewService() }
func AuthorizeWrite() { NewIdentityService() }
func unrelated() { go NewService() }
`

func TestRWA1RepositoryAuthorityGuardRejectsStructuralCounterexamples(test *testing.T) {
	parse := func(source string) wave42LegacyGitSourceSet {
		test.Helper()
		set, err := wave42LegacyGitParseSources(map[string]string{"renamed_authority.go": source})
		if err != nil {
			test.Fatal(err)
		}
		return set
	}
	inspect := func(set wave42LegacyGitSourceSet) []string {
		violations := rwa1AuthorityOwnerViolations(set)
		violations = append(violations, rwa1AuthorityCompleteViolations(set)...)
		return append(violations, rwa1AuthorityCaptureViolations(set)...)
	}
	if violations := inspect(parse(rwa1AuthorityFixture)); len(violations) != 0 {
		test.Fatal("positive control rejected narrow owner, receiver alias, method value, or unrelated legacy code", violations)
	}
	test.Run("return only owner shell", func(test *testing.T) {
		set := parse(rwa1AuthorityFixture)
		set.methods["RepositoryAuthority.AuthorizeWrite"].Body.List = []ast.Stmt{&ast.ReturnStmt{Results: []ast.Expr{ast.NewIdent("nil")}}}
		matched := false
		for _, violation := range inspect(set) {
			matched = matched || strings.HasPrefix(violation, "owner-shell:")
		}
		if !matched {
			test.Fatal("guard accepted a return-only authority shell")
		}
	})
	for _, fixture := range []struct {
		name string
		from string
		to   string
		want string
	}{
		{"broad owner field", "metadata RepositoryAuthorityMetadata;", "metadata outbound.MetadataStore;", "broad-field:"},
		{"broad config field", "Teams RepositoryAuthorityTeams;", "Teams outbound.TeamStore;", "broad-field:"},
		{"service field", "transactional bool }", "transactional bool; parent *Service }", "broad-field:"},
		{"embedded broad port", "type repositoryRoleSources struct {", "type repositoryRoleSources struct { outbound.RepositoryStore;", "broad-field:"},
		{"broad interface embedding", "type RepositoryAuthorityTeams interface {", "type RepositoryAuthorityTeams interface { outbound.TeamStore;", "narrow-port:"},
		{"uninvoked closure shell", "inbound.RepositoryActor(ctx); authority.metadata.GetRepo(ctx, repo); authority.locker", "_ = func() { inbound.RepositoryActor(ctx); authority.metadata.GetRepo(ctx, repo) }; authority.locker", "owner-shell:"},
		{"parent backdelegate alias", "resolve(ctx); return nil", "resolve(ctx); parent := &Service{}; execute := parent.AuthorizeWrite; execute(); return nil", "parent-backdelegate:"},
		{"same named free function", "resolve(ctx); return nil", "resolve(ctx); AuthorizeWrite(); return nil", "parent-backdelegate:"},
		{"runtime authority assertion", "resolve(ctx); return nil", "resolve(ctx); _, _ = any(authority.repositories).(outbound.RepositoryAccessLocker); return nil", "runtime-assertion:"},
		{"runtime source discovery", "resolve(ctx); return nil", "resolve(ctx); repositoryAuthorityFromStores(RepositoryAuthorityCompatibility); return nil", "runtime-discovery:"},
		{"runtime revision assertion", "return work.authority.AuthorizeWrite(ctx, repo)", "_, _ = any(work).(outbound.RepositoryRevisions); return work.authority.AuthorizeWrite(ctx, repo)", "runtime-assertion:"},
		{"missing factory capture", "revisions, _ := metadata.(outbound.RepositoryRevisions);", "var revisions outbound.RepositoryRevisions;", "factory-capture:"},
		{"discarded factory capability", "revisions: revisions", "revisions: nil", "factory-capture:"},
		{"implicit compatibility", "repositoryAuthorityFromStores(nil, RepositoryAuthorityCompatibility)", "repositoryAuthorityFromStores(nil, RepositoryAuthorityComplete)", "factory-capture:"},
		{"weaker legacy capability discovery", "repositories.(outbound.TeamStore)", "repositories.(RepositoryAuthorityTeams)", "factory-capture:"},
		{"duplicate role implementation", "func organizationAudit() {}", "func organizationAudit() {}\nfunc duplicateRole() { domain.EffectiveRepositoryRole() }", "shared-policy:"},
		{"facade bypass", "return work.authority.AuthorizeWrite(ctx, repo)", "return nil", "direct-delegation:"},
		{"disconnected Complete profile", "if profile == RepositoryAssetsComplete", "if profile == RepositoryAssetsCompatibility", "complete-validation:"},
		{"ignored Complete error", "if err := service.work.validateCompleteAuthority(); err != nil { return nil, err }", "_ = service.work.validateCompleteAuthority()", "complete-validation:"},
		{"hidden Complete validation", "if err := service.work.validateCompleteAuthority(); err != nil { return nil, err }", "_ = func() { if err := service.work.validateCompleteAuthority(); err != nil { return } }", "complete-validation:"},
		{"shadowed Complete validator", "if err := service.work.validateCompleteAuthority(); err != nil { return nil, err }", "validateCompleteAuthority := func() error { return nil }; if err := validateCompleteAuthority(); err != nil { return nil, err }", "complete-validation:"},
		{"wrong scope work", "repositoryAssetScope{work: service.work}", "repositoryAssetScope{work: disconnected}", "complete-validation:"},
		{"wrong authority profile", "Profile: RepositoryAuthorityComplete", "Profile: RepositoryAuthorityCompatibility", "complete-validation:"},
	} {
		test.Run(fixture.name, func(test *testing.T) {
			mutated := strings.Replace(rwa1AuthorityFixture, fixture.from, fixture.to, 1)
			if mutated == rwa1AuthorityFixture {
				test.Fatal("counterexample did not alter its AST input")
			}
			matched := false
			for _, violation := range inspect(parse(mutated)) {
				matched = matched || strings.HasPrefix(violation, fixture.want)
			}
			if !matched {
				test.Fatal("guard missed targeted structural counterexample", fixture.want)
			}
		})
	}
}
