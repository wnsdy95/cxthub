//go:build postgres

package store

import (
	"context"
	"errors"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

func TestGraphQualificationInputsRequireOwnedReadSnapshot(t *testing.T) {
	storage := &PostgresStore{}
	repo := domain.HashContent([]byte(t.Name()))
	for _, ctx := range []context.Context{
		context.Background(), outbound.WithDocReadOnly(context.Background()),
		context.WithValue(context.Background(), repositoryTxKey{}, &repositoryTx{owner: storage}),
		context.WithValue(context.Background(), repositoryTxKey{}, &repositoryTx{owner: &PostgresStore{}, readOnly: true}),
	} {
		if err := storage.CheckGraphQualificationInputs(ctx, repo); !errors.Is(err, domain.ErrConflict) {
			t.Fatalf("input sizing escaped read snapshot: %v", err)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := storage.CheckGraphQualificationInputs(ctx, repo); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
}

func TestGraphQualificationInputTableCoverage(t *testing.T) {
	expected := []string{"repos", "snapshots", "refs", "context_history", "pending_contexts", "unsync_contexts", "session_archives", "reflog", "repository_revisions",
		"git_commit_trees", "git_tree_nodes", "git_commit_deltas", "git_ref_observations", "pr_history_checkpoints", "pr_participations", "pr_contributions",
		"git_contributions", "git_head_receipts", "git_contribution_admissions", "git_context_heads", "git_contribution_proofs"}
	var actual []string
	for _, table := range regexp.MustCompile(`FROM (\w+) source WHERE source\.(\w+)=\$1`).FindAllStringSubmatch(graphQualificationInputSizesSQL, -1) {
		column := "repo_id"
		if table[1] == "repos" {
			column = "id"
		}
		if table[2] != column {
			t.Fatalf("unscoped input table: %v", table)
		}
		actual = append(actual, table[1])
	}
	if !slices.Equal(expected, actual) || strings.Count(graphQualificationInputSizesSQL, "octet_length(to_jsonb(source)::text)") != len(expected) ||
		!strings.Contains(graphQualificationInputSizesSQL, "LIMIT $2") {
		t.Fatalf("qualification no longer budgets every actual input independently: %v", actual)
	}
}
