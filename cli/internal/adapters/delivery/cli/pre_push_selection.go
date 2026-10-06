package cli

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"slices"
	"strings"

	"github.com/wnsdy95/cxthub/cli/internal/adapters/remotecfg"
	"github.com/wnsdy95/cxthub/cli/internal/domain"
	"github.com/wnsdy95/cxthub/cli/internal/ports/inbound"
	"github.com/wnsdy95/cxthub/cli/internal/ports/outbound"
)

const maxPrePushBytes = 1 << 20

type gitPushUpdate struct {
	LocalRef, LocalOID, RemoteRef, RemoteOID string
}

// A hook receives the Git destination, not the CXT destination. Automatic
// publication only pairs named Git origin with the configured CXT origin.
func validateGitPushRemote(ctx context.Context, cwd string, args []string) error {
	if len(args) != 2 || args[0] != "origin" {
		return fmt.Errorf("automatic context publication requires the named Git remote origin")
	}
	raw, err := exec.CommandContext(ctx, "git", "-C", cwd, "remote", "get-url", "--push", "--all", "origin").Output()
	if err != nil {
		return fmt.Errorf("cannot verify Git origin push destination")
	}
	for _, configured := range strings.Split(strings.TrimSpace(string(raw)), "\n") {
		if configured != "" && configured == args[1] {
			return nil
		}
	}
	return fmt.Errorf("Git push destination does not match configured origin")
}

func checkGitPushObjects(ctx context.Context, cwd string, updates []gitPushUpdate) error {
	for _, u := range updates {
		raw, err := exec.CommandContext(ctx, "git", "-C", cwd, "rev-parse", "--verify", u.LocalRef).Output()
		if err != nil || !strings.EqualFold(strings.TrimSpace(string(raw)), u.LocalOID) {
			return fmt.Errorf("Git branch %q changed after push selection; context publication remains pending", u.LocalRef)
		}
	}
	return nil
}

func resolveGitPushScope(ctx context.Context, c *Container, cwd string, updates []gitPushUpdate) (domain.PublicationScope, error) {
	var scope domain.PublicationScope
	if len(updates) == 0 {
		return scope, fmt.Errorf("no Git branches selected for context publication")
	}
	if c.History == nil {
		return scope, fmt.Errorf("context branch identity is unavailable")
	}
	repo, err := resolvePublicationRepo(ctx, c, cwd)
	if err != nil {
		return scope, err
	}
	seen := map[string]string{}
	for _, u := range updates {
		local, remote := strings.TrimPrefix(u.LocalRef, "refs/heads/"), strings.TrimPrefix(u.RemoteRef, "refs/heads/")
		binding, err := c.History.ResolveLocalBranch(ctx, repo.ID, local)
		if err != nil {
			return domain.PublicationScope{}, err
		}
		if binding.Inactive || binding.LocalBranch != local || binding.Branch == "" || binding.BranchID == "" {
			return domain.PublicationScope{}, fmt.Errorf("Git branch %q has no active context binding", local)
		}
		// Same-name pushes, the canonical shared name, and another explicitly
		// recorded tracking alias are meaningful. An arbitrary refspec is not
		// evidence that a new destination names this context identity.
		if remote != local && remote != binding.Branch {
			destination, err := c.History.ResolveLocalBranch(ctx, repo.ID, remote)
			if err != nil || !destination.Tracking || destination.Inactive || destination.LocalBranch != remote ||
				destination.BranchID != binding.BranchID || destination.Branch != binding.Branch {
				return domain.PublicationScope{}, fmt.Errorf("Git destination %q has no matching context alias", remote)
			}
		}
		if name, exists := seen[binding.BranchID]; exists {
			if name != binding.Branch {
				return domain.PublicationScope{}, fmt.Errorf("context branch identity changed during push selection")
			}
			continue
		}
		seen[binding.BranchID] = binding.Branch
		scope.Branches = append(scope.Branches, domain.PublicationBranch{Branch: binding.Branch, BranchID: binding.BranchID})
	}
	return scope, nil
}

