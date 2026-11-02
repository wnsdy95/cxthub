package architecture

import (
	"go/ast"
	"path/filepath"
	"strings"
	"testing"
)

func wave46EmailStructViolations(set wave42LegacyGitSourceSet, owner string, expected map[string]string) []string {
	declaration := set.types[owner]
	if declaration == nil {
		return []string{"missing invitation email resource type: " + owner}
	}
	shape, concrete := declaration.Type.(*ast.StructType)
	if !concrete || declaration.Assign.IsValid() {
		return []string{"invitation email resource is an alias or callback interface: " + owner}
	}
	var violations []string
	observed := map[string]bool{}
	for _, field := range shape.Fields.List {
		if len(field.Names) == 0 {
			violations = append(violations, "embedded invitation email dependency: "+owner)
		}
		for _, name := range field.Names {
			if kind, allowed := expected[name.Name]; !allowed || kind != wave42LegacyGitType(field.Type) {
				violations = append(violations, "broad/private/callback invitation email dependency: "+owner+"."+name.Name)
			}
			observed[name.Name] = true
		}
	}
	for name := range expected {
		if !observed[name] {
			violations = append(violations, "missing invitation email dependency: "+owner+"."+name)
		}
	}
	return violations
}

func wave46EmailFieldTypes(fields *ast.FieldList) string {
	var kinds []string
	if fields != nil {
		for _, field := range fields.List {
			for count := 0; count < max(1, len(field.Names)); count++ {
				kinds = append(kinds, wave42LegacyGitType(field.Type))
			}
		}
	}
	return strings.Join(kinds, ",")
}

func wave46EmailReaderViolations(set wave42LegacyGitSourceSet) []string {
	var violations []string
	for owner, expected := range map[string]string{
		"InvitationReader":             "GetCollaborationInvite(context.Context,string)(domain.CollaborationInvite,error)",
		"InvitationOrganizationReader": "GetOrganizationMembership(context.Context,string,string)(domain.OrganizationMembership,error)",
		"InvitationEnterpriseReader":   "ListEnterpriseMembers(context.Context,string)([]domain.EnterpriseMembership,error)",
	} {
		declaration := set.types[owner]
		if declaration == nil {
			violations = append(violations, "missing narrow invitation reader: "+owner)
			continue
		}
		shape, narrow := declaration.Type.(*ast.InterfaceType)
		if !narrow || declaration.Assign.IsValid() || len(shape.Methods.List) != 1 {
			violations = append(violations, "invitation reader is broad, aliased, or not an interface: "+owner)
			continue
		}
		method := shape.Methods.List[0]
		signature, callable := method.Type.(*ast.FuncType)
		if !callable || len(method.Names) != 1 {
			violations = append(violations, "invitation reader embeds another capability: "+owner)
			continue
		}
		actual := method.Names[0].Name + "(" + wave46EmailFieldTypes(signature.Params) + ")(" + wave46EmailFieldTypes(signature.Results) + ")"
		if actual != expected {
			violations = append(violations, "invitation reader contract changed: "+owner+" "+actual)
		}
	}
	return violations
}

