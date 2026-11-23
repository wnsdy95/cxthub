package architecture

import (
	"go/ast"
	"strings"
	"testing"
)

func wave47GitHubStructViolations(set wave42LegacyGitSourceSet, owner string, expected map[string]string) []string {
	declaration := set.types[owner]
	if declaration == nil {
		return []string{"missing GitHub ownership type: " + owner}
	}
	shape, concrete := declaration.Type.(*ast.StructType)
	if !concrete || declaration.Assign.IsValid() {
		return []string{"GitHub ownership type is an alias or callback interface: " + owner}
	}
	var violations []string
	observed := map[string]bool{}
	for _, field := range shape.Fields.List {
		if len(field.Names) == 0 {
			violations = append(violations, "embedded GitHub owner dependency: "+owner)
		}
		for _, name := range field.Names {
			if kind, allowed := expected[name.Name]; !allowed || kind != wave42LegacyGitType(field.Type) {
				violations = append(violations, "broad/private/callback GitHub dependency: "+owner+"."+name.Name)
			}
			observed[name.Name] = true
		}
	}
	for name := range expected {
		if !observed[name] {
			violations = append(violations, "missing GitHub owner dependency: "+owner+"."+name)
		}
	}
	return violations
}

func wave47GitHubOwnerViolations(set wave42LegacyGitSourceSet) []string {
	var violations []string
	for owner, fields := range map[string]map[string]string{
		"GitHubWorker": {"store": "outbound.GitHubStore", "remote": "outbound.GitHubApp", "metadata": "outbound.MetadataStore",
			"repositories": "outbound.RepositoryStore", "organizations": "outbound.OrganizationStore", "teams": "outbound.TeamStore",
			"transactions": "outbound.IdentityTransactions", "work": "*repositoryWork", "publications": "*prPublicationCommands"},
		"GitHubWorkerConfig": {"Profile": "GitHubWorkerProfile", "Store": "outbound.GitHubStore", "Remote": "outbound.GitHubApp",
			"Metadata": "outbound.MetadataStore", "Blobs": "outbound.BlobStore", "Repositories": "outbound.RepositoryStore",
			"Organizations": "outbound.OrganizationStore", "Teams": "outbound.TeamStore", "IdentityTransactions": "outbound.IdentityTransactions",
			"Evidence": "outbound.RepositoryReader", "Memory": "outbound.MemoryObjectReader", "RequireCompleteMemory": "bool"},
		"prPublicationCommands": {"work": "*repositoryWork", "evidence": "outbound.RepositoryReader", "memory": "outbound.MemoryObjectReader",
			"requireCompleteMemory": "bool", "revisions": "inbound.RepositoryStateQueries", "revisionError": "error"},
	} {
		violations = append(violations, wave47GitHubStructViolations(set, owner, fields)...)
	}
	if set.methods["NewGitHubWorker"] == nil {
		violations = append(violations, "missing public GitHub worker factory")
	}
	for root := range set.methods {
		constructor := root == "NewGitHubWorker" || root == "assembleGitHubWorker" || root == "validateGitHubWorkerDependencies"
		if !constructor && !strings.HasPrefix(root, "GitHubWorker.") && !strings.HasPrefix(root, "prPublicationCommands.") {
			continue
		}
		for key, function := range wave45PRReachable(set, root) {
			if strings.HasPrefix(key, "Service.") || strings.HasPrefix(key, "IdentityService.") || strings.HasPrefix(key, "GitHubConnections.") {
				violations = append(violations, "actual GitHub owner reaches legacy execution: "+key)
			}
			ast.Inspect(function, func(node ast.Node) bool {
				switch value := node.(type) {
				case *ast.Ident:
					switch value.Name {
					case "Service", "IdentityService", "GitHubConnections", "NewService", "NewIdentityService", "NewGitHubConnections":
						violations = append(violations, "reachable GitHub reverse dependency: "+key)
					}
				case *ast.GoStmt:
					if constructor {
						violations = append(violations, "GitHub factory starts hidden work: "+key)
					}
				}
				return true
			})
			if constructor {
				calls := wave42LegacyGitCalls(function.Body)
				for _, operation := range []string{"Run", "Tick", "NewTicker", "NewTimer", "WithinIdentity", "WithinRepository", "WithinReadSnapshot",
					"Installation", "Repositories", "Teams", "TeamMembers", "MergedPullRequests", "PullRequestHeadHistory",
					"GetRepo", "ListGitHubConnections", "ListGitHubDeliveries", "GetPRJob", "RepositoryRevision", "Send", "Start"} {
					if calls[operation] {
						violations = append(violations, "GitHub factory executes adapter or lifecycle work: "+key+"."+operation)
					}
				}
			}
		}
	}
	return violations
}