func runGitPrePush(ctx context.Context, c *Container, cwd string, args []string, input io.Reader) error {
	if err := validateGitPushRemote(ctx, cwd, args); err != nil {
		hookWarn("context push skipped: %v", err)
		return nil
	}
	updates, err := readGitPushUpdates(input)
	if err != nil {
		hookWarn("context push selection failed: %v", err)
		return nil
	}
	if len(updates) == 0 {
		return nil
	}
	if _, ok := remotecfg.Origin(cxtRepoRoot(ctx, cwd)); !ok && os.Getenv("CXT_REMOTE") == "" {
		hookWarn("Context not pushed — connect with cxt remote add origin <url>")
		return nil
	}
	if err := checkGitPushObjects(ctx, cwd, updates); err != nil {
		hookWarn("%v", err)
		return nil
	}
	endReplay := outbound.BeginSyncDiagnostic(ctx, outbound.SyncStageHookReplay, outbound.SyncDiagnosticCounts{})
	// Replay only the pushed refs, which need not include HEAD. Applied votes
	// remain durable; this foreground path must not spawn a broad publisher.
	seen := map[string]bool{}
	for _, u := range updates {
		if seen[u.LocalRef] {
			continue
		}
		seen[u.LocalRef] = true
		if err = replayBranchOperationsForRefWithPublication(ctx, c, cwd, u.LocalRef, func(string) {}); err != nil {
			endReplay(err)
			hookWarn("selected branch operation remains queued: %v", err)
			return nil
		}
	}
	scope, err := resolveGitPushScope(ctx, c, cwd, updates)
	if err == nil {
		ids := map[string]bool{}
		for _, b := range scope.Branches {
			ids[b.BranchID] = true
		}
		err = replayRewriteHistoryForBranches(ctx, c, cwd, ids)
	}
	endReplay(err)
	if err != nil {
		hookWarn("selected context publication remains pending: %v", err)
		return nil
	}
	defer wakeHistoricalSync(c, cwd)
	check := func() error {
		if err := checkGitPushObjects(ctx, cwd, updates); err != nil {
			return err
		}
		current, err := resolveGitPushScope(ctx, c, cwd, updates)
		if err != nil {
			return err
		}
		if !slices.Equal(current.Branches, scope.Branches) {
			return fmt.Errorf("context identity changed after Git push selection")
		}
		return checkGitPushAliasProof(ctx, c, cwd, updates)
	}
	var out inbound.SyncOutput
	for attempt := 1; attempt <= 2; attempt++ {
		if err = check(); err != nil {
			break
		}
		pushCtx := outbound.WithSyncDiagnosticAttempt(ctx, attempt)
		// Preserve the exact identity scope across the append retry. A new
		// same-name branch must never inherit an earlier operation's authority.
		frozen := scope
		frozen.Branches = slices.Clone(scope.Branches)
		out, err = c.Sync.Push(pushCtx, inbound.SyncInput{Cwd: cwd, ForegroundOnly: true, Append: attempt == 2, Publication: &frozen})
		if attempt == 2 {
			if err == nil {
				fmt.Println("cxt: repositioned and appended after the remote head — no history lost")
			} else {
				err = fmt.Errorf("auto append failed (%w) — run 'cxt push --append' manually", err)
			}
		}
		if !errors.Is(err, domain.ErrSyncConflict) {
			break
		}
	}
	if err != nil {
		syncWarn(cwd, "push", err)
		return nil
	}
	clearAuthHint(cwd)
	fmt.Printf("cxt: pushed %d snapshot(s), checked %d ref(s) → origin\n", out.Pushed, len(out.NewRefs))
	if out.BackfillPending > 0 {
		fmt.Printf("cxt: %d retained historical snapshot(s) remain queued; current publication completed\n", out.BackfillPending)
	}
	return nil
}