func wave46EmailOwnerViolations(set wave42LegacyGitSourceSet) []string {
	var violations []string
	for owner, expected := range map[string]map[string]string{
		"InvitationEmailWorker": {"queue": "outbound.InvitationEmailStore", "invitations": "InvitationReader", "authority": "invitationAuthority",
			"transactions": "outbound.IdentityTransactions", "sender": "outbound.InvitationMailer"},
		"invitationAuthority": {"organizations": "InvitationOrganizationReader", "enterprises": "InvitationEnterpriseReader"},
		"InvitationEmailWorkerConfig": {"Profile": "InvitationEmailProfile", "Queue": "outbound.InvitationEmailStore", "Invitations": "InvitationReader",
			"Organizations": "InvitationOrganizationReader", "Enterprises": "InvitationEnterpriseReader", "Transactions": "outbound.IdentityTransactions", "Sender": "outbound.InvitationMailer"},
	} {
		violations = append(violations, wave46EmailStructViolations(set, owner, expected)...)
	}
	violations = append(violations, wave46EmailReaderViolations(set)...)
	if set.methods["NewInvitationEmailWorker"] == nil {
		violations = append(violations, "missing public invitation email factory")
	}
	for root := range set.methods {
		constructor := root == "NewInvitationEmailWorker" || root == "assembleInvitationEmailWorker"
		if !constructor && !strings.HasPrefix(root, "InvitationEmailWorker.") && !strings.HasPrefix(root, "invitationAuthority.") &&
			root != "identityResultWithTransactions" && root != "withIdentityTransactions" {
			continue
		}
		for key, function := range wave45PRReachable(set, root) {
			if strings.HasPrefix(key, "Service.") || strings.HasPrefix(key, "IdentityService.") {
				violations = append(violations, "actual invitation email owner reaches legacy method: "+key)
			}
			ast.Inspect(function, func(node ast.Node) bool {
				switch value := node.(type) {
				case *ast.Ident:
					if value.Name == "Service" || value.Name == "IdentityService" || value.Name == "NewService" || value.Name == "NewIdentityService" {
						violations = append(violations, "reachable invitation email reverse dependency: "+key)
					}
				case *ast.GoStmt:
					if constructor {
						violations = append(violations, "invitation email factory starts hidden work: "+key)
					}
				}
				return true
			})
			if constructor && (strings.HasPrefix(function.Name.Name, "Run") || strings.HasPrefix(function.Name.Name, "Process") ||
				function.Name.Name == "claimInvitationEmail" || function.Name.Name == "authorizeInvitation" || function.Name.Name == "WithinIdentity") {
				violations = append(violations, "invitation email factory executes work: "+key)
			}
		}
	}
	return violations
}

