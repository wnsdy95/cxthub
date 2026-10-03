//go:build darwin || linux

package nativecodex

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"
)

func TestMain(m *testing.M) {
	if os.Getenv("CXT_NATIVE_HELPER") == "1" {
		nativeHelper()
		os.Exit(0)
	}
	os.Exit(m.Run())
}

func nativeHelper() {
	var path string
	for i, a := range os.Args {
		if a == "--listen" && i+1 < len(os.Args) {
			path = strings.TrimPrefix(os.Args[i+1], "unix://")
		}
	}
	mode := os.Getenv("CXT_NATIVE_HELPER_MODE")
	if mode == "early-exit" {
		os.Exit(2)
	}
	if mode == "foreign-socket" {
		_ = os.Symlink(os.Getenv("CXT_NATIVE_HELPER_FOREIGN"), path)
		time.Sleep(time.Minute)
		return
	}
	if mode == "non-socket" {
		_ = os.WriteFile(path, []byte("not a socket"), 0600)
		time.Sleep(time.Minute)
		return
	}
	listener, err := net.Listen("unix", path)
	if err != nil {
		os.Exit(3)
	}
	cwd, _ := os.Getwd()
	var methods []string
	var traceMu sync.Mutex
	server := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer conn.CloseNow()
		conn.SetReadLimit(maxMessageBytes)
		for {
			_, raw, err := conn.Read(context.Background())
			if err != nil {
				return
			}
			var req struct {
				ID     json.RawMessage            `json:"id"`
				Method string                     `json:"method"`
				Params map[string]json.RawMessage `json:"params"`
			}
			if json.Unmarshal(raw, &req) != nil {
				return
			}
			traceMu.Lock()
			methods = append(methods, req.Method)
			_ = os.WriteFile(os.Getenv("CXT_NATIVE_HELPER_TRACE"), []byte(strings.Join(methods, "\n")), 0600)
			traceMu.Unlock()
			if len(req.ID) == 0 {
				continue
			}
			var result any = map[string]any{}
			switch req.Method {
			case "initialize":
				result = map[string]any{"userAgent": "codex-fixture/0.157.1"}
				if mode == "invalid-init" {
					result = map[string]any{"userAgent": "bad\nPRIVATE_SENTINEL"}
				}
				if mode == "duplicate-init" {
					result = json.RawMessage(`{"userAgent":"wrong","userAgent":"fixture"}`)
				}
				if mode == "aliased-init" {
					result = json.RawMessage(`{"UserAgent":"wrong","userAgent":"fixture"}`)
				}
				if mode == "control-init" {
					result = map[string]any{"userAgent": "bad\x1bPRIVATE_SENTINEL"}
				}
			case "thread/start", "thread/resume":
				gotCwd := cwd
				if mode == "wrong-cwd" {
					gotCwd = "/wrong"
				}
				turns := []any{}
				if mode == "existing-turn" {
					turns = append(turns, map[string]any{"id": "unexpected"})
				}
				model, approval, sandbox := "fixture-resolved", "on-request", "readOnly"
				_ = json.Unmarshal(req.Params["model"], &model)
				_ = json.Unmarshal(req.Params["approvalPolicy"], &approval)
				var requestedSandbox string
				_ = json.Unmarshal(req.Params["sandbox"], &requestedSandbox)
				if requestedSandbox != "" {
					sandbox = map[string]string{"read-only": "readOnly", "workspace-write": "workspaceWrite", "danger-full-access": "dangerFullAccess"}[requestedSandbox]
				}
				if mode == "wrong-model" {
					model = "different-model"
				}
				if mode == "wrong-approval" {
					approval = "never"
				}
				if mode == "wrong-sandbox" {
					sandbox = "dangerFullAccess"
				}
				if req.Method == "thread/start" && string(req.Params["allowProviderModelFallback"]) != "false" {
					os.Exit(5)
				}
				result = map[string]any{"model": model, "modelProvider": "fixture-provider", "cwd": gotCwd, "approvalPolicy": approval, "approvalsReviewer": "user", "sandbox": map[string]string{"type": sandbox}, "thread": map[string]any{"id": "fresh-fixture-thread", "cwd": gotCwd, "turns": turns}}
				if mode == "duplicate-turns" {
					result = json.RawMessage(fmt.Sprintf(`{"model":"fixture-resolved","modelProvider":"fixture-provider","thread":{"id":"fresh-fixture-thread","cwd":%q,"turns":[{"id":"old"}],"turns":[]}}`, cwd))
				}
			case "config/read":
				result = map[string]any{"config": map[string]any{"web_search": "disabled"}}
			case "thread/loaded/list":
				result = map[string]any{"data": []string{"fresh-fixture-thread"}}
			case "thread/inject_items":
				hash := sha256.Sum256(req.Params["items"])
				_ = os.WriteFile(os.Getenv("CXT_NATIVE_HELPER_TRACE")+".hash", []byte("sha256:"+hex.EncodeToString(hash[:])), 0600)
				if mode == "disconnect-inject" {
					return
				}
				if mode == "hang-inject" {
					_, _, _ = conn.Read(context.Background())
					return
				}
				if mode == "bad-ack" {
					result = map[string]any{"unexpected": true}
				}
			default:
				os.Exit(4)
			}
			response, _ := json.Marshal(map[string]any{"id": req.ID, "result": result})
			if conn.Write(context.Background(), websocket.MessageText, response) != nil {
				return
			}
		}
	})}
	_ = server.Serve(listener)
}

