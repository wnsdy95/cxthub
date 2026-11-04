package architecture

import (
	"go/ast"
	"go/token"
	"path/filepath"
	"strings"
	"testing"
)

var brc1Public = map[string]string{
	"Fork": "context.Context,ForkInput)(ForkOutput,error", "PreviewJoin": "context.Context,JoinPreviewInput)(JoinPreviewOutput,error",
	"ConfirmJoin": "context.Context,ConfirmJoinInput)(JoinOutput,error", "EnableContextProtocol": "context.Context,domain.ContentHash)(error",
	"ListHistory": "context.Context,domain.ContentHash)([]domain.HistoryEvent,error",
}

var brc1Config = map[string]string{
	"Profile": "ContextBranchProfile", "Store": "ContextBranchStore", "Scope": "ContextBranchScope", "History": "ContextBranchHistory",
	"Recorder": "inbound.HistoryCommands", "Protocol": "ContextBranchProtocol", "Access": "ContextBranchAccess", "Pending": "ContextBranchPending",
}

func brc1Ports(set wave42LegacyGitSourceSet) []dpc1Finding {
	contracts := map[string]map[string]string{"ContextBranchOperations": brc1Public}
	if set.types["contextBranchOperations"] != nil {
		contracts = map[string]map[string]string{
			"ContextBranchStore":   {"GetRepo": "context.Context,domain.ContentHash)(domain.Repo,error", "GetSnapshot": "context.Context,domain.ContentHash,domain.ContentHash)(domain.Snapshot,error", "GetRef": "context.Context,domain.ContentHash,domain.RefKind,string)(domain.Ref,error", "CompareAndSwapRef": "context.Context,domain.ContentHash,domain.Ref,domain.ContentHash)(error", "ListSnapshots": "context.Context,domain.ContentHash,string)([]domain.Snapshot,error", "ListRefs": "context.Context,domain.ContentHash)([]domain.Ref,error", "ListPendings": "context.Context,domain.ContentHash)([]domain.Pending,error", "ReadReflog": "context.Context,domain.ContentHash)([]domain.RefLogEntry,error", "ApplyJoin": "context.Context,domain.JoinMutation)(error"},
			"ContextBranchHistory": {"ListHistoryEvents": "context.Context,domain.ContentHash)([]domain.HistoryEvent,error"}, "ContextBranchProtocol": {"EnableContextProtocol": "context.Context,domain.ContentHash)(error"},
			"ContextBranchAccess": {"AuthorizeJoin": "context.Context,domain.ContentHash,string,bool)(error"}, "ContextBranchPending": {"ReconcileSharedPending": "context.Context,domain.ContentHash)(error"}, "ContextBranchScope": {},
		}
		for method, result := range map[string]string{"WithinFork": "inbound.ForkOutput,error", "WithinJoin": "inbound.JoinOutput,error", "WithinProtocol": "error", "WithinPreview": "inbound.JoinPreviewOutput,error", "WithinHistory": "[]domain.HistoryEvent,error"} {
			contracts["ContextBranchScope"][method] = "context.Context,domain.ContentHash,func(context.Context)(" + result + "))(" + result
		}
	}
	var findings []dpc1Finding
	for owner, expected := range contracts {
		var names []string
		for name := range expected {
			names = append(names, name)
		}
		findings = append(findings, dpc1Interface(set, owner, names)...)
		if declaration := set.types[owner]; declaration != nil {
			if contract, narrow := declaration.Type.(*ast.InterfaceType); narrow {
				for _, method := range contract.Methods.List {
					if signature, callable := method.Type.(*ast.FuncType); callable && len(method.Names) == 1 && prc1Signature(signature.Params)+")("+prc1Signature(signature.Results) != expected[method.Names[0].Name] {
						findings = append(findings, dpc1Finding{"ports", "changed signature " + owner + "." + method.Names[0].Name})
					}
				}
			}
		}
	}
	return findings
}