func TestWave46InvitationEmailActualOwnerHasExactPortsNoReverseDependencyOrAutostart(test *testing.T) {
	set, err := wave42LegacyGitParseSources(wave42LegacyGitReadSources(test, filepath.Join("..", "app")))
	if err != nil {
		test.Fatal(err)
	}
	for _, violation := range wave46EmailOwnerViolations(set) {
		test.Error(violation)
	}
	for entry, dependencies := range map[string][]string{
		"InvitationEmailWorker.Run":                    {"InvitationEmailWorker.RunInvitationEmailWorker", "InvitationEmailWorker.ProcessInvitationEmail", "InvitationEmailWorker.claimInvitationEmail"},
		"InvitationEmailWorker.claimInvitationEmail":   {"identityResultWithTransactions", "withIdentityTransactions", "invitationAuthority.authorizeInvitation"},
		"InvitationEmailWorker.ProcessInvitationEmail": {"InvitationEmailWorker.claimInvitationEmail", "withIdentityTransactions"},
		"IdentityService.claimInvitationEmail":         {"InvitationEmailWorker.claimInvitationEmail"},
		"IdentityService.ProcessInvitationEmail":       {"InvitationEmailWorker.ProcessInvitationEmail"},
		"IdentityService.RunInvitationEmailWorker":     {"InvitationEmailWorker.RunInvitationEmailWorker"},
		"IdentityService.authorizeInvitation":          {"invitationAuthority.authorizeInvitation"},
		"IdentityService.withIdentity":                 {"withIdentityTransactions"},
		"identityResult":                               {"identityResultWithTransactions", "withIdentityTransactions"},
	} {
		if set.methods[entry] == nil {
			test.Error("missing original invitation email execution entry", entry)
		}
		reachable := wave45PRReachable(set, entry)
		for _, dependency := range dependencies {
			if reachable[dependency] == nil {
				test.Error("invitation email entry bypasses its actual execution or shared policy owner", entry, dependency)
			}
		}
	}
	for entry, operations := range map[string][]string{
		"InvitationEmailWorker.claimInvitationEmail":     {"NextInvitationEmail", "GetCollaborationInvite", "authorizeInvitation", "PutInvitationEmail"},
		"InvitationEmailWorker.ProcessInvitationEmail":   {"WithTimeout", "Send", "GetInvitationEmail", "PutInvitationEmail"},
		"InvitationEmailWorker.RunInvitationEmailWorker": {"NewTimer", "Stop", "Reset", "ProcessInvitationEmail"},
		"invitationAuthority.authorizeInvitation":        {"ValidateCollaborationScope", "GetOrganizationMembership", "ListEnterpriseMembers", "AtLeast"},
		"withIdentityTransactions":                       {"WithinIdentity", "Lock", "Unlock", "WithValue"},
	} {
		function := set.methods[entry]
		if function == nil {
			test.Error("missing actual invitation email algorithm", entry)
			continue
		}
		calls := wave42LegacyGitCalls(function.Body)
		for _, operation := range operations {
			if !calls[operation] {
				test.Error("algorithm remained outside the actual invitation email owner", entry, operation)
			}
		}
	}
	for _, entry := range []string{"IdentityService.claimInvitationEmail", "IdentityService.ProcessInvitationEmail", "IdentityService.RunInvitationEmailWorker", "IdentityService.authorizeInvitation", "IdentityService.withIdentity", "identityResult"} {
		if function := set.methods[entry]; function != nil && (function.Body == nil || len(function.Body.List) > 2) {
			test.Error("legacy invitation email or identity entry is not a thin facade", entry)
		}
	}
	for _, entry := range []string{"IdentityService.invitationEmailWorker", "IdentityService.InvitationEmailWorkerConfig"} {
		function := set.methods[entry]
		if function == nil {
			test.Error("missing lazy legacy invitation email compatibility assembly", entry)
			continue
		}
		for path, file := range set.files {
			for _, declaration := range file.Decls {
				if declaration == function && filepath.Base(path) != "invitation_email_compat.go" {
					test.Error("legacy invitation email assembly escaped its compatibility file", entry, path)
				}
			}
		}
	}
	shared := set.methods["withIdentityTransactions"]
	if shared != nil && (!wave42LegacyGitMentions(shared.Body, "identityMutationKey") || !wave42LegacyGitMentions(shared.Body, "localIdentityMutation")) {
		test.Error("shared identity transaction helper changed the existing development mutex or nested context key")
	}
	locks, keys := 0, 0
	for _, file := range set.files {
		for _, declaration := range file.Decls {
			group, declared := declaration.(*ast.GenDecl)
			if !declared {
				continue
			}
			for _, specification := range group.Specs {
				switch value := specification.(type) {
				case *ast.TypeSpec:
					if value.Name.Name == "identityMutationKey" {
						keys++
					}
				case *ast.ValueSpec:
					for _, name := range value.Names {
						if name.Name == "localIdentityMutation" {
							locks++
						}
					}
				}
			}
		}
	}
	if locks != 1 || keys != 1 {
		test.Error("identity policy no longer has its single original development mutex and nested key", locks, keys)
	}
}

