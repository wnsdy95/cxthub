package cli

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/wnsdy95/cxthub/cli/internal/adapters/remotecfg"
	"github.com/wnsdy95/cxthub/cli/internal/domain"
	"github.com/wnsdy95/cxthub/cli/internal/ports/inbound"
)

type artifactSourceHistory struct{ seen inbound.HistoryQueryInput }

func (h *artifactSourceHistory) QueryHistory(_ context.Context, in inbound.HistoryQueryInput) (domain.HistoryQueryResult, error) {
	h.seen = in
	return domain.HistoryQueryResult{Position: domain.HashContent([]byte("authorized selection")), Selection: domain.HistorySelection{Branch: in.Branch}}, nil
}
func TestArtifactDefaultPreviewsMainAndExplicitRefInspectsArchive(t *testing.T) {
	for _, ref := range []string{"", "release-archive"} {
		t.Run(ref, func(t *testing.T) {
			cwd := t.TempDir()
			history, preparer := &artifactSourceHistory{}, &personalArtifactPreparer{}
			c := &Container{PrepareAgent: preparer, HistoryQuery: history, ResolveRepo: func(context.Context, string) (domain.Repo, error) { return domain.Repo{ID: "repo"}, nil }}
			args := []string{"--provider", "codex", "--output", filepath.Join(cwd, "context.json")}
			if ref != "" {
				args = append([]string{ref}, args...)
			}
			parsed, _, err := parseCommand("load", args)
			if err != nil {
				t.Fatal(err)
			}
			if err := runAgentArtifact(context.Background(), c, cwd, parsed); err != nil {
				t.Fatal(err)
			}
			if !preparer.seen.ArtifactOnly || preparer.seen.LatestMain != (ref == "") {
				t.Fatal("wrong artifact policy", preparer.seen)
			}
			if ref == "" {
				if !history.seen.ServerTip || history.seen.Branch != "main" || history.seen.Ref != "" {
					t.Fatal("preview followed local HEAD", history.seen)
				}
			} else if history.seen.ServerTip || history.seen.Ref != ref {
				t.Fatal("explicit archive replaced by main", history.seen)
			}
		})
	}
}

func TestStoredReplayPreferenceCannotOverrideManagedMain(t *testing.T) {
	cwd := t.TempDir()
	for _, mode := range []string{"full", "reconstructed", "memory"} {
		if err := remotecfg.SetLoadMode(context.Background(), cwd, mode); err != nil {
			t.Fatal(err)
		}
		if got := loadModeOr(cwd, ""); got != "" {
			t.Fatal("saved preference overrode managed input", got)
		}
		if got := hookLoadMode(cwd); got != "" {
			t.Fatal("Git hook replayed local branch", got)
		}
		if got := loadModeOr(cwd, mode); got != mode {
			t.Fatal("explicit archive restore lost", got)
		}
	}
}
