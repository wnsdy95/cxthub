package remotecfg

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/wnsdy95/cxthub/cli/internal/adapters/providerfs"
)

// This is an explicit server repair with a separately verified repository URL.
// Invalid remote syntax must not discard valid preferences or block restoration.
func TestRepairOriginInvalidURLPreservesPreferences(t *testing.T) {
	root, backup := t.TempDir(), t.TempDir()
	raw := []byte(`{"load_mode":"memory","future":{"kept":true},"remotes":{"origin":"not-a-repository-url","mirror":"https://example.invalid/team/mirror"}}`)
	if err := providerfs.WriteRepoFileAtomic(root, ".cxt/config", raw, 0600); err != nil {
		t.Fatal(err)
	}
	observed, err := Observe(context.Background(), root)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := observed.Remotes(); err == nil {
		t.Fatal("fixture must enter explicit repair's invalid-config branch")
	}
	const origin = "https://example.invalid/team/repo"
	if err := RepairOrigin(context.Background(), observed, origin, backup); err != nil {
		t.Fatalf("explicit repair cannot replace corrupt origin while preserving valid preferences: %v", err)
	}
	saved, err := os.ReadFile(filepath.Join(backup, "config.before"))
	if err != nil || string(saved) != string(raw) {
		t.Fatalf("exact backup lost: %v", err)
	}
	remotes, err := Load(root)
	if err != nil || remotes["origin"] != origin || remotes["mirror"] != "https://example.invalid/team/mirror" || LoadMode(root) != "memory" {
		t.Fatalf("repaired origin/preferences mismatch: %v %v", remotes, err)
	}
	o, err := Observe(context.Background(), root)
	if err != nil {
		t.Fatal(err)
	}
	f, err := o.fields()
	if err != nil || len(f["future"]) == 0 {
		t.Fatalf("unknown preference lost: %v", err)
	}
}

func TestRepairOriginRejectsForeignAndInvalidOtherRemotes(t *testing.T) {
	const requested = "https://example.invalid/team/repo"
	for _, tc := range []struct {
		name, raw string
		conflict  bool
	}{
		{"foreign-origin", `{"load_mode":"memory","remotes":{"origin":"https://example.invalid/team/foreign"}}`, true},
		{"foreign-origin-invalid-preference", `{"secrets_minlen":"broken","remotes":{"origin":"https://example.invalid/team/foreign"}}`, false},
		{"foreign-origin-invalid-version", `{"mutation_id":"broken","remotes":{"origin":"https://example.invalid/team/foreign"}}`, false},
		{"invalid-mirror", `{"remotes":{"origin":"broken","mirror":"broken"}}`, false},
		{"invalid-remote-name", `{"remotes":{"origin":"broken","bad/name":"https://example.invalid/team/mirror"}}`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root, backup := t.TempDir(), t.TempDir()
			if err := providerfs.WriteRepoFileAtomic(root, ".cxt/config", []byte(tc.raw), 0600); err != nil {
				t.Fatal(err)
			}
			observed := observeConfig(t, root)
			err := RepairOrigin(context.Background(), observed, requested, backup)
			if err == nil || (tc.conflict && !errors.Is(err, ErrChanged)) {
				t.Fatalf("repair bypassed existing evidence: %v", err)
			}
			if !sameObservation(observed, observeConfig(t, root)) {
				t.Fatal("rejected repair changed config")
			}
			if files, err := os.ReadDir(backup); err != nil || len(files) != 0 {
				t.Fatalf("rejected repair wrote backup: %v", err)
			}
		})
	}
}

func TestRepairOriginSemanticErrorsPreserveExactConfig(t *testing.T) {
	for _, field := range []string{`"load_mode":false`, `"mutation_id":"broken"`, `"secrets_minlen":"broken"`} {
		t.Run(field, func(t *testing.T) {
			root, backup := t.TempDir(), t.TempDir()
			raw := []byte(`{"checkout_mode":"prepare","future":{"kept":true},"remotes":{"origin":"broken","mirror":"https://example.invalid/team/mirror"},` + field + `}`)
			if err := providerfs.WriteRepoFileAtomic(root, ".cxt/config", raw, 0600); err != nil {
				t.Fatal(err)
			}
			expected := observeConfig(t, root)
			if _, err := expected.fields(); err == nil {
				t.Fatal("fixture must contain a semantic config error")
			}
			if err := RepairOrigin(context.Background(), expected, "https://example.invalid/team/repo", backup); err == nil {
				t.Fatal("semantic error silently discarded valid preferences/unknown fields")
			}
			if !sameObservation(expected, observeConfig(t, root)) {
				t.Fatal("semantic-error repair changed exact config bytes")
			}
			if files, err := os.ReadDir(backup); err != nil || len(files) != 0 {
				t.Fatalf("rejected semantic repair wrote backup: %v", err)
			}
		})
	}
}

func TestRepairOriginSameExpectationHasOneWinner(t *testing.T) {
	root := t.TempDir()
	raw := []byte(`{"load_mode":"memory","future":{"kept":true},"remotes":{"origin":"broken","mirror":"https://example.invalid/team/mirror"}}`)
	if err := providerfs.WriteRepoFileAtomic(root, ".cxt/config", raw, 0600); err != nil {
		t.Fatal(err)
	}
	expected := observeConfig(t, root)
	backups := []string{t.TempDir(), t.TempDir()}
	type result struct {
		index int
		err   error
	}
	results := make(chan result, 2)
	start := make(chan struct{})
	for i := range backups {
		go func(i int) {
			<-start
			results <- result{i, RepairOrigin(context.Background(), expected, "https://example.invalid/team/repo", backups[i])}
		}(i)
	}
	close(start)
	winners := 0
	for range backups {
		r := <-results
		if r.err == nil {
			winners++
			saved, err := os.ReadFile(filepath.Join(backups[r.index], "config.before"))
			if err != nil || string(saved) != string(raw) {
				t.Errorf("winner lost exact backup: %v", err)
			}
		} else {
			if !errors.Is(r.err, ErrChanged) {
				t.Errorf("loser did not report stale expectation: %v", r.err)
			}
			if files, err := os.ReadDir(backups[r.index]); err != nil || len(files) != 0 {
				t.Errorf("loser wrote backup: %v", err)
			}
		}
	}
	if winners != 1 {
		t.Fatalf("got %d acknowledged repairs for one expectation", winners)
	}
	current := observeConfig(t, root)
	if sameObservation(expected, current) {
		t.Fatal("successful repair reused old version")
	}
	remotes, err := current.Remotes()
	if err != nil || remotes["origin"] != "https://example.invalid/team/repo" || remotes["mirror"] != "https://example.invalid/team/mirror" || LoadMode(root) != "memory" {
		t.Fatalf("winner lost fields: %v %v", remotes, err)
	}
	fields, err := current.fields()
	if err != nil || len(fields["future"]) == 0 {
		t.Fatalf("winner lost unknown field: %v", err)
	}
}