func TestWave46InvitationEmailOwnerGuardRejectsCallbackShellsAliasesAndHiddenStarts(test *testing.T) {
	const valid = `package app
type InvitationEmailProfile string
type InvitationEmailWorkerConfig struct { Profile InvitationEmailProfile; Queue outbound.InvitationEmailStore; Invitations InvitationReader; Organizations InvitationOrganizationReader; Enterprises InvitationEnterpriseReader; Transactions outbound.IdentityTransactions; Sender outbound.InvitationMailer }
type InvitationEmailWorker struct { queue outbound.InvitationEmailStore; invitations InvitationReader; authority invitationAuthority; transactions outbound.IdentityTransactions; sender outbound.InvitationMailer }
type invitationAuthority struct { organizations InvitationOrganizationReader; enterprises InvitationEnterpriseReader }
type InvitationReader interface { GetCollaborationInvite(context.Context, string) (domain.CollaborationInvite, error) }
type InvitationOrganizationReader interface { GetOrganizationMembership(context.Context, string, string) (domain.OrganizationMembership, error) }
type InvitationEnterpriseReader interface { ListEnterpriseMembers(context.Context, string) ([]domain.EnterpriseMembership, error) }
func NewInvitationEmailWorker() { validateEmail(); assembleInvitationEmailWorker() }
func validateEmail() {}
func assembleInvitationEmailWorker() *InvitationEmailWorker { return &InvitationEmailWorker{} }
func (worker *InvitationEmailWorker) Run() { alias := worker; process := alias.ProcessInvitationEmail; process() }
func (worker *InvitationEmailWorker) ProcessInvitationEmail() { identityResultWithTransactions[int](); worker.authority.authorizeInvitation() }
func (authority invitationAuthority) authorizeInvitation() { commonAuthority() }
func commonAuthority() {}
func identityResultWithTransactions[Result any]() { withIdentityTransactions() }
func withIdentityTransactions() {}
func (service *IdentityService) ProcessInvitationEmail() { worker := assembleInvitationEmailWorker(); worker.ProcessInvitationEmail() }
func ProcessInvitationEmail() { NewIdentityService() }
func unrelated() { go NewService() }
`
	parse := func(source string) wave42LegacyGitSourceSet {
		test.Helper()
		set, err := wave42LegacyGitParseSources(map[string]string{"renamed.go": source})
		if err != nil {
			test.Fatal(err)
		}
		return set
	}
	set := parse(valid)
	if violations := wave46EmailOwnerViolations(set); len(violations) != 0 {
		test.Fatal("guard rejected a valid exact owner or unrelated legacy code", violations)
	}
	reachable := wave45PRReachable(set, "InvitationEmailWorker.Run")
	if reachable["InvitationEmailWorker.ProcessInvitationEmail"] == nil || reachable["withIdentityTransactions"] == nil || reachable["commonAuthority"] == nil ||
		reachable["ProcessInvitationEmail"] != nil || reachable["IdentityService.ProcessInvitationEmail"] != nil {
		test.Fatal("guard confused exact receivers, method aliases, generic helpers, or shared authority")
	}
	for _, invalid := range []string{
		strings.Replace(valid, "process()", "ProcessInvitationEmail()", 1),
		strings.Replace(valid, "process()", "hidden()", 1) + "\nfunc hidden() { NewIdentityService() }",
		strings.Replace(valid, "process()", "service := &IdentityService{}; service.ProcessInvitationEmail()", 1),
		strings.Replace(valid, "func commonAuthority() {}", "func commonAuthority() { NewService() }", 1),
		strings.Replace(valid, "func withIdentityTransactions() {}", "func withIdentityTransactions() { NewIdentityService() }", 1),
		strings.Replace(valid, "func validateEmail() {}", "func validateEmail() { go hiddenPoll() }", 1) + "\nfunc hiddenPoll() {}",
		strings.Replace(valid, "validateEmail();", "worker := assembleInvitationEmailWorker(); worker.Run();", 1),
		strings.Replace(valid, "validateEmail();", "start := validateEmail; start();", 1) + "\nfunc hiddenPoll() {}",
		strings.Replace(valid, "authority invitationAuthority;", "authority *IdentityService;", 1),
		strings.Replace(valid, "type invitationAuthority struct {", "type invitationAuthority struct { callback func();", 1),
		strings.Replace(valid, "type InvitationEmailWorker struct {", "type InvitationEmailWorker struct { IdentityService;", 1),
		strings.Replace(valid, "type InvitationReader interface {", "type InvitationReader interface { outbound.CollaborationInviteStore;", 1),
		strings.Replace(valid, "type InvitationOrganizationReader interface {", "type InvitationOrganizationReader = interface {", 1),
		strings.Replace(valid, "type InvitationEmailWorker struct {", "type InvitationEmailWorker = struct {", 1),
		strings.Replace(valid, "Sender outbound.InvitationMailer }", "Sender outbound.InvitationMailer; Execute func() }", 1),
	} {
		if strings.Contains(invalid, "start := validateEmail") {
			invalid = strings.Replace(invalid, "func validateEmail() {}", "func validateEmail() { go hiddenPoll() }", 1)
		}
		if violations := wave46EmailOwnerViolations(parse(invalid)); len(violations) == 0 {
			test.Fatal("guard accepted a callback shell, reverse dependency, hidden constructor work, or broadened port")
		}
	}
}
