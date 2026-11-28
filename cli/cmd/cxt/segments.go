package main

import (
	"context"
	"os"
	"path/filepath"
	"time"

	"github.com/wnsdy95/cxthub-cli/internal/adapters/backendclient"
	"github.com/wnsdy95/cxthub-cli/internal/adapters/capture"
	"github.com/wnsdy95/cxthub-cli/internal/adapters/codec"
	"github.com/wnsdy95/cxthub-cli/internal/adapters/gitctx"
	"github.com/wnsdy95/cxthub-cli/internal/adapters/memory"
	"github.com/wnsdy95/cxthub-cli/internal/adapters/providerfs"
	"github.com/wnsdy95/cxthub-cli/internal/adapters/remotecfg"
	adaptersession "github.com/wnsdy95/cxthub-cli/internal/adapters/session"
	"github.com/wnsdy95/cxthub-cli/internal/adapters/sessionrecords"
	"github.com/wnsdy95/cxthub-cli/internal/adapters/storage"
	"github.com/wnsdy95/cxthub-cli/internal/app"
	"github.com/wnsdy95/cxthub-cli/internal/domain"
	"github.com/wnsdy95/cxthub-cli/internal/ports/inbound"
	"github.com/wnsdy95/cxthub-cli/internal/ports/outbound"
)

// newSegmentReporter wires the read-only readers: Git, the Claude Code session
// files, and the Codex thread databases beside its sessions directory. With a
// cache directory the readers keep what they made of unchanged sources there;
// without one they write nothing anywhere.
func newSegmentReporter(cache string) (*app.SegmentReportService, error) {
	projects, err := providerfs.ClaudeProjectsDir()
	if err != nil {
		return nil, err
	}
	sessions, err := providerfs.CodexSessionsDir()
	if err != nil {
		return nil, err
	}
	claude := sessionrecords.NewClaudeRecords(projects)
	codex := sessionrecords.NewCodexRecords(filepath.Dir(sessions), gitctx.NormalizeRemoteURL)
	if cache != "" {
		claude, codex = claude.WithCache(cache), codex.WithCache(cache)
	}
	return app.NewSegmentReportService(gitctx.NewSessionGitRecords(), claude, codex), nil
}

// newSegmentUploader adds the repository's server to the same readers. It
// builds no local store.
func newSegmentUploader(cfg config, cache string) (*app.SegmentUploadService, error) {
	reports, err := newSegmentReporter(cache)
	if err != nil {
		return nil, err
	}
	remote := backendclient.NewResolvedBackendClient(func() (string, string, error) { return configuredTransport(cfg) })
	// The label tells the uploader's own computers apart, like a device login's.
	machine, _ := os.Hostname()
	service := app.NewSegmentUploadService(reports, remote, remotecfg.Wrap(cfg.RepoRoot, gitctx.NewGitContextAdapter()).CurrentRepo, machine)
	// Content is masked with the repository's policy and sent in the document
	// identity the repository captures with, both exactly as capture does.
	injectScrubOptions()
	content := backendclient.NewSessionContentUploader(remote, func(ctx context.Context) (domain.DocumentIdentity, error) {
		observed, err := remotecfg.Observe(ctx, cfg.RepoRoot)
		if err != nil {
			return "", err
		}
		return observed.CaptureIdentity()
	})
	// A Codex thread's conversation is read from the same directory its
	// records were listed from.
	codexSessions, err := providerfs.CodexSessionsDir()
	if err != nil {
		return nil, err
	}
	service.WithSessionContent(content, map[domain.ProviderKind]outbound.SessionContentSource{
		domain.ProviderClaude: sessionrecords.NewClaudeContent(),
		domain.ProviderCodex:  sessionrecords.NewCodexContent(filepath.Dir(codexSessions)),
	})
	// A session is written down at each commit it made, the way a capture's
	// memory is made: by rules, from the conversation up to the commit and the
	// tool's own memory, masked like the conversation.
	service.WithSummaries(remote, memory.NewSessionSummarizer(map[domain.ProviderKind]outbound.MemorySource{
		domain.ProviderClaude: memory.NewClaudeMemorySource(),
		domain.ProviderCodex:  memory.NewCodexMemorySource(),
	}, memory.NewRuleDistiller(), capture.ScrubNativeMemory))
	// Sessions that were removed from the server are not sent again.
	service.WithRemovals(remote)
	// A copy of what is sent for each session, and a log of the uploads, is
	// kept in the repository's .cxt folder, where cxt is set up.
	service.WithUploadRecords(storage.NewSessionUploadFiles(gitctx.ContextRoot), time.Now)
	// Branch IDs live in Git's own branch configuration.
	// The server is told the names of this computer's worktrees, never their
	// paths, so that a worktree no session worked in is still listed.
	// An automatic run asks the server to read the Git host once this clone's
	// branches of origin changed: after a push or a fetch.
	service.WithRemoteReadRequests(gitctx.OriginBranches, remote.RequestRemoteGitRefresh)
	// An automatic run also tells the server which pushes this clone made
	// and whether the run ended well, so that a push without a session can be
	// told from one made on a computer where cxt does not run.
	service.WithCollectorReports(remote, gitctx.OriginPushes, version)
	return service.WithBranchIDs(gitctx.NewBranchIDs(), remote, gitctx.NewBranchID).WithWorktreeList(remote), nil
}