func brc1Constructor(set wave42LegacyGitSourceSet) []dpc1Finding {
	function := set.methods["NewContextBranchOperations"]
	if function == nil || function.Body == nil || prc1Signature(function.Type.Params) != "ContextBranchOperationsConfig" || prc1Signature(function.Type.Results) != "*ContextBranchOperations,error" || len(function.Type.Params.List[0].Names) != 1 {
		return []dpc1Finding{{"constructor", "missing exact constructor"}}
	}
	config, allocation := function.Type.Params.List[0].Names[0].Name, function.Body.End()
	var literal *ast.CompositeLit
	var loops []*ast.RangeStmt
	var completeBlocks []*ast.BlockStmt
	ast.Inspect(function.Body, func(node ast.Node) bool {
		if candidate, composite := node.(*ast.CompositeLit); composite && (wave42LegacyGitType(candidate.Type) == "contextBranchOperations" || wave42LegacyGitType(candidate.Type) == "ContextBranchOperations") && candidate.Pos() < allocation {
			literal, allocation = candidate, candidate.Pos()
		}
		if loop, ranged := node.(*ast.RangeStmt); ranged {
			loops = append(loops, loop)
		}
		if conditional, condition := node.(*ast.IfStmt); condition && dpc1Compare(conditional.Cond, token.EQL, config+".Profile", "ContextBranchComplete") {
			completeBlocks = append(completeBlocks, conditional.Body)
		}
		return true
	})
	checks, complete := map[string]token.Pos{}, allocation
	ast.Inspect(function.Body, func(node ast.Node) bool {
		conditional, condition := node.(*ast.IfStmt)
		if !condition || conditional.End() >= allocation {
			return true
		}
		validation := dpc1ReturnsError(conditional.Body, "domain.ErrValidation")
		if validation && dpc1Joined(conditional.Cond, token.LAND, func(expression ast.Expr) bool {
			return dpc1Compare(expression, token.NEQ, config+".Profile", "ContextBranchCompatibility")
		}) && dpc1Joined(conditional.Cond, token.LAND, func(expression ast.Expr) bool {
			return dpc1Compare(expression, token.NEQ, config+".Profile", "ContextBranchComplete")
		}) {
			checks["profile"] = conditional.Pos()
		}
		paths := map[string][]string{}
		for field := range brc1Config {
			if field != "Profile" {
				paths[config+"."+field] = []string{field}
			}
		}
		for _, loop := range loops {
			if values, listed := loop.X.(*ast.CompositeLit); listed && wave48Inside(conditional, []*ast.BlockStmt{loop.Body}) {
				for _, value := range values.Elts {
					paths[workerSelectorPath(loop.Value)] = append(paths[workerSelectorPath(loop.Value)], strings.TrimPrefix(workerSelectorPath(value), config+"."))
				}
			}
		}
		for path, fields := range paths {
			absent := dpc1Joined(conditional.Cond, token.LOR, func(expression ast.Expr) bool { return rac1AssetAbsent(expression, path) })
			typed := dpc1Joined(conditional.Cond, token.LAND, func(expression ast.Expr) bool { return rac1AssetAbsent(expression, path) }) && dpc1Joined(conditional.Cond, token.LAND, func(expression ast.Expr) bool { return dpc1Compare(expression, token.NEQ, path, "nil") })
			qualified := wave48Inside(conditional, completeBlocks) || dpc1Joined(conditional.Cond, token.LAND, func(expression ast.Expr) bool {
				return dpc1Compare(expression, token.EQL, config+".Profile", "ContextBranchComplete")
			})
			missing := dpc1Joined(conditional.Cond, token.LAND, func(expression ast.Expr) bool {
				return dpc1Joined(expression, token.LOR, func(candidate ast.Expr) bool { return dpc1Compare(candidate, token.EQL, path, "nil") })
			})
			for _, field := range fields {
				if validation && !qualified && (typed || absent && (field == "Store" || field == "Scope")) {
					checks["nil "+field] = conditional.Pos()
				}
				if validation && !qualified && (absent || dpc1Joined(conditional.Cond, token.LOR, func(expression ast.Expr) bool { return dpc1Compare(expression, token.EQL, path, "nil") })) {
					checks["required "+field] = conditional.Pos()
				}
				if qualified && missing && dpc1ReturnsError(conditional.Body, "domain.ErrCatalogUnsupported") {
					checks["complete "+field] = conditional.Pos()
					if conditional.Pos() < complete {
						complete = conditional.Pos()
					}
				}
			}
		}
		return true
	})
	var findings []dpc1Finding
	if literal == nil || checks["profile"] == 0 {
		findings = append(findings, dpc1Finding{"constructor", "missing allocation/profile validation"})
	}
	for field, kind := range brc1Config {
		if field != "Profile" && (checks["nil "+field] == 0 || checks["nil "+field] >= complete || field != "Store" && field != "Scope" && checks["complete "+field] == 0) {
			findings = append(findings, dpc1Finding{"constructor", "missing/late nil rejection " + field})
		}
		if (field == "Store" || field == "Scope") && checks["required "+field] == 0 {
			findings = append(findings, dpc1Finding{"constructor", "missing required " + field})
		}
		bound := false
		if declaration := set.types["contextBranchOperations"]; declaration != nil {
			if shape, concrete := declaration.Type.(*ast.StructType); concrete {
				for _, member := range shape.Fields.List {
					for _, name := range member.Names {
						bound = bound || wave42LegacyGitType(member.Type) == kind && workerSelectorPath(rwa1AuthorityField(literal, name.Name)) == config+"."+field
					}
				}
			}
		}
		if !bound {
			findings = append(findings, dpc1Finding{"constructor", "discarded configured " + field})
		}
	}
	for _, reachable := range dpc1Reachable(set, "NewContextBranchOperations") {
		for target := range dpc1Calls(set, reachable, reachable.Body, nil) {
			if strings.HasPrefix(target, "ContextBranch") || strings.HasPrefix(target, "inbound.") || strings.HasPrefix(target, "Service.") || strings.HasPrefix(target, "repository") || target == "newRepositoryWork" || target == "NewService" {
				findings = append(findings, dpc1Finding{"constructor", "constructor performs work " + target})
			}
			for _, prefix := range []string{"outbound.", "os.", "net.", "http.", "sql.", "pgx.", "exec."} {
				if strings.HasPrefix(target, prefix) {
					findings = append(findings, dpc1Finding{"constructor", "constructor performs IO " + target})
				}
			}
		}
		ast.Inspect(reachable.Body, func(node ast.Node) bool {
			if _, starts := node.(*ast.GoStmt); starts {
				findings = append(findings, dpc1Finding{"constructor", "constructor starts independent work"})
			}
			return true
		})
	}
	presence := wave42LegacyGitCalls(bodyOfDPC1(set.methods["rootAdapterPresent"]))
	if !presence["IsNil"] || !presence["ValueOf"] {
		findings = append(findings, dpc1Finding{"constructor", "typed-nil predicate is not genuine"})
	}
	return findings
}

func brc1Steps(set wave42LegacyGitSourceSet, entry string, active map[string]bool) []string {
	if active[entry] {
		return nil
	}
	active[entry] = true
	defer delete(active, entry)
	var steps []string
	function := set.methods[entry]
	dpc1Calls(set, function, bodyOfDPC1(function), func(call *ast.CallExpr, target string) {
		steps = append(steps, target)
		if set.methods[target] != nil {
			steps = append(steps, brc1Steps(set, target, active)...)
		}
	})
	return steps
}

