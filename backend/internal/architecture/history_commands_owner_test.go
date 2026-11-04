package architecture

import (
	"go/ast"
	"go/token"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

func hrc1Require(set wave42LegacyGitSourceSet, entry, rule string, operations ...string) []dpc1Finding {
	var findings []dpc1Finding
	calls := dpc1ExecutionCalls(set, entry)
	for _, operation := range operations {
		if !calls[operation] {
			findings = append(findings, dpc1Finding{rule, entry + " lost " + operation})
		}
	}
	return findings
}

func hrc1Fields(set wave42LegacyGitSourceSet) []dpc1Finding {
	ports := []string{"HistoryMetadata", "HistoryEventStore", "HistoryMemoryReader", "HistoryDocumentSources", "HistoryWriteScope", "HistoryFollowups", "outbound.HistoryDocumentProofStore", "[]domain.DocumentIdentity"}
	findings := dpc1Fields(set, "historyRecorder", append(ports, "HistoryProfile", "error"), ports)
	for _, detail := range wave47GitHubStructViolations(set, "HistoryCommandsConfig", map[string]string{
		"Profile": "HistoryProfile", "Metadata": "HistoryMetadata", "Events": "HistoryEventStore", "Memories": "HistoryMemoryReader",
		"Sources": "HistoryDocumentSources", "Scope": "HistoryWriteScope", "Followups": "HistoryFollowups",
		"Preparation": "outbound.HistoryDocumentProofStore", "SupportedIdentities": "[]domain.DocumentIdentity",
	}) {
		findings = append(findings, dpc1Finding{"fields", detail})
	}
	alias := set.types["HistoryCommands"]
	if alias == nil || !alias.Assign.IsValid() || wave42LegacyGitType(alias.Type) != "historyRecorder" {
		findings = append(findings, dpc1Finding{"alias", "HistoryCommands must alias the actual private historyRecorder, not Service"})
	}
	for port, methods := range map[string][]string{
		"HistoryMetadata": {"GetRepo", "GetSnapshot"}, "HistoryEventStore": {"ListHistoryEvents", "ApplyHistoryEvent"},
		"HistoryMemoryReader": {"GetMemory"}, "HistoryDocumentSources": {"VerifySnapshotDocument"},
		"HistoryWriteScope": {"WithinHistoryWrite", "WithinHistoryObservation", "WithinHistoryRead"},
		"HistoryFollowups":  {"NotifyRefUpdate", "QueueHistoryGitScan", "WakePRSourceJobs"},
	} {
		findings = append(findings, dpc1Interface(set, port, methods)...)
	}
	return findings
}

func hrc1Constructor(set wave42LegacyGitSourceSet) []dpc1Finding {
	function := set.methods["NewHistoryCommands"]
	if function == nil || function.Body == nil || wave46EmailFieldTypes(function.Type.Params) != "HistoryCommandsConfig" || wave46EmailFieldTypes(function.Type.Results) != "*HistoryCommands,error" || len(function.Type.Params.List[0].Names) != 1 {
		return []dpc1Finding{{"constructor", "missing exact public constructor"}}
	}
	config := function.Type.Params.List[0].Names[0].Name
	allocation, complete := function.Body.End(), function.Body.End()
	checks := map[string]token.Pos{}
	var conditions []*ast.IfStmt
	var loops []*ast.RangeStmt
	blocks := map[string][]*ast.BlockStmt{"complete": nil, "preparation": nil}
	ast.Inspect(function.Body, func(node ast.Node) bool {
		if literal, composite := node.(*ast.CompositeLit); composite && (wave42LegacyGitType(literal.Type) == "historyRecorder" || wave42LegacyGitType(literal.Type) == "HistoryCommands") && literal.Pos() < allocation {
			allocation = literal.Pos()
		}
		if conditional, condition := node.(*ast.IfStmt); condition {
			conditions = append(conditions, conditional)
			for profile, comparison := range map[string]bool{"complete": dpc1Compare(conditional.Cond, token.EQL, config+".Profile", "HistoryComplete"), "preparation": dpc1Compare(conditional.Cond, token.NEQ, config+".Preparation", "nil")} {
				if comparison {
					blocks[profile] = append(blocks[profile], conditional.Body)
				}
			}
		}
		if loop, ranged := node.(*ast.RangeStmt); ranged {
			loops = append(loops, loop)
		}
		return true
	})
	for _, conditional := range conditions {
		if conditional.End() >= allocation {
			continue
		}
		paths := map[string][]string{}
		for _, dependency := range []string{"Metadata", "Scope", "Events", "Memories", "Sources", "Followups", "Preparation"} {
			paths[config+"."+dependency] = []string{config + "." + dependency}
		}
		for _, loop := range loops {
			values, listed := loop.X.(*ast.CompositeLit)
			if listed && loop.Pos() < conditional.Pos() && conditional.End() < loop.End() {
				for _, value := range values.Elts {
					paths[workerSelectorPath(loop.Value)] = append(paths[workerSelectorPath(loop.Value)], workerSelectorPath(value))
				}
			}
		}
		validation := dpc1ReturnsError(conditional.Body, "domain.ErrValidation")
		if validation && dpc1Joined(conditional.Cond, token.LAND, func(expression ast.Expr) bool {
			return dpc1Compare(expression, token.NEQ, config+".Profile", "HistoryCompatibility")
		}) && dpc1Joined(conditional.Cond, token.LAND, func(expression ast.Expr) bool {
			return dpc1Compare(expression, token.NEQ, config+".Profile", "HistoryComplete")
		}) {
			checks["profile"] = conditional.Pos()
		}
		for dependency, values := range paths {
			absent := dpc1Joined(conditional.Cond, token.LOR, func(expression ast.Expr) bool { return rac1AssetAbsent(expression, dependency) })
			typed := dpc1Joined(conditional.Cond, token.LAND, func(expression ast.Expr) bool { return rac1AssetAbsent(expression, dependency) }) && dpc1Joined(conditional.Cond, token.LAND, func(expression ast.Expr) bool { return dpc1Compare(expression, token.NEQ, dependency, "nil") })
			for _, value := range values {
				if validation && (typed || absent && (value == config+".Metadata" || value == config+".Scope")) {
					checks["nil "+value] = conditional.Pos()
				}
				for profile, scopes := range blocks {
					qualified := wave48Inside(conditional, scopes) || dpc1Joined(conditional.Cond, token.LAND, func(expression ast.Expr) bool {
						return profile == "complete" && dpc1Compare(expression, token.EQL, config+".Profile", "HistoryComplete") || profile == "preparation" && dpc1Compare(expression, token.NEQ, config+".Preparation", "nil")
					})
					missing := dpc1Joined(conditional.Cond, token.LAND, func(expression ast.Expr) bool {
						return dpc1Joined(expression, token.LOR, func(candidate ast.Expr) bool { return dpc1Compare(candidate, token.EQL, dependency, "nil") })
					})
					if qualified && missing && dpc1ReturnsError(conditional.Body, "domain.ErrCatalogUnsupported") {
						checks[profile+" "+value] = conditional.Pos()
						if conditional.Pos() < complete {
							complete = conditional.Pos()
						}
					}
				}
			}
		}
	}
	var findings []dpc1Finding
	for _, requirement := range []string{"profile", "nil " + config + ".Metadata", "nil " + config + ".Scope", "nil " + config + ".Events", "nil " + config + ".Memories", "nil " + config + ".Sources", "nil " + config + ".Followups", "nil " + config + ".Preparation", "complete " + config + ".Events", "complete " + config + ".Memories", "complete " + config + ".Sources", "complete " + config + ".Followups", "preparation " + config + ".Events", "preparation " + config + ".Sources"} {
		if checks[requirement] == 0 || strings.HasPrefix(requirement, "nil ") && checks[requirement] >= complete {
			findings = append(findings, dpc1Finding{"constructor", "missing/late explicit rejection: " + requirement})
		}
	}
	copied := false
	for _, reachable := range dpc1Reachable(set, "NewHistoryCommands") {
		dpc1Calls(set, reachable, reachable.Body, func(call *ast.CallExpr, target string) {
			copied = copied || target == "append" && call.Ellipsis.IsValid() && len(call.Args) > 1 && workerSelectorPath(call.Args[len(call.Args)-1]) == config+".SupportedIdentities"
			if strings.HasPrefix(target, "HistoryMetadata.") || strings.HasPrefix(target, "HistoryEventStore.") || strings.HasPrefix(target, "HistoryWriteScope.") || strings.HasPrefix(target, "HistoryFollowups.") || strings.HasPrefix(target, "HistoryDocumentSources.") || strings.HasPrefix(target, "HistoryMemoryReader.") || strings.HasPrefix(target, "outbound.") || target == "newRepositoryWork" || target == "NewService" || strings.HasPrefix(target, "resourceWork") {
				findings = append(findings, dpc1Finding{"constructor", "constructor performs IO/work: " + target})
			}
		})
	}
	if allocation == function.Body.End() || !copied || set.methods["rootAdapterPresent"] == nil || !wave42LegacyGitCalls(bodyOfDPC1(set.methods["rootAdapterPresent"]))["IsNil"] {
		findings = append(findings, dpc1Finding{"constructor", "missing actual allocation, defensive identities copy or typed-nil implementation"})
	}
	return findings
}

func hrc1Bound(set wave42LegacyGitSourceSet, entry, scope string, operations ...string) []dpc1Finding {
	function := set.methods[entry]
	boundCalls := map[string]bool{}
	dpc1Calls(set, function, bodyOfDPC1(function), func(call *ast.CallExpr, target string) {
		if target != scope {
			return
		}
		for _, argument := range call.Args {
			callback, literal := argument.(*ast.FuncLit)
			if !literal || callback.Type.Params == nil || len(callback.Type.Params.List) == 0 || len(callback.Type.Params.List[0].Names) != 1 {
				continue
			}
			bound := callback.Type.Params.List[0].Names[0].Name
			dpc1Calls(set, function, callback.Body, func(nested *ast.CallExpr, operation string) {
				forwarded := len(nested.Args) > 0 && workerSelectorPath(nested.Args[0]) == bound
				boundCalls[operation] = boundCalls[operation] || forwarded
				if operation == "historyRecorder.verifyStoredSnapshotDoc" && forwarded {
					helper := set.methods[operation]
					if helper != nil && len(helper.Type.Params.List) > 0 && len(helper.Type.Params.List[0].Names) == 1 {
						contextName := helper.Type.Params.List[0].Names[0].Name
						dpc1Calls(set, helper, helper.Body, func(leaf *ast.CallExpr, target string) {
							boundCalls[target] = boundCalls[target] || len(leaf.Args) > 0 && workerSelectorPath(leaf.Args[0]) == contextName
						})
					}
				}
			})
			if scope == "historyRecorder.recordHistoryPrepared" && len(callback.Type.Params.List) == 2 && len(callback.Type.Params.List[1].Names) == 1 {
				current, pin, allocation, roots := callback.Type.Params.List[1].Names[0].Name, token.NoPos, token.NoPos, false
				dpc1Calls(set, function, callback.Body, func(nested *ast.CallExpr, operation string) {
					if operation == "outbound.HistoryDocumentProof.Pin" {
						pin = nested.Pos()
					}
					if operation == "make" && len(nested.Args) > 0 && workerSelectorPath(nested.Args[0]) == "historyVerification" {
						allocation = nested.Pos()
					}
					if operation == "reflect.DeepEqual" {
						for _, argument := range nested.Args {
							if rootSet, called := argument.(*ast.CallExpr); called && workerSelectorPath(rootSet.Fun) == "historyRootSet" && len(rootSet.Args) == 1 && workerSelectorPath(rootSet.Args[0]) == current {
								roots = true
							}
						}
					}
				})
				boundCalls["current roots and retry-local verification after pins"] = roots && pin != 0 && allocation > pin
			}
		}
	})
	var findings []dpc1Finding
	for _, operation := range operations {
		if !boundCalls[operation] {
			findings = append(findings, dpc1Finding{"bound-context", entry + " lost " + scope + " bound " + operation})
		}
	}
	return findings
}

func hrc1Owner(set wave42LegacyGitSourceSet) []dpc1Finding {
	findings := append(hrc1Fields(set), hrc1Constructor(set)...)
	for entry, operations := range map[string][]string{
		"recordHistory":                        {"historyRecorder.recordHistoryPrepared"},
		"recordHistoryPrepared":                {"HistoryEventStore.ListHistoryEvents", "HistoryEventStore.ApplyHistoryEvent", "HistoryMetadata.GetRepo", "HistoryMetadata.GetSnapshot", "HistoryMemoryReader.GetMemory", "HistoryDocumentSources.VerifySnapshotDocument", "HistoryFollowups.NotifyRefUpdate", "domain.ValidateHistoryEvent", "domain.ValidateCreationOrigin", "domain.IsInitialMemorySelection", "domain.IsPublicationProof", "domain.MemoryDigestHash", "historyRecorder.wakePublishedPRJobs"},
		"wakePublishedPRJobs":                  {"HistoryFollowups.QueueHistoryGitScan", "HistoryFollowups.WakePRSourceJobs"},
		"recordHistoryWithDocumentPreparation": {"outbound.HistoryDocumentProofStore.WithinHistoryDocumentProof", "HistoryWriteScope.WithinHistoryObservation", "HistoryWriteScope.WithinHistoryRead", "HistoryWriteScope.WithinHistoryWrite", "outbound.HistoryDocumentProof.Capture", "outbound.HistoryDocumentProof.Pin", "historyRootSet", "reflect.DeepEqual"},
		"RecordHistory":                        {"historyRecorder.recordHistoryWithDocumentPreparation", "acquireResources"},
	} {
		findings = append(findings, hrc1Require(set, "historyRecorder."+entry, "payload", operations...)...)
		if facade := set.methods["Service."+entry]; facade != nil {
			findings = append(findings, hrc1Require(set, "Service."+entry, "facade", "historyRecorder."+entry)...)
		}
	}
	for entry, function := range set.methods {
		if !strings.HasPrefix(entry, "historyRecorder.") && !strings.HasPrefix(entry, "historyCommandScope.") {
			continue
		}
		for target := range dpc1ExecutionCalls(set, entry) {
			if strings.HasPrefix(target, "Service.") || target == "NewService" || strings.HasPrefix(target, "reflect.") && target != "reflect.DeepEqual" {
				findings = append(findings, dpc1Finding{"reverse", entry + " reaches parent/discovery " + target})
			}
		}
		ast.Inspect(function, func(node ast.Node) bool {
			switch value := node.(type) {
			case *ast.TypeAssertExpr, *ast.TypeSwitchStmt:
				findings = append(findings, dpc1Finding{"runtime-assertion", "capability discovery outside composition: " + entry})
			case *ast.Ident:
				if value.Name == "Service" || value.Name == "NewService" || value.Name == "MetadataStore" || value.Name == "BlobStore" || value.Name == "GitEngine" {
					findings = append(findings, dpc1Finding{"reverse", "broad/reverse owner dependency: " + entry})
				}
			}
			return true
		})
	}
	preparation := "historyRecorder.recordHistoryWithDocumentPreparation"
	for scope, operations := range map[string][]string{
		"outbound.HistoryDocumentProofStore.WithinHistoryDocumentProof": {"HistoryWriteScope.WithinHistoryObservation", "HistoryWriteScope.WithinHistoryRead", "HistoryWriteScope.WithinHistoryWrite"},
		"HistoryWriteScope.WithinHistoryObservation":                    {"HistoryEventStore.ListHistoryEvents"},
		"HistoryWriteScope.WithinHistoryRead":                           {"HistoryMetadata.GetSnapshot", "HistoryDocumentSources.VerifySnapshotDocument", "outbound.HistoryDocumentProof.Capture"},
		"HistoryWriteScope.WithinHistoryWrite":                          {"historyRecorder.recordHistoryPrepared"},
		"historyRecorder.recordHistoryPrepared":                         {"outbound.HistoryDocumentProof.Pin", "current roots and retry-local verification after pins"},
	} {
		findings = append(findings, hrc1Bound(set, preparation, scope, operations...)...)
	}
	for _, entry := range []string{"recordHistoryPrepared", "recordHistoryWithDocumentPreparation"} {
		function, rooted := set.methods["historyRecorder."+entry], false
		if function != nil {
			ast.Inspect(function.Body, func(node ast.Node) bool {
				if literal, composite := node.(*ast.CompositeLit); composite && wave42LegacyGitType(literal.Type) == "[]domain.ContentHash" {
					var fields []string
					for _, value := range literal.Elts {
						if selector, selected := value.(*ast.SelectorExpr); selected {
							fields = append(fields, selector.Sel.Name)
						}
					}
					rooted = rooted || strings.Join(fields, ",") == "Source,Target,SharedTarget,MemorySource"
				}
				return true
			})
		}
		if !rooted || !wave42LegacyGitMentions(bodyOfDPC1(function), "MemorySelectionParent") {
			findings = append(findings, dpc1Finding{"roots", entry + " lost actual event/predecessor root set"})
		}
	}
	return findings
}

func hrc1Resolve(function *ast.FuncDecl, expression ast.Expr, before token.Pos) ast.Expr {
	for depth := 0; depth < 8 && function != nil; depth++ {
		name, named := expression.(*ast.Ident)
		if !named {
			break
		}
		var resolved ast.Expr
		position := token.NoPos
		ast.Inspect(function.Body, func(node ast.Node) bool {
			if assignment, assigned := node.(*ast.AssignStmt); assigned && assignment.Pos() < before && assignment.Pos() > position {
				for index, left := range assignment.Lhs {
					if workerSelectorPath(left) == name.Name && index < len(assignment.Rhs) {
						resolved, position = assignment.Rhs[index], assignment.Pos()
					}
				}
			}
			return true
		})
		if resolved == nil {
			break
		}
		expression, before = resolved, position
	}
	return expression
}

func hrc1Work(function *ast.FuncDecl) (int, string) {
	if function != nil && function.Type.Params != nil {
		for index, field := range function.Type.Params.List {
			if wave42LegacyGitType(field.Type) == "*repositoryWork" && len(field.Names) == 1 {
				return index, field.Names[0].Name
			}
		}
	}
	return -1, ""
}

func hrc1Scope(set wave42LegacyGitSourceSet) []dpc1Finding {
	var findings []dpc1Finding
	factories := map[string]bool{}
	for entry, function := range set.methods {
		_, work := hrc1Work(function)
		dpc1Calls(set, function, function.Body, func(call *ast.CallExpr, target string) {
			if target != "NewHistoryCommands" || len(call.Args) != 1 {
				return
			}
			config := rwa1AuthorityComposite(hrc1Resolve(function, call.Args[0], call.Pos()), "HistoryCommandsConfig")
			scope := rwa1AuthorityComposite(hrc1Resolve(function, rwa1AuthorityField(config, "Scope"), call.Pos()), "historyCommandScope")
			factories[entry] = work != "" && scope != nil && workerSelectorPath(rwa1AuthorityField(scope, "work")) == work
		})
	}
	usesWork := func(function *ast.FuncDecl, expression ast.Expr, expected string) bool {
		call, called := hrc1Resolve(function, expression, function.Body.End()).(*ast.CallExpr)
		if !called || expected == "" {
			return false
		}
		factory := wave42LegacyGitFunctionName(call.Fun)
		index, parameter := hrc1Work(set.methods[factory])
		forwarded := factories[factory]
		dpc1Calls(set, set.methods[factory], bodyOfDPC1(set.methods[factory]), func(nested *ast.CallExpr, selected string) {
			position, _ := hrc1Work(set.methods[selected])
			forwarded = forwarded || factories[selected] && position >= 0 && position < len(nested.Args) && workerSelectorPath(nested.Args[position]) == parameter
		})
		return forwarded && index >= 0 && index < len(call.Args) && workerSelectorPath(call.Args[index]) == expected
	}
	assembly := set.methods["assemblePRPromotionWorker"]
	_, passed := hrc1Work(assembly)
	workerBound := false
	if assembly != nil {
		ast.Inspect(assembly.Body, func(node ast.Node) bool {
			if literal, composite := node.(*ast.CompositeLit); composite && wave42LegacyGitType(literal.Type) == "PRPromotionWorker" {
				workerBound = passed != "" && workerSelectorPath(rwa1AuthorityField(literal, "work")) == passed && usesWork(assembly, rwa1AuthorityField(literal, "history"), passed)
			}
			return true
		})
	}
	rootBound := false
	for factory, valid := range factories {
		rootBound = rootBound || valid && dpc1ExecutionCalls(set, "Service.HistoryCommands")[factory]
		if valid && dpc1ExecutionCalls(set, factory)["newRepositoryWork"] {
			findings = append(findings, dpc1Finding{"shared-work", "composition reconstructs passed work"})
		}
	}
	if !workerBound || !rootBound || wave44PreparationMember(set, "historyCommandScope", "work", false, map[string]bool{}) != "repositoryWork" {
		findings = append(findings, dpc1Finding{"shared-work", "actual root/PR factory must bind the same passed work through historyCommandScope"})
	}
	write := set.methods["historyCommandScope.WithinHistoryWrite"]
	shared := false
	dpc1Calls(set, write, bodyOfDPC1(write), func(call *ast.CallExpr, target string) {
		shared = shared || target == "repositoryWriteErrorWithWork" && len(call.Args) > 1 && rfc1Origin(write, call.Args[1], call.Pos()) == "historyCommandScope.work"
	})
	if !shared {
		findings = append(findings, dpc1Finding{"shared-work", "write scope replaced original work"})
	}
	for _, entry := range []string{"WithinHistoryObservation", "WithinHistoryRead"} {
		for target := range dpc1ExecutionCalls(set, "historyCommandScope."+entry) {
			if strings.Contains(target, "repositoryWrite") || strings.HasSuffix(target, ".AuditWrite") || strings.HasSuffix(target, ".revisionWrite") || entry == "WithinHistoryRead" && (strings.HasSuffix(target, ".authorizeRepositoryWrite") || strings.HasSuffix(target, ".AuthorizeWrite")) {
				findings = append(findings, dpc1Finding{"observation", entry + " reaches writing authorization/audit: " + target})
			}
		}
	}
	findings = append(findings, hrc1Require(set, "PRPromotionWorker.promoteBoundPRInTransaction", "worker", "historyRecorder.recordHistoryPrepared")...)
	findings = append(findings, hrc1Require(set, "historyCommandScope.WithinHistoryObservation", "observation", "outbound.RepositoryTransactions.WithinRepository")...)
	findings = append(findings, hrc1Require(set, "historyCommandScope.WithinHistoryRead", "observation", "outbound.RepositoryTransactions.WithinReadSnapshot")...)
	return findings
}

func hrc1HTTP(set wave42LegacyGitSourceSet) []dpc1Finding {
	var findings []dpc1Finding
	request := set.methods["Server.recordHistoryEvent"]
	admission, decode, release := token.NoPos, token.NoPos, token.NoPos
	dpc1Calls(set, request, bodyOfDPC1(request), func(call *ast.CallExpr, target string) {
		if target == "Server.admitResourceRequest" && len(call.Args) == 3 && workerSelectorPath(call.Args[2]) == "app.ResourceBulk" {
			admission = call.Pos()
		}
		if target == "Server.decodeLimited" {
			decode = call.Pos()
		}
	})
	if request != nil {
		ast.Inspect(request.Body, func(node ast.Node) bool {
			if deferred, ok := node.(*ast.DeferStmt); ok && workerSelectorPath(deferred.Call.Fun) == "release" {
				release = deferred.Pos()
			}
			return true
		})
	}
	if admission == 0 || decode <= admission || release <= admission || release >= decode {
		findings = append(findings, dpc1Finding{"http", "Bulk admission must cover decode through deferred response completion"})
	}
	setter, bound := set.methods["Server.SetHistoryCommands"], false
	if setter != nil {
		ast.Inspect(setter.Body, func(node ast.Node) bool {
			if assignment, assigned := node.(*ast.AssignStmt); assigned && len(assignment.Lhs) == 1 && len(assignment.Rhs) == 1 {
				bound = bound || rfc1Origin(setter, assignment.Lhs[0], assignment.Pos()) == "Server.historyCommands" && rfc1Origin(setter, assignment.Rhs[0], assignment.Pos()) == "inbound.HistoryCommands"
			}
			return true
		})
	}
	if !bound || wave44PreparationMember(set, "Server", "historyCommands", false, map[string]bool{}) != "inbound.HistoryCommands" {
		findings = append(findings, dpc1Finding{"http", "setter must retain the actual narrow injection"})
	}
	for route, handler := range map[string]string{"POST /api/v1/repos/{repoID}/history": "recordHistory", "POST /api/v1/repos/{repoID}/history/memory-selection": "recordMemorySelection"} {
		registered, used := false, false
		for _, function := range dpc1Reachable(set, "Server.Handler") {
			dpc1Calls(set, function, function.Body, func(call *ast.CallExpr, target string) {
				if !strings.HasSuffix(target, ".HandleFunc") || len(call.Args) != 2 {
					return
				}
				literal, quoted := call.Args[0].(*ast.BasicLit)
				if quoted {
					pattern, err := strconv.Unquote(literal.Value)
					ast.Inspect(call.Args[1], func(node ast.Node) bool {
						if selector, selected := node.(*ast.SelectorExpr); selected {
							registered = registered || err == nil && pattern == route && rfc1Origin(function, selector, call.Pos()) == "Server."+handler
						}
						return true
					})
				}
			})
		}
		for _, function := range dpc1Reachable(set, "Server."+handler) {
			dpc1Calls(set, function, function.Body, func(call *ast.CallExpr, target string) {
				if strings.HasSuffix(target, ".RecordHistory") {
					correct := target == "inbound.HistoryCommands.RecordHistory" && rfc1Origin(function, call.Fun, call.Pos()) == "Server.historyCommands.RecordHistory"
					used = used || correct
					if !correct {
						findings = append(findings, dpc1Finding{"http", "route bypasses injected historyCommands: " + target})
					}
				}
			})
		}
		if !registered || !used {
			findings = append(findings, dpc1Finding{"http", "disconnected live History route: " + route})
		}
	}
	return findings
}

func hrc1Composition(set wave42LegacyGitSourceSet, check func(wave42LegacyGitSourceSet) []dpc1Finding) []dpc1Finding {
	names := strings.NewReplacer("ConfigureHistoryCommands", "ConfigureRepositoryRefCommands", "SetHistoryCommands", "SetRepositoryRefCommands", "NewHistoryCommands", "NewRepositoryRefCommands", "hrc1Factory", "RepositoryRefCommands", "hrc1Field", "RepositoryRefs", "historyCommands", "repositoryRefs", "HistoryCommands", "RepositoryRefCommands")
	for _, file := range set.files {
		ast.Inspect(file, func(node ast.Node) bool {
			switch value := node.(type) {
			case *ast.CallExpr:
				if selector, selected := value.Fun.(*ast.SelectorExpr); selected && selector.Sel.Name == "HistoryCommands" {
					selector.Sel.Name = "hrc1Factory"
				}
			case *ast.SelectorExpr:
				if value.Sel.Name == "HistoryCommands" && workerSelectorPath(value.X) != "inbound" && workerSelectorPath(value.X) != "app" {
					value.Sel.Name = "hrc1Field"
				}
			case *ast.Field:
				for _, name := range value.Names {
					if name.Name == "HistoryCommands" {
						name.Name = "hrc1Field"
					}
				}
			}
			if identifier, named := node.(*ast.Ident); named {
				if strings.Contains(identifier.Name, "RepositoryRef") || identifier.Name == "repositoryRefs" {
					identifier.Name = "hrc1Excluded" + identifier.Name
				} else {
					identifier.Name = names.Replace(identifier.Name)
				}
			}
			return true
		})
	}
	methods, types := map[string]*ast.FuncDecl{}, map[string]*ast.TypeSpec{}
	for _, function := range set.methods {
		name := function.Name.Name
		if function.Recv != nil {
			name = wave44PreparationNamedType(function.Recv.List[0].Type) + "." + name
		}
		methods[name] = function
	}
	for _, declaration := range set.types {
		types[declaration.Name.Name] = declaration
	}
	set.methods, set.types = methods, types
	return check(set)
}

func TestHRC1ActualHistoryOwnerAndSharedWork(test *testing.T) {
	set := wave48Package(test, filepath.Join("..", "app"))
	for _, finding := range append(hrc1Owner(set), hrc1Scope(set)...) {
		test.Errorf("HRC-1 %s: %s", finding.rule, finding.detail)
	}
	dpc1Report(test, dpc1Interface(wave48Package(test, filepath.Join("..", "ports", "inbound")), "HistoryCommands", []string{"RecordHistory"}))
}

func TestHRC1DirectHTTPRuntimeAPIAndMCPConsumers(test *testing.T) {
	dpc1Report(test, hrc1HTTP(wave48Package(test, filepath.Join("..", "adapters", "delivery", "http"))))
	dpc1Report(test, hrc1Composition(wave48Package(test, filepath.Join("..", "serverruntime")), rfc1Runtime))
	dpc1Report(test, hrc1Composition(wave48Package(test, filepath.Join("..", "mcpserver")), rfc1MCP))
	dpc1Report(test, hrc1Composition(wave48Package(test, filepath.Join("..", "..", "cmd", "cxtd")), rfc1APIBinding))
	dpc1Report(test, hrc1Composition(wave48Package(test, filepath.Join("..", "app")), func(set wave42LegacyGitSourceSet) []dpc1Finding {
		var findings []dpc1Finding
		if set.methods["NewReadServices"] == nil {
			return []dpc1Finding{{"mcp", "missing actual read-only root"}}
		}
		for entry, function := range dpc1Reachable(set, "NewReadServices") {
			if rfc1Initializes(function.Body) || wave42LegacyGitCalls(function.Body)["NewService"] {
				findings = append(findings, dpc1Finding{"mcp", "read-only root constructs History: " + entry})
			}
		}
		return findings
	}))
}

func TestHRC1MutatedActualSourcesRejectReverseDisconnectedAndWrongWork(test *testing.T) {
	for _, scenario := range []struct {
		name, directory, rule string
		check                 func(wave42LegacyGitSourceSet) []dpc1Finding
		mutate                func(wave42LegacyGitSourceSet)
	}{
		{"Service callback", "app", "reverse", hrc1Owner, func(set wave42LegacyGitSourceSet) {
			set.methods["historyRecorder.recordHistoryPrepared"].Body = dpc1Body(test, "var service *Service; return service.RecordHistory(ctx, event)")
		}},
		{"public Service alias", "app", "alias", hrc1Owner, func(set wave42LegacyGitSourceSet) { set.types["HistoryCommands"].Type = ast.NewIdent("Service") }},
		{"constructor shell", "app", "constructor", hrc1Owner, func(set wave42LegacyGitSourceSet) {
			set.methods["NewHistoryCommands"].Body = dpc1Body(test, "return &historyRecorder{}, nil")
		}},
		{"unbound verification caller", "app", "bound-context", hrc1Owner, func(set wave42LegacyGitSourceSet) {
			ast.Inspect(set.methods["historyRecorder.recordHistoryWithDocumentPreparation"].Body, func(node ast.Node) bool {
				if call, called := node.(*ast.CallExpr); called && len(call.Args) > 0 {
					if selector, selected := call.Fun.(*ast.SelectorExpr); selected && selector.Sel.Name == "verifyStoredSnapshotDoc" {
						call.Args[0] = ast.NewIdent("unbound")
					}
				}
				return true
			})
		}},
		{"unbound verification helper", "app", "bound-context", hrc1Owner, func(set wave42LegacyGitSourceSet) {
			ast.Inspect(set.methods["historyRecorder.verifyStoredSnapshotDoc"].Body, func(node ast.Node) bool {
				if call, called := node.(*ast.CallExpr); called && len(call.Args) > 0 {
					if selector, selected := call.Fun.(*ast.SelectorExpr); selected && selector.Sel.Name == "VerifySnapshotDocument" {
						call.Args[0] = ast.NewIdent("unbound")
					}
				}
				return true
			})
		}},
		{"wrong PR work", "app", "shared-work", hrc1Scope, func(set wave42LegacyGitSourceSet) {
			ast.Inspect(set.methods["assemblePRPromotionWorker"].Body, func(node ast.Node) bool {
				if literal, composite := node.(*ast.CompositeLit); composite && wave42LegacyGitType(literal.Type) == "PRPromotionWorker" {
					for _, element := range literal.Elts {
						if field, keyed := element.(*ast.KeyValueExpr); keyed && workerSelectorPath(field.Key) == "work" {
							field.Value = ast.NewIdent("unrelatedWork")
						}
					}
				}
				return true
			})
		}},
		{"same-type disconnected HTTP owner", "adapters/delivery/http", "http", hrc1HTTP, func(set wave42LegacyGitSourceSet) {
			shape := set.types["Server"].Type.(*ast.StructType)
			shape.Fields.List = append(shape.Fields.List, &ast.Field{Names: []*ast.Ident{ast.NewIdent("unrelatedHistory")}, Type: &ast.SelectorExpr{X: ast.NewIdent("inbound"), Sel: ast.NewIdent("HistoryCommands")}})
			ast.Inspect(set.methods["Server.recordHistoryEvent"].Body, func(node ast.Node) bool {
				if selector, selected := node.(*ast.SelectorExpr); selected && selector.Sel.Name == "historyCommands" {
					selector.Sel.Name = "unrelatedHistory"
				}
				return true
			})
		}},
	} {
		test.Run(scenario.name, func(test *testing.T) {
			set := wave48Package(test, filepath.Join("..", scenario.directory))
			if findings := scenario.check(set); len(findings) != 0 {
				test.Fatalf("unmodified source must satisfy guard: %+v", findings)
			}
			scenario.mutate(set)
			dpc1RequireRule(test, scenario.check(set), scenario.rule)
		})
	}
}
