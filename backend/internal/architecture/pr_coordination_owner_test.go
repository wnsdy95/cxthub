package architecture

import (
	"go/ast"
	"go/token"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

func prc1Fields(set wave42LegacyGitSourceSet) []dpc1Finding {
	var findings []dpc1Finding
	for owner, fields := range map[string]map[string]string{
		"prCoordinationCommands":    {"scope": "PRPromotionScope", "jobs": "PRPromotionJobControl", "submission": "PRPromotionSubmission", "executor": "PRPromotionExecutor", "associations": "outbound.PRJobAssociationStore", "supported": "[]domain.DocumentIdentity", "profile": "PRCoordinationProfile", "configurationError": "error"},
		"PRPromotionCommandsConfig": {"Scope": "PRPromotionScope", "Jobs": "PRPromotionJobControl", "Submission": "PRPromotionSubmission", "Executor": "PRPromotionExecutor", "Associations": "outbound.PRJobAssociationStore", "SupportedIdentities": "[]domain.DocumentIdentity", "Profile": "PRCoordinationProfile"},
		"prCoordinationScope":       {"work": "*repositoryWork"},
	} {
		for _, detail := range wave47GitHubStructViolations(set, owner, fields) {
			findings = append(findings, dpc1Finding{"fields", detail})
		}
	}
	alias := set.types["PRPromotionCommands"]
	if alias == nil || !alias.Assign.IsValid() || wave42LegacyGitType(alias.Type) != "prCoordinationCommands" {
		findings = append(findings, dpc1Finding{"alias", "public PRPromotionCommands must alias the actual private coordination owner"})
	}
	for port, methods := range map[string][]string{
		"PRPromotionScope":      {"WithinPRSubmit", "WithinPRRetry", "WithinPRRead"},
		"PRPromotionJobControl": {"GetPRJob", "ListPRJobs", "ClaimPRJob", "RetryPRJob"},
		"PRPromotionSubmission": {"SubmitPRPromotion"}, "PRPromotionExecutor": {"PromoteBoundPR", "RunPRJob"},
	} {
		findings = append(findings, dpc1Interface(set, port, methods)...)
	}
	return findings
}

func prc1Signature(fields *ast.FieldList) string {
	var kinds []string
	if fields != nil {
		for _, field := range fields.List {
			kind := wave42LegacyGitType(field.Type)
			if callback, function := field.Type.(*ast.FuncType); function {
				kind = "func(" + prc1Signature(callback.Params) + ")(" + prc1Signature(callback.Results) + ")"
			}
			for count := 0; count < max(1, len(field.Names)); count++ {
				kinds = append(kinds, kind)
			}
		}
	}
	return strings.Join(kinds, ",")
}

func prc1Ports(set wave42LegacyGitSourceSet) []dpc1Finding {
	var findings []dpc1Finding
	for owner, methods := range map[string]map[string]string{
		"PRPromotionJobControl": {
			"GetPRJob": "context.Context,domain.ContentHash,string)(domain.PRPromotionJob,error", "ListPRJobs": "context.Context,domain.ContentHash)([]domain.PRPromotionJob,error",
			"ClaimPRJob": "context.Context,domain.ContentHash,string,time.Time,time.Duration)(domain.PRPromotionJob,error", "RetryPRJob": "context.Context,domain.ContentHash,string,time.Time)(error",
		},
		"PRPromotionSubmission": {"SubmitPRPromotion": "context.Context,domain.ContentHash,domain.PullRequestMerge)(domain.PRPromotionJob,error"},
		"PRPromotionExecutor":   {"PromoteBoundPR": "context.Context,domain.ContentHash,domain.PullRequestMerge,string)(inbound.UpdateRefOutput,error", "RunPRJob": "context.Context,domain.PRPromotionJob)(inbound.UpdateRefOutput,error"},
		"PRPromotionScope": {
			"WithinPRSubmit": "context.Context,domain.ContentHash,func(context.Context)(domain.PRPromotionJob,error))(domain.PRPromotionJob,error",
			"WithinPRRetry":  "context.Context,domain.ContentHash,func(context.Context)(error))(error", "WithinPRRead": "context.Context,domain.ContentHash,func(context.Context)([]domain.PRPromotionJob,error))([]domain.PRPromotionJob,error",
		},
	} {
		declaration := set.types[owner]
		if declaration == nil {
			continue
		}
		contract, narrow := declaration.Type.(*ast.InterfaceType)
		if !narrow {
			continue
		}
		for _, method := range contract.Methods.List {
			if signature, function := method.Type.(*ast.FuncType); function && len(method.Names) == 1 && prc1Signature(signature.Params)+")("+prc1Signature(signature.Results) != methods[method.Names[0].Name] {
				findings = append(findings, dpc1Finding{"ports", "changed narrow signature " + owner + "." + method.Names[0].Name})
			}
		}
	}
	return findings
}

func prc1InboundMethods(set wave42LegacyGitSourceSet, owner string, active map[string]bool) (map[string]*ast.FuncType, []dpc1Finding) {
	methods := map[string]*ast.FuncType{}
	declaration := set.types[owner]
	if declaration == nil || active[owner] {
		return methods, []dpc1Finding{{"ports", "missing/cyclic inbound contract " + owner}}
	}
	contract, narrow := declaration.Type.(*ast.InterfaceType)
	if !narrow || declaration.Assign.IsValid() {
		return methods, []dpc1Finding{{"ports", "aliased/aggregate inbound contract " + owner}}
	}
	active[owner] = true
	defer delete(active, owner)
	var findings []dpc1Finding
	for _, method := range contract.Methods.List {
		if len(method.Names) == 0 {
			embedded := wave42LegacyGitType(method.Type)
			if owner != "PRPromotionCommands" || embedded != "PRPromotionDeliverer" {
				findings = append(findings, dpc1Finding{"ports", "unrelated embedded authority in " + owner})
			}
			inherited, violations := prc1InboundMethods(set, embedded, active)
			findings = append(findings, violations...)
			for name, signature := range inherited {
				if methods[name] != nil {
					findings = append(findings, dpc1Finding{"ports", "duplicate capability " + owner + "." + name})
				}
				methods[name] = signature
			}
			continue
		}
		signature, callable := method.Type.(*ast.FuncType)
		if !callable || len(method.Names) != 1 || methods[method.Names[0].Name] != nil {
			findings = append(findings, dpc1Finding{"ports", "invalid/duplicate capability in " + owner})
			continue
		}
		methods[method.Names[0].Name] = signature
	}
	return methods, findings
}

func prc1InboundPorts(set wave42LegacyGitSourceSet) []dpc1Finding {
	wanted := map[string]string{
		"DeliverPRPromotion":  "context.Context,domain.ContentHash,domain.PullRequestMerge)(UpdateRefOutput,error",
		"SubmitPRPromotion":   "context.Context,domain.ContentHash,domain.PullRequestMerge)(domain.PRPromotionJob,error",
		"PromoteRepositoryPR": "context.Context,domain.ContentHash,domain.PullRequestMerge)(UpdateRefOutput,error",
		"RetryPRPromotion":    "context.Context,domain.ContentHash,string)(error",
		"ListPRPromotions":    "context.Context,domain.ContentHash)([]domain.PRPromotionJob,error",
	}
	var findings []dpc1Finding
	for _, owner := range []string{"PRPromotionCommands", "PRPromotionDeliverer"} {
		methods, violations := prc1InboundMethods(set, owner, map[string]bool{})
		findings = append(findings, violations...)
		for name, signature := range methods {
			if owner == "PRPromotionDeliverer" && name != "DeliverPRPromotion" || prc1Signature(signature.Params)+")("+prc1Signature(signature.Results) != wanted[name] {
				findings = append(findings, dpc1Finding{"ports", "changed/unrelated inbound signature " + owner + "." + name})
			}
		}
		for name := range wanted {
			if (owner == "PRPromotionCommands" || name == "DeliverPRPromotion") && methods[name] == nil {
				findings = append(findings, dpc1Finding{"ports", "missing inbound capability " + owner + "." + name})
			}
		}
	}
	return findings
}

func prc1Constructor(set wave42LegacyGitSourceSet) []dpc1Finding {
	function := set.methods["NewPRPromotionCommands"]
	if function == nil || function.Body == nil || wave46EmailFieldTypes(function.Type.Params) != "PRPromotionCommandsConfig" || wave46EmailFieldTypes(function.Type.Results) != "*PRPromotionCommands,error" || len(function.Type.Params.List[0].Names) != 1 {
		return []dpc1Finding{{"constructor", "missing exact public constructor"}}
	}
	config, allocation := function.Type.Params.List[0].Names[0].Name, function.Body.End()
	var literal *ast.CompositeLit
	var loops []*ast.RangeStmt
	var completeBlocks []*ast.BlockStmt
	ast.Inspect(function.Body, func(node ast.Node) bool {
		if candidate, composite := node.(*ast.CompositeLit); composite && (wave42LegacyGitType(candidate.Type) == "prCoordinationCommands" || wave42LegacyGitType(candidate.Type) == "PRPromotionCommands") && candidate.Pos() < allocation {
			literal, allocation = candidate, candidate.Pos()
		}
		if loop, ranged := node.(*ast.RangeStmt); ranged {
			loops = append(loops, loop)
		}
		if conditional, condition := node.(*ast.IfStmt); condition && dpc1Compare(conditional.Cond, token.EQL, config+".Profile", "PRCoordinationComplete") {
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
		paths := map[string][]string{}
		for _, dependency := range []string{"Scope", "Jobs", "Submission", "Executor", "Associations"} {
			paths[config+"."+dependency] = []string{config + "." + dependency}
		}
		for _, loop := range loops {
			if values, listed := loop.X.(*ast.CompositeLit); listed && wave48Inside(conditional, []*ast.BlockStmt{loop.Body}) {
				for _, value := range values.Elts {
					paths[workerSelectorPath(loop.Value)] = append(paths[workerSelectorPath(loop.Value)], workerSelectorPath(value))
				}
			}
		}
		validation := dpc1ReturnsError(conditional.Body, "domain.ErrValidation")
		if validation && dpc1Joined(conditional.Cond, token.LAND, func(expression ast.Expr) bool {
			return dpc1Compare(expression, token.NEQ, config+".Profile", "PRCoordinationCompatibility")
		}) && dpc1Joined(conditional.Cond, token.LAND, func(expression ast.Expr) bool {
			return dpc1Compare(expression, token.NEQ, config+".Profile", "PRCoordinationComplete")
		}) {
			checks["profile"] = conditional.Pos()
		}
		for path, values := range paths {
			absent := dpc1Joined(conditional.Cond, token.LOR, func(expression ast.Expr) bool { return rac1AssetAbsent(expression, path) })
			typed := dpc1Joined(conditional.Cond, token.LAND, func(expression ast.Expr) bool { return rac1AssetAbsent(expression, path) }) && dpc1Joined(conditional.Cond, token.LAND, func(expression ast.Expr) bool { return dpc1Compare(expression, token.NEQ, path, "nil") })
			qualified := wave48Inside(conditional, completeBlocks) || dpc1Joined(conditional.Cond, token.LAND, func(expression ast.Expr) bool {
				return dpc1Compare(expression, token.EQL, config+".Profile", "PRCoordinationComplete")
			})
			missing := dpc1Joined(conditional.Cond, token.LAND, func(expression ast.Expr) bool {
				return dpc1Joined(expression, token.LOR, func(candidate ast.Expr) bool { return dpc1Compare(candidate, token.EQL, path, "nil") })
			})
			for _, value := range values {
				if validation && (typed || absent && value == config+".Scope" && !qualified) {
					checks["nil "+value] = conditional.Pos()
				}
				if validation && !qualified && value == config+".Scope" && (absent || dpc1Joined(conditional.Cond, token.LOR, func(expression ast.Expr) bool { return dpc1Compare(expression, token.EQL, path, "nil") })) {
					checks["required scope"] = conditional.Pos()
				}
				if qualified && missing && dpc1ReturnsError(conditional.Body, "domain.ErrCatalogUnsupported") {
					checks["complete "+value] = conditional.Pos()
					if conditional.Pos() < complete {
						complete = conditional.Pos()
					}
				}
			}
		}
		return true
	})
	var findings []dpc1Finding
	for _, requirement := range []string{"profile", "required scope", "nil " + config + ".Scope", "nil " + config + ".Jobs", "nil " + config + ".Submission", "nil " + config + ".Executor", "nil " + config + ".Associations", "complete " + config + ".Jobs", "complete " + config + ".Submission", "complete " + config + ".Executor"} {
		if checks[requirement] == 0 || strings.HasPrefix(requirement, "nil ") && checks[requirement] >= complete {
			findings = append(findings, dpc1Finding{"constructor", "missing/late rejection: " + requirement})
		}
	}
	for field, source := range map[string]string{"scope": "Scope", "jobs": "Jobs", "submission": "Submission", "executor": "Executor", "associations": "Associations", "profile": "Profile"} {
		if workerSelectorPath(rwa1AuthorityField(literal, field)) != config+"."+source {
			findings = append(findings, dpc1Finding{"constructor", "discarded configured " + source})
		}
	}
	copied := false
	dpc1Calls(set, function, hrc1Resolve(function, rwa1AuthorityField(literal, "supported"), allocation), func(call *ast.CallExpr, target string) {
		copied = copied || target == "append" && call.Ellipsis.IsValid() && len(call.Args) > 1 && workerSelectorPath(call.Args[len(call.Args)-1]) == config+".SupportedIdentities"
	})
	for _, reachable := range dpc1Reachable(set, "NewPRPromotionCommands") {
		for target := range dpc1Calls(set, reachable, reachable.Body, nil) {
			for _, prefix := range []string{"PRPromotionScope.", "PRPromotionJobControl.", "PRPromotionSubmission.", "PRPromotionExecutor.", "outbound.", "Service.", "repositoryWrite", "repositoryRead", "os.", "http.", "net.", "sql.", "pgx.", "exec."} {
				if strings.HasPrefix(target, prefix) {
					findings = append(findings, dpc1Finding{"constructor", "constructor performs IO/work: " + target})
				}
			}
			if target == "NewService" || target == "newRepositoryWork" || target == "AcquireResources" {
				findings = append(findings, dpc1Finding{"constructor", "constructor constructs hidden work: " + target})
			}
		}
		ast.Inspect(reachable.Body, func(node ast.Node) bool {
			if _, background := node.(*ast.GoStmt); background {
				findings = append(findings, dpc1Finding{"constructor", "constructor starts work"})
			}
			return true
		})
	}
	presence := wave42LegacyGitCalls(bodyOfDPC1(set.methods["rootAdapterPresent"]))
	if literal == nil || !copied || !presence["IsNil"] || !presence["ValueOf"] {
		findings = append(findings, dpc1Finding{"constructor", "missing allocation, bound defensive copy or genuine typed-nil check"})
	}
	return findings
}

func prc1Order(set wave42LegacyGitSourceSet) []dpc1Finding {
	function := set.methods["prCoordinationCommands.DeliverPRPromotion"]
	if function == nil || function.Body == nil {
		return []dpc1Finding{{"durable-order", "missing actual Deliver coordination"}}
	}
	sequence, submit, bulk, claim := 0, 0, 0, 0
	targets := map[*ast.CallExpr]string{}
	dpc1Calls(set, function, bodyOfDPC1(function), func(call *ast.CallExpr, target string) {
		sequence++
		targets[call] = target
		switch target {
		case "prCoordinationCommands.SubmitPRPromotion":
			submit = sequence
		case "AcquireResources":
			bulk = sequence
		case "PRPromotionJobControl.ClaimPRJob":
			claim = sequence
		}
	})
	accepted, claimed := "", ""
	ast.Inspect(bodyOfDPC1(function), func(node ast.Node) bool {
		if assignment, assigned := node.(*ast.AssignStmt); assigned && len(assignment.Rhs) == 1 && len(assignment.Lhs) > 0 {
			if call, called := assignment.Rhs[0].(*ast.CallExpr); called {
				if targets[call] == "prCoordinationCommands.SubmitPRPromotion" {
					accepted = workerSelectorPath(assignment.Lhs[0])
				}
				if targets[call] == "PRPromotionJobControl.ClaimPRJob" {
					claimed = workerSelectorPath(assignment.Lhs[0])
				}
			}
		}
		return true
	})
	sameJob, replay, executed, borrowed, completed, queued := false, false, false, false, false, false
	for call, target := range targets {
		if target == "PRPromotionJobControl.ClaimPRJob" {
			sameJob = len(call.Args) == 5 && workerSelectorPath(call.Args[2]) == accepted+".ID"
		}
		if target == "PRPromotionExecutor.PromoteBoundPR" {
			replay = len(call.Args) == 4 && workerSelectorPath(call.Args[3]) == accepted+".BaseBranchID"
		}
		if target == "PRPromotionExecutor.RunPRJob" {
			executed = len(call.Args) == 2 && workerSelectorPath(call.Args[1]) == claimed && claimed != ""
		}
		if target == "AcquireResources" && (len(call.Args) != 2 || workerSelectorPath(call.Args[1]) != "ResourceBulk") {
			borrowed = true
		}
	}
	ast.Inspect(bodyOfDPC1(function), func(node ast.Node) bool {
		if callback, literal := node.(*ast.FuncLit); literal && wave42LegacyGitCalls(callback.Body)["AcquireResources"] {
			borrowed = true
		}
		if conditional, condition := node.(*ast.IfStmt); condition {
			completed = completed || prc1String(conditional.Cond, "completed") && wave42LegacyGitMentions(conditional.Cond, "State") && dpc1Calls(set, function, conditional.Body, nil)["PRPromotionExecutor.PromoteBoundPR"]
			queued = queued || wave42LegacyGitMentions(conditional.Cond, "ErrNotFound") && wave42LegacyGitMentions(conditional.Body, "ErrConflict")
		}
		return true
	})
	for _, entry := range []string{"prCoordinationCommands.SubmitPRPromotion", "prCoordinationScope.WithinPRSubmit", "prCoordinationScope.WithinPRRetry"} {
		borrowed = borrowed || dpc1ExecutionCalls(set, entry)["AcquireResources"]
	}
	if submit == 0 || bulk <= submit || claim <= bulk || accepted == "" || !sameJob || !replay || !executed || !completed || !queued || borrowed {
		return []dpc1Finding{{"durable-order", "Deliver must durably submit before unborrowed Bulk, replay accepted base and claim/execute that exact job"}}
	}
	return nil
}

func prc1Owner(set wave42LegacyGitSourceSet) []dpc1Finding {
	findings := append(prc1Fields(set), prc1Ports(set)...)
	findings = append(findings, prc1Constructor(set)...)
	for entry, operations := range map[string][]string{
		"SubmitPRPromotion":   {"PRPromotionScope.WithinPRSubmit", "PRPromotionSubmission.SubmitPRPromotion"},
		"DeliverPRPromotion":  {"prCoordinationCommands.SubmitPRPromotion", "AcquireResources", "outbound.WithDocumentIdentityCompatibility", "inbound.DocumentIdentities", "PRPromotionJobControl.ClaimPRJob", "PRPromotionExecutor.PromoteBoundPR", "PRPromotionExecutor.RunPRJob", "errors.Is", "fmt.Errorf"},
		"PromoteRepositoryPR": {"PRPromotionExecutor.PromoteBoundPR"}, "RetryPRPromotion": {"PRPromotionScope.WithinPRRetry", "PRPromotionJobControl.RetryPRJob"},
		"ListPRPromotions":         {"PRPromotionScope.WithinPRRead", "PRPromotionJobControl.ListPRJobs", "prCoordinationCommands.projectPRJobAssociations"},
		"projectPRJobAssociations": {"outbound.PRJobAssociationStore.ReadPRJobAssociationEvidence", "normalizeGitURL", "append"},
	} {
		findings = append(findings, hrc1Require(set, "prCoordinationCommands."+entry, "payload", operations...)...)
	}
	for entry, function := range set.methods {
		if !strings.HasPrefix(entry, "prCoordinationCommands.") && !strings.HasPrefix(entry, "prCoordinationScope.") {
			continue
		}
		if strings.HasPrefix(entry, "prCoordinationCommands.") && ast.IsExported(function.Name.Name) {
			switch function.Name.Name {
			case "SubmitPRPromotion", "DeliverPRPromotion", "PromoteRepositoryPR", "RetryPRPromotion", "ListPRPromotions":
			default:
				findings = append(findings, dpc1Finding{"ports", "unrelated public coordination capability: " + entry})
			}
		}
		for target := range dpc1ExecutionCalls(set, entry) {
			if strings.HasPrefix(target, "Service.") || target == "NewService" {
				findings = append(findings, dpc1Finding{"reverse", entry + " calls back into " + target})
			}
		}
		ast.Inspect(function, func(node ast.Node) bool {
			switch value := node.(type) {
			case *ast.TypeAssertExpr, *ast.TypeSwitchStmt:
				findings = append(findings, dpc1Finding{"reverse", "runtime capability discovery in " + entry})
			case *ast.Ident:
				if value.Name == "Service" || value.Name == "NewService" {
					findings = append(findings, dpc1Finding{"reverse", "parent dependency in " + entry})
				}
			}
			return true
		})
	}
	projection := bodyOfDPC1(set.methods["prCoordinationCommands.projectPRJobAssociations"])
	if !prc1String(projection, "unavailable") || !prc1String(projection, "not_evaluated") {
		findings = append(findings, dpc1Finding{"payload", "association projection fabricates availability or applicability"})
	}
	for _, name := range []string{"CheckpointsComplete", "ParticipationsComplete", "ContributionsComplete", "BaseBranchID", "DestinationBranchID", "CurrentApplicability", "ProviderCheckpointCount", "PublicationCount"} {
		if projection == nil || !wave42LegacyGitMentions(projection, name) {
			findings = append(findings, dpc1Finding{"payload", "actual association projection lost " + name})
		}
	}
	for entry, target := range map[string]string{"prPublicationCommands.SubmitPRPromotion": "prPublicationCommands.submitPRPromotion", "PRPromotionWorker.PromoteBoundPR": "PRPromotionWorker.promoteBoundPR", "PRPromotionWorker.RunPRJob": "PRPromotionWorker.runPRJob"} {
		findings = append(findings, hrc1Require(set, entry, "actual-policy", target)...)
	}
	for _, name := range []string{"SubmitPRPromotion", "DeliverPRPromotion", "PromoteRepositoryPR", "RetryPRPromotion", "ListPRPromotions", "projectPRJobAssociations"} {
		function := set.methods["Service."+name]
		for target := range dpc1Calls(set, function, bodyOfDPC1(function), nil) {
			if strings.HasSuffix(target, ".ClaimPRJob") || strings.HasSuffix(target, ".RetryPRJob") || strings.HasSuffix(target, ".ListPRJobs") || strings.HasSuffix(target, ".ReadPRJobAssociationEvidence") || target == "AcquireResources" || strings.HasPrefix(target, "repositoryWrite") || target == "repositoryReadForRepo" {
				findings = append(findings, dpc1Finding{"facade", "coordination policy retained in Service." + name})
			}
		}
	}
	return append(findings, prc1Order(set)...)
}

func prc1WorkBindings(block *ast.BlockStmt, name, work string, before token.Pos, values []ast.Expr) []ast.Expr {
	if block == nil {
		return values
	}
	for _, statement := range block.List {
		if statement.Pos() >= before || len(values) == 0 {
			break
		}
		switch current := statement.(type) {
		case *ast.AssignStmt:
			if current.End() >= before {
				continue
			}
			for index, left := range current.Lhs {
				if workerSelectorPath(left) == name {
					values = []ast.Expr{nil}
					if index < len(current.Rhs) {
						values[0] = current.Rhs[index]
					}
				}
			}
		case *ast.DeclStmt:
			ast.Inspect(current, func(node ast.Node) bool {
				if declaration, declared := node.(*ast.ValueSpec); declared {
					for index, identifier := range declaration.Names {
						if identifier.Name == name {
							values = []ast.Expr{nil}
							if index < len(declaration.Values) {
								values[0] = declaration.Values[index]
							}
						}
					}
				}
				return true
			})
		case *ast.IfStmt:
			if current.Body.Pos() >= before {
				continue
			}
			alternative := &ast.BlockStmt{}
			if current.Else != nil {
				alternative.List = []ast.Stmt{current.Else}
			}
			if dpc1Compare(current.Cond, token.EQL, work, "nil") {
				values = prc1WorkBindings(alternative, name, work, before, values)
			} else if current.Body.End() >= before {
				values = prc1WorkBindings(current.Body, name, work, before, values)
			} else if current.Else != nil && current.Else.Pos() < before && current.Else.End() >= before {
				values = prc1WorkBindings(alternative, name, work, before, values)
			} else {
				primary := prc1WorkBindings(current.Body, name, work, before, append([]ast.Expr(nil), values...))
				values = append(primary, prc1WorkBindings(alternative, name, work, before, values)...)
			}
		case *ast.BlockStmt:
			values = prc1WorkBindings(current, name, work, before, values)
		case *ast.ReturnStmt:
			if current.End() < before {
				return nil
			}
		}
	}
	return values
}

func prc1UsesWork(set wave42LegacyGitSourceSet, function *ast.FuncDecl, expression ast.Expr, owner, work string, before token.Pos, depth int) bool {
	if function == nil || work == "" || depth > 10 {
		return false
	}
	if identifier, named := expression.(*ast.Ident); named {
		values := prc1WorkBindings(function.Body, identifier.Name, work, before, []ast.Expr{nil})
		if len(values) == 0 {
			return false
		}
		for _, value := range values {
			if value == nil {
				if owner != "" || identifier.Name != work {
					return false
				}
			} else if !prc1UsesWork(set, function, value, owner, work, value.Pos(), depth+1) {
				return false
			}
		}
		return true
	}
	if literal := rwa1AuthorityComposite(expression, owner); literal != nil {
		return prc1UsesWork(set, function, rwa1AuthorityField(literal, "work"), "", work, before, depth+1)
	}
	call, called := expression.(*ast.CallExpr)
	if !called {
		return false
	}
	var factory *ast.FuncDecl
	dpc1Calls(set, function, function.Body, func(candidate *ast.CallExpr, target string) {
		if candidate == call {
			factory = set.methods[target]
		}
	})
	index, parameter := hrc1Work(factory)
	if owner == "" || index < 0 || index >= len(call.Args) || !prc1UsesWork(set, function, call.Args[index], "", work, call.Pos(), depth+1) {
		return false
	}
	bound, returnedOwner := true, false
	ast.Inspect(bodyOfDPC1(factory), func(node ast.Node) bool {
		if _, callback := node.(*ast.FuncLit); callback {
			return false
		}
		if returned, returns := node.(*ast.ReturnStmt); returns {
			if len(returned.Results) == 2 && workerSelectorPath(returned.Results[0]) == "nil" && workerSelectorPath(returned.Results[1]) != "nil" {
				return true
			}
			if len(returned.Results) > 0 {
				returnedOwner = true
				bound = prc1UsesWork(set, factory, returned.Results[0], owner, parameter, returned.Pos(), depth+1) && bound
			}
		}
		return true
	})
	return bound && returnedOwner
}

func prc1SharedWork(set wave42LegacyGitSourceSet) []dpc1Finding {
	var findings []dpc1Finding
	rooted := false
	rootCalls := dpc1ExecutionCalls(set, "Service.PRPromotionCommands")
	for entry, function := range set.methods {
		_, work := hrc1Work(function)
		if work == "" {
			continue
		}
		dpc1Calls(set, function, function.Body, func(call *ast.CallExpr, target string) {
			if target != "NewPRPromotionCommands" || len(call.Args) != 1 {
				return
			}
			config := rwa1AuthorityComposite(hrc1Resolve(function, call.Args[0], call.Pos()), "PRPromotionCommandsConfig")
			nilRejected := false
			for _, statement := range function.Body.List {
				if conditional, condition := statement.(*ast.IfStmt); condition && conditional.End() < call.Pos() && dpc1Compare(conditional.Cond, token.EQL, work, "nil") && dpc1ReturnsError(conditional.Body, "domain.ErrCatalogUnsupported") {
					nilRejected = true
				}
			}
			bound := config != nil && nilRejected
			for field, owner := range map[string]string{"Scope": "prCoordinationScope", "Submission": "prPublicationCommands", "Executor": "PRPromotionWorker"} {
				bound = prc1UsesWork(set, function, rwa1AuthorityField(config, field), owner, work, call.Pos(), 0) && bound
			}
			if !bound {
				findings = append(findings, dpc1Finding{"shared-work", entry + " must bind original passed work to actual scope, publisher and worker"})
			}
			rooted = rooted || bound && rootCalls[entry]
		})
	}
	root := set.methods["Service.PRPromotionCommands"]
	if !rooted || root == nil || wave46EmailFieldTypes(root.Type.Results) != "inbound.PRPromotionCommands,error" {
		findings = append(findings, dpc1Finding{"shared-work", "Service factory is disconnected from passed-work composition"})
	}
	for method, operation := range map[string]string{"WithinPRSubmit": "repositoryWriteWithWork", "WithinPRRetry": "repositoryWriteErrorWithWork", "WithinPRRead": "repositoryReadForRepoWithWork"} {
		function, bound := set.methods["prCoordinationScope."+method], false
		dpc1Calls(set, function, bodyOfDPC1(function), func(call *ast.CallExpr, target string) {
			bound = bound || target == operation && len(call.Args) > 1 && rfc1Origin(function, call.Args[1], call.Pos()) == "prCoordinationScope.work"
		})
		if !bound {
			findings = append(findings, dpc1Finding{"shared-work", method + " replaced original work/snapshot helper"})
		}
		if method != "WithinPRRead" {
			retained := false
			for _, entry := range []string{"prCoordinationScope." + method, "prCoordinationCommands." + map[string]string{"WithinPRSubmit": "SubmitPRPromotion", "WithinPRRetry": "RetryPRPromotion"}[method]} {
				for _, reachable := range dpc1Reachable(set, entry) {
					retained = retained || dpc1Calls(set, reachable, reachable.Body, nil)["context.WithValue"] && wave42LegacyGitMentions(reachable.Body, "revisionScopeKey") && prc1String(reachable.Body, "none")
				}
			}
			if !retained {
				findings = append(findings, dpc1Finding{"scope", method + " lost no-graph revision context"})
			}
		}
	}
	audited := false
	for _, entry := range []string{"prCoordinationCommands.SubmitPRPromotion", "prCoordinationScope.WithinPRSubmit"} {
		for _, function := range dpc1Reachable(set, entry) {
			audited = audited || dpc1Calls(set, function, function.Body, nil)["auditOperation"] && prc1String(function.Body, "context.promotion.submitted")
		}
	}
	if !audited {
		findings = append(findings, dpc1Finding{"scope", "submission lost original audit"})
	}
	return findings
}

func prc1String(node ast.Node, expected string) bool {
	if node == nil {
		return false
	}
	found := false
	ast.Inspect(node, func(nested ast.Node) bool {
		if literal, quoted := nested.(*ast.BasicLit); quoted && literal.Kind == token.STRING {
			value, err := strconv.Unquote(literal.Value)
			found = found || err == nil && value == expected
		}
		return true
	})
	return found
}

func prc1HTTP(set wave42LegacyGitSourceSet) []dpc1Finding {
	var findings []dpc1Finding
	setter, injections := set.methods["Server.SetPRPromotionCommands"], map[string]string{}
	if setter == nil || setter.Body == nil || wave46EmailFieldTypes(setter.Type.Params) != "inbound.PRPromotionCommands" || len(setter.Type.Params.List[0].Names) != 1 {
		return []dpc1Finding{{"http", "missing actual PR owner setter"}}
	}
	parameter := setter.Type.Params.List[0].Names[0].Name
	ast.Inspect(bodyOfDPC1(setter), func(node ast.Node) bool {
		if assignment, assigned := node.(*ast.AssignStmt); assigned && len(assignment.Lhs) == len(assignment.Rhs) {
			for index, left := range assignment.Lhs {
				injection := rfc1Origin(setter, left, assignment.Pos())
				if !strings.HasPrefix(injection, "Server.") {
					continue
				}
				kind := wave44PreparationMember(set, "Server", strings.TrimPrefix(injection, "Server."), false, map[string]bool{})
				if workerSelectorPath(hrc1Resolve(setter, assignment.Rhs[index], assignment.Pos())) == parameter && (kind == "inbound.PRPromotionCommands" || kind == "inbound.PRPromotionDeliverer") {
					injections[injection] = kind
				} else {
					delete(injections, injection)
				}
			}
		}
		return true
	})
	if len(injections) == 0 || !wave42LegacyGitCalls(bodyOfDPC1(setter))["IsNil"] {
		findings = append(findings, dpc1Finding{"http", "setter must bind actual narrow injection and reject typed nils"})
	}
	for route, expected := range map[string][]string{
		"POST /api/v1/repos/{repoID}/prs/promote": {"promoteRepositoryPR", "DeliverPRPromotion"}, "POST /api/v1/repos/{repoID}/prs/promotions": {"submitPRPromotion", "SubmitPRPromotion"},
		"GET /api/v1/repos/{repoID}/prs/promotions": {"listPRPromotions", "ListPRPromotions"}, "POST /api/v1/repos/{repoID}/prs/promotions/{jobID}/retry": {"retryPRPromotion", "RetryPRPromotion"},
	} {
		registered, used := false, false
		for _, function := range dpc1Reachable(set, "Server.Handler") {
			dpc1Calls(set, function, function.Body, func(call *ast.CallExpr, target string) {
				if !strings.HasSuffix(target, ".HandleFunc") || len(call.Args) != 2 {
					return
				}
				literal, quoted := call.Args[0].(*ast.BasicLit)
				if !quoted || literal.Kind != token.STRING {
					return
				}
				pattern, err := strconv.Unquote(literal.Value)
				if err == nil && pattern == route {
					ast.Inspect(call.Args[1], func(node ast.Node) bool {
						if selector, selected := node.(*ast.SelectorExpr); selected {
							registered = registered || rfc1Origin(function, selector, call.Pos()) == "Server."+expected[0]
						}
						return true
					})
				}
			})
		}
		for _, function := range dpc1Reachable(set, "Server."+expected[0]) {
			dpc1Calls(set, function, function.Body, func(call *ast.CallExpr, target string) {
				if strings.HasSuffix(target, "."+expected[1]) {
					origin := strings.TrimSuffix(rfc1Origin(function, call.Fun, call.Pos()), "."+expected[1])
					kind := injections[origin]
					correct := kind != "" && target == kind+"."+expected[1] && (kind == "inbound.PRPromotionCommands" || expected[1] == "DeliverPRPromotion")
					used = used || correct
					if !correct {
						findings = append(findings, dpc1Finding{"http", route + " bypasses the setter-bound owner"})
					}
				}
			})
			ast.Inspect(function.Body, func(node ast.Node) bool {
				if assertion, asserted := node.(*ast.TypeAssertExpr); asserted && rfc1Origin(function, assertion.X, assertion.Pos()) == "Server.b" {
					findings = append(findings, dpc1Finding{"http", "per-request backend discovery"})
				}
				return true
			})
		}
		if !registered || !used {
			findings = append(findings, dpc1Finding{"http", "disconnected actual PR route: " + route})
		}
	}
	function := set.methods["Server.promoteRepositoryPR"]
	if function == nil || function.Body == nil {
		return append(findings, dpc1Finding{"http", "missing live promotion handler"})
	}
	admission, decode, release := token.NoPos, token.NoPos, token.NoPos
	dpc1Calls(set, function, bodyOfDPC1(function), func(call *ast.CallExpr, target string) {
		if target == "Server.admitResourceRequest" && len(call.Args) == 3 && workerSelectorPath(call.Args[2]) == "app.ResourceInteractive" {
			admission = call.Pos()
		}
		if target == "Server.decodeLimited" {
			decode = call.Pos()
		}
	})
	ast.Inspect(bodyOfDPC1(function), func(node ast.Node) bool {
		if deferred, defers := node.(*ast.DeferStmt); defers && workerSelectorPath(deferred.Call.Fun) == "release" {
			release = deferred.Pos()
		}
		return true
	})
	if admission == 0 || release <= admission || decode <= release {
		findings = append(findings, dpc1Finding{"http", "Interactive admission must cover decode and deferred response completion"})
	}
	for entry, method := range set.methods {
		if strings.HasPrefix(entry, "Server.") {
			continue
		}
		for _, command := range []string{"SubmitPRPromotion", "DeliverPRPromotion", "PromoteRepositoryPR", "RetryPRPromotion", "ListPRPromotions"} {
			if strings.HasSuffix(entry, "."+command) {
				ast.Inspect(method.Body, func(node ast.Node) bool {
					if _, assertion := node.(*ast.TypeAssertExpr); assertion {
						findings = append(findings, dpc1Finding{"http", "compatibility capability selection must happen in NewServer, not " + entry})
					}
					return true
				})
			}
		}
	}
	return findings
}

func prc1Composition(set wave42LegacyGitSourceSet, check func(wave42LegacyGitSourceSet) []dpc1Finding) []dpc1Finding {
	originals := map[*ast.Ident]string{}
	names := map[string]string{"ConfigurePRPromotionCommands": "ConfigureHistoryCommands", "SetPRPromotionCommands": "SetHistoryCommands", "NewPRPromotionCommands": "NewHistoryCommands", "PRPromotionCommands": "HistoryCommands", "PRPromotions": "HistoryCommands", "prPromotions": "historyCommands"}
	for _, file := range set.files {
		ast.Inspect(file, func(node ast.Node) bool {
			if identifier, named := node.(*ast.Ident); named {
				originals[identifier] = identifier.Name
				if replacement := names[identifier.Name]; replacement != "" {
					identifier.Name = replacement
				} else if strings.Contains(identifier.Name, "HistoryCommands") || identifier.Name == "historyCommands" {
					identifier.Name = "prc1Excluded" + identifier.Name
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

func TestPRC1ActualOwnerConstructorAndOriginalWork(test *testing.T) {
	set := wave48Package(test, filepath.Join("..", "app"))
	dpc1Report(test, prc1Owner(set))
	dpc1Report(test, prc1SharedWork(set))
	inbound := wave48Package(test, filepath.Join("..", "ports", "inbound"))
	dpc1Report(test, prc1InboundPorts(inbound))
	methods, _ := prc1InboundMethods(inbound, "PRPromotionCommands", map[string]bool{})
	for name, signature := range methods {
		actual := set.methods["prCoordinationCommands."+name]
		if actual == nil || strings.ReplaceAll(prc1Signature(actual.Type.Params)+")("+prc1Signature(actual.Type.Results), "inbound.", "") != prc1Signature(signature.Params)+")("+prc1Signature(signature.Results) {
			test.Error("PRC-1 actual public method changed inbound signature", name)
		}
	}
}

func TestPRC1RealHTTPStartupInjectionAndMCPExclusion(test *testing.T) {
	dpc1Report(test, prc1HTTP(wave48Package(test, filepath.Join("..", "adapters", "delivery", "http"))))
	for directory, check := range map[string]func(wave42LegacyGitSourceSet) []dpc1Finding{"serverruntime": rfc1Runtime, "mcpserver": rfc1MCP, "../cmd/cxtd": rfc1APIBinding} {
		dpc1Report(test, prc1Composition(wave48Package(test, filepath.Join("..", directory)), check))
	}
	for directory, entry := range map[string]string{"app": "NewReadServices", "serverruntime": "Runtime.configureMCPReads", "../cmd/cxt-mcp": ""} {
		dpc1Report(test, prc1Composition(wave48Package(test, filepath.Join("..", directory)), func(set wave42LegacyGitSourceSet) []dpc1Finding {
			var findings []dpc1Finding
			if entry == "" {
				for path, file := range set.files {
					if rfc1Initializes(file) {
						findings = append(findings, dpc1Finding{"mcp", "MCP process constructs PR commands: " + path})
					}
				}
				return findings
			}
			if set.methods[entry] == nil {
				return []dpc1Finding{{"mcp", "missing read-only root " + entry}}
			}
			for target, function := range dpc1Reachable(set, entry) {
				if rfc1Initializes(function.Body) || wave42LegacyGitCalls(function.Body)["NewService"] {
					findings = append(findings, dpc1Finding{"mcp", "read-only root constructs PR commands: " + target})
				}
			}
			return findings
		}))
	}
}

func TestPRC1MutatedActualSourcesRejectLostOrderReverseDisconnectedAndWrongWork(test *testing.T) {
	for _, scenario := range []struct {
		name, directory, rule string
		check                 func(wave42LegacyGitSourceSet) []dpc1Finding
		mutate                func(wave42LegacyGitSourceSet)
	}{
		{"broad embedded inbound authority", "ports/inbound", "ports", prc1InboundPorts, func(set wave42LegacyGitSourceSet) {
			set.types["PRPromotionCommands"].Type.(*ast.InterfaceType).Methods.List[0].Type = ast.NewIdent("Backend")
		}},
		{"extra embedded deliverer method", "ports/inbound", "ports", prc1InboundPorts, func(set wave42LegacyGitSourceSet) {
			contract := set.types["PRPromotionDeliverer"].Type.(*ast.InterfaceType)
			contract.Methods.List = append(contract.Methods.List, &ast.Field{Names: []*ast.Ident{ast.NewIdent("DeleteRepository")}, Type: &ast.FuncType{Params: &ast.FieldList{}}})
		}},
		{"cyclic embedded inbound authority", "ports/inbound", "ports", prc1InboundPorts, func(set wave42LegacyGitSourceSet) {
			contract := set.types["PRPromotionDeliverer"].Type.(*ast.InterfaceType)
			contract.Methods.List = append(contract.Methods.List, &ast.Field{Type: ast.NewIdent("PRPromotionCommands")})
		}},
		{"changed embedded deliverer signature", "ports/inbound", "ports", prc1InboundPorts, func(set wave42LegacyGitSourceSet) {
			signature := set.types["PRPromotionDeliverer"].Type.(*ast.InterfaceType).Methods.List[0].Type.(*ast.FuncType)
			signature.Params.List[2].Type = ast.NewIdent("string")
		}},
		{"Service backcall", "app", "reverse", prc1Owner, func(set wave42LegacyGitSourceSet) {
			set.methods["prCoordinationCommands.SubmitPRPromotion"].Body = dpc1Body(test, "var service *Service; return service.SubmitPRPromotion(ctx, repo, request)")
		}},
		{"public Service alias", "app", "alias", prc1Owner, func(set wave42LegacyGitSourceSet) { set.types["PRPromotionCommands"].Type = ast.NewIdent("Service") }},
		{"constructor shell", "app", "constructor", prc1Owner, func(set wave42LegacyGitSourceSet) {
			set.methods["NewPRPromotionCommands"].Body = dpc1Body(test, "return &prCoordinationCommands{}, nil")
		}},
		{"constructor performs port IO", "app", "constructor", prc1Constructor, func(set wave42LegacyGitSourceSet) {
			function := set.methods["NewPRPromotionCommands"]
			position := function.Body.Lbrace + 1
			config := &ast.Ident{NamePos: position, Name: function.Type.Params.List[0].Names[0].Name}
			jobs := &ast.SelectorExpr{X: config, Sel: ast.NewIdent("Jobs")}
			call := &ast.CallExpr{Fun: &ast.SelectorExpr{X: jobs, Sel: ast.NewIdent("ListPRJobs")}, Rparen: position + 1}
			function.Body.List = append(function.Body.List, &ast.ExprStmt{X: call})
		}},
		{"borrowed acceptance Bulk", "app", "durable-order", prc1Order, func(set wave42LegacyGitSourceSet) {
			set.methods["prCoordinationScope.WithinPRSubmit"].Body = dpc1Body(test, "AcquireResources(ctx, ResourceBulk)")
		}},
		{"lost durable order", "app", "durable-order", prc1Order, func(set wave42LegacyGitSourceSet) {
			function := set.methods["prCoordinationCommands.DeliverPRPromotion"]
			submitIndex, bulkIndex := -1, -1
			for index, statement := range function.Body.List {
				calls := dpc1Calls(set, function, statement, nil)
				if calls["prCoordinationCommands.SubmitPRPromotion"] {
					submitIndex = index
				}
				if calls["AcquireResources"] {
					bulkIndex = index
				}
			}
			if submitIndex < 0 || bulkIndex < 0 {
				test.Fatal("missing actual top-level durable acceptance/Bulk statements")
			}
			function.Body.List[submitIndex], function.Body.List[bulkIndex] = function.Body.List[bulkIndex], function.Body.List[submitIndex]
		}},
		{"wrong original worker work", "app", "shared-work", prc1SharedWork, func(set wave42LegacyGitSourceSet) {
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
		{"wrong Complete branch work", "app", "shared-work", prc1SharedWork, func(set wave42LegacyGitSourceSet) {
			changed := false
			dpc1Calls(set, set.methods["Service.prCoordinationFromService"], set.methods["Service.prCoordinationFromService"].Body, func(call *ast.CallExpr, target string) {
				if target == "newPRPromotionWorkerWithWork" {
					call.Args[1], changed = ast.NewIdent("unrelatedWork"), true
				}
			})
			if !changed {
				test.Fatal("missing actual Complete executor branch")
			}
		}},
		{"wrong Compatibility branch work", "app", "shared-work", prc1SharedWork, func(set wave42LegacyGitSourceSet) {
			changed := false
			dpc1Calls(set, set.methods["Service.prCoordinationFromService"], set.methods["Service.prCoordinationFromService"].Body, func(call *ast.CallExpr, target string) {
				if target == "assemblePRPromotionWorker" {
					call.Args[1], changed = ast.NewIdent("unrelatedWork"), true
				}
			})
			if !changed {
				test.Fatal("missing actual Compatibility executor branch")
			}
		}},
		{"worker helper discards passed work", "app", "shared-work", prc1SharedWork, func(set wave42LegacyGitSourceSet) {
			changed := false
			function := set.methods["newPRPromotionWorkerWithWork"]
			var replacement ast.Expr
			dpc1Calls(set, function, function.Body, func(call *ast.CallExpr, target string) {
				if target == "newRepositoryWork" {
					replacement = call
				}
			})
			if replacement == nil {
				test.Fatal("missing actual nil-work fallback allocation")
			}
			dpc1Calls(set, function, function.Body, func(call *ast.CallExpr, target string) {
				if target == "assemblePRPromotionWorker" {
					call.Args[1], changed = replacement, true
				}
			})
			if !changed {
				test.Fatal("missing actual helper assembly")
			}
		}},
		{"coordination accepts nil work", "app", "shared-work", prc1SharedWork, func(set wave42LegacyGitSourceSet) {
			function := set.methods["Service.prCoordinationFromService"]
			function.Body.List[0].(*ast.IfStmt).Body = dpc1Body(test, "return nil, nil")
		}},
		{"same-type disconnected HTTP consumer", "adapters/delivery/http", "http", prc1HTTP, func(set wave42LegacyGitSourceSet) {
			shape := set.types["Server"].Type.(*ast.StructType)
			shape.Fields.List = append(shape.Fields.List, &ast.Field{Names: []*ast.Ident{ast.NewIdent("unrelatedPR")}, Type: &ast.SelectorExpr{X: ast.NewIdent("inbound"), Sel: ast.NewIdent("PRPromotionCommands")}})
			shape.Fields.List = append(shape.Fields.List, &ast.Field{Names: []*ast.Ident{ast.NewIdent("unrelatedDeliverer")}, Type: &ast.SelectorExpr{X: ast.NewIdent("inbound"), Sel: ast.NewIdent("PRPromotionDeliverer")}})
			function := set.methods["Server.promoteRepositoryPR"]
			changed := false
			ast.Inspect(function.Body, func(node ast.Node) bool {
				if selector, selected := node.(*ast.SelectorExpr); selected {
					switch wave44PreparationMember(set, "Server", selector.Sel.Name, false, map[string]bool{}) {
					case "inbound.PRPromotionCommands":
						selector.Sel.Name, changed = "unrelatedPR", true
					case "inbound.PRPromotionDeliverer":
						selector.Sel.Name, changed = "unrelatedDeliverer", true
					}
				}
				return true
			})
			if !changed {
				test.Fatal("same-type mutation did not change the actual promotion consumer")
			}
		}},
		{"paired setter discards deliverer injection", "adapters/delivery/http", "http", prc1HTTP, func(set wave42LegacyGitSourceSet) {
			changed := false
			ast.Inspect(set.methods["Server.SetPRPromotionCommands"].Body, func(node ast.Node) bool {
				if assignment, assigned := node.(*ast.AssignStmt); assigned && len(assignment.Lhs) == 2 && len(assignment.Rhs) == 2 {
					assignment.Rhs[0], changed = ast.NewIdent("unrelatedCommands"), true
				}
				return true
			})
			if !changed {
				test.Fatal("missing actual paired setter assignment")
			}
		}},
		{"registered route contains only a decoy literal", "adapters/delivery/http", "http", prc1HTTP, func(set wave42LegacyGitSourceSet) {
			changed := false
			for _, function := range dpc1Reachable(set, "Server.Handler") {
				dpc1Calls(set, function, function.Body, func(call *ast.CallExpr, target string) {
					if strings.HasSuffix(target, ".HandleFunc") && len(call.Args) == 2 && prc1String(call.Args[0], "POST /api/v1/repos/{repoID}/prs/promote") {
						call.Args[0], changed = &ast.BinaryExpr{X: call.Args[0], Op: token.ADD, Y: &ast.BasicLit{Kind: token.STRING, Value: `"/unregistered"`}}, true
					}
				})
			}
			if !changed {
				test.Fatal("missing actual promotion route registration")
			}
		}},
		{"disconnected runtime factory result", "serverruntime", "runtime", func(set wave42LegacyGitSourceSet) []dpc1Finding { return prc1Composition(set, rfc1Runtime) }, func(set wave42LegacyGitSourceSet) {
			ast.Inspect(set.methods["Runtime.ConfigurePRPromotionCommands"].Body, func(node ast.Node) bool {
				if assignment, assigned := node.(*ast.AssignStmt); assigned && len(assignment.Lhs) == 1 && len(assignment.Rhs) == 1 {
					if selector, selected := assignment.Lhs[0].(*ast.SelectorExpr); selected && selector.Sel.Name == "PRPromotions" {
						assignment.Rhs[0] = ast.NewIdent("unrelatedOwner")
					}
				}
				return true
			})
		}},
	} {
		test.Run(scenario.name, func(test *testing.T) {
			set := wave48Package(test, filepath.Join("..", scenario.directory))
			if findings := scenario.check(set); len(findings) != 0 {
				test.Fatalf("unmodified actual source must satisfy guard: %+v", findings)
			}
			scenario.mutate(set)
			dpc1RequireRule(test, scenario.check(set), scenario.rule)
		})
	}
}