func brc1Owner(set wave42LegacyGitSourceSet) []dpc1Finding {
	findings := append(brc1Ports(set), brc1Constructor(set)...)
	var allowed, required []string
	for _, kind := range brc1Config {
		allowed = append(allowed, kind)
		required = append(required, kind)
	}
	findings = append(findings, dpc1Fields(set, "contextBranchOperations", append(allowed, "error"), required)...)
	for _, detail := range wave47GitHubStructViolations(set, "ContextBranchOperationsConfig", brc1Config) {
		findings = append(findings, dpc1Finding{"fields", detail})
	}
	alias := set.types["ContextBranchOperations"]
	if alias == nil || !alias.Assign.IsValid() || wave42LegacyGitType(alias.Type) != "contextBranchOperations" {
		findings = append(findings, dpc1Finding{"alias", "public alias must name the private owner"})
	}
	for method, signature := range brc1Public {
		entry, function := "contextBranchOperations."+method, set.methods["contextBranchOperations."+method]
		if function == nil || strings.ReplaceAll(prc1Signature(function.Type.Params)+")("+prc1Signature(function.Type.Results), "inbound.", "") != signature {
			findings = append(findings, dpc1Finding{"ports", "missing/changed actual " + method})
		}
		scope := map[string]string{"Fork": "WithinFork", "PreviewJoin": "WithinPreview", "ConfirmJoin": "WithinJoin", "EnableContextProtocol": "WithinProtocol", "ListHistory": "WithinHistory"}[method]
		operations := map[string][]string{
			"Fork":                  {"ContextBranchStore.GetSnapshot", "ContextBranchStore.CompareAndSwapRef", "inbound.HistoryCommands.RecordHistory", "domain.ProjectContextBranches"},
			"PreviewJoin":           {"ContextBranchAccess.AuthorizeJoin", "domain.PlanJoin", "domain.ProjectSnapshotBranchMemberships", "sort.Slice"},
			"ConfirmJoin":           {"ContextBranchAccess.AuthorizeJoin", "domain.PlanJoin", "domain.ProjectSnapshotBranchMemberships", "ContextBranchStore.ApplyJoin", "ContextBranchPending.ReconcileSharedPending"},
			"EnableContextProtocol": {"ContextBranchStore.GetRepo", "ContextBranchProtocol.EnableContextProtocol"}, "ListHistory": {"ContextBranchHistory.ListHistoryEvents"},
		}[method]
		findings = append(findings, hrc1Require(set, entry, "policy", append(operations, "ContextBranchScope."+scope)...)...)
		if method == "PreviewJoin" || method == "ConfirmJoin" {
			locked := "false"
			if method == "ConfirmJoin" {
				locked = "true"
			}
			for _, reachable := range dpc1Reachable(set, entry) {
				dpc1Calls(set, reachable, reachable.Body, func(call *ast.CallExpr, target string) {
					if target == "ContextBranchAccess.AuthorizeJoin" && (len(call.Args) != 4 || workerSelectorPath(call.Args[3]) != locked) {
						findings = append(findings, dpc1Finding{"policy", method + " changed current actor/access lock policy"})
					}
				})
			}
		}
		bound, discarded := false, false
		dpc1Calls(set, function, bodyOfDPC1(function), func(call *ast.CallExpr, target string) {
			if target != "ContextBranchScope."+scope {
				return
			}
			for _, argument := range call.Args {
				if callback, literal := argument.(*ast.FuncLit); literal {
					dpc1Calls(set, function, callback.Body, func(nested *ast.CallExpr, selected string) {
						if strings.HasPrefix(selected, "contextBranchOperations.") || strings.HasPrefix(selected, "ContextBranch") || selected == "inbound.HistoryCommands.RecordHistory" {
							bound = true
							discarded = discarded || len(hrc1Bound(set, entry, target, selected)) != 0
						}
					})
				}
			}
		})
		if !bound || discarded {
			findings = append(findings, dpc1Finding{"bound-context", entry + " discards scoped context"})
		}
		for target, reachable := range dpc1Reachable(set, entry) {
			if strings.HasPrefix(target, "Service.") || target == "NewService" || target == "newRepositoryWork" {
				findings = append(findings, dpc1Finding{"reverse", "reverse delegation to " + target})
			}
			for operation := range dpc1Calls(set, reachable, reachable.Body, nil) {
				if strings.HasPrefix(operation, "reflect.") {
					findings = append(findings, dpc1Finding{"runtime-assertion", "reflective capability discovery in " + target})
				}
			}
			ast.Inspect(reachable.Body, func(node ast.Node) bool {
				switch value := node.(type) {
				case *ast.TypeAssertExpr, *ast.TypeSwitchStmt:
					findings = append(findings, dpc1Finding{"runtime-assertion", "capability discovery in " + target})
				case *ast.GoStmt, *ast.ChanType:
					findings = append(findings, dpc1Finding{"reverse", "independent lifetime in " + target})
				case *ast.Ident:
					if value.Name == "Service" || value.Name == "NewService" || value.Name == "repositoryWork" || value.Name == "MetadataStore" {
						findings = append(findings, dpc1Finding{"reverse", "broad parent state in " + target})
					}
				}
				return true
			})
		}
		steps, previous := brc1Steps(set, entry, map[string]bool{}), -1
		ordered := operations
		if method == "PreviewJoin" {
			ordered = []string{"domain.ValidateContentHash", "ContextBranchScope.WithinPreview", "ContextBranchAccess.AuthorizeJoin", "domain.PlanJoin"}
		}
		if method == "ConfirmJoin" {
			ordered = []string{"inbound.WithRepositoryActor", "ContextBranchScope.WithinJoin", "ContextBranchAccess.AuthorizeJoin", "domain.PlanJoin", "ContextBranchStore.ApplyJoin", "ContextBranchPending.ReconcileSharedPending"}
		}
		if method != "PreviewJoin" && method != "ConfirmJoin" {
			continue
		}
		for _, operation := range ordered {
			position := -1
			for index, step := range steps {
				if step == operation && position < 0 {
					position = index
				}
			}
			if position <= previous {
				findings = append(findings, dpc1Finding{"order", method + " reordered " + operation})
			}
			previous = position
		}
		if method == "ConfirmJoin" {
			guarded, currentPlan := false, false
			dpc1Calls(set, function, bodyOfDPC1(function), func(call *ast.CallExpr, target string) {
				if target == "ContextBranchScope.WithinJoin" {
					for _, statement := range function.Body.List {
						if dpc1Calls(set, function, statement, nil)[target] {
							break
						}
						if conditional, condition := statement.(*ast.IfStmt); condition && dpc1ReturnsError(conditional.Body, "domain.ErrJoinPreviewChanged") && wave42LegacyGitMentions(conditional.Cond, "PlanRevision") && wave42LegacyGitMentions(conditional.Cond, "ExpectedHead") {
							guarded = true
						}
					}
				}
			})
			if !guarded {
				findings = append(findings, dpc1Finding{"order", "missing-preview rejection must precede write scope"})
			}
			for _, reachable := range dpc1Reachable(set, entry) {
				ast.Inspect(reachable.Body, func(node ast.Node) bool {
					if block, scoped := node.(*ast.BlockStmt); scoped {
						planner, checked, applied := -1, -1, -1
						for index, statement := range block.List {
							calls := dpc1Calls(set, reachable, statement, nil)
							if calls["domain.PlanJoin"] {
								planner = index
							}
							if calls["ContextBranchStore.ApplyJoin"] {
								applied = index
							}
							if conditional, condition := statement.(*ast.IfStmt); condition && dpc1ReturnsError(conditional.Body, "domain.ErrJoinPreviewChanged") && wave42LegacyGitMentions(conditional.Cond, "PlanRevision") && wave42LegacyGitMentions(conditional.Cond, "ExpectedHead") && wave42LegacyGitMentions(conditional.Cond, "BranchID") && wave42LegacyGitCalls(conditional.Cond)["Revision"] {
								checked = index
							}
						}
						currentPlan = currentPlan || planner >= 0 && checked > planner && applied > checked
					}
					return true
				})
			}
			if !currentPlan {
				findings = append(findings, dpc1Finding{"order", "exact current Domain plan must be checked before atomic ApplyJoin"})
			}
		}
	}
	return findings
}