// A persistent alias binding is not evidence at every Git revision. Run this
// after selected local replay so durable publication journals can supply their
// existing proof; never synthesize one from the requested refspec.
func checkGitPushAliasProof(ctx context.Context, c *Container, cwd string, updates []gitPushUpdate) error {
	repo, err := resolvePublicationRepo(ctx, c, cwd)
	if err != nil {
		return err
	}
	var events []domain.HistoryEvent
	loaded := false
	for _, u := range updates {
		local, remote := strings.TrimPrefix(u.LocalRef, "refs/heads/"), strings.TrimPrefix(u.RemoteRef, "refs/heads/")
		binding, err := c.History.ResolveLocalBranch(ctx, repo.ID, local)
		if err != nil {
			return err
		}
		if remote == local || remote == binding.Branch {
			continue
		}
		if !loaded {
			events, err = c.History.ListHistory(ctx, repo.ID)
			if err != nil {
				return err
			}
			loaded = true
		}
		proven := false
		for _, e := range events {
			if e.Kind == "publish" && e.RepoID == repo.ID && e.BranchID == binding.BranchID && e.GitAfter == strings.ToLower(u.LocalOID) && domain.ValidateHistoryEvent(e) == nil && slices.Contains(domain.PublicationSourceNames(e, events), remote) {
				proven = true
				break
			}
		}
		if !proven {
			return fmt.Errorf("Git destination %q has no completed context alias proof at the pushed revision", remote)
		}
	}
	return nil
}

// Read the complete input before authorizing any replay or publication. An
// empty result means no work, never the manual all-branches push default.
func readGitPushUpdates(input io.Reader) ([]gitPushUpdate, error) {
	raw, err := io.ReadAll(io.LimitReader(input, maxPrePushBytes+1))
	if err != nil {
		return nil, err
	}
	if len(raw) > maxPrePushBytes {
		return nil, fmt.Errorf("Git push selection exceeds %d bytes", maxPrePushBytes)
	}
	var selected []gitPushUpdate
	destinations := map[string]gitPushUpdate{}
	sources := map[string]string{}
	oidWidth := 0
	for line, rawLine := range bytes.Split(raw, []byte{'\n'}) {
		fields := strings.Fields(string(rawLine))
		if len(fields) == 0 {
			continue
		}
		if len(fields) != 4 {
			return nil, fmt.Errorf("incomplete Git push selection at line %d", line+1)
		}
		u := gitPushUpdate{fields[0], fields[1], fields[2], fields[3]}
		localZero, remoteZero := isZeroGitOID(u.LocalOID), isZeroGitOID(u.RemoteOID)
		if (!localZero && !validNonZeroGitOID(u.LocalOID)) || (!remoteZero && !validNonZeroGitOID(u.RemoteOID)) ||
			len(u.LocalOID) != len(u.RemoteOID) || (localZero && remoteZero) ||
			(oidWidth != 0 && oidWidth != len(u.LocalOID)) || (localZero != (u.LocalRef == "(delete)")) {
			return nil, fmt.Errorf("invalid Git push object IDs at line %d", line+1)
		}
		oidWidth = len(u.LocalOID)
		// Git supplies full destination refs. Check them even for ignored tags
		// so malformed rows cannot be mistaken for an empty selection.
		if !strings.HasPrefix(u.RemoteRef, "refs/") || domain.ValidateBranchName(strings.TrimPrefix(u.RemoteRef, "refs/")) != nil {
			return nil, fmt.Errorf("invalid Git push destination at line %d", line+1)
		}
		if strings.HasPrefix(u.LocalRef, "refs/") && domain.ValidateBranchName(strings.TrimPrefix(u.LocalRef, "refs/")) != nil {
			return nil, fmt.Errorf("invalid Git push source at line %d", line+1)
		}
		if previous, ok := destinations[u.RemoteRef]; ok {
			if previous != u {
				return nil, fmt.Errorf("conflicting Git push destination %q", u.RemoteRef)
			}
			continue
		}
		destinations[u.RemoteRef] = u
		if !localZero {
			if oid, ok := sources[u.LocalRef]; ok && oid != u.LocalOID {
				return nil, fmt.Errorf("Git push source %q changed within the selection", u.LocalRef)
			}
			sources[u.LocalRef] = u.LocalOID
		}
		// Tags, deletions and expressions such as HEAD~1 do not authorize a
		// context branch. Exact refspec/alias mapping happens after this parser.
		if !localZero && strings.HasPrefix(u.LocalRef, "refs/heads/") && strings.HasPrefix(u.RemoteRef, "refs/heads/") {
			selected = append(selected, u)
		}
	}
	return selected, nil
}