func TestWave47GitHubOwnerGuardRejectsAliasesCallbackShellsAndHiddenStarts(test *testing.T) {
	const valid = `package app
type GitHubWorkerProfile string
type GitHubWorkerConfig struct { Profile GitHubWorkerProfile; Store outbound.GitHubStore; Remote outbound.GitHubApp; Metadata outbound.MetadataStore; Blobs outbound.BlobStore; Repositories outbound.RepositoryStore; Organizations outbound.OrganizationStore; Teams outbound.TeamStore; IdentityTransactions outbound.IdentityTransactions; Evidence outbound.RepositoryReader; Memory outbound.MemoryObjectReader; RequireCompleteMemory bool }
type GitHubWorker struct { store outbound.GitHubStore; remote outbound.GitHubApp; metadata outbound.MetadataStore; repositories outbound.RepositoryStore; organizations outbound.OrganizationStore; teams outbound.TeamStore; transactions outbound.IdentityTransactions; work *repositoryWork; publications *prPublicationCommands }
type prPublicationCommands struct { work *repositoryWork; evidence outbound.RepositoryReader; memory outbound.MemoryObjectReader; requireCompleteMemory bool; revisions inbound.RepositoryStateQueries; revisionError error }
type repositoryWork struct { metadata outbound.MetadataStore }
func NewGitHubWorker() *GitHubWorker { validateGitHubWorkerDependencies(); return assembleGitHubWorker() }
func validateGitHubWorkerDependencies() {}
func assembleGitHubWorker() *GitHubWorker { return &GitHubWorker{} }
func (worker *GitHubWorker) Run() { alias := worker; tick := alias.Tick; tick() }
func (worker *GitHubWorker) Tick() { worker.publications.preparePRContributions(); commonPolicy[int]() }
func (commands *prPublicationCommands) preparePRContributions() { commands.work.checkDocumentIdentity() }
func (work *repositoryWork) checkDocumentIdentity() {}
func commonPolicy[Result any]() {}
func (connections *GitHubConnections) Tick() { NewService() }
func Tick() { NewIdentityService() }
func unrelated() { go NewGitHubConnections() }
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
	if violations := wave47GitHubOwnerViolations(set); len(violations) != 0 {
		test.Fatal("guard rejected unrelated legacy code or exact public owner", violations)
	}
	reachable := wave45PRReachable(set, "GitHubWorker.Run")
	if reachable["GitHubWorker.Tick"] == nil || reachable["prPublicationCommands.preparePRContributions"] == nil ||
		reachable["repositoryWork.checkDocumentIdentity"] == nil || reachable["commonPolicy"] == nil || reachable["Tick"] != nil || reachable["GitHubConnections.Tick"] != nil {
		test.Fatal("guard confused receiver scopes, field owners, method aliases, or generic helpers")
	}
	for _, invalid := range []string{
		strings.Replace(valid, "tick()", "Tick()", 1),
		strings.Replace(valid, "tick()", "legacy := &GitHubConnections{}; legacy.Tick()", 1),
		strings.Replace(valid, "tick()", "hidden()", 1) + "\nfunc hidden() { NewIdentityService() }",
		strings.Replace(valid, "func commonPolicy[Result any]() {}", "func commonPolicy[Result any]() { NewService() }", 1),
		strings.Replace(valid, "func (work *repositoryWork) checkDocumentIdentity() {}", "func (work *repositoryWork) checkDocumentIdentity() { NewGitHubConnections() }", 1),
		strings.Replace(valid, "func validateGitHubWorkerDependencies() {}", "func validateGitHubWorkerDependencies() { go hiddenPoll() }", 1) + "\nfunc hiddenPoll() {}",
		strings.Replace(valid, "validateGitHubWorkerDependencies();", "owner := assembleGitHubWorker(); owner.Run();", 1),
		strings.Replace(valid, "func validateGitHubWorkerDependencies() {}", "func validateGitHubWorkerDependencies() { remote := &FakeRemote{}; remote.Installation() }", 1),
		strings.Replace(valid, "type GitHubWorker struct {", "type GitHubWorker struct { GitHubConnections;", 1),
		strings.Replace(valid, "type GitHubWorker struct {", "type GitHubWorker = struct {", 1),
		strings.Replace(valid, "publications *prPublicationCommands", "publications *Service", 1),
		strings.Replace(valid, "type prPublicationCommands struct {", "type prPublicationCommands struct { execute func();", 1),
		strings.Replace(valid, "type prPublicationCommands struct {", "type prPublicationCommands = struct {", 1),
		strings.Replace(valid, "RequireCompleteMemory bool }", "RequireCompleteMemory bool; Callback func() }", 1),
	} {
		if violations := wave47GitHubOwnerViolations(parse(invalid)); len(violations) == 0 {
			test.Fatal("guard accepted broad reverse ownership, callback shell, alias, or hidden factory work")
		}
	}
}