func brc1SharedWork(set wave42LegacyGitSourceSet) []dpc1Finding {
	var findings []dpc1Finding
	rooted := false
	root := dpc1Reachable(set, "Service.ContextBranchOperations")
	for entry, function := range set.methods {
		_, work := hrc1Work(function)
		dpc1Calls(set, function, bodyOfDPC1(function), func(call *ast.CallExpr, target string) {
			if target != "NewContextBranchOperations" || len(call.Args) != 1 {
				return
			}
			config := rwa1AuthorityComposite(hrc1Resolve(function, call.Args[0], call.Pos()), "ContextBranchOperationsConfig")
			scope := hrc1Resolve(function, rwa1AuthorityField(config, "Scope"), call.Pos())
			kind := wave44PreparationExpressionType(set, scope, map[string]string{})
			bound := kind != "" && prc1UsesWork(set, function, scope, kind, work, call.Pos(), 0)
			for method, helper := range map[string]string{"WithinFork": "repositoryWriteWithWork", "WithinJoin": "repositoryWriteWithWork", "WithinProtocol": "repositoryWriteErrorWithWork", "WithinPreview": "repositoryReadForRepoWithWork", "WithinHistory": "repositoryReadForRepoWithWork"} {
				shared := false
				for _, reachable := range dpc1Reachable(set, kind+"."+method) {
					dpc1Calls(set, reachable, reachable.Body, func(nested *ast.CallExpr, selected string) {
						shared = shared || selected == helper && len(nested.Args) > 1 && rfc1Origin(reachable, nested.Args[1], nested.Pos()) == kind+".work"
					})
				}
				if !shared {
					findings = append(findings, dpc1Finding{"shared-work", method + " replaced original passed-work helper"})
				}
				if method == "WithinFork" || method == "WithinProtocol" {
					operation, literal, command := "auditOperation", "context.forked", "Fork"
					if method == "WithinProtocol" {
						operation, literal, command = "writeAction", "manage", "EnableContextProtocol"
					}
					retained := false
					for _, rootEntry := range []string{kind + "." + method, "contextBranchOperations." + command} {
						for _, reachable := range dpc1Reachable(set, rootEntry) {
							dpc1Calls(set, reachable, reachable.Body, func(nested *ast.CallExpr, selected string) {
								retained = retained || selected == operation && len(nested.Args) == 2 && prc1String(nested.Args[1], literal)
							})
						}
					}
					if !retained {
						findings = append(findings, dpc1Finding{"shared-work", method + " lost existing audit/manage action"})
					}
				}
			}
			recorders := []ast.Expr{rwa1AuthorityField(config, "Recorder")}
			if identifier, named := recorders[0].(*ast.Ident); named {
				recorders = prc1WorkBindings(function.Body, identifier.Name, work, call.Pos(), []ast.Expr{nil})
			}
			factories := map[*ast.CallExpr]bool{}
			for _, recorder := range recorders {
				dpc1Calls(set, function, function.Body, func(candidate *ast.CallExpr, selected string) {
					if candidate != recorder {
						return
					}
					index, parameter := hrc1Work(set.methods[selected])
					if index < 0 || index >= len(candidate.Args) || !prc1UsesWork(set, function, candidate.Args[index], "", work, candidate.Pos(), 0) {
						return
					}
					forwarded := selected == "historyCommandsFromStores"
					if selected == "historyCommandsForWork" {
						wrapper := set.methods[selected]
						dpc1Calls(set, wrapper, wrapper.Body, func(nested *ast.CallExpr, operation string) {
							position, _ := hrc1Work(set.methods[operation])
							forwarded = forwarded || operation == "historyCommandsFromStores" && position >= 0 && position < len(nested.Args) && prc1UsesWork(set, wrapper, nested.Args[position], "", parameter, nested.Pos(), 0)
						})
					}
					factory := set.methods["historyCommandsFromStores"]
					if forwarded && factory != nil {
						_, passed := hrc1Work(factory)
						dpc1Calls(set, factory, factory.Body, func(nested *ast.CallExpr, operation string) {
							if operation == "NewHistoryCommands" && len(nested.Args) == 1 {
								history := rwa1AuthorityComposite(hrc1Resolve(factory, nested.Args[0], nested.Pos()), "HistoryCommandsConfig")
								factories[candidate] = factories[candidate] || prc1UsesWork(set, factory, rwa1AuthorityField(history, "Scope"), "historyCommandScope", passed, nested.Pos(), 0)
							}
						})
					}
				})
			}
			recorded, produced := len(recorders) != 0, false
			for _, recorder := range recorders {
				if candidate, called := recorder.(*ast.CallExpr); called {
					recorded = recorded && factories[candidate]
					produced = produced || factories[candidate]
					continue
				}
				sentinel := rwa1AuthorityComposite(recorder, "historyRecorder")
				errorOrigin := workerSelectorPath(rwa1AuthorityField(sentinel, "configurationError"))
				failedFactory := false
				if sentinel != nil && len(sentinel.Elts) == 1 && errorOrigin != "" && errorOrigin != "nil" {
					ast.Inspect(function.Body, func(node ast.Node) bool {
						if assignment, assigned := node.(*ast.AssignStmt); assigned && len(assignment.Rhs) == 1 && len(assignment.Lhs) == 2 && workerSelectorPath(assignment.Lhs[1]) == errorOrigin {
							if candidate, called := assignment.Rhs[0].(*ast.CallExpr); called && factories[candidate] {
								ast.Inspect(function.Body, func(nested ast.Node) bool {
									if conditional, condition := nested.(*ast.IfStmt); condition && conditional.Pos() > assignment.End() && dpc1Compare(conditional.Cond, token.NEQ, errorOrigin, "nil") && wave48Inside(recorder, []*ast.BlockStmt{conditional.Body}) {
										failedFactory = true
									}
									return true
								})
							}
						}
						return true
					})
				}
				recorded = recorded && failedFactory
			}
			recorded = recorded && produced
			pending := hrc1Resolve(function, rwa1AuthorityField(config, "Pending"), call.Pos())
			pendingKind := wave44PreparationExpressionType(set, pending, map[string]string{})
			reconciled := rfc1UsesPassedWork(set, pending, work)
			if pendingKind != "" && prc1UsesWork(set, function, pending, pendingKind, work, call.Pos(), 0) {
				for _, reachable := range dpc1Reachable(set, pendingKind+".ReconcileSharedPending") {
					dpc1Calls(set, reachable, reachable.Body, func(nested *ast.CallExpr, operation string) {
						index, _ := hrc1Work(set.methods[operation])
						reconciled = reconciled || (operation == "repositoryRefCommandsForWork" || operation == "repositoryRefsFromStores") && index >= 0 && index < len(nested.Args) && rfc1Origin(reachable, nested.Args[index], nested.Pos()) == pendingKind+".work"
					})
				}
				for _, reachable := range dpc1Reachable(set, pendingKind+".ReconcileSharedPending") {
					dpc1Calls(set, reachable, reachable.Body, func(nested *ast.CallExpr, operation string) {
						if operation != "repositoryRefCommands.reconcileSharedPendingPointers" {
							return
						}
						origin := strings.TrimSuffix(rfc1Origin(reachable, nested.Fun, nested.Pos()), ".reconcileSharedPendingPointers")
						if !strings.HasPrefix(origin, pendingKind+".") {
							return
						}
						field := strings.TrimPrefix(origin, pendingKind+".")
						dependency := rwa1AuthorityField(rwa1AuthorityComposite(pending, pendingKind), field)
						reconciled = reconciled || rfc1UsesPassedWork(set, hrc1Resolve(function, dependency, call.Pos()), work)
					})
				}
				reconciled = reconciled && dpc1ExecutionCalls(set, pendingKind+".ReconcileSharedPending")["repositoryRefCommands.reconcileSharedPendingPointers"]
			}
			if !bound || !recorded || !reconciled {
				findings = append(findings, dpc1Finding{"shared-work", entry + " must bind original work to actual scope, recorder and ref pending policy"})
			}
			rooted = rooted || root[entry] != nil && bound && recorded && reconciled
		})
	}
	if !rooted {
		findings = append(findings, dpc1Finding{"shared-work", "public Service factory disconnected from passed-work composition"})
	}
	return findings
}

