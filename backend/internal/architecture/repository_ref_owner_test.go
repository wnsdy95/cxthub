package architecture

import (
	"go/ast"
	"go/token"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

func rfc1Fields(set wave42LegacyGitSourceSet) []dpc1Finding {
	var findings []dpc1Finding
	for owner, expected := range map[string]map[string]string{
		"repositoryRefCommands": {"meta": "RepositoryRefStore", "engine": "RepositoryRefEngine", "scope": "RepositoryRefWriteScope",
			"history": "RepositoryRefHistory", "append": "RepositoryRefAppend", "revisions": "RepositoryRefRevisions",
			"notifications": "RepositoryRefNotifications", "transactional": "bool", "profile": "RepositoryRefProfile", "configurationError": "error"},
		"RepositoryRefCommandsConfig": {"Profile": "RepositoryRefProfile", "Store": "RepositoryRefStore", "Engine": "RepositoryRefEngine",
			"Scope": "RepositoryRefWriteScope", "History": "RepositoryRefHistory", "Append": "RepositoryRefAppend", "Revisions": "RepositoryRefRevisions",
			"Notifications": "RepositoryRefNotifications", "Transactional": "bool"},
	} {
		for _, detail := range wave47GitHubStructViolations(set, owner, expected) {
			findings = append(findings, dpc1Finding{"fields", detail})
		}
	}
	alias := set.types["RepositoryRefCommands"]
	if alias == nil || !alias.Assign.IsValid() || wave42LegacyGitType(alias.Type) != "repositoryRefCommands" {
		findings = append(findings, dpc1Finding{"alias", "public RepositoryRefCommands must alias the actual private repositoryRefCommands, not Service"})
	}
	for owner, expected := range map[string]map[string]string{
		"RepositoryRefStore": {
			"GetRepo": "context.Context,domain.ContentHash)(domain.Repo,error", "GetSnapshot": "context.Context,domain.ContentHash,domain.ContentHash)(domain.Snapshot,error",
			"GetRef": "context.Context,domain.ContentHash,domain.RefKind,string)(domain.Ref,error", "CompareAndSwapRef": "context.Context,domain.ContentHash,domain.Ref,domain.ContentHash)(error",
			"ApplyBranchLifecycleRef": "context.Context,domain.ContentHash,domain.Ref)(error", "ListPendings": "context.Context,domain.ContentHash)([]domain.Pending,error",
			"ListRefs": "context.Context,domain.ContentHash)([]domain.Ref,error", "ListSnapshots": "context.Context,domain.ContentHash,string)([]domain.Snapshot,error",
			"CompareAndDeletePending": "context.Context,domain.ContentHash,string,domain.ContentHash)(domain.PendingDeleteResult,error",
		},
		"RepositoryRefEngine":        {"ClassifyRefMove": "context.Context,domain.ContentHash,domain.ContentHash,domain.ContentHash)(outbound.RefMoveClass,error"},
		"RepositoryRefHistory":       {"ListHistory": "context.Context,domain.ContentHash)([]domain.HistoryEvent,error"},
		"RepositoryRefAppend":        {"AppendRef": "context.Context,domain.ContentHash,domain.Ref,domain.ContentHash)(error"},
		"RepositoryRefRevisions":     {"RepositoryRevision": "context.Context,domain.ContentHash)(domain.RepositoryRevision,error"},
		"RepositoryRefNotifications": {"NotifyRefUpdate": "context.Context,domain.ContentHash,domain.Ref,bool,bool)(error"},
		"RepositoryRefWriteScope":    {"WithinRefWrite": "context.Context,domain.ContentHash,)(inbound.UpdateRefOutput,error", "WithinRefsWrite": "context.Context,domain.ContentHash,bool,)(inbound.UpdateRefsOutput,error"},
	} {
		var methods []string
		for method := range expected {
			methods = append(methods, method)
		}
		findings = append(findings, dpc1Interface(set, owner, methods)...)
		declaration := set.types[owner]
		if declaration == nil {
			continue
		}
		contract, narrow := declaration.Type.(*ast.InterfaceType)
		if !narrow {
			continue
		}
		for _, method := range contract.Methods.List {
			signature, callable := method.Type.(*ast.FuncType)
			if !callable || len(method.Names) != 1 {
				continue
			}
			name := method.Names[0].Name
			if actual := wave46EmailFieldTypes(signature.Params) + ")(" + wave46EmailFieldTypes(signature.Results); actual != expected[name] {
				findings = append(findings, dpc1Finding{"ports", "changed ref signature " + owner + "." + name})
			}
			if owner == "RepositoryRefWriteScope" && signature.Params != nil && len(signature.Params.List) > 0 {
				callback, function := signature.Params.List[len(signature.Params.List)-1].Type.(*ast.FuncType)
				result := "inbound.UpdateRefOutput,error"
				if name == "WithinRefsWrite" {
					result = "inbound.UpdateRefsOutput,error"
				}
				if !function || wave46EmailFieldTypes(callback.Params) != "context.Context" || wave46EmailFieldTypes(callback.Results) != result {
					findings = append(findings, dpc1Finding{"ports", "changed bound-context callback " + name})
				}
			}
		}
	}
	return findings
}

func rfc1Constructor(set wave42LegacyGitSourceSet) []dpc1Finding {
	constructor := set.methods["NewRepositoryRefCommands"]
	if constructor == nil || constructor.Body == nil || wave46EmailFieldTypes(constructor.Type.Params) != "RepositoryRefCommandsConfig" || wave46EmailFieldTypes(constructor.Type.Results) != "*RepositoryRefCommands,error" || len(constructor.Type.Params.List[0].Names) != 1 {
		return []dpc1Finding{{"constructor", "missing exact public ref constructor signature"}}
	}
	config := constructor.Type.Params.List[0].Names[0].Name
	allocation := constructor.Body.End()
	var literal *ast.CompositeLit
	ast.Inspect(constructor.Body, func(node ast.Node) bool {
		if candidate, composite := node.(*ast.CompositeLit); composite && (wave42LegacyGitType(candidate.Type) == "RepositoryRefCommands" || wave42LegacyGitType(candidate.Type) == "repositoryRefCommands") && candidate.Pos() < allocation {
			literal, allocation = candidate, candidate.Pos()
		}
		return true
	})
	var findings []dpc1Finding
	if literal == nil {
		findings = append(findings, dpc1Finding{"constructor", "constructor must allocate the actual owner"})
	}
	for field, source := range map[string]string{"meta": "Store", "engine": "Engine", "scope": "Scope", "history": "History", "append": "Append", "revisions": "Revisions", "notifications": "Notifications", "transactional": "Transactional", "profile": "Profile"} {
		if workerSelectorPath(rwa1AuthorityField(literal, field)) != config+"."+source {
			findings = append(findings, dpc1Finding{"constructor", "constructor discards configured " + source})
		}
	}
	guarded := map[string]bool{}
	var completeBlocks []*ast.BlockStmt
	ast.Inspect(constructor.Body, func(node ast.Node) bool {
		conditional, condition := node.(*ast.IfStmt)
		if !condition || conditional.End() >= allocation {
			return true
		}
		if dpc1Compare(conditional.Cond, token.EQL, config+".Profile", "RepositoryRefComplete") {
			completeBlocks = append(completeBlocks, conditional.Body)
		}
		if !dpc1ReturnsError(conditional.Body, "domain.ErrValidation") {
			return true
		}
		for _, required := range []string{"Store", "Scope"} {
			guarded[required] = guarded[required] || dpc1Joined(conditional.Cond, token.LOR, func(expression ast.Expr) bool {
				return rac1AssetAbsent(expression, config+"."+required)
			})
		}
		profiles := true
		for _, profile := range []string{"RepositoryRefCompatibility", "RepositoryRefComplete"} {
			profiles = profiles && dpc1Joined(conditional.Cond, token.LAND, func(expression ast.Expr) bool {
				return dpc1Compare(expression, token.NEQ, config+".Profile", profile)
			})
		}
		guarded["profiles"] = guarded["profiles"] || profiles
		return true
	})
	ast.Inspect(constructor.Body, func(node ast.Node) bool {
		loop, ranged := node.(*ast.RangeStmt)
		if !ranged || loop.End() >= allocation {
			return true
		}
		dependency, named := loop.Value.(*ast.Ident)
		values, listed := loop.X.(*ast.CompositeLit)
		if !named || !listed {
			return true
		}
		for _, statement := range loop.Body.List {
			conditional, condition := statement.(*ast.IfStmt)
			if !condition {
				continue
			}
			typedNil := dpc1ReturnsError(conditional.Body, "domain.ErrValidation") && dpc1Joined(conditional.Cond, token.LAND, func(expression ast.Expr) bool {
				return dpc1Compare(expression, token.NEQ, dependency.Name, "nil")
			}) && dpc1Joined(conditional.Cond, token.LAND, func(expression ast.Expr) bool { return rac1AssetAbsent(expression, dependency.Name) })
			completeProfile := wave48Inside(loop, completeBlocks) || dpc1Joined(conditional.Cond, token.LAND, func(expression ast.Expr) bool {
				return dpc1Compare(expression, token.EQL, config+".Profile", "RepositoryRefComplete")
			})
			complete := dpc1ReturnsError(conditional.Body, "domain.ErrCatalogUnsupported") && completeProfile && dpc1Joined(conditional.Cond, token.LAND, func(expression ast.Expr) bool { return dpc1Compare(expression, token.EQL, dependency.Name, "nil") })
			for _, value := range values.Elts {
				if typedNil {
					guarded["typed nil "+workerSelectorPath(value)] = true
				}
				if complete {
					guarded["complete "+workerSelectorPath(value)] = true
				}
			}
		}
		return true
	})
	for _, requirement := range []string{"Store", "Scope", "profiles"} {
		if !guarded[requirement] {
			findings = append(findings, dpc1Finding{"constructor", "missing pre-allocation rejection: " + requirement})
		}
	}
	for _, dependency := range []string{"Engine", "History", "Append", "Revisions", "Notifications"} {
		for _, rejection := range []string{"typed nil ", "complete "} {
			if !guarded[rejection+config+"."+dependency] {
				findings = append(findings, dpc1Finding{"constructor", "missing " + rejection + dependency + " rejection"})
			}
		}
	}
	for _, function := range dpc1Reachable(set, "NewRepositoryRefCommands") {
		for operation := range rac1AssetCalls(set, function, function.Body, nil) {
			if strings.HasPrefix(operation, "RepositoryRef") || operation == "newRepositoryWork" || operation == "repositoryWriteWithWork" || strings.HasSuffix(operation, ".Run") {
				findings = append(findings, dpc1Finding{"constructor", "constructor performs IO/work: " + operation})
			}
		}
		ast.Inspect(function.Body, func(node ast.Node) bool {
			if _, background := node.(*ast.GoStmt); background {
				findings = append(findings, dpc1Finding{"constructor", "constructor starts hidden work"})
			}
			return true
		})
	}
	presence := set.methods["rootAdapterPresent"]
	if presence == nil || !wave42LegacyGitCalls(presence.Body)["IsNil"] || !wave42LegacyGitCalls(presence.Body)["ValueOf"] {
		findings = append(findings, dpc1Finding{"constructor", "missing genuine typed-nil validation"})
	}
	return findings
}

func rfc1RuntimeDiscovery(set wave42LegacyGitSourceSet) []dpc1Finding {
	var findings []dpc1Finding
	for entry := range set.methods {
		if !strings.HasPrefix(entry, "repositoryRefCommands.") {
			continue
		}
		for target, function := range dpc1Reachable(set, entry) {
			if target == "repositoryRefsFromStores" || target == "NewRepositoryRefCommands" || target == "newRepositoryWork" || strings.HasPrefix(target, "Service.") || strings.HasPrefix(target, "IdentityService.") {
				findings = append(findings, dpc1Finding{"reverse", entry + " reaches factory/parent " + target})
			}
			for operation := range rac1AssetCalls(set, function, function.Body, nil) {
				if strings.HasPrefix(operation, "reflect.") {
					findings = append(findings, dpc1Finding{"runtime-assertion", "reflection-based capability discovery in " + target})
				}
			}
			ast.Inspect(function.Body, func(node ast.Node) bool {
				switch value := node.(type) {
				case *ast.TypeAssertExpr, *ast.TypeSwitchStmt:
					findings = append(findings, dpc1Finding{"runtime-assertion", "capability discovery in actual algorithm " + target})
				case *ast.GoStmt, *ast.ChanType:
					findings = append(findings, dpc1Finding{"reverse", "independent background work in " + target})
				case *ast.Ident:
					switch value.Name {
					case "Service", "NewService", "IdentityService", "NewIdentityService", "MetadataStore", "GitEngine", "repositoryWork":
						findings = append(findings, dpc1Finding{"reverse", "deep broad state/parent retained in " + target + ": " + value.Name})
					}
				}
				return true
			})
		}
	}
	return findings
}

func rfc1CAS(set wave42LegacyGitSourceSet) []dpc1Finding {
	function := set.methods["repositoryRefCommands.updateRef"]
	if function == nil || function.Body == nil {
		return []dpc1Finding{{"cas", "missing actual ref algorithm"}}
	}
	observed := map[string]bool{}
	ast.Inspect(function.Body, func(node ast.Node) bool {
		assignment, assigned := node.(*ast.AssignStmt)
		if !assigned || len(assignment.Lhs) != 1 || len(assignment.Rhs) != 1 {
			return true
		}
		if target, selected := assignment.Rhs[0].(*ast.SelectorExpr); selected && target.Sel.Name == "Target" {
			if current := strings.TrimSuffix(workerSelectorPath(target), ".Target"); current != "" {
				ast.Inspect(function.Body, func(nested ast.Node) bool {
					if read, bound := nested.(*ast.AssignStmt); bound && len(read.Lhs) > 0 && workerSelectorPath(read.Lhs[0]) == current && len(read.Rhs) == 1 {
						if call, called := read.Rhs[0].(*ast.CallExpr); called && wave42LegacyGitFunctionName(call.Fun) == "GetRef" {
							observed[workerSelectorPath(assignment.Lhs[0])] = true
						}
					}
					return true
				})
			}
		}
		return true
	})
	var findings []dpc1Finding
	count := 0
	rac1AssetCalls(set, function, function.Body, func(call *ast.CallExpr, target string) {
		if target != "RepositoryRefStore.CompareAndSwapRef" {
			return
		}
		count++
		if len(call.Args) != 4 {
			findings = append(findings, dpc1Finding{"cas", "changed ref CAS arity"})
			return
		}
		expected := workerSelectorPath(call.Args[3])
		literal, empty := call.Args[3].(*ast.BasicLit)
		empty = empty && literal.Kind == token.STRING && literal.Value == `""`
		if !observed[expected] && !empty {
			findings = append(findings, dpc1Finding{"cas", "CAS no longer uses observed current target: " + expected})
		}
	})
	if count < 4 {
		findings = append(findings, dpc1Finding{"cas", "HEAD/force/tag/fast-forward CAS branches lost"})
	}
	return findings
}

func rfc1Owner(set wave42LegacyGitSourceSet) []dpc1Finding {
	findings := rfc1Fields(set)
	findings = append(findings, rfc1Constructor(set)...)
	findings = append(findings, rfc1RuntimeDiscovery(set)...)
	findings = append(findings, rfc1CAS(set)...)
	for entry, operations := range map[string][]string{
		"UpdateRef":  {"RepositoryRefWriteScope.WithinRefWrite", "repositoryRefCommands.updateRefWithPending"},
		"UpdateRefs": {"RepositoryRefWriteScope.WithinRefsWrite", "repositoryRefCommands.updateRefs"},
		"updateRef": {"domain.ValidateOptionalContentHash", "domain.ValidateContentHash", "domain.ValidateRef", "repositoryRefCommands.checkContextWrite",
			"RepositoryRefStore.GetRepo", "RepositoryRefStore.GetSnapshot", "RepositoryRefStore.GetRef", "RepositoryRefStore.CompareAndSwapRef",
			"RepositoryRefStore.ApplyBranchLifecycleRef", "RepositoryRefEngine.ClassifyRefMove", "RepositoryRefRevisions.RepositoryRevision", "domain.ParseBranchLifecycleRef", "repositoryRefCommands.appendDiverged"},
		"appendDiverged":                 {"RepositoryRefAppend.AppendRef", "RepositoryRefNotifications.NotifyRefUpdate"},
		"checkContextWrite":              {"RepositoryRefStore.GetRepo", "RepositoryRefStore.GetRef", "RepositoryRefHistory.ListHistory", "domain.ContextRefWriteNeedsState", "domain.ValidateContextRefWrite"},
		"updateRefs":                     {"domain.ValidateContentHash", "repositoryRefCommands.updateRef", "repositoryRefCommands.reconcileSharedPendingPointers", "errors.Is", "strings.Join"},
		"reconcileSharedPendingPointers": {"RepositoryRefStore.ListPendings", "RepositoryRefStore.ListRefs", "RepositoryRefStore.ListSnapshots", "RepositoryRefStore.CompareAndDeletePending", "sharedTimelineRef"},
	} {
		calls := dpc1ExecutionCalls(set, "repositoryRefCommands."+entry)
		for _, operation := range operations {
			if !calls[operation] {
				findings = append(findings, dpc1Finding{"payload", "actual " + entry + " lost " + operation})
			}
		}
	}
	for public, scope := range map[string]string{"UpdateRef": "WithinRefWrite", "UpdateRefs": "WithinRefsWrite"} {
		function := set.methods["repositoryRefCommands."+public]
		scoped, clearsCompleteError := false, false
		rac1AssetCalls(set, function, bodyOfDPC1(function), func(call *ast.CallExpr, target string) {
			if target != "RepositoryRefWriteScope."+scope {
				return
			}
			for _, argument := range call.Args {
				callback, literal := argument.(*ast.FuncLit)
				if !literal || callback.Type.Params == nil || len(callback.Type.Params.List) != 1 || len(callback.Type.Params.List[0].Names) != 1 {
					continue
				}
				bound := callback.Type.Params.List[0].Names[0].Name
				rac1AssetCalls(set, function, callback.Body, func(nested *ast.CallExpr, selected string) {
					payload := "repositoryRefCommands.updateRefs"
					if public == "UpdateRef" {
						payload = "repositoryRefCommands.updateRefWithPending"
					}
					scoped = scoped || selected == payload && len(nested.Args) > 0 && workerSelectorPath(nested.Args[0]) == bound
				})
			}
		})
		if !scoped {
			findings = append(findings, dpc1Finding{"scope", public + " lost bound-context execution"})
		}
		if function != nil && function.Body != nil && function.Recv != nil && len(function.Recv.List[0].Names) == 1 {
			receiver := function.Recv.List[0].Names[0].Name
			ast.Inspect(function.Body, func(node ast.Node) bool {
				conditional, condition := node.(*ast.IfStmt)
				if !condition || !dpc1Joined(conditional.Cond, token.LAND, func(expression ast.Expr) bool {
					return dpc1Compare(expression, token.EQL, receiver+".profile", "RepositoryRefComplete")
				}) || len(conditional.Body.List) != 1 {
					return true
				}
				returned, returns := conditional.Body.List[0].(*ast.ReturnStmt)
				if returns && len(returned.Results) == 2 {
					output := rwa1AuthorityComposite(returned.Results[0], "inbound."+public+"Output")
					clearsCompleteError = clearsCompleteError || output != nil && len(output.Elts) == 0 && workerSelectorPath(returned.Results[1]) != "nil"
				}
				return true
			})
		}
		if !clearsCompleteError {
			findings = append(findings, dpc1Finding{"complete-output", public + " must clear success-shaped output on Complete error"})
		}
	}
	batch := set.methods["repositoryRefCommands.updateRefs"]
	if batch == nil || !wave42LegacyGitMentions(batch.Body, "MaxRefBatchUpdates") || !wave42LegacyGitMentions(batch.Body, "ErrNonFastForward") {
		findings = append(findings, dpc1Finding{"batch", "batch lost its bound or non-fast-forward aggregation"})
	}
	batchCalls := rac1AssetExecutionCalls(set, "repositoryRefCommands.updateRefs", map[string]bool{})
	if batchCalls["repositoryRefCommands.reconcileSharedPendingPointers"] != 1 || batchCalls["repositoryRefCommands.updateRef"] != 1 || batchCalls["repositoryRefCommands.UpdateRef"] != 0 || batchCalls["repositoryRefCommands.updateRefWithPending"] != 0 {
		findings = append(findings, dpc1Finding{"batch", "batch must update directly and reconcile pending pointers once, not per item"})
	}
	pending := set.methods["repositoryRefCommands.reconcileSharedPendingPointers"]
	if pending == nil || !wave42LegacyGitCalls(pending.Body)["ReachabilityParents"] {
		findings = append(findings, dpc1Finding{"pending", "pending walk lost shared Parents + GraftParents reachability"})
	}
	rac1AssetCalls(set, pending, bodyOfDPC1(pending), func(call *ast.CallExpr, target string) {
		if target == "RepositoryRefStore.CompareAndDeletePending" {
			if len(call.Args) != 4 || !strings.HasSuffix(workerSelectorPath(call.Args[2]), ".SessionID") || !strings.HasSuffix(workerSelectorPath(call.Args[3]), ".Target") {
				findings = append(findings, dpc1Finding{"pending", "pending deletion lost exact session/target qualification"})
			}
		}
		for _, forbidden := range []string{"DeletePending", "DeleteSnapshot", "DeleteDoc", "DeleteSession", "UpdateRef"} {
			if strings.HasSuffix(target, "."+forbidden) {
				findings = append(findings, dpc1Finding{"pending", "pending reconciliation deletes immutable history or re-enters public scope: " + target})
			}
		}
	})
	return findings
}

func rfc1UsesPassedWork(set wave42LegacyGitSourceSet, expression ast.Expr, expected string) bool {
	call, called := expression.(*ast.CallExpr)
	if !called || expected == "" {
		return false
	}
	name := wave42LegacyGitFunctionName(call.Fun)
	if name != "repositoryRefsFromStores" && name != "repositoryRefCommandsForWork" {
		return false
	}
	function := set.methods[name]
	if function == nil || function.Type.Params == nil {
		return false
	}
	parameter := ""
	for index, field := range function.Type.Params.List {
		if wave42LegacyGitType(field.Type) == "*repositoryWork" && len(field.Names) == 1 && index < len(call.Args) && workerSelectorPath(call.Args[index]) == expected {
			parameter = field.Names[0].Name
		}
	}
	if parameter == "" || name == "repositoryRefsFromStores" {
		return parameter != ""
	}
	forwarded := false
	rac1AssetCalls(set, function, function.Body, func(nested *ast.CallExpr, target string) {
		if target == "repositoryRefsFromStores" {
			forwarded = forwarded || rfc1UsesPassedWork(set, nested, parameter)
		}
	})
	return forwarded
}

func rfc1ScopeAndConsumers(set wave42LegacyGitSourceSet) []dpc1Finding {
	var findings []dpc1Finding
	if wave44PreparationMember(set, "repositoryRefScope", "work", false, map[string]bool{}) != "repositoryWork" {
		findings = append(findings, dpc1Finding{"shared-work", "ref scope must retain the original repositoryWork outside the owner"})
	}
	findings = append(findings, dpc1Fields(set, "repositoryRefScope", []string{"*repositoryWork", "RepositoryAuthorityMetadata", "repositoryRefRepositoryReader", "repositoryRefHistoryReader", "RepositoryNotificationQueue"}, []string{"*repositoryWork", "RepositoryAuthorityMetadata", "repositoryRefRepositoryReader", "repositoryRefHistoryReader", "RepositoryNotificationQueue"})...)
	findings = append(findings, dpc1Interface(set, "repositoryRefRepositoryReader", []string{"GetRepository"})...)
	findings = append(findings, dpc1Interface(set, "repositoryRefHistoryReader", []string{"ListHistoryEvents"})...)
	for _, entry := range []string{"repositoryRefScope.NotifyRefUpdate", "notifyRefUpdateFromStores"} {
		if !dpc1ExecutionCalls(set, entry)["notifyRefUpdateWithSources"] {
			findings = append(findings, dpc1Finding{"notifications", entry + " duplicates or bypasses shared notification formatting"})
		}
	}
	for entry, audit := range map[string]string{"WithinRefWrite": "context.ref.updated", "WithinRefsWrite": "context.refs.updated"} {
		function := set.methods["repositoryRefScope."+entry]
		shared, namedAudit := false, false
		rac1AssetCalls(set, function, bodyOfDPC1(function), func(call *ast.CallExpr, target string) {
			if target == "repositoryWriteWithWork" && len(call.Args) > 1 && function.Recv != nil && len(function.Recv.List[0].Names) == 1 {
				shared = workerSelectorPath(call.Args[1]) == function.Recv.List[0].Names[0].Name+".work"
			}
			if target == "auditOperation" && len(call.Args) == 2 {
				if literal, quoted := call.Args[1].(*ast.BasicLit); quoted {
					value, err := strconv.Unquote(literal.Value)
					namedAudit = namedAudit || err == nil && value == audit
				}
			}
		})
		if !shared || !namedAudit {
			findings = append(findings, dpc1Finding{"shared-work", entry + " lost existing repository write/audit policy"})
		}
		if entry == "WithinRefsWrite" && !dpc1ExecutionCalls(set, "repositoryRefScope."+entry)["pendingWriteContext"] {
			findings = append(findings, dpc1Finding{"shared-work", "empty retry lost pending-only revision policy"})
		}
	}
	factory := set.methods["repositoryRefsFromStores"]
	passedWork := ""
	if factory != nil && factory.Type.Params != nil {
		for _, parameter := range factory.Type.Params.List {
			if wave42LegacyGitType(parameter.Type) == "*repositoryWork" && len(parameter.Names) == 1 {
				passedWork = parameter.Names[0].Name
			}
		}
	}
	captured := false
	if factory != nil {
		ast.Inspect(factory.Body, func(node ast.Node) bool {
			if literal, composite := node.(*ast.CompositeLit); composite && wave42LegacyGitType(literal.Type) == "repositoryRefScope" {
				captured = captured || passedWork != "" && workerSelectorPath(rwa1AuthorityField(literal, "work")) == passedWork
			}
			if call, called := node.(*ast.CallExpr); called && wave42LegacyGitFunctionName(call.Fun) == "newRepositoryWork" {
				findings = append(findings, dpc1Finding{"shared-work", "ref factory reconstructs repositoryWork instead of capturing passed work"})
			}
			return true
		})
	}
	if !captured || !dpc1ExecutionCalls(set, "repositoryRefsFromStores")["NewRepositoryRefCommands"] {
		findings = append(findings, dpc1Finding{"shared-work", "store factory does not bind passed work into the actual ref scope/constructor"})
	}
	assembly := set.methods["assemblePRPromotionWorker"]
	workerWork := ""
	if assembly != nil && assembly.Type.Params != nil {
		for _, parameter := range assembly.Type.Params.List {
			if wave42LegacyGitType(parameter.Type) == "*repositoryWork" && len(parameter.Names) == 1 {
				workerWork = parameter.Names[0].Name
			}
		}
	}
	assembledRefs, boundRefs, boundWork := map[string]bool{}, false, false
	var workerLiteral *ast.CompositeLit
	if assembly != nil {
		ast.Inspect(assembly.Body, func(node ast.Node) bool {
			if assignment, assigned := node.(*ast.AssignStmt); assigned && len(assignment.Lhs) > 0 && len(assignment.Rhs) == 1 {
				if rfc1UsesPassedWork(set, assignment.Rhs[0], workerWork) {
					assembledRefs[workerSelectorPath(assignment.Lhs[0])] = true
				}
			}
			if literal, composite := node.(*ast.CompositeLit); composite && wave42LegacyGitType(literal.Type) == "PRPromotionWorker" {
				workerLiteral = literal
				boundWork = boundWork || workerWork != "" && workerSelectorPath(rwa1AuthorityField(literal, "work")) == workerWork
				refs := rwa1AuthorityField(literal, "refs")
				boundRefs = boundRefs || assembledRefs[workerSelectorPath(refs)] || rfc1UsesPassedWork(set, refs, workerWork)
			}
			return true
		})
	}
	if !boundRefs || !boundWork {
		findings = append(findings, dpc1Finding{"worker", "PRPromotionWorker.refs must come from repositoryRefsFromStores with the same passed work"})
	}
	completeWorker, delegated, assembled, mapped := false, false, false, false
	completeValidation, workerAssembly := token.NoPos, token.NoPos
	workerFactory := set.methods["NewPRPromotionWorker"]
	helper := set.methods["newPRPromotionWorkerWithWork"]
	_, helperWork := hrc1Work(helper)
	if workerFactory != nil && workerFactory.Body != nil && wave46EmailFieldTypes(workerFactory.Type.Params) == "PRPromotionWorkerConfig" && len(workerFactory.Type.Params.List[0].Names) == 1 {
		config := workerFactory.Type.Params.List[0].Names[0].Name
		for _, statement := range workerFactory.Body.List {
			if returned, returns := statement.(*ast.ReturnStmt); returns && len(returned.Results) == 1 {
				if call, called := returned.Results[0].(*ast.CallExpr); called && workerSelectorPath(call.Fun) == "newPRPromotionWorkerWithWork" && len(call.Args) == 2 {
					delegated = workerSelectorPath(call.Args[0]) == config && workerSelectorPath(call.Args[1]) == "nil"
				}
			}
		}
	}
	if delegated && helper != nil && helper.Body != nil && helperWork != "" && len(helper.Type.Params.List[0].Names) == 1 {
		config := helper.Type.Params.List[0].Names[0].Name
		for _, statement := range helper.Body.List {
			if returned, returns := statement.(*ast.ReturnStmt); returns && len(returned.Results) == 2 && workerSelectorPath(returned.Results[1]) == "nil" {
				if call, called := returned.Results[0].(*ast.CallExpr); called && workerSelectorPath(call.Fun) == "assemblePRPromotionWorker" && len(call.Args) == 2 && workerSelectorPath(call.Args[0]) == config {
					assembled = prc1UsesWork(set, helper, call, "PRPromotionWorker", helperWork, returned.Pos(), 0)
					workerAssembly = returned.Pos()
				}
			}
			conditional, condition := statement.(*ast.IfStmt)
			if !condition || !dpc1Compare(conditional.Cond, token.EQL, config+".Profile", "PRPromotionComplete") {
				continue
			}
			ast.Inspect(conditional.Body, func(node ast.Node) bool {
				check, checks := node.(*ast.IfStmt)
				if !checks {
					return true
				}
				assignment, assigned := check.Init.(*ast.AssignStmt)
				if !assigned || len(assignment.Lhs) != 2 || len(assignment.Rhs) != 1 {
					return true
				}
				call, called := assignment.Rhs[0].(*ast.CallExpr)
				if !called || workerSelectorPath(call.Fun) != "repositoryRefsFromStores" || len(call.Args) != 5 {
					return true
				}
				errorName := workerSelectorPath(assignment.Lhs[1])
				if workerSelectorPath(call.Args[0]) == config+".Metadata" && workerSelectorPath(call.Args[1]) == config+".Engine" && workerSelectorPath(call.Args[2]) == config+".Repositories" && prc1UsesWork(set, helper, call.Args[3], "", helperWork, call.Pos(), 0) && workerSelectorPath(call.Args[4]) == "RepositoryRefComplete" && dpc1Compare(check.Cond, token.NEQ, errorName, "nil") && dpc1ReturnsError(check.Body, errorName) {
					completeWorker, completeValidation = true, conditional.End()
				}
				return true
			})
		}
	}
	if assembly != nil && assembly.Body != nil && workerLiteral != nil && len(assembly.Type.Params.List[0].Names) == 1 {
		config := assembly.Type.Params.List[0].Names[0].Name
		if refs, called := rwa1AuthorityField(workerLiteral, "refs").(*ast.CallExpr); called && workerSelectorPath(refs.Fun) == "repositoryRefCommandsForWork" && len(refs.Args) == 5 {
			profile := workerSelectorPath(refs.Args[4])
			for _, statement := range assembly.Body.List {
				if conditional, condition := statement.(*ast.IfStmt); condition && conditional.End() < refs.Pos() && conditional.Else == nil && dpc1Compare(conditional.Cond, token.EQL, config+".Profile", "PRPromotionComplete") {
					complete := prc1WorkBindings(conditional.Body, profile, workerWork, conditional.End(), []ast.Expr{nil})
					if len(complete) == 1 && workerSelectorPath(complete[0]) == "RepositoryRefComplete" {
						defaults := prc1WorkBindings(assembly.Body, profile, workerWork, conditional.Pos(), []ast.Expr{nil})
						mapped = len(defaults) == 1 && workerSelectorPath(defaults[0]) == "RepositoryRefCompatibility"
					}
				}
			}
			profiles := prc1WorkBindings(assembly.Body, profile, workerWork, refs.Pos(), []ast.Expr{nil})
			compatibility, complete := false, false
			for _, value := range profiles {
				compatibility = compatibility || workerSelectorPath(value) == "RepositoryRefCompatibility"
				complete = complete || workerSelectorPath(value) == "RepositoryRefComplete"
				mapped = mapped && (workerSelectorPath(value) == "RepositoryRefCompatibility" || workerSelectorPath(value) == "RepositoryRefComplete")
			}
			mapped = mapped && compatibility && complete
		}
	}
	if !completeWorker || !assembled || !mapped || completeValidation >= workerAssembly {
		findings = append(findings, dpc1Finding{"worker", "PR factory/assembler lost complete validation or explicit profile mapping: delegated=" + strconv.FormatBool(delegated) + ", complete=" + strconv.FormatBool(completeWorker) + ", assembled=" + strconv.FormatBool(assembled) + ", mapped=" + strconv.FormatBool(mapped) + ", ordered=" + strconv.FormatBool(completeValidation < workerAssembly)})
	}
	for entry, owned := range map[string]string{"PRPromotionWorker.UpdateRef": "repositoryRefCommands.UpdateRef", "PRPromotionWorker.promoteBoundPRInTransaction": "repositoryRefCommands.updateRef"} {
		if dpc1Reachable(set, entry)[owned] == nil {
			findings = append(findings, dpc1Finding{"worker", entry + " bypasses the actual ref owner"})
		}
	}
	public := set.methods["Service.RepositoryRefCommands"]
	if public == nil || wave46EmailFieldTypes(public.Type.Results) != "inbound.RepositoryRefCommands,error" || !dpc1ExecutionCalls(set, "Service.RepositoryRefCommands")["repositoryRefsFromStores"] {
		findings = append(findings, dpc1Finding{"facade", "public Service factory must return the narrow inbound ref contract"})
	}
	completePublic := false
	rac1AssetCalls(set, public, bodyOfDPC1(public), func(call *ast.CallExpr, target string) {
		completePublic = completePublic || target == "repositoryRefsFromStores" && len(call.Args) > 0 && workerSelectorPath(call.Args[len(call.Args)-1]) == "RepositoryRefComplete"
	})
	if !completePublic {
		findings = append(findings, dpc1Finding{"facade", "public Service factory must explicitly select Complete"})
	}
	compatibility := set.methods["repositoryRefCommandsForWork"]
	sentinel, constructionError := false, ""
	if compatibility != nil && compatibility.Body != nil {
		ast.Inspect(compatibility.Body, func(node ast.Node) bool {
			if assignment, assigned := node.(*ast.AssignStmt); assigned && len(assignment.Lhs) == 2 && len(assignment.Rhs) == 1 {
				if call, called := assignment.Rhs[0].(*ast.CallExpr); called && wave42LegacyGitFunctionName(call.Fun) == "repositoryRefsFromStores" {
					constructionError = workerSelectorPath(assignment.Lhs[1])
				}
			}
			if conditional, condition := node.(*ast.IfStmt); condition && constructionError != "" && dpc1Compare(conditional.Cond, token.NEQ, constructionError, "nil") && len(conditional.Body.List) == 1 {
				if returned, returns := conditional.Body.List[0].(*ast.ReturnStmt); returns && len(returned.Results) == 1 {
					literal := rwa1AuthorityComposite(returned.Results[0], "repositoryRefCommands")
					sentinel = sentinel || literal != nil && len(literal.Elts) == 1 && workerSelectorPath(rwa1AuthorityField(literal, "configurationError")) == constructionError
				}
			}
			return true
		})
	}
	if !sentinel || !dpc1ExecutionCalls(set, "repositoryRefCommandsForWork")["repositoryRefsFromStores"] {
		findings = append(findings, dpc1Finding{"facade", "compatibility assembly must preserve construction failure, not create a weak fallback"})
	}
	for _, entry := range []string{"UpdateRef", "UpdateRefs", "updateRef", "updateRefWithPending", "updateRefs", "appendDiverged", "reconcileSharedPendingPointers", "checkContextWrite"} {
		function := set.methods["Service."+entry]
		if function == nil {
			continue
		}
		if dpc1Reachable(set, "Service."+entry)["repositoryRefCommands."+entry] == nil {
			findings = append(findings, dpc1Finding{"facade", "compatibility Service." + entry + " bypasses actual owner"})
		}
		ast.Inspect(function.Body, func(node ast.Node) bool {
			switch node.(type) {
			case *ast.ForStmt, *ast.RangeStmt, *ast.FuncLit, *ast.GoStmt, *ast.DeferStmt, *ast.TypeSwitchStmt:
				findings = append(findings, dpc1Finding{"facade", "ref algorithm/scope retained on Service." + entry})
			}
			return true
		})
	}
	return findings
}

func rfc1Origin(function *ast.FuncDecl, expression ast.Expr, before token.Pos) string {
	walker := rac1AssetCallVisitor{visitor: wave45PRCallVisitor{bindings: map[string]string{}}, origins: map[string]string{}}
	for _, fields := range []*ast.FieldList{function.Recv, function.Type.Params} {
		if fields != nil {
			for _, field := range fields.List {
				for _, name := range field.Names {
					walker.visitor.bindings[name.Name] = wave44PreparationNamedType(field.Type)
				}
			}
		}
	}
	ast.Inspect(function.Body, func(node ast.Node) bool {
		if node == nil || node.Pos() >= before {
			return false
		}
		if assignment, assigned := node.(*ast.AssignStmt); assigned {
			for index, left := range assignment.Lhs {
				if name, named := left.(*ast.Ident); named && index < len(assignment.Rhs) {
					walker.origins[name.Name] = rac1AssetCallOrigin(walker, assignment.Rhs[index])
				}
			}
		}
		return true
	})
	return rac1AssetCallOrigin(walker, expression)
}

func rfc1HTTP(set wave42LegacyGitSourceSet) []dpc1Finding {
	var findings []dpc1Finding
	if wave44PreparationMember(set, "Server", "repositoryRefs", false, map[string]bool{}) != "inbound.RepositoryRefCommands" {
		findings = append(findings, dpc1Finding{"http", "Server must retain narrow injected repositoryRefs"})
	}
	setter := set.methods["Server.SetRepositoryRefCommands"]
	bound := false
	if setter != nil && setter.Body != nil && setter.Recv != nil && len(setter.Recv.List[0].Names) == 1 && setter.Type.Params != nil {
		receiver := setter.Recv.List[0].Names[0].Name
		ast.Inspect(setter.Body, func(node ast.Node) bool {
			if assignment, assigned := node.(*ast.AssignStmt); assigned && len(assignment.Lhs) == 1 && len(assignment.Rhs) == 1 && workerSelectorPath(assignment.Lhs[0]) == receiver+".repositoryRefs" {
				for _, parameter := range setter.Type.Params.List {
					if wave42LegacyGitType(parameter.Type) == "inbound.RepositoryRefCommands" {
						for _, name := range parameter.Names {
							bound = bound || workerSelectorPath(assignment.Rhs[0]) == name.Name
						}
					}
				}
			}
			return true
		})
	}
	if !bound || !dpc1ExecutionCalls(set, "Server.SetRepositoryRefCommands")["reflect.Value.IsNil"] && (setter == nil || !wave42LegacyGitCalls(setter.Body)["IsNil"]) {
		findings = append(findings, dpc1Finding{"http", "setter must retain injection and reject typed nils"})
	}
	for route, expected := range map[string][]string{
		"PUT /api/v1/repos/{repoID}/refs/{kind}/{name...}":             {"putRef", "UpdateRef"},
		"POST /api/v1/repos/{repoID}/conditional-appends/v1/{name...}": {"conditionalAppendV1", "UpdateRef"},
		"POST /api/v1/repos/{repoID}/refs/batch":                       {"putRefs", "UpdateRefs"},
	} {
		registered := false
		for _, function := range dpc1Reachable(set, "Server.Handler") {
			rac1AssetCalls(set, function, function.Body, func(call *ast.CallExpr, target string) {
				if !strings.HasSuffix(target, ".HandleFunc") || len(call.Args) != 2 {
					return
				}
				literal, quoted := call.Args[0].(*ast.BasicLit)
				if !quoted || literal.Kind != token.STRING {
					return
				}
				pattern, err := strconv.Unquote(literal.Value)
				if err == nil && pattern == route {
					registered = rfc1Origin(function, call.Args[1], call.Pos()) == "Server."+expected[0]
					ast.Inspect(call.Args[1], func(node ast.Node) bool {
						if selector, selected := node.(*ast.SelectorExpr); selected && selector.Sel.Name == expected[0] {
							registered = registered || rfc1Origin(function, selector, call.Pos()) == "Server."+expected[0]
						}
						return true
					})
				}
			})
		}
		used := false
		for _, function := range dpc1Reachable(set, "Server."+expected[0]) {
			rac1AssetCalls(set, function, function.Body, func(call *ast.CallExpr, target string) {
				if !strings.HasSuffix(target, ".UpdateRef") && !strings.HasSuffix(target, ".UpdateRefs") {
					return
				}
				correct := target == "inbound.RepositoryRefCommands."+expected[1] && rfc1Origin(function, call.Fun, call.Pos()) == "Server.repositoryRefs."+expected[1]
				used = used || correct
				if !correct {
					findings = append(findings, dpc1Finding{"http", route + " bypasses injected repositoryRefs: " + target})
				}
			})
		}
		if !registered || !used {
			findings = append(findings, dpc1Finding{"http", "disconnected live route/owner: " + route})
		}
	}
	return findings
}

func rfc1Initializes(node ast.Node) bool {
	if node == nil {
		return false
	}
	found := false
	ast.Inspect(node, func(nested ast.Node) bool {
		if identifier, named := nested.(*ast.Ident); named {
			switch identifier.Name {
			case "RepositoryRefs", "repositoryRefs", "RepositoryRefCommands", "NewRepositoryRefCommands", "ConfigureRepositoryRefCommands", "SetRepositoryRefCommands":
				found = true
			}
		}
		return true
	})
	return found
}

func rfc1Runtime(set wave42LegacyGitSourceSet) []dpc1Finding {
	var findings []dpc1Finding
	if wave44PreparationMember(set, "Runtime", "RepositoryRefs", false, map[string]bool{}) != "inbound.RepositoryRefCommands" {
		findings = append(findings, dpc1Finding{"runtime", "Runtime lost narrow RepositoryRefs"})
	}
	for entry, queryOnly := range map[string]string{"Open": "false", "OpenMCP": "true"} {
		correct := false
		function := set.methods[entry]
		rac1AssetCalls(set, function, bodyOfDPC1(function), func(call *ast.CallExpr, target string) {
			correct = correct || target == "openRuntime" && len(call.Args) > 0 && workerSelectorPath(call.Args[len(call.Args)-1]) == queryOnly
		})
		if !correct {
			findings = append(findings, dpc1Finding{"runtime", "API/MCP entry lost explicit query-only profile: " + entry})
		}
	}
	function := set.methods["openRuntime"]
	if function == nil || function.Body == nil {
		return append(findings, dpc1Finding{"runtime", "missing runtime composition"})
	}
	boundary := -1
	for index, statement := range function.Body.List {
		if conditional, condition := statement.(*ast.IfStmt); condition && workerSelectorPath(conditional.Cond) == "queriesOnly" && conditional.Else == nil && len(conditional.Body.List) > 0 {
			_, returns := conditional.Body.List[len(conditional.Body.List)-1].(*ast.ReturnStmt)
			if returns && rac1AssetCalls(set, function, conditional.Body, nil)["Runtime.configureMCPReads"] > 0 {
				boundary = index
			}
		}
	}
	configured := false
	for index, statement := range function.Body.List {
		calls := rac1AssetCalls(set, function, statement, nil)
		if boundary >= 0 && index > boundary {
			configured = configured || calls["Runtime.ConfigureRepositoryRefCommands"] > 0
			continue
		}
		if rfc1Initializes(statement) || wave42LegacyGitCalls(statement)["NewService"] {
			findings = append(findings, dpc1Finding{"runtime", "ref owner constructed on MCP prefix"})
		}
		for target := range calls {
			for key, reachable := range dpc1Reachable(set, target) {
				if rfc1Initializes(reachable.Body) || wave42LegacyGitCalls(reachable.Body)["NewService"] {
					findings = append(findings, dpc1Finding{"runtime", "MCP prefix indirectly constructs ref owner: " + key})
				}
			}
		}
	}
	if boundary < 0 || !configured {
		findings = append(findings, dpc1Finding{"runtime", "API-only configuration missing after MCP return"})
	}
	configuration := set.methods["Runtime.ConfigureRepositoryRefCommands"]
	output, bound, contextGuard := "", false, false
	if configuration != nil && configuration.Body != nil && configuration.Recv != nil && len(configuration.Recv.List[0].Names) == 1 {
		receiver := configuration.Recv.List[0].Names[0].Name
		for _, statement := range configuration.Body.List {
			if conditional, condition := statement.(*ast.IfStmt); condition && rac1AssetRejects(conditional.Body) {
				contextGuard = contextGuard || dpc1Joined(conditional.Cond, token.LOR, func(expression ast.Expr) bool { return dpc1Compare(expression, token.EQL, receiver+".Context", "nil") })
			}
		}
		ast.Inspect(configuration.Body, func(node ast.Node) bool {
			assignment, assigned := node.(*ast.AssignStmt)
			if !assigned || len(assignment.Rhs) != 1 || len(assignment.Lhs) == 0 {
				return true
			}
			if call, called := assignment.Rhs[0].(*ast.CallExpr); called && workerSelectorPath(call.Fun) == receiver+".Context.RepositoryRefCommands" {
				output = workerSelectorPath(assignment.Lhs[0])
			}
			bound = bound || output != "" && workerSelectorPath(assignment.Lhs[0]) == receiver+".RepositoryRefs" && workerSelectorPath(assignment.Rhs[0]) == output
			return true
		})
	}
	if !bound || !contextGuard {
		findings = append(findings, dpc1Finding{"runtime", "runtime lost Context-nil guard or actual factory result binding"})
	}
	return findings
}

func rfc1MCP(set wave42LegacyGitSourceSet) []dpc1Finding {
	function := set.methods["HandlerWithReadPreparation"]
	rejected := false
	if function != nil && function.Body != nil && function.Type.Params != nil && len(function.Type.Params.List[0].Names) == 1 {
		runtime := function.Type.Params.List[0].Names[0].Name
		for _, statement := range function.Body.List {
			if conditional, condition := statement.(*ast.IfStmt); condition && rac1AssetRejects(conditional.Body) {
				rejected = rejected || dpc1Joined(conditional.Cond, token.LOR, func(expression ast.Expr) bool {
					return dpc1Compare(expression, token.NEQ, runtime+".RepositoryRefs", "nil")
				})
			}
		}
	}
	if !rejected {
		return []dpc1Finding{{"mcp", "MCP accepts a runtime carrying RepositoryRefs"}}
	}
	return nil
}

func rfc1APIBinding(set wave42LegacyGitSourceSet) []dpc1Finding {
	configured, bound := false, false
	for _, function := range dpc1Reachable(set, "serve") {
		owner := ""
		rac1AssetCalls(set, function, function.Body, func(call *ast.CallExpr, target string) {
			if strings.HasSuffix(target, ".ConfigureRepositoryRefCommands") {
				configured = true
				owner = strings.TrimSuffix(workerSelectorPath(call.Fun), ".ConfigureRepositoryRefCommands")
			}
			bound = bound || owner != "" && strings.HasSuffix(target, ".SetRepositoryRefCommands") && len(call.Args) == 1 && workerSelectorPath(call.Args[0]) == owner+".RepositoryRefs"
		})
	}
	if !configured || !bound {
		return []dpc1Finding{{"binding", "API root must configure then inject the actual runtime.RepositoryRefs"}}
	}
	return nil
}

func rfc1Report(test *testing.T, findings []dpc1Finding) {
	test.Helper()
	for _, finding := range findings {
		test.Errorf("RFC-1 %s: %s", finding.rule, finding.detail)
	}
}

func TestRFC1ActualPrivateRefOwnerRetainsBothAlgorithmsAndNarrowPorts(test *testing.T) {
	application := wave48Package(test, filepath.Join("..", "app"))
	rfc1Report(test, rfc1Owner(application))
	rfc1Report(test, rfc1ScopeAndConsumers(application))
	rfc1Report(test, dpc1Interface(wave48Package(test, filepath.Join("..", "ports", "inbound")), "RepositoryRefCommands", []string{"UpdateRef", "UpdateRefs"}))
	domain := wave48Package(test, filepath.Join("..", "domain"))
	parents := domain.methods["Snapshot.ReachabilityParents"]
	if parents == nil || !wave42LegacyGitMentions(parents.Body, "Parents") || !wave42LegacyGitMentions(parents.Body, "GraftParents") {
		test.Error("RFC-1 shared reachability must include original and graft parents")
	}
}

func TestRFC1RefHTTPAndAPICompositionUseInjectedOwnerAndExcludeMCP(test *testing.T) {
	rfc1Report(test, rfc1HTTP(wave48Package(test, filepath.Join("..", "adapters", "delivery", "http"))))
	rfc1Report(test, rfc1Runtime(wave48Package(test, filepath.Join("..", "serverruntime"))))
	rfc1Report(test, rfc1MCP(wave48Package(test, filepath.Join("..", "mcpserver"))))
	rfc1Report(test, rfc1APIBinding(wave48Package(test, filepath.Join("..", "..", "cmd", "cxtd"))))
	for _, boundary := range []struct{ directory, entry string }{
		{filepath.Join("..", "app"), "NewReadServices"}, {filepath.Join("..", "serverruntime"), "Runtime.configureMCPReads"},
	} {
		set := wave48Package(test, boundary.directory)
		if set.methods[boundary.entry] == nil {
			test.Fatal("missing read-only composition", boundary.entry)
		}
		for key, function := range dpc1Reachable(set, boundary.entry) {
			if rfc1Initializes(function.Body) || wave42LegacyGitCalls(function.Body)["NewService"] {
				test.Error("RFC-1 read-only composition initializes refs", key)
			}
		}
	}
	for path, file := range wave48Package(test, filepath.Join("..", "..", "cmd", "cxt-mcp")).files {
		if rfc1Initializes(file) {
			test.Error("RFC-1 MCP process initializes ref commands", path)
		}
	}
}

func TestRFC1NegativeActualOwnerModelsRejectBroadShellRuntimeAndSingleCASRewrite(test *testing.T) {
	sources := wave42LegacyGitReadSources(test, filepath.Join("..", "app"))
	for _, scenario := range []struct {
		name, rule string
		mutate     func(wave42LegacyGitSourceSet)
	}{
		{"actual private owner retains broad metadata", "fields", func(set wave42LegacyGitSourceSet) {
			shape := set.types["repositoryRefCommands"].Type.(*ast.StructType)
			shape.Fields.List = append(shape.Fields.List, &ast.Field{Names: []*ast.Ident{ast.NewIdent("metadata")}, Type: &ast.SelectorExpr{X: ast.NewIdent("outbound"), Sel: ast.NewIdent("MetadataStore")}})
		}},
		{"actual private owner retains work", "fields", func(set wave42LegacyGitSourceSet) {
			shape := set.types["repositoryRefCommands"].Type.(*ast.StructType)
			shape.Fields.List = append(shape.Fields.List, &ast.Field{Names: []*ast.Ident{ast.NewIdent("work")}, Type: &ast.StarExpr{X: ast.NewIdent("repositoryWork")}})
		}},
		{"public owner aliases service", "alias", func(set wave42LegacyGitSourceSet) {
			set.types["RepositoryRefCommands"].Type = ast.NewIdent("Service")
		}},
		{"scope retains broad history", "fields", func(set wave42LegacyGitSourceSet) {
			shape := set.types["repositoryRefScope"].Type.(*ast.StructType)
			for _, field := range shape.Fields.List {
				if len(field.Names) == 1 && field.Names[0].Name == "history" {
					field.Type = &ast.SelectorExpr{X: ast.NewIdent("outbound"), Sel: ast.NewIdent("HistoryStore")}
				}
			}
		}},
		{"single-ref return shell", "payload", func(set wave42LegacyGitSourceSet) {
			set.methods["repositoryRefCommands.updateRef"].Body = dpc1Body(test, "return inbound.UpdateRefOutput{}, nil")
		}},
		{"whole-batch return shell", "payload", func(set wave42LegacyGitSourceSet) {
			set.methods["repositoryRefCommands.updateRefs"].Body = dpc1Body(test, "return inbound.UpdateRefsOutput{}, nil")
		}},
		{"runtime capability rediscovery", "runtime-assertion", func(set wave42LegacyGitSourceSet) {
			set.methods["repositoryRefCommands.appendDiverged"].Body = dpc1Body(test, "atomic, supported := commands.meta.(RepositoryRefAppend); if !supported { return inbound.UpdateRefOutput{}, domain.ErrCatalogUnsupported }; return output, atomic.AppendRef(ctx, input.RepoID, input.Ref, serverTarget)")
		}},
		{"constructor accepts missing ports", "constructor", func(set wave42LegacyGitSourceSet) {
			set.methods["NewRepositoryRefCommands"].Body = dpc1Body(test, "return &repositoryRefCommands{}, nil")
		}},
		{"one expected-only CAS rewrite", "cas", func(set wave42LegacyGitSourceSet) {
			changed := false
			function := set.methods["repositoryRefCommands.updateRef"]
			rac1AssetCalls(set, function, function.Body, func(call *ast.CallExpr, target string) {
				if !changed && target == "RepositoryRefStore.CompareAndSwapRef" && len(call.Args) == 4 {
					call.Args[3] = &ast.SelectorExpr{X: ast.NewIdent("input"), Sel: ast.NewIdent("ExpectedTarget")}
					changed = true
				}
			})
			if !changed {
				test.Fatal("CAS mutation did not change a real CAS call")
			}
		}},
		{"batch reenters single public scope", "batch", func(set wave42LegacyGitSourceSet) {
			ast.Inspect(set.methods["repositoryRefCommands.updateRefs"].Body, func(node ast.Node) bool {
				if selector, selected := node.(*ast.SelectorExpr); selected && selector.Sel.Name == "updateRef" {
					selector.Sel.Name = "UpdateRef"
				}
				return true
			})
		}},
		{"service batch regains algorithm", "facade", func(set wave42LegacyGitSourceSet) {
			set.methods["Service.UpdateRefs"].Body = dpc1Body(test, "for _, update := range input.Updates { service.UpdateRef(ctx, update) }; return inbound.UpdateRefsOutput{}, nil")
		}},
		{"PR worker loses passed ref owner", "worker", func(set wave42LegacyGitSourceSet) {
			ast.Inspect(set.methods["assemblePRPromotionWorker"].Body, func(node ast.Node) bool {
				if literal, composite := node.(*ast.CompositeLit); composite && wave42LegacyGitType(literal.Type) == "PRPromotionWorker" {
					for _, element := range literal.Elts {
						if field, keyed := element.(*ast.KeyValueExpr); keyed && workerSelectorPath(field.Key) == "refs" {
							field.Value = ast.NewIdent("nil")
						}
					}
				}
				return true
			})
		}},
		{"PR helper loses Complete ref validation", "worker", func(set wave42LegacyGitSourceSet) {
			changed := false
			function := set.methods["newPRPromotionWorkerWithWork"]
			rac1AssetCalls(set, function, function.Body, func(call *ast.CallExpr, target string) {
				if target == "repositoryRefsFromStores" && len(call.Args) == 5 {
					call.Args[4], changed = ast.NewIdent("RepositoryRefCompatibility"), true
				}
			})
			if !changed {
				test.Fatal("missing actual helper Complete ref validation")
			}
		}},
		{"PR helper loses passed assembly work", "worker", func(set wave42LegacyGitSourceSet) {
			changed := false
			function := set.methods["newPRPromotionWorkerWithWork"]
			rac1AssetCalls(set, function, function.Body, func(call *ast.CallExpr, target string) {
				if target == "assemblePRPromotionWorker" && len(call.Args) == 2 {
					call.Args[1], changed = ast.NewIdent("unrelatedWork"), true
				}
			})
			if !changed {
				test.Fatal("missing actual helper assembly")
			}
		}},
		{"PR assembler loses Compatibility ref profile", "worker", func(set wave42LegacyGitSourceSet) {
			changed := false
			ast.Inspect(set.methods["assemblePRPromotionWorker"].Body, func(node ast.Node) bool {
				if identifier, named := node.(*ast.Ident); named && identifier.Name == "RepositoryRefCompatibility" {
					identifier.Name, changed = "RepositoryRefComplete", true
				}
				return true
			})
			if !changed {
				test.Fatal("missing actual Compatibility ref profile")
			}
		}},
	} {
		test.Run(scenario.name, func(test *testing.T) {
			set, err := wave42LegacyGitParseSources(sources)
			if err != nil {
				test.Fatal(err)
			}
			if baseline := append(rfc1Owner(set), rfc1ScopeAndConsumers(set)...); len(baseline) != 0 {
				test.Fatalf("unmodified production must satisfy ref guard before mutation: %+v", baseline)
			}
			scenario.mutate(set)
			dpc1RequireRule(test, append(rfc1Owner(set), rfc1ScopeAndConsumers(set)...), scenario.rule)
		})
	}
}

func TestRFC1NegativeConsumersRejectDisconnectedHTTPRuntimeAPIAndMCP(test *testing.T) {
	for _, scenario := range []struct {
		name, directory, rule string
		check                 func(wave42LegacyGitSourceSet) []dpc1Finding
		mutate                func(wave42LegacyGitSourceSet)
	}{
		{"ordinary HTTP backend fallback", filepath.Join("..", "adapters", "delivery", "http"), "http", rfc1HTTP, func(set wave42LegacyGitSourceSet) {
			ast.Inspect(set.methods["Server.putRef"].Body, func(node ast.Node) bool {
				if selector, selected := node.(*ast.SelectorExpr); selected && selector.Sel.Name == "repositoryRefs" {
					selector.Sel.Name = "b"
				}
				return true
			})
		}},
		{"conditional HTTP shell", filepath.Join("..", "adapters", "delivery", "http"), "http", rfc1HTTP, func(set wave42LegacyGitSourceSet) {
			set.methods["Server.conditionalAppendV1"].Body = dpc1Body(test, "return")
		}},
		{"batch HTTP same-type disconnected owner", filepath.Join("..", "adapters", "delivery", "http"), "http", rfc1HTTP, func(set wave42LegacyGitSourceSet) {
			shape := set.types["Server"].Type.(*ast.StructType)
			shape.Fields.List = append(shape.Fields.List, &ast.Field{Names: []*ast.Ident{ast.NewIdent("otherRefs")}, Type: &ast.SelectorExpr{X: ast.NewIdent("inbound"), Sel: ast.NewIdent("RepositoryRefCommands")}})
			ast.Inspect(set.methods["Server.putRefs"].Body, func(node ast.Node) bool {
				if selector, selected := node.(*ast.SelectorExpr); selected && selector.Sel.Name == "repositoryRefs" {
					selector.Sel.Name = "otherRefs"
				}
				return true
			})
		}},
		{"runtime discards configured owner", filepath.Join("..", "serverruntime"), "runtime", rfc1Runtime, func(set wave42LegacyGitSourceSet) {
			set.methods["Runtime.ConfigureRepositoryRefCommands"].Body = dpc1Body(test, "return nil")
		}},
		{"MCP indirectly constructs refs", filepath.Join("..", "serverruntime"), "runtime", rfc1Runtime, func(set wave42LegacyGitSourceSet) {
			set.methods["Runtime.configureMCPReads"].Body = dpc1Body(test, "return runtime.ConfigureRepositoryRefCommands()")
		}},
		{"API setter disconnected", filepath.Join("..", "..", "cmd", "cxtd"), "binding", rfc1APIBinding, func(set wave42LegacyGitSourceSet) {
			for _, function := range dpc1Reachable(set, "serve") {
				ast.Inspect(function.Body, func(node ast.Node) bool {
					if selector, selected := node.(*ast.SelectorExpr); selected && selector.Sel.Name == "SetRepositoryRefCommands" {
						selector.Sel.Name = "SetUnrelatedCommands"
					}
					return true
				})
			}
		}},
		{"MCP accepts ref commands", filepath.Join("..", "mcpserver"), "mcp", rfc1MCP, func(set wave42LegacyGitSourceSet) {
			ast.Inspect(set.methods["HandlerWithReadPreparation"].Body, func(node ast.Node) bool {
				if selector, selected := node.(*ast.SelectorExpr); selected && selector.Sel.Name == "RepositoryRefs" {
					selector.Sel.Name = "UnrelatedCommands"
				}
				return true
			})
		}},
	} {
		test.Run(scenario.name, func(test *testing.T) {
			set := wave48Package(test, scenario.directory)
			if baseline := scenario.check(set); len(baseline) != 0 {
				test.Fatalf("unmodified production must satisfy consumer guard before mutation: %+v", baseline)
			}
			scenario.mutate(set)
			dpc1RequireRule(test, scenario.check(set), scenario.rule)
		})
	}
}
