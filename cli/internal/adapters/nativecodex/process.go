package nativecodex

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/coder/websocket"
)

// Options describes an owned local app-server. ConfigArgs must contain only
// native config/feature overrides; an arbitrary listener or command is rejected.
// The wrapper must still preserve original launch arguments before using this
// adapter in production. No runtime capacity evidence is manufactured here.
type Options struct {
	Executable string
	Cwd        string
	Env        []string
	ConfigArgs []string
}

type ownedProcess struct {
	cmd   *exec.Cmd
	done  chan struct{}
	dir   string
	state *ownedProcessState // shared even when Session copies ownedProcess
}

const startupTimeout = 15 * time.Second

func Start(ctx context.Context, opts Options) (*Session, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if !localSocketSupported() {
		return nil, fmt.Errorf("%w: Unix sockets unavailable", ErrProtocol)
	}
	if !validConfigArgs(opts.ConfigArgs) || opts.Executable == "" || !filepath.IsAbs(opts.Cwd) {
		return nil, ErrState
	}
	cwd, err := filepath.EvalSymlinks(opts.Cwd)
	if err != nil {
		return nil, fmt.Errorf("%w: working directory unavailable", ErrState)
	}
	info, err := os.Stat(cwd)
	if err != nil || !info.IsDir() {
		return nil, fmt.Errorf("%w: working directory unavailable", ErrState)
	}
	dir, err := os.MkdirTemp("", "cxt-native-")
	if err != nil {
		return nil, fmt.Errorf("%w: private transport directory unavailable", ErrState)
	}
	socketPath := filepath.Join(dir, "rpc.sock")
	args := []string{"app-server", "--listen", "unix://" + socketPath}
	args = append(args, opts.ConfigArgs...)
	cmd := exec.CommandContext(ctx, opts.Executable, args...)
	cmd.Dir = cwd
	if opts.Env != nil {
		cmd.Env = append([]string{}, opts.Env...)
	}
	p, err := startOwnedProcess(cmd, dir)
	if err != nil {
		_ = os.RemoveAll(dir)
		return nil, fmt.Errorf("%w: app-server process could not start", ErrProtocol)
	}
	ok := false
	defer func() {
		if !ok {
			_ = p.stop()
		}
	}()
	startup, cancel := context.WithTimeout(ctx, startupTimeout)
	defer cancel()
	if err = waitSocket(startup, socketPath, p.done); err != nil {
		return nil, err
	}
	transport := &http.Transport{
		Proxy:                  nil,
		MaxResponseHeaderBytes: 8192,
		DialContext: func(dialCtx context.Context, _, _ string) (net.Conn, error) {
			var dialer net.Dialer
			conn, dialErr := dialer.DialContext(dialCtx, "unix", socketPath)
			if dialErr != nil {
				return nil, dialErr
			}
			// Codex publishes a symlink to its own private socket. Verify the
			// kernel peer identity, not the name or permissions of that target.
			if !p.ownsPeer(conn) {
				_ = conn.Close()
				return nil, fmt.Errorf("%w: listener is not the owned app-server", ErrProtocol)
			}
			return conn, nil
		},
	}
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	conn, response, err := websocket.Dial(startup, "ws://localhost/rpc", &websocket.DialOptions{HTTPClient: client, CompressionMode: websocket.CompressionDisabled})
	if err != nil {
		if response != nil && response.Body != nil {
			_ = response.Body.Close()
		}
		if startup.Err() != nil {
			return nil, startup.Err()
		}
		return nil, fmt.Errorf("%w: local WebSocket handshake failed", ErrProtocol)
	}
	rpc := newRPC(conn)
	defer func() {
		if !ok {
			_ = rpc.close()
		}
	}()
	raw, err := rpc.call(startup, "initialize", map[string]any{"clientInfo": map[string]string{"name": "cxthub_native_transport", "version": "1"}, "capabilities": map[string]any{"experimentalApi": true}})
	if err != nil {
		return nil, err
	}
	initialized, valid := identityObject(raw, "userAgent")
	var host string
	if !valid || json.Unmarshal(initialized["userAgent"], &host) != nil || host == "" || len(host) > 512 || strings.IndexFunc(host, func(r rune) bool { return r < 32 || r == 127 }) >= 0 {
		return nil, fmt.Errorf("%w: invalid host initialization", ErrProtocol)
	}
	if err = rpc.notify(startup, "initialized", struct{}{}); err != nil {
		return nil, err
	}
	s := &Session{rpc: rpc, process: p, cwd: cwd, host: host, closed: make(chan struct{}), gate: make(chan struct{}, 1)}
	ok = true
	go func() {
		select {
		case <-ctx.Done():
			_ = s.Close()
		case <-s.closed:
		}
	}()
	return s, nil
}

func validConfigArgs(args []string) bool {
	if len(args)%2 != 0 || len(args) > 256 {
		return false
	}
	for i := 0; i < len(args); i += 2 {
		if args[i] != "-c" && args[i] != "--config" && args[i] != "--enable" && args[i] != "--disable" {
			return false
		}
		if args[i+1] == "" || len(args[i+1]) > 64<<10 || strings.HasPrefix(args[i+1], "-") || strings.ContainsRune(args[i+1], 0) {
			return false
		}
	}
	return true
}

func waitSocket(ctx context.Context, path string, done <-chan struct{}) error {
	ticker := time.NewTicker(20 * time.Millisecond)
	defer ticker.Stop()
	for {
		if info, err := os.Stat(path); err == nil {
			if info.Mode()&os.ModeSocket == 0 {
				return fmt.Errorf("%w: listener is not a socket (mode %s)", ErrProtocol, info.Mode().Type())
			}
			return nil
		} else if !os.IsNotExist(err) {
			return fmt.Errorf("%w: listener unavailable", ErrProtocol)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-done:
			return fmt.Errorf("%w: app-server exited before initialization", ErrProtocol)
		case <-ticker.C:
		}
	}
}

func (p ownedProcess) stop() error {
	select {
	case <-p.done:
	default:
		_ = p.signal(false)
		select {
		case <-p.done:
		case <-time.After(2 * time.Second):
			_ = p.signal(true)
			select {
			case <-p.done:
			case <-time.After(2 * time.Second):
				return fmt.Errorf("%w: owned process cleanup incomplete", ErrProtocol)
			}
		}
	}
	if err := os.RemoveAll(p.dir); err != nil {
		return fmt.Errorf("%w: private transport cleanup incomplete", ErrProtocol)
	}
	return p.state.observationErr // published before done is closed
}