func brc1HTTP(set wave42LegacyGitSourceSet) []dpc1Finding {
	setter := set.methods["Server.SetContextBranchOperations"]
	if setter == nil || setter.Body == nil || prc1Signature(setter.Type.Params) != "inbound.ContextBranchOperations" || len(setter.Type.Params.List[0].Names) != 1 {
		return []dpc1Finding{{"http", "missing typed setter"}}
	}
	parameter, injections := setter.Type.Params.List[0].Names[0].Name, map[string]bool{}
	nilRejected := false
	ast.Inspect(setter.Body, func(node ast.Node) bool {
		if conditional, condition := node.(*ast.IfStmt); condition && rac1AssetRejects(conditional.Body) && dpc1Compare(conditional.Cond, token.EQL, parameter, "nil") {
			nilRejected = true
		}
		if assignment, assigned := node.(*ast.AssignStmt); assigned && len(assignment.Lhs) == len(assignment.Rhs) {
			for index, left := range assignment.Lhs {
				origin := rfc1Origin(setter, left, assignment.Pos())
				if strings.HasPrefix(origin, "Server.") && wave44PreparationMember(set, "Server", strings.TrimPrefix(origin, "Server."), false, map[string]bool{}) == "inbound.ContextBranchOperations" {
					injections[origin] = workerSelectorPath(hrc1Resolve(setter, assignment.Rhs[index], assignment.Pos())) == parameter
				}
			}
		}
		return true
	})
	var findings []dpc1Finding
	if len(injections) == 0 || !wave42LegacyGitCalls(setter.Body)["IsNil"] || !nilRejected {
		findings = append(findings, dpc1Finding{"http", "setter lost typed-nil/nil rejection or injection"})
	}
	for route, expected := range map[string][]string{
		"POST /api/v1/repos/{repoID}/fork": {"fork", "Fork"}, "GET /api/v1/repos/{repoID}/join/preview": {"previewJoin", "PreviewJoin"}, "POST /api/v1/repos/{repoID}/join": {"joinSnapshot", "ConfirmJoin"},
		"POST /api/v1/repos/{repoID}/context-protocol": {"enableContextProtocol", "EnableContextProtocol"}, "GET /api/v1/repos/{repoID}/history": {"listHistory", "ListHistory"},
	} {
		registered, used := false, false
		for _, function := range dpc1Reachable(set, "Server.Handler") {
			dpc1Calls(set, function, function.Body, func(call *ast.CallExpr, target string) {
				if !strings.HasSuffix(target, ".HandleFunc") || len(call.Args) != 2 {
					return
				}
				literal, quoted := call.Args[0].(*ast.BasicLit)
				if quoted && literal.Kind == token.STRING && literal.Value == "\""+route+"\"" {
					ast.Inspect(call.Args[1], func(node ast.Node) bool {
						if selector, selected := node.(*ast.SelectorExpr); selected {
							registered = registered || rfc1Origin(function, selector, call.Pos()) == "Server."+expected[0]
						}
						return true
					})
				}
			})
		}
		function := set.methods["Server."+expected[0]]
		admission, release, response, decode, command, jsonGuard := token.NoPos, token.NoPos, token.NoPos, token.NoPos, token.NoPos, token.NoPos
		resource := "app.ResourceBulk"
		if expected[1] == "EnableContextProtocol" {
			resource = "app.ResourceInteractive"
		}
		for _, reachable := range dpc1Reachable(set, "Server."+expected[0]) {
			dpc1Calls(set, reachable, reachable.Body, func(call *ast.CallExpr, target string) {
				if strings.HasSuffix(target, "."+expected[1]) {
					origin := strings.TrimSuffix(rfc1Origin(reachable, call.Fun, call.Pos()), "."+expected[1])
					correct := target == "inbound.ContextBranchOperations."+expected[1] && injections[origin]
					used = used || correct
					command = call.Pos()
					if !correct {
						findings = append(findings, dpc1Finding{"http", route + " bypasses setter-bound consumer"})
					}
				}
				if reachable != function {
					return
				}
				if target == "Server.admitResourceRequest" && len(call.Args) == 3 && workerSelectorPath(call.Args[2]) == resource {
					admission = call.Pos()
				}
				if target == "Server.decode" || target == "Server.decodeLimited" {
					decode = call.Pos()
				}
				if target == "Server.respond" {
					response = call.Pos()
				}
				if target == "isJSONBody" {
					jsonGuard = call.Pos()
				}
			})
			ast.Inspect(reachable.Body, func(node ast.Node) bool {
				if assertion, asserted := node.(*ast.TypeAssertExpr); asserted {
					capability := rfc1Origin(reachable, assertion.X, assertion.Pos()) == "Server.b" || wave42LegacyGitType(assertion.Type) == "inbound.ContextBranchOperations"
					if contract, narrow := assertion.Type.(*ast.InterfaceType); narrow {
						for _, method := range contract.Methods.List {
							for _, name := range method.Names {
								capability = capability || brc1Public[name.Name] != ""
							}
						}
					}
					if capability {
						findings = append(findings, dpc1Finding{"http", "legacy capability selection outside NewServer"})
					}
				}
				if deferred, defers := node.(*ast.DeferStmt); defers && reachable == function && workerSelectorPath(deferred.Call.Fun) == "release" {
					release = deferred.Pos()
				}
				return true
			})
		}
		if !registered || !used {
			findings = append(findings, dpc1Finding{"http", "disconnected literal route " + route})
		}
		if admission == 0 || release <= admission || response <= release || command <= admission || decode != 0 && decode <= release || expected[1] == "EnableContextProtocol" && (jsonGuard == 0 || jsonGuard >= admission) {
			findings = append(findings, dpc1Finding{"http", route + " lost existing admission through decode/response"})
		}
	}
	return findings
}

