package capture

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"

	"github.com/wnsdy95/cxthub-cli/internal/adapters/providerfs"
	"github.com/wnsdy95/cxthub-cli/internal/domain"
	"github.com/wnsdy95/cxthub-cli/internal/ports/outbound"
)

type CommandSessionResolver struct {
	claude *ClaudeCaptureSource
	codex  *CodexCaptureSource
}

var _ outbound.CommandSessionResolver = (*CommandSessionResolver)(nil)
var _ outbound.ProviderSessionInventory = (*CommandSessionResolver)(nil)

func NewCommandSessionResolver(claude *ClaudeCaptureSource, codex *CodexCaptureSource) *CommandSessionResolver {
	return &CommandSessionResolver{claude: claude, codex: codex}
}

func (resolver *CommandSessionResolver) LocateActiveSession(ctx context.Context, cwd string, provider domain.ProviderKind) (string, error) {
	if resolver == nil || resolver.claude == nil || resolver.codex == nil {
		return "", fmt.Errorf("provider session inventory unavailable")
	}
	switch provider {
	case domain.ProviderClaude:
		return resolver.claude.LocateActiveSession(ctx, cwd)
	case domain.ProviderCodex:
		return resolver.codex.LocateActiveSession(ctx, cwd)
	default:
		return "", fmt.Errorf("unsupported session inventory provider %q", provider)
	}
}

func (resolver *CommandSessionResolver) SessionFilesForCwd(ctx context.Context, cwd string, provider domain.ProviderKind) []string {
	if resolver == nil || resolver.claude == nil || resolver.codex == nil {
		return nil
	}
	switch provider {
	case domain.ProviderClaude:
		return resolver.claude.SessionFilesForCwd(ctx, cwd)
	case domain.ProviderCodex:
		return resolver.codex.SessionFilesForCwd(ctx, cwd)
	default:
		return nil
	}
}

func (resolver *CommandSessionResolver) ResolveCommandSession(ctx context.Context, cwd string, explicit domain.ProviderKind) (outbound.CommandSessionSelection, error) {
	if resolver == nil || resolver.claude == nil || resolver.codex == nil {
		return outbound.CommandSessionSelection{}, fmt.Errorf("command session capture sources unavailable")
	}
	wrappedProvider, managed := resolver.SupervisedProvider(ctx, cwd)
	appPath := ""
	if !managed && (explicit == "" || explicit == domain.ProviderCodex) {
		id := strings.TrimSpace(os.Getenv("CODEX_THREAD_ID"))
		if id == "" {
			id = strings.TrimSpace(os.Getenv("CODEX_SESSION_ID"))
		}
		if providerfs.ValidSessionID(id) {
			var err error
			appPath, err = LocateRegisteredAppSession(cwd, domain.ProviderCodex, id)
			if errors.Is(err, domain.ErrNoActiveSession) {
				appPath, err = resolver.codex.LocateSession(ctx, cwd, id)
			}
			if err == nil || errors.Is(err, domain.ErrNoActiveSession) {
				verified, verifyErr := LocateCodexCommandSession(ctx, cwd, id)
				if verifyErr != nil {
					err = verifyErr
				} else if err == nil && verified != appPath {
					err = fmt.Errorf("ambiguous exact Codex command session")
				} else {
					appPath, err = verified, nil
				}
			}
			if err != nil {
				return outbound.CommandSessionSelection{}, err
			}
		}
	}
	provider := explicit
	if provider == "" {
		if managed {
			provider = wrappedProvider
		} else if appPath != "" {
			provider = domain.ProviderCodex
		} else {
			provider = resolver.activeProviderForCwd(ctx, cwd)
		}
	}
	target := outbound.CommandSessionSelection{Provider: provider}
	if !managed && provider == domain.ProviderCodex && appPath != "" {
		target.SessionPath = appPath
		target.SessionID = strings.TrimSpace(os.Getenv("CODEX_THREAD_ID"))
		if target.SessionID == "" {
			target.SessionID = strings.TrimSpace(os.Getenv("CODEX_SESSION_ID"))
		}
		return target, nil
	}
	if !managed || provider != wrappedProvider {
		return target, nil
	}
	sessionID := strings.TrimSpace(os.Getenv("CXT_WRAPPED_SESSION_ID"))
	if protocol := os.Getenv("CXT_WRAPPED_CAPTURE_PROTOCOL"); protocol != "" && provider == domain.ProviderCodex {
		if protocol != NativeWrapperCaptureProtocol {
			return outbound.CommandSessionSelection{}, fmt.Errorf("unsupported native wrapper capture protocol")
		}
		pid, err := strconv.Atoi(os.Getenv("CXT_WRAPPER_PID"))
		if err != nil {
			return outbound.CommandSessionSelection{}, fmt.Errorf("invalid native capture supervisor")
		}
		sessionID, err = NativeWrapperSession(cwd, pid, strings.TrimSpace(os.Getenv("CODEX_THREAD_ID")))
		if err != nil {
			return outbound.CommandSessionSelection{}, fmt.Errorf("cannot identify this wrapper's owned native Codex thread")
		}
	} else if !providerfs.ValidSessionID(sessionID) {
		sessionID = SessionAffinity(cwd, provider)
	}
	if !providerfs.ValidSessionID(sessionID) {
		return outbound.CommandSessionSelection{}, fmt.Errorf("cannot identify the %s session owned by this cxt wrapper yet", provider)
	}
	var path string
	var err error
	switch provider {
	case domain.ProviderClaude:
		path, err = resolver.claude.LocateSession(ctx, cwd, sessionID)
	case domain.ProviderCodex:
		path, err = resolver.codex.LocateSession(ctx, cwd, sessionID)
	default:
		return target, nil
	}
	if errors.Is(err, domain.ErrNoActiveSession) {
		path, err = LocateRegisteredAppSession(cwd, provider, sessionID)
	}
	if err != nil {
		return outbound.CommandSessionSelection{}, fmt.Errorf("cannot locate the %s session %s owned by this cxt wrapper: %w", provider, sessionID, err)
	}
	target.SessionPath, target.SessionID = path, sessionID
	return target, nil
}

