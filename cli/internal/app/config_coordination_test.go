package app

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/wnsdy95/cxthub/cli/internal/adapters/gitctx"
	"github.com/wnsdy95/cxthub/cli/internal/adapters/remotecfg"
	"github.com/wnsdy95/cxthub/cli/internal/adapters/storage"
	"github.com/wnsdy95/cxthub/cli/internal/domain"
	"github.com/wnsdy95/cxthub/cli/internal/ports/inbound"
	"github.com/wnsdy95/cxthub/cli/internal/ports/outbound"
)

type configCoordinationProjection struct {
	store   *storage.FileStore
	entered chan struct{}
	release chan struct{}
}

func (*configCoordinationProjection) Eligible(string, string) bool { return true }
func (*configCoordinationProjection) Settings(string, string) (domain.SettingsBundle, bool) {
	return domain.SettingsBundle{}, false
}
func (*configCoordinationProjection) RecordAffinity(string, domain.ProviderKind, string) {}
func (p *configCoordinationProjection) Project(ctx context.Context, root, path string, source outbound.CaptureSource, codec outbound.ProviderCodec, partial bool) (domain.Envelope, domain.ContentHash, int64, *time.Time, error) {
	close(p.entered)
	<-p.release
	envelope := domain.Envelope{SessionOriginID: "config-race", GitBranch: "main"}
	h, err := p.store.PutDoc(ctx, domain.SessionDoc{CIR: domain.CIRDocument{Envelope: envelope}})
	return envelope, h, 1, nil, err
}

// Pause real Save after admission. Config publication must wait for the entire
// durable snapshot/pending capture, using the same OS gate.
func TestConfigCoordinationCaptureCannotCrossRemotePublication(t *testing.T) {
	t.Setenv("GIT_CONFIG_GLOBAL", "/dev/null")
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	root := t.TempDir()
	if out, err := exec.Command("git", "-C", root, "init", "-q", "-b", "main").CombinedOutput(); err != nil {
		t.Fatalf("git: %s %v", out, err)
	}
	const a = "https://example.invalid/team/a"
	const b = "https://example.invalid/team/b"
	initial, err := remotecfg.Observe(context.Background(), root)
	if err != nil {
		t.Fatal(err)
	}
	if err := remotecfg.Replace(context.Background(), initial, remotecfg.Remotes{"origin": a}); err != nil {
		t.Fatal(err)
	}
	st := storage.NewFileStore(root)
	project := &configCoordinationProjection{store: st, entered: make(chan struct{}), release: make(chan struct{})}
	svc := NewSaveSessionService(remotecfg.Wrap(root, gitctx.NewGitContextAdapter()), map[domain.ProviderKind]outbound.CaptureSource{domain.ProviderClaude: nil}, map[domain.ProviderKind]outbound.ProviderCodec{domain.ProviderClaude: nil}, st, project, nil)
	type result struct {
		out inbound.SaveOutput
		err error
	}
	done := make(chan result, 1)
	go func() {
		o, e := svc.Save(context.Background(), inbound.SaveInput{Cwd: root, SessionPath: "test-owned", Pending: true})
		done <- result{o, e}
	}()
	<-project.entered
	f, err := os.OpenFile(filepath.Join(root, ".cxt", "locks", "first-tracking", "repo.flock"), os.O_RDWR, 0600)
	if err != nil {
		close(project.release)
		<-done
		t.Fatal(err)
	}
	probe := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
	if probe == nil {
		_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
	}
	_ = f.Close()
	// No deadline/poll race: the capture actor is waiting on our channel.
	expected, err := remotecfg.Observe(context.Background(), root)
	if err != nil {
		close(project.release)
		<-done
		t.Fatal(err)
	}
	waiting, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	writeErr := remotecfg.Replace(waiting, expected, remotecfg.Remotes{"origin": b})
	close(project.release)
	got := <-done
	if probe != syscall.EWOULDBLOCK && probe != syscall.EAGAIN {
		t.Fatalf("capture SH was not held: %v", probe)
	}
	if !errors.Is(writeErr, context.DeadlineExceeded) || got.err != nil {
		t.Fatalf("schedule failed before proof: writer=%v capture=%v", writeErr, got.err)
	}
	snap, err := st.GetSnapshot(context.Background(), got.out.SnapshotID)
	if err != nil {
		t.Fatal(err)
	}
	pending, err := st.ListPendings(context.Background(), remotecfg.RepoIDFor(a))
	if err != nil || len(pending) != 1 {
		t.Fatalf("missing durable pending: %v %v", pending, err)
	}
	origin, _ := remotecfg.Origin(root)
	if snap.RepoID != remotecfg.RepoIDFor(origin) {
		t.Fatalf("capture no longer matches stable config")
	}
	if err := remotecfg.Replace(context.Background(), expected, remotecfg.Remotes{"origin": b}); err != nil {
		t.Fatalf("gate not released after capture: %v", err)
	}

}