func brc1Composition(set wave42LegacyGitSourceSet, check func(wave42LegacyGitSourceSet) []dpc1Finding) []dpc1Finding {
	originals := map[*ast.Ident]string{}
	names := map[string]string{"ConfigureContextBranchOperations": "ConfigureHistoryCommands", "SetContextBranchOperations": "SetHistoryCommands", "NewContextBranchOperations": "NewHistoryCommands", "ContextBranchOperations": "HistoryCommands", "ContextBranches": "HistoryCommands", "contextBranches": "historyCommands"}
	for _, file := range set.files {
		ast.Inspect(file, func(node ast.Node) bool {
			if identifier, named := node.(*ast.Ident); named {
				originals[identifier] = identifier.Name
				if replacement := names[identifier.Name]; replacement != "" {
					identifier.Name = replacement
				} else if strings.Contains(identifier.Name, "HistoryCommands") || identifier.Name == "historyCommands" {
					identifier.Name = "brc1Excluded" + identifier.Name
				}
			}
			return true
		})
	}
	defer func() {
		for identifier, original := range originals {
			identifier.Name = original
		}
	}()
	return hrc1Composition(set, check)
}

func brc1Runtime(set wave42LegacyGitSourceSet) []dpc1Finding {
	findings := brc1Composition(set, rfc1Runtime)
	function := set.methods["Runtime.ConfigureContextBranchOperations"]
	cleared, factory := token.NoPos, false
	if function != nil && function.Body != nil {
		for _, statement := range function.Body.List {
			ast.Inspect(statement, func(node ast.Node) bool {
				if assignment, assigned := node.(*ast.AssignStmt); assigned && len(assignment.Lhs) == len(assignment.Rhs) {
					for index, left := range assignment.Lhs {
						if rfc1Origin(function, left, assignment.Pos()) == "Runtime.ContextBranches" && workerSelectorPath(assignment.Rhs[index]) == "nil" {
							cleared = assignment.Pos()
						}
					}
				}
				return true
			})
			dpc1Calls(set, function, statement, func(call *ast.CallExpr, target string) {
				if strings.HasSuffix(target, ".ContextBranchOperations") {
					factory = true
					if cleared == token.NoPos || cleared >= call.Pos() {
						findings = append(findings, dpc1Finding{"runtime", "stale owner not cleared before factory"})
					}
				}
			})
		}
	}
	if !factory {
		findings = append(findings, dpc1Finding{"runtime", "missing actual Context factory"})
	}
	return findings
}