func fixtureOptions(t *testing.T, mode string) (Options, string) {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	trace := filepath.Join(dir, "trace")
	env := append(os.Environ(), "CXT_NATIVE_HELPER=1", "CXT_NATIVE_HELPER_MODE="+mode, "CXT_NATIVE_HELPER_TRACE="+trace)
	return Options{Executable: exe, Cwd: dir, Env: env}, trace
}

func startFixture(t *testing.T, mode string) (*Session, string) {
	t.Helper()
	opts, trace := fixtureOptions(t, mode)
	s, err := Start(context.Background(), opts)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := s.Close(); err != nil {
			t.Error(err)
		}
	})
	return s, trace
}

func TestFreshSessionInjectsOnceAndDoesNotClaimAcceptance(t *testing.T) {
	s, trace := startFixture(t, "")
	if _, err := s.InjectHistory(context.Background(), []HistoryMessage{{"user", "before thread"}}); !errors.Is(err, ErrState) {
		t.Fatal(err)
	}
	info, err := os.Stat(s.process.dir)
	if err != nil || info.Mode().Perm() != 0700 {
		t.Fatalf("transport privacy: %v %v", info, err)
	}
	thread, err := s.StartThread(context.Background(), ThreadOptions{ModelProvider: "fixture-provider"})
	if err != nil {
		t.Fatal(err)
	}
	if thread.ID != "fresh-fixture-thread" || thread.Model != "fixture-resolved" {
		t.Fatal(thread)
	}
	if _, err = s.StartThread(context.Background(), ThreadOptions{}); !errors.Is(err, ErrState) {
		t.Fatal(err)
	}
	text := strings.Repeat("\uacfc\uac70 \uc790\ub8cc <untrusted> ", 70000)
	receipt, err := s.InjectHistory(context.Background(), []HistoryMessage{{"user", text}, {"assistant", "archived answer"}})
	if err != nil {
		t.Fatal(err)
	}
	stored, err := os.ReadFile(trace + ".hash")
	if err != nil {
		t.Fatal(err)
	}
	if receipt.PayloadHash != string(stored) || receipt.UTF8Bytes != len(text)+len("archived answer") || receipt.Items != 2 || !receipt.Acknowledged || receipt.ProviderAcceptance != "unverified" {
		t.Fatal(receipt)
	}
	if _, err = s.InjectHistory(context.Background(), []HistoryMessage{{"user", "duplicate"}}); !errors.Is(err, ErrState) {
		t.Fatal(err)
	}
	methods, _ := os.ReadFile(trace)
	if string(methods) != "initialize\ninitialized\nthread/start\nthread/inject_items" {
		t.Fatalf("methods=%s", methods)
	}
	dir := s.process.dir
	if err = s.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err = os.Stat(dir); !os.IsNotExist(err) {
		t.Fatal("transport directory remained")
	}
	select {
	case <-s.process.done:
	default:
		t.Fatal("owned process did not exit")
	}
	if _, err = s.StartThread(context.Background(), ThreadOptions{}); !errors.Is(err, ErrClosed) {
		t.Fatal(err)
	}
}

