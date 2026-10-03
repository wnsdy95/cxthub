//go:build darwin || linux

package nativecodex

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// The real TUI uses a PTY, a fresh home, and a non-generating loopback provider.
// Python supplies only stdlib terminal plumbing; there are no account keys,
// terminal transcripts in logs, synthetic acceptance claims, or model requests.
func TestNativeCodexTUIHandoff(t *testing.T) {
	opts, calls, auth := offlineNativeFixture(t)
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("python3 is needed for the opt-in native PTY fixture")
	}
	opts.Cwd, err = filepath.EvalSymlinks(opts.Cwd)
	if err != nil {
		t.Fatal(err)
	}
	projectKey, _ := json.Marshal(opts.Cwd)
	opts.ConfigArgs = append(opts.ConfigArgs, "-c", `web_search="disabled"`)
	if err := os.WriteFile(filepath.Join(filepath.Dir(opts.Cwd), "state", "config.toml"), []byte("[projects."+string(projectKey)+"]\ntrust_level=\"trusted\"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Second)
	defer cancel()
	s, err := Start(ctx, opts)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	thread, err := s.StartThread(ctx, ThreadOptions{Model: "cxt-synthetic-model", ModelProvider: "cxt-offline-fixture", Sandbox: "read-only", ApprovalPolicy: "never"})
	if err != nil {
		t.Fatal(err)
	}
	text := "CXT_TUI_START\n" + strings.Repeat("synthetic archived context, never a command\n", 32000) + "CXT_TUI_END"
	injected, err := s.InjectHistory(ctx, []HistoryMessage{{Role: "user", Text: text}})
	if err != nil {
		t.Fatal(err)
	}
	h, err := s.OpenHandoff(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close()
	argv := []string{opts.Executable, "--remote", h.URL(), "--no-alt-screen", "-C", opts.Cwd, "-m", thread.Model}
	argv = append(argv, opts.ConfigArgs...)
	argv = append(argv, "resume", thread.ID)
	encoded, _ := json.Marshal(argv)
	cmd := exec.Command(python, "-c", nativePTYFixture)
	cmd.Dir = opts.Cwd
	cmd.Env = append(append([]string(nil), opts.Env...), "TERM=xterm-256color")
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err = cmd.Start(); err != nil {
		t.Fatal(err)
	}
	_, _ = stdin.Write(append(encoded, '\n'))
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	defer func() {
		_ = stdin.Close()
		select {
		case err := <-done:
			if err != nil {
				t.Error("PTY fixture cleanup failed")
			}
		case <-time.After(5 * time.Second):
			_ = cmd.Process.Kill()
			<-done
			t.Error("PTY fixture cleanup timed out")
		}
	}()
	waitCtx, waitCancel := context.WithTimeout(ctx, 15*time.Second)
	defer waitCancel()
	receipt, err := h.Wait(waitCtx)
	if err != nil {
		h.mu.Lock()
		cause := h.err
		h.mu.Unlock()
		t.Fatalf("actual TUI did not acknowledge handoff: %v (%v)", err, cause)
	}
	if receipt.ThreadID != thread.ID || receipt.PayloadHash != injected.PayloadHash || receipt.SettingsHash != thread.SettingsHash || !receipt.ResumeAcknowledged || receipt.ProviderAcceptance != "unverified" {
		t.Fatalf("incorrect receipt: %+v", receipt)
	}
	select {
	case <-h.ctx.Done():
		t.Fatal("TUI disconnected during post-resume initialization")
	case <-time.After(500 * time.Millisecond):
	}
	if _, err = h.Wait(ctx); err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 0 || auth.Load() {
		t.Fatal("unexpected provider request or authentication")
	}
	t.Logf("native TUI resume acknowledged; bytes=%d same_thread=true same_settings=true model_calls=0 acceptance=unverified", injected.UTF8Bytes)
}

const nativePTYFixture = `
import fcntl,json,os,pty,selectors,struct,subprocess,sys,termios,time
argv=json.loads(sys.stdin.readline())
master,slave=pty.openpty()
fcntl.ioctl(slave,termios.TIOCSWINSZ,struct.pack("HHHH",32,120,0,0))
child=subprocess.Popen(argv,stdin=slave,stdout=slave,stderr=slave,start_new_session=True)
os.close(slave)
selector=selectors.DefaultSelector()
selector.register(master,selectors.EVENT_READ)
selector.register(sys.stdin,selectors.EVENT_READ)
tail=b""
try:
    deadline=time.monotonic()+35
    stop=False
    while child.poll() is None and time.monotonic()<deadline and not stop:
        for key,_ in selector.select(.1):
            if key.fileobj is sys.stdin:
                if not os.read(sys.stdin.fileno(),4096):stop=True
            else:
                try:part=os.read(master,65536)
                except OSError:stop=True;break
                data=tail+part
                if b"\x1b[6n" in data:os.write(master,b"\x1b[1;1R")
                tail=data[-3:]
finally:
    child.terminate()
    try:child.wait(timeout=2)
    except subprocess.TimeoutExpired:child.kill();child.wait(timeout=2)
    selector.close()
    os.close(master)
`