func brc1MCP(set wave42LegacyGitSourceSet) []dpc1Finding {
	var findings []dpc1Finding
	for _, entry := range []string{"Handler", "HandlerWithMemoryPreparation", "HandlerWithReadPreparation"} {
		rejected := false
		for _, function := range dpc1Reachable(set, entry) {
			if function.Type.Params != nil && len(function.Type.Params.List[0].Names) == 1 {
				parameter := function.Type.Params.List[0].Names[0].Name
				for _, statement := range function.Body.List {
					if conditional, condition := statement.(*ast.IfStmt); condition && rac1AssetRejects(conditional.Body) {
						rejected = rejected || dpc1Joined(conditional.Cond, token.LOR, func(expression ast.Expr) bool {
							return dpc1Compare(expression, token.NEQ, parameter+".ContextBranches", "nil")
						})
					}
				}
			}
		}
		if !rejected {
			findings = append(findings, dpc1Finding{"mcp", entry + " accepts actual/typed-nil commands"})
		}
	}
	return findings
}

func TestBRC1ActualOwnerPoliciesPassedWorkAndConsumers(test *testing.T) {
	application := wave48Package(test, filepath.Join("..", "app"))
	dpc1Report(test, append(brc1Owner(application), brc1SharedWork(application)...))
	dpc1Report(test, brc1Ports(wave48Package(test, filepath.Join("..", "ports", "inbound"))))
	dpc1Report(test, brc1HTTP(wave48Package(test, filepath.Join("..", "adapters", "delivery", "http"))))
	dpc1Report(test, brc1Runtime(wave48Package(test, filepath.Join("..", "serverruntime"))))
	dpc1Report(test, brc1MCP(wave48Package(test, filepath.Join("..", "mcpserver"))))
	dpc1Report(test, brc1Composition(wave48Package(test, filepath.Join("..", "..", "cmd", "cxtd")), rfc1APIBinding))
}