// newSegmentCollector is the same upload for the detached collector a hook
// wakes. It adds the memory of what was sent, kept beside Git's own files.
func newSegmentCollector(cfg config) (inbound.SegmentCollector, *gitctx.CollectorFiles, error) {
	// A run nobody waits for reads again only what changed. The cache is the
	// user's own, outside every repository and every tool's records.
	cache := ""
	if dir, err := os.UserCacheDir(); err == nil {
		cache = filepath.Join(dir, "cxthub", "session-records-v1")
	}
	service, err := newSegmentUploader(cfg, cache)
	if err != nil {
		return nil, nil, err
	}
	files := gitctx.NewCollectorFiles()
	// Content is rebuilt when the masking policy or the document identity changes.
	epoch := func(ctx context.Context, _ string) string {
		policy, _ := capture.ScrubPolicyFingerprint(cfg.RepoRoot)
		identity := ""
		if observed, err := remotecfg.Observe(ctx, cfg.RepoRoot); err == nil {
			if captured, err := observed.CaptureIdentity(); err == nil {
				identity = string(captured)
			}
		}
		return policy + "\n" + identity
	}
	return service.WithCollectorState(files, time.Now, epoch), files, nil
}

// newSegmentCare wires what reads the records kept in the repository's .cxt
// folder. It needs no server and no reader of the tools' records.
func newSegmentCare() *app.SegmentCareService {
	return app.NewSegmentCareService(storage.NewSessionUploadFiles(gitctx.ContextRoot))
}

// newSegmentCareWithServer adds the repository's server and the uploader,
// for what changes a session there.
func newSegmentCareWithServer(cfg config) (*app.SegmentCareService, error) {
	uploader, err := newSegmentUploader(cfg, "")
	if err != nil {
		return nil, err
	}
	reporter, err := newSegmentReporter("")
	if err != nil {
		return nil, err
	}
	remote := backendclient.NewResolvedBackendClient(func() (string, string, error) { return configuredTransport(cfg) })
	return newSegmentCare().WithServer(remote, remotecfg.Wrap(cfg.RepoRoot, gitctx.NewGitContextAdapter()).CurrentRepo, uploader, time.Now).
		WithLocalFacts(reporter, segmentLocalFacts, gitctx.CommitsOnOrigin), nil
}

// segmentLocalFacts reads what this computer says about uploading from the
// repository holding cwd: whether cxt is set up there, whether automatic
// upload is on, whether a server is configured, and the collector's memory.
func segmentLocalFacts(ctx context.Context, cwd string) app.SegmentLocalFacts {
	root, setUp := gitctx.ContextRoot(ctx, cwd)
	if !setUp {
		root = cwd
	}
	_, origin := remotecfg.Origin(root)
	facts := app.SegmentLocalFacts{SetUp: setUp, AutoOn: remotecfg.SegmentsAuto(root), Connected: origin || os.Getenv("CXT_REMOTE") != ""}
	if state, err := gitctx.NewCollectorFiles().LoadCollectorState(ctx, cwd); err == nil && state.Version == domain.CollectorStateVersion && state.EnabledAt != "" {
		facts.Collector = &state
	}
	return facts
}

// segmentValueFlags are the flags of cxt segments that take a value.
var segmentValueFlags = map[string]bool{"--at": true, "--branch": true, "--to": true}

// segmentPositionals returns the arguments of cxt segments that are neither
// flags nor the value of one. They were validated before this path is
// reached.
func segmentPositionals(args []string) []string {
	var out []string
	for index := 0; index < len(args); index++ {
		arg := args[index]
		switch {
		case segmentValueFlags[arg]:
			index++ // its value
		case len(arg) > 0 && arg[0] != '-':
			out = append(out, arg)
		}
	}
	return out
}

// segmentFlagValue returns the value given for a flag, as "--flag value" or
// "--flag=value"; empty when it was not given.
func segmentFlagValue(args []string, name string) string {
	for index, arg := range args {
		if arg == name && index+1 < len(args) {
			return args[index+1]
		}
		if len(arg) > len(name) && arg[:len(name)+1] == name+"=" {
			return arg[len(name)+1:]
		}
	}
	return ""
}

// newSessionFork wires what a fork needs: Git for a branch, and for a
// continued conversation the repository's server and the writers of each
// tool's session files.
func newSessionFork(cfg config) *app.SessionForkService {
	remote := backendclient.NewResolvedBackendClient(func() (string, string, error) { return configuredTransport(cfg) })
	return app.NewSessionForkService(gitctx.NewForkBranches(), remote, remotecfg.Wrap(cfg.RepoRoot, gitctx.NewGitContextAdapter()).CurrentRepo,
		map[domain.ProviderKind]outbound.ProviderCodec{domain.ProviderClaude: codec.NewClaudeCodec(), domain.ProviderCodex: codec.NewCodexCodec()},
		map[domain.ProviderKind]outbound.SessionMaterializer{domain.ProviderClaude: adaptersession.NewClaudeMaterializer(), domain.ProviderCodex: adaptersession.NewCodexMaterializer()})
}
