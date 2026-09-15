package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"time"

	"github.com/wnsdy95/cxthub/cli/internal/adapters/authcfg"
	"github.com/wnsdy95/cxthub/cli/internal/adapters/backendclient"
	"github.com/wnsdy95/cxthub/cli/internal/adapters/branchjournal"
	"github.com/wnsdy95/cxthub/cli/internal/adapters/gitctx"
	"github.com/wnsdy95/cxthub/cli/internal/adapters/providerfs"
	"github.com/wnsdy95/cxthub/cli/internal/adapters/remotecfg"
	"github.com/wnsdy95/cxthub/cli/internal/adapters/storage"
	"github.com/wnsdy95/cxthub/cli/internal/app"
	"github.com/wnsdy95/cxthub/cli/internal/domain"
	"github.com/wnsdy95/cxthub/cli/internal/ports/inbound"
)

// Repair has its own composition root: damaged replica metadata must not run
// ordinary replay, capture or provider setup before cloud objects are verified.
func runRepair(args []string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	cwd, err := os.Getwd()
	if err != nil {
		return err
	}
	roots, err := gitctx.ResolveRepositoryRoots(ctx, cwd)
	if err != nil {
		return err
	}
	j, err := branchjournal.Open(ctx, cwd)
	if err != nil {
		return err
	}
	repoID, err := j.Repository()
	if err != nil {
		return fmt.Errorf("cannot establish repository identity from the Git journal: %w; preserve .cxt and run cxt doctor", err)
	}
	var explicit string
	for i := 0; i < len(args)-1; i++ {
		if args[i] == "--remote" {
			explicit = args[i+1]
		}
	}
	remotes, configErr := remotecfg.Load(roots.SharedRoot)
	origin := explicit
	if origin == "" {
		origin = remotes["origin"]
	}
	if origin == "" {
		return fmt.Errorf("verified repository URL required: cxt repair --from-server --remote <repository-url>")
	}
	origin, err = remotecfg.CanonicalURL(origin)
	if err != nil {
		return err
	}
	if string(remotecfg.RepoIDFor(origin)) != repoID {
		return fmt.Errorf("repair URL conflicts with durable repository identity; no local objects changed")
	}
	if configErr == nil && remotes["origin"] != "" && string(remotecfg.RepoIDFor(remotes["origin"])) != repoID {
		return fmt.Errorf("local config conflicts with durable repository identity; inspect before repair")
	}
	endpoint, err := remotecfg.APIBase(origin)
	if err != nil {
		return err
	}
	u, _ := url.Parse(endpoint)
	token := func() string {
		if token := os.Getenv("CXT_TOKEN"); token != "" {
			return token
		}
		return authcfg.Token(u.Host)
	}
	backup, err := j.StartRepair(repoID)
	if err != nil {
		return err
	}
	stage := storage.NewFileStore(filepath.Join(backup, "server"))
	remote := backendclient.NewBackendClient(func() string { return endpoint }, token, domain.TeamIdentity{})
	remote.SetChunkLocal(stage)
	syncer := app.NewSyncRepoService(stage, remote, nil)
	if _, err := syncer.Pull(ctx, inbound.SyncInput{RepoID: repoID, FetchOnly: true}); err != nil {
		return fmt.Errorf("server verification failed; live replica unchanged (recovery evidence: %s): %w", backup, err)
	}
	manifest, err := remote.RemoteManifest(ctx, repoID)
	if err != nil {
		return err
	}
	if manifest.RepoID != repoID {
		return domain.ErrHashMismatch
	}
	cloudRepo, err := remote.Repository(ctx, repoID)
	if err != nil {
		return err
	}
	// Server HEAD is not a user's worktree cursor. Use the configured default only
	// as the shared initialization marker; preserve any existing healthy HEAD.
	filtered := make([]domain.Ref, 0, len(manifest.Refs)+1)
	defaultPresent := false
	for _, ref := range manifest.Refs {
		if ref.Kind == domain.RefHEAD {
			continue
		}
		filtered = append(filtered, ref)
		if ref.Kind == domain.RefBranch && ref.Name == cloudRepo.DefaultBranch {
			defaultPresent = true
		}
	}
	if !defaultPresent {
		return fmt.Errorf("server default branch has no context; repair cannot invent an initial position")
	}
	filtered = append(filtered, domain.Ref{RepoID: repoID, Kind: domain.RefHEAD, Name: "HEAD", Symbolic: cloudRepo.DefaultBranch})
	report, repairErr := storage.NewFileStore(roots.SharedRoot).RepairFromReplica(ctx, stage, repoID, filtered, backup)
	// Restore only absent/unparseable config. Valid local preferences are kept.
	// A malformed file is retained exactly, including unknown fields.
	if configErr != nil || remotes["origin"] == "" {
		raw, readErr := providerfs.ReadRepoFile(roots.SharedRoot, ".cxt/config")
		if readErr != nil && !os.IsNotExist(readErr) {
			return readErr
		}
		if readErr == nil {
			if err := providerfs.WriteRepoFileAtomic(backup, "config.before", raw, 0600); err != nil {
				return err
			}
		}
		data, _ := json.Marshal(map[string]any{"remotes": map[string]string{"origin": origin}})
		if err := providerfs.WriteRepoFileAtomic(roots.SharedRoot, ".cxt/config", data, 0600); err != nil {
			return err
		}
	}
	fmt.Printf("Restored %d verified server records. Quarantine and recovery evidence: %s\n", len(report.Repaired), backup)
	for _, issue := range report.Issues {
		fmt.Printf("Unresolved local record: %s\n", issue)
	}
	fmt.Println("Healthy local-only records and worktree positions were preserved. Run cxt doctor, then cxt branch replay for queued operations.")
	return repairErr
}