func TestBRC1RejectsActualSourceMutations(test *testing.T) {
	for _, scenario := range []struct {
		name, directory, rule string
		check                 func(wave42LegacyGitSourceSet) []dpc1Finding
		mutate                func(wave42LegacyGitSourceSet)
	}{
		{"reverse Service wrapper", "app", "reverse", brc1Owner, func(set wave42LegacyGitSourceSet) {
			set.methods["contextBranchOperations.Fork"].Body = dpc1Body(test, "var service *Service; return service.Fork(ctx, request)")
		}},
		{"public alias hides Service", "app", "alias", brc1Owner, func(set wave42LegacyGitSourceSet) {
			set.types["ContextBranchOperations"].Type = ast.NewIdent("Service")
		}},
		{"constructor missing", "app", "constructor", brc1Constructor, func(set wave42LegacyGitSourceSet) { delete(set.methods, "NewContextBranchOperations") }},
		{"typed nil accepted", "app", "constructor", brc1Constructor, func(set wave42LegacyGitSourceSet) {
			ast.Inspect(set.methods["NewContextBranchOperations"].Body, func(node ast.Node) bool {
				if call, called := node.(*ast.CallExpr); called && workerSelectorPath(call.Fun) == "rootAdapterPresent" {
					call.Fun = ast.NewIdent("acceptTypedNil")
				}
				return true
			})
		}},
		{"discarded scoped context", "app", "bound-context", brc1Owner, func(set wave42LegacyGitSourceSet) {
			function := set.methods["contextBranchOperations.ConfirmJoin"]
			changed := false
			dpc1Calls(set, function, function.Body, func(call *ast.CallExpr, target string) {
				if target == "ContextBranchScope.WithinJoin" {
					for _, argument := range call.Args {
						if callback, literal := argument.(*ast.FuncLit); literal {
							dpc1Calls(set, function, callback.Body, func(nested *ast.CallExpr, operation string) {
								if len(nested.Args) > 0 && (strings.HasPrefix(operation, "contextBranchOperations.") || operation == "ContextBranchAccess.AuthorizeJoin") {
									nested.Args[0], changed = ast.NewIdent("discardedContext"), true
								}
							})
						}
					}
				}
			})
			if !changed {
				test.Fatal("no actual scoped confirmation call changed")
			}
		}},
		{"actual Domain planner bypassed", "app", "policy", brc1Owner, func(set wave42LegacyGitSourceSet) {
			changed := false
			for _, function := range dpc1Reachable(set, "contextBranchOperations.ConfirmJoin") {
				dpc1Calls(set, function, function.Body, func(call *ast.CallExpr, target string) {
					if target == "domain.PlanJoin" {
						call.Fun.(*ast.SelectorExpr).Sel.Name, changed = "BypassPlanJoin", true
					}
				})
			}
			if !changed {
				test.Fatal("no actual Domain planner changed")
			}
		}},
		{"wrong original work", "app", "shared-work", brc1SharedWork, func(set wave42LegacyGitSourceSet) {
			changed := false
			for _, function := range set.methods {
				dpc1Calls(set, function, bodyOfDPC1(function), func(call *ast.CallExpr, target string) {
					if target == "NewContextBranchOperations" && len(call.Args) == 1 {
						config := rwa1AuthorityComposite(hrc1Resolve(function, call.Args[0], call.Pos()), "ContextBranchOperationsConfig")
						scope := hrc1Resolve(function, rwa1AuthorityField(config, "Scope"), call.Pos())
						ast.Inspect(scope, func(node ast.Node) bool {
							if field, keyed := node.(*ast.KeyValueExpr); keyed && workerSelectorPath(field.Key) == "work" {
								field.Value, changed = ast.NewIdent("unrelatedWork"), true
							}
							return true
						})
					}
				})
			}
			if !changed {
				test.Fatal("no actual scope work binding changed")
			}
		}},
		{"recorder fallback cannot hide wrong work", "app", "shared-work", brc1SharedWork, func(set wave42LegacyGitSourceSet) {
			changed := false
			for _, function := range set.methods {
				if !dpc1Calls(set, function, bodyOfDPC1(function), nil)["NewContextBranchOperations"] {
					continue
				}
				dpc1Calls(set, function, function.Body, func(call *ast.CallExpr, target string) {
					if target == "historyCommandsFromStores" || target == "historyCommandsForWork" {
						index, _ := hrc1Work(set.methods[target])
						if index >= 0 && index < len(call.Args) {
							call.Args[index], changed = ast.NewIdent("unrelatedWork"), true
						}
					}
				})
			}
			if !changed {
				test.Fatal("no actual recorder factory work changed")
			}
		}},
		{"pending field cannot hide wrong work", "app", "shared-work", brc1SharedWork, func(set wave42LegacyGitSourceSet) {
			changed := false
			for _, function := range set.methods {
				_, work := hrc1Work(function)
				dpc1Calls(set, function, bodyOfDPC1(function), func(call *ast.CallExpr, target string) {
					if target != "NewContextBranchOperations" || len(call.Args) != 1 {
						return
					}
					config := rwa1AuthorityComposite(hrc1Resolve(function, call.Args[0], call.Pos()), "ContextBranchOperationsConfig")
					pending := hrc1Resolve(function, rwa1AuthorityField(config, "Pending"), call.Pos())
					ast.Inspect(pending, func(node ast.Node) bool {
						if nested, called := node.(*ast.CallExpr); called && rfc1UsesPassedWork(set, nested, work) {
							index, _ := hrc1Work(set.methods[wave42LegacyGitFunctionName(nested.Fun)])
							if index >= 0 && index < len(nested.Args) {
								nested.Args[index], changed = ast.NewIdent("unrelatedWork"), true
							}
						}
						return true
					})
				})
			}
			if !changed {
				test.Fatal("no actual pending field factory work changed")
			}
		}},
		{"per-request backend capability discovery", "adapters/delivery/http", "http", brc1HTTP, func(set wave42LegacyGitSourceSet) {
			function := set.methods["Server.fork"]
			receiver := function.Recv.List[0].Names[0].Name
			discovery := dpc1Body(test, "_ = "+receiver+".b.(inbound.ContextBranchOperations)")
			function.Body.List = append(discovery.List, function.Body.List...)
		}},
		{"same-typed disconnected HTTP consumer", "adapters/delivery/http", "http", brc1HTTP, func(set wave42LegacyGitSourceSet) {
			shape := set.types["Server"].Type.(*ast.StructType)
			shape.Fields.List = append(shape.Fields.List, &ast.Field{Names: []*ast.Ident{ast.NewIdent("unrelatedBranches")}, Type: &ast.SelectorExpr{X: ast.NewIdent("inbound"), Sel: ast.NewIdent("ContextBranchOperations")}})
			changed := false
			for _, function := range dpc1Reachable(set, "Server.fork") {
				ast.Inspect(function.Body, func(node ast.Node) bool {
					if selector, selected := node.(*ast.SelectorExpr); selected && wave44PreparationMember(set, "Server", selector.Sel.Name, false, map[string]bool{}) == "inbound.ContextBranchOperations" {
						selector.Sel.Name, changed = "unrelatedBranches", true
					}
					return true
				})
			}
			if !changed {
				test.Fatal("no actual typed Fork consumer changed")
			}
		}},
		{"preview validation reordered", "app", "order", brc1Owner, func(set wave42LegacyGitSourceSet) {
			function := set.methods["contextBranchOperations.PreviewJoin"]
			changed := false
			for index, statement := range function.Body.List {
				if dpc1Calls(set, function, statement, nil)["domain.ValidateContentHash"] {
					for destination, candidate := range function.Body.List {
						if destination != index && dpc1Calls(set, function, candidate, nil)["ContextBranchScope.WithinPreview"] {
							function.Body.List[index], function.Body.List[destination], changed = candidate, statement, true
							break
						}
					}
					break
				}
			}
			if !changed {
				test.Fatal("no actual preview validation/scope pair")
			}
		}},
		{"confirmation preview rejection reordered", "app", "order", brc1Owner, func(set wave42LegacyGitSourceSet) {
			function := set.methods["contextBranchOperations.ConfirmJoin"]
			changed := false
			for index, statement := range function.Body.List {
				if conditional, condition := statement.(*ast.IfStmt); condition && dpc1ReturnsError(conditional.Body, "domain.ErrJoinPreviewChanged") {
					for destination, candidate := range function.Body.List {
						if dpc1Calls(set, function, candidate, nil)["ContextBranchScope.WithinJoin"] {
							function.Body.List[index], function.Body.List[destination], changed = candidate, statement, true
							break
						}
					}
					break
				}
			}
			if !changed {
				test.Fatal("no actual confirmation rejection/scope pair")
			}
		}},
	} {
		test.Run(scenario.name, func(test *testing.T) {
			set := wave48Package(test, filepath.Join("..", scenario.directory))
			if findings := scenario.check(set); len(findings) != 0 {
				test.Fatalf("unmodified production must pass before mutation: %+v", findings)
			}
			scenario.mutate(set)
			dpc1RequireRule(test, scenario.check(set), scenario.rule)
		})
	}
}