func (resolver *CommandSessionResolver) SupervisedProvider(ctx context.Context, cwd string) (domain.ProviderKind, bool) {
	if resolver == nil || resolver.claude == nil || resolver.codex == nil {
		return "", false
	}
	agent := domain.ProviderKind(os.Getenv("CXT_WRAPPED_AGENT"))
	if agent != domain.ProviderClaude && agent != domain.ProviderCodex {
		agent = resolver.activeProviderForCwd(ctx, cwd)
	}
	if os.Getenv("CXT_WRAPPED") != "1" {
		return agent, false
	}
	wrapperPID, err := strconv.Atoi(os.Getenv("CXT_WRAPPER_PID"))
	if err != nil || wrapperPID <= 1 {
		return agent, false
	}
	return agent, hasProcessAncestor(os.Getppid(), wrapperPID, snapshotParentPID())
}

func (resolver *CommandSessionResolver) activeProviderForCwd(ctx context.Context, cwd string) domain.ProviderKind {
	mtime := func(path string, err error) int64 {
		if err != nil || path == "" {
			return 0
		}
		if info, statErr := os.Stat(path); statErr == nil {
			return info.ModTime().UnixNano()
		}
		return 0
	}
	claudePath, claudeErr := resolver.claude.LocateActiveSession(ctx, cwd)
	codexPath, codexErr := resolver.codex.LocateActiveSession(ctx, cwd)
	return providerByRecency(mtime(claudePath, claudeErr), mtime(codexPath, codexErr))
}

func providerByRecency(claudeNewest, codexNewest int64) domain.ProviderKind {
	if codexNewest > claudeNewest {
		return domain.ProviderCodex
	}
	return domain.ProviderClaude
}

func snapshotParentPID() func(int) (int, bool) {
	out, err := exec.Command("ps", "-axo", "pid=,ppid=").Output()
	if err != nil {
		return processParentPID
	}
	table := parsePIDTable(out)
	return func(pid int) (int, bool) {
		ppid, ok := table[pid]
		return ppid, ok && ppid > 0
	}
}

func parsePIDTable(out []byte) map[int]int {
	table := map[int]int{}
	for _, line := range strings.Split(string(out), "\n") {
		fields := strings.Fields(line)
		if len(fields) != 2 {
			continue
		}
		pid, perr := strconv.Atoi(fields[0])
		ppid, qerr := strconv.Atoi(fields[1])
		if perr == nil && qerr == nil && pid > 0 {
			table[pid] = ppid
		}
	}
	return table
}

func processParentPID(pid int) (int, bool) {
	out, err := exec.Command("ps", "-o", "ppid=", "-p", strconv.Itoa(pid)).Output()
	if err != nil {
		return 0, false
	}
	ppid, err := strconv.Atoi(strings.TrimSpace(string(out)))
	return ppid, err == nil && ppid > 0
}

func hasProcessAncestor(start, want int, parent func(int) (int, bool)) bool {
	if start <= 1 || want <= 1 {
		return false
	}
	seen := map[int]bool{}
	for pid, depth := start, 0; pid > 1 && depth < 64 && !seen[pid]; depth++ {
		if pid == want {
			return true
		}
		seen[pid] = true
		next, ok := parent(pid)
		if !ok {
			return false
		}
		pid = next
	}
	return false
}