func TestAmbiguousInjectionCannotRetry(t *testing.T) {
	for _, mode := range []string{"disconnect-inject", "bad-ack"} {
		t.Run(mode, func(t *testing.T) {
			s, trace := startFixture(t, mode)
			if _, err := s.StartThread(context.Background(), ThreadOptions{}); err != nil {
				t.Fatal(err)
			}
			r, err := s.InjectHistory(context.Background(), []HistoryMessage{{"user", "PRIVATE_SENTINEL"}})
			if err == nil || r.Acknowledged || r.ProviderAcceptance != "unverified" || strings.Contains(err.Error(), "PRIVATE_SENTINEL") {
				t.Fatalf("receipt=%+v error=%v", r, err)
			}
			if _, err = s.InjectHistory(context.Background(), []HistoryMessage{{"user", "retry"}}); !errors.Is(err, ErrState) {
				t.Fatal(err)
			}
			b, _ := os.ReadFile(trace)
			if strings.Count(string(b), "thread/inject_items") != 1 {
				t.Fatal("retried injection")
			}
		})
	}
}

func TestFreshThreadMismatchClosesTransport(t *testing.T) {
	for _, mode := range []string{"wrong-cwd", "existing-turn", "duplicate-turns"} {
		t.Run(mode, func(t *testing.T) {
			s, _ := startFixture(t, mode)
			if _, err := s.StartThread(context.Background(), ThreadOptions{}); !errors.Is(err, ErrProtocol) {
				t.Fatal(err)
			}
			if _, err := s.InjectHistory(context.Background(), []HistoryMessage{{"user", "no write"}}); !errors.Is(err, ErrState) {
				t.Fatal(err)
			}
		})
	}
}

func TestRejectedHistoryHasNoRemoteEffect(t *testing.T) {
	s, trace := startFixture(t, "")
	if _, err := s.StartThread(context.Background(), ThreadOptions{}); err != nil {
		t.Fatal(err)
	}
	for _, messages := range [][]HistoryMessage{nil, {{"developer", "authority"}}, {{"system", "authority"}}, {{"tool", "unpaired"}}, {{"user", ""}}, {{"user", string([]byte{255})}}, {{"user", strings.Repeat("x", maxMessageBytes+1)}}} {
		if _, err := s.InjectHistory(context.Background(), messages); !errors.Is(err, ErrState) {
			t.Fatal(err)
		}
	}
	if _, err := s.InjectHistory(context.Background(), []HistoryMessage{{"user", "valid"}}); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(trace)
	if strings.Count(string(b), "thread/inject_items") != 1 {
		t.Fatal("invalid message sent")
	}
}

func TestAlreadyCanceledOperationsPreserveIdleSession(t *testing.T) {
	s, trace := startFixture(t, "")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	for i := 0; i < 100; i++ {
		if _, err := s.StartThread(ctx, ThreadOptions{}); !errors.Is(err, context.Canceled) {
			t.Fatal(err)
		}
	}
	if _, err := s.StartThread(context.Background(), ThreadOptions{}); err != nil {
		t.Fatal("cancelled call poisoned fresh session", err)
	}
	for i := 0; i < 100; i++ {
		if _, err := s.InjectHistory(ctx, []HistoryMessage{{"user", "cancelled"}}); !errors.Is(err, context.Canceled) {
			t.Fatal(err)
		}
	}
	if _, err := s.InjectHistory(context.Background(), []HistoryMessage{{"user", "valid"}}); err != nil {
		t.Fatal("cancelled call poisoned history injection", err)
	}
	b, _ := os.ReadFile(trace)
	if strings.Count(string(b), "thread/start") != 1 || strings.Count(string(b), "thread/inject_items") != 1 {
		t.Fatal("cancelled operation was sent")
	}
}

