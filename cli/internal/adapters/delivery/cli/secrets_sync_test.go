package cli

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/wnsdy95/cxthub-cli/internal/adapters/secretscrypto"
	"github.com/wnsdy95/cxthub-cli/internal/adapters/storage"
	"github.com/wnsdy95/cxthub-cli/internal/domain"
)

type secretsTestRemote struct {
	raw      []byte
	revision string
	writes   int
	reads    int
	fail     error
}

func (r *secretsTestRemote) SecretsOrigin() string { return "https://api.example.test" }

func secretsLockForTest(ctx context.Context, cwd string, work func() error) error {
	return storage.NewFileStore(cwd).WithSecretsLock(ctx, work)
}

func (r *secretsTestRemote) PullSecrets(context.Context, string) ([]byte, error) {
	r.reads++
	if r.fail != nil {
		return nil, r.fail
	}
	if r.raw == nil {
		return nil, domain.ErrNotFound
	}
	return r.raw, nil
}
func (r *secretsTestRemote) PushSecrets(_ context.Context, _ string, raw []byte, _ bool, _ string, revision string) (string, error) {
	expected := r.revision
	if expected == "" {
		expected = "absent"
	}
	if expected != revision {
		return "", errors.New("secrets_conflict")
	}
	r.writes++
	r.revision = string(rune('a' + r.writes))
	var env map[string]any
	_ = json.Unmarshal(raw, &env)
	env["revision"] = r.revision
	r.raw, _ = json.Marshal(env)
	return r.revision, nil
}
func TestSecretsEditBaselineSurvivesConflictAndProtectsDirtyPull(t *testing.T) {
	ctx := context.Background()
	cwd := t.TempDir()
	repo := "test-repository"
	pass := "amber orbit violet meadow"
	r := &secretsTestRemote{}
	if err := os.WriteFile(filepath.Join(cwd, ".cxtsecrets"), []byte("first"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := syncSecrets(ctx, secretsLockForTest, r, cwd, repo, "push", pass, false, false); err != nil {
		t.Fatal(err)
	}
	baseline, err := os.ReadFile(filepath.Join(cwd, secretsBaselinePath))
	if err != nil {
		t.Fatal(err)
	}
	// A teammate edits with the same passphrase.
	env, err := secretscrypto.Encrypt(pass, "teammate", repo)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(env)
	if _, err := r.PushSecrets(ctx, repo, raw, false, "", r.revision); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cwd, ".cxtsecrets"), []byte("my draft"), 0600); err != nil {
		t.Fatal(err)
	}
	for _, rotate := range []bool{false, true} {
		if err := syncSecrets(ctx, secretsLockForTest, r, cwd, repo, "push", pass, rotate, false); err == nil || !strings.Contains(err.Error(), "secrets_conflict") {
			t.Fatalf("stale write accepted: %v", err)
		}
	}
	after, _ := os.ReadFile(filepath.Join(cwd, secretsBaselinePath))
	if string(after) != string(baseline) {
		t.Fatal("conflict advanced baseline")
	}
	if err := syncSecrets(ctx, secretsLockForTest, r, cwd, repo, "pull", pass, false, false); err == nil {
		t.Fatal("dirty pull overwrote draft")
	}
	draft, _ := os.ReadFile(filepath.Join(cwd, ".cxtsecrets"))
	if string(draft) != "my draft" {
		t.Fatal("draft lost")
	}
	if err := syncSecrets(ctx, secretsLockForTest, r, cwd, repo, "pull", pass, false, true); err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(filepath.Join(cwd, ".cxtsecrets"))
	if string(got) != "teammate" {
		t.Fatal("force pull failed")
	}
	if err := syncSecrets(ctx, secretsLockForTest, r, cwd, repo, "push", pass, false, false); err != nil {
		t.Fatal(err)
	}
}
func TestSecretsMissingBaselineNeverAdoptsCurrentServerRevision(t *testing.T) {
	cwd := t.TempDir()
	_ = os.WriteFile(filepath.Join(cwd, ".cxtsecrets"), []byte("local draft"), 0600)
	r := &secretsTestRemote{raw: []byte(`{"revision":"server"}`), revision: "server"}
	if err := syncSecrets(context.Background(), secretsLockForTest, r, cwd, "repo", "push", "amber orbit violet meadow", false, false); err == nil {
		t.Fatal("adopted latest revision for stale draft")
	}
	if r.writes != 0 {
		t.Fatal("unguarded push")
	}
	r.raw = nil
	r.fail = errors.New("server unavailable")
	if err := syncSecrets(context.Background(), secretsLockForTest, r, cwd, "repo", "push", "amber orbit violet meadow", false, false); err == nil {
		t.Fatal("network error treated as absence")
	}
}

func TestSecretsUsesInjectedLockBeforeReadingOrSending(test *testing.T) {
	ctx := context.Background()
	cwd := test.TempDir()
	remote := &secretsTestRemote{}
	if err := os.WriteFile(filepath.Join(cwd, ".cxtsecrets"), []byte("draft"), 0600); err != nil {
		test.Fatal(err)
	}
	if err := syncSecrets(ctx, nil, remote, cwd, "repo", "push", "amber orbit violet meadow", false, false); err == nil {
		test.Fatal("missing lock accepted")
	}
	blocked := errors.New("secrets lock failed")
	calls := 0
	withLock := func(current context.Context, path string, work func() error) error {
		calls++
		if current != ctx || path != cwd {
			test.Fatal("worktree or context changed")
		}
		return blocked
	}
	if err := syncSecrets(ctx, withLock, remote, cwd, "repo", "push", "amber orbit violet meadow", false, false); !errors.Is(err, blocked) {
		test.Fatalf("lock error changed: %v", err)
	}
	if calls != 1 || remote.reads != 0 || remote.writes != 0 {
		test.Fatalf("secrets bypassed exclusion: calls=%d remote=%+v", calls, remote)
	}
	if _, err := os.Stat(filepath.Join(cwd, ".cxt")); !errors.Is(err, os.ErrNotExist) {
		test.Fatalf("driver constructed a fallback store: %v", err)
	}
}