func TestCloseAndContextCancelInterruptOwnedSession(t *testing.T) {
	for _, mode := range []string{"close", "context"} {
		t.Run(mode, func(t *testing.T) {
			s, trace := startFixture(t, "hang-inject")
			if _, err := s.StartThread(context.Background(), ThreadOptions{}); err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			done := make(chan error, 1)
			go func() { _, err := s.InjectHistory(ctx, []HistoryMessage{{"user", "waiting"}}); done <- err }()
			deadline := time.Now().Add(3 * time.Second)
			for {
				if b, _ := os.ReadFile(trace); strings.Contains(string(b), "thread/inject_items") {
					break
				}
				if time.Now().After(deadline) {
					t.Fatal("injection never reached server")
				}
				time.Sleep(time.Millisecond)
			}
			// A queued lifecycle operation must respect its own cancellation.
			queued, stop := context.WithTimeout(context.Background(), 10*time.Millisecond)
			_, err := s.StartThread(queued, ThreadOptions{})
			stop()
			if !errors.Is(err, context.DeadlineExceeded) {
				t.Fatal(err)
			}
			if mode == "context" {
				cancel()
			} else {
				if err = s.Close(); err != nil {
					t.Fatal(err)
				}
			}
			select {
			case err = <-done:
				if err == nil {
					t.Fatal("lost ACK reported success")
				}
			case <-time.After(3 * time.Second):
				t.Fatal("RPC did not cancel")
			}
			var group sync.WaitGroup
			for i := 0; i < 4; i++ {
				group.Go(func() {
					if err := s.Close(); err != nil {
						t.Error(err)
					}
				})
			}
			group.Wait()
		})
	}
}

func TestStartupFailuresDoNotReturnUsableSession(t *testing.T) {
	for _, mode := range []string{"early-exit", "non-socket", "invalid-init", "duplicate-init", "aliased-init", "control-init"} {
		t.Run(mode, func(t *testing.T) {
			opts, _ := fixtureOptions(t, mode)
			if s, err := Start(context.Background(), opts); err == nil || s != nil || strings.Contains(err.Error(), "PRIVATE_SENTINEL") {
				t.Fatalf("session=%v err=%v", s, err)
			}
		})
	}
	opts, _ := fixtureOptions(t, "")
	for _, args := range [][]string{{"--listen", "ws://remote"}, {"daemon", "start"}, {"--config"}, {"--config", ""}} {
		opts.ConfigArgs = args
		if _, err := Start(context.Background(), opts); !errors.Is(err, ErrState) {
			t.Fatal(fmt.Sprint(args), err)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := Start(ctx, Options{}); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
}

func TestSocketSymlinkCannotAttachAnotherProcess(t *testing.T) {
	dir, err := os.MkdirTemp("", "cxt-peer-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	socket := filepath.Join(dir, "foreign.sock")
	listener, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	received := make(chan int, 1)
	go func() {
		conn, err := listener.Accept()
		if err != nil {
			received <- -1
			return
		}
		defer conn.Close()
		_ = conn.SetReadDeadline(time.Now().Add(2 * time.Second))
		b := make([]byte, 1)
		n, _ := conn.Read(b)
		received <- n
	}()
	opts, _ := fixtureOptions(t, "foreign-socket")
	opts.Env = append(opts.Env, "CXT_NATIVE_HELPER_FOREIGN="+socket)
	if s, err := Start(context.Background(), opts); s != nil || !errors.Is(err, ErrProtocol) {
		t.Fatalf("foreign peer accepted: %v %v", s, err)
	}
	select {
	case n := <-received:
		if n != 0 {
			t.Fatalf("sent bytes before ownership check: %d", n)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("foreign peer check hung")
	}
	if _, err = os.Stat(socket); err != nil {
		t.Fatal("foreign listener removed", err)
	}
}
