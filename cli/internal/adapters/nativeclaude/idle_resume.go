package nativeclaude

import (
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// ErrIdleExit is deliberately independent of native diagnostics, which may
// contain account details, paths or transcript content.
var ErrIdleExit = errors.New("native Claude idle resume exited unsuccessfully")

type idlePathEntry struct {
	path string
	info os.FileInfo
}

type idleLaunch struct {
	executable, cwd, model string
	env, configArgs        []string
	executablePath         []idlePathEntry
	cwdPath                []idlePathEntry
	authority              *idleResumeAuthority
}

type idleResumeAuthority struct {
	mu       sync.Mutex
	prepared bool
}

func freezeIdleLaunch(opts Options, env []string, cwd string) (idleLaunch, error) {
	launch := idleLaunch{executable: opts.Executable, cwd: cwd, model: opts.Model,
		env: append([]string{}, env...), configArgs: append([]string{}, opts.ConfigArgs...), authority: &idleResumeAuthority{}}
	var err error
	launch.executablePath, err = inspectIdlePath(launch.executable, false)
	if err != nil {
		return idleLaunch{}, err
	}
	launch.cwdPath, err = inspectIdlePath(cwd, true)
	if err != nil {
		return idleLaunch{}, err
	}
	return launch, nil
}

// Path identities are checked without following symlinks, including parent
// directories. Directory contents may legitimately change during native use;
// replacing a directory or changing a bound regular file is not permitted.
func inspectIdlePath(path string, directory bool) ([]idlePathEntry, error) {
	if !filepath.IsAbs(path) || filepath.Clean(path) != path {
		return nil, ErrState
	}
	var reverse []string
	for current := path; ; current = filepath.Dir(current) {
		reverse = append(reverse, current)
		if filepath.Dir(current) == current {
			break
		}
	}
	entries := make([]idlePathEntry, 0, len(reverse))
	for i := len(reverse) - 1; i >= 0; i-- {
		info, err := os.Lstat(reverse[i])
		if err != nil || info.Mode()&os.ModeSymlink != 0 || (i > 0 || directory) && !info.IsDir() || i == 0 && !directory && !info.Mode().IsRegular() {
			return nil, ErrState
		}
		entries = append(entries, idlePathEntry{path: reverse[i], info: info})
	}
	return entries, nil
}

func sameIdleFile(before, after os.FileInfo) bool {
	return before != nil && after != nil && os.SameFile(before, after) && before.Mode() == after.Mode() &&
		(before.IsDir() || before.Size() == after.Size() && before.ModTime().Equal(after.ModTime()))
}

func validateIdlePath(entries []idlePathEntry) error {
	if len(entries) == 0 {
		return ErrState
	}
	for _, entry := range entries {
		info, err := os.Lstat(entry.path)
		if err != nil || info.Mode()&os.ModeSymlink != 0 || !sameIdleFile(entry.info, info) {
			return ErrState
		}
	}
	return nil
}

func (l idleLaunch) validate() error {
	if !validArgs(l.configArgs) || l.executable == "" || l.cwd == "" {
		return ErrState
	}
	if err := validateIdlePath(l.executablePath); err != nil {
		return err
	}
	return validateIdlePath(l.cwdPath)
}

func (l idleLaunch) resumeArgs(archivePath string) ([]string, error) {
	if !validArgs(l.configArgs) || strings.HasPrefix(l.model, "-") || strings.ContainsRune(l.model, 0) || len(l.model) > 256 || !validIdleArchivePath(archivePath) {
		return nil, ErrState
	}
	// A separated option-looking value can be interpreted by native as another
	// flag. Do not silently rewrite it to the equals form during handoff.
	flags := map[string]bool{"--bare": true, "--safe-mode": true, "--strict-mcp-config": true, "--no-chrome": true, "--disable-slash-commands": true}
	for i := 0; i < len(l.configArgs); i++ {
		name, _, equal := strings.Cut(l.configArgs[i], "=")
		if equal || flags[name] {
			continue
		}
		i++ // validArgs already established a paired value.
		if strings.HasPrefix(l.configArgs[i], "-") {
			return nil, ErrState
		}
	}
	args := append([]string{}, l.configArgs...)
	if l.model != "" {
		args = append(args, "--model", l.model)
	}
	return append(args, "--resume", archivePath), nil
}

func validIdleArchivePath(path string) bool {
	if !filepath.IsAbs(path) || filepath.Clean(path) != path || strings.ContainsRune(path, 0) || filepath.Ext(path) != ".jsonl" {
		return false
	}
	id := strings.TrimSuffix(filepath.Base(path), ".jsonl")
	if len(id) != 36 {
		return false
	}
	for i, ch := range id {
		if i == 8 || i == 13 || i == 18 || i == 23 {
			if ch != '-' {
				return false
			}
		} else if !(ch >= '0' && ch <= '9' || ch >= 'a' && ch <= 'f') {
			return false
		}
	}
	return true
}

type idleContextReader struct {
	ctx context.Context
	r   io.Reader
}

func (r idleContextReader) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.r.Read(p)
}

// IdleResumePlan is an opaque, one-shot continuation of a retired no-turn
// session. It is not a public delivery route or evidence of native readiness.
// Neither its launch state nor its archive identity can be changed by callers.
type IdleResumePlan struct {
	*idleResumeState
}

// A shared private state preserves one-shot ownership even if callers copy the
// exported opaque value. Copying a mutex/attempted bit would create a retry.
type idleResumeState struct {
	mu          sync.Mutex
	attempted   bool
	source      *Session
	launch      idleLaunch
	archive     archiveVerification
	archivePath []idlePathEntry
}

// PrepareIdleResume requires successful Close and a prior successful
// VerifyArchive for ownedPath. It freshly verifies the same raw archive,
// including metadata rows. One Session can issue at most one plan, even when
// preparation or the later launch fails. Archives are never rewritten here.
func (s *Session) PrepareIdleResume(ctx context.Context, ownedPath string) (*IdleResumePlan, error) {
	ctx, cancel := context.WithTimeout(ctx, operationTimeout)
	defer cancel()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	select {
	case <-s.closed:
	default:
		return nil, ErrState
	}
	s.mu.Lock()
	if s.closeErr != nil || s.verifiedArchive == nil || s.verifiedArchive.path != ownedPath || s.version != "2.1.285" {
		s.mu.Unlock()
		return nil, ErrState
	}
	previous := *s.verifiedArchive
	launch := s.launch
	s.mu.Unlock()
	if launch.authority == nil {
		return nil, ErrState
	}
	launch.authority.mu.Lock()
	issued := launch.authority.prepared
	launch.authority.prepared = true
	launch.authority.mu.Unlock()
	if issued {
		return nil, ErrState
	}
	if err := launch.validate(); err != nil {
		return nil, err
	}
	if _, err := launch.resumeArgs(ownedPath); err != nil {
		return nil, err
	}
	path, err := inspectIdlePath(ownedPath, false)
	if err != nil {
		return nil, err
	}
	verified, err := s.verifyArchive(ctx, ownedPath)
	if err != nil {
		return nil, err
	}
	if verified.digest != previous.digest || !sameIdleFile(previous.info, verified.info) || !sameIdleFile(path[len(path)-1].info, verified.info) {
		return nil, ErrState
	}
	if err := validateIdlePath(path); err != nil {
		return nil, err
	}
	return &IdleResumePlan{idleResumeState: &idleResumeState{source: s, launch: launch, archive: verified, archivePath: path}}, nil
}

// Start launches only the original allowed settings and --resume with the exact
// owned absolute <UUID>.jsonl pathname, avoiding native session search. It has no
// prompt or helper protocol flags. Success means process creation only, never
// composer readiness or provider acceptance. The supplied files remain owned
// by the caller. Setpgid cleanup is suitable for an isolated PTY; this API does
// not implement foreground job control for the user's controlling terminal.
// The first attempt consumes the plan, including cancellation or launch error.
func (p *IdleResumePlan) Start(ctx context.Context, stdin, stdout, stderr *os.File) (*IdleProcess, error) {
	if p == nil || p.idleResumeState == nil {
		return nil, ErrState
	}
	p.mu.Lock()
	if p.attempted {
		p.mu.Unlock()
		return nil, ErrState
	}
	p.attempted = true
	p.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if p.source == nil || stdin == nil || stdout == nil || stderr == nil {
		return nil, ErrState
	}
	for _, file := range []*os.File{stdin, stdout, stderr} {
		if _, err := file.Stat(); err != nil {
			return nil, ErrState
		}
	}
	if err := p.launch.validate(); err != nil {
		return nil, err
	}
	args, err := p.launch.resumeArgs(p.archive.path)
	if err != nil {
		return nil, err
	}
	observer, err := newProcessExitObserver()
	if err != nil {
		return nil, ErrState
	}
	started := false
	defer func() {
		if !started {
			observer.close()
		}
	}()
	cmd := exec.Command(p.launch.executable, args...)
	cmd.Dir, cmd.Env = p.launch.cwd, append([]string{}, p.launch.env...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = stdin, stdout, stderr
	configureProcess(cmd)
	// Keep the final bounded raw read immediately adjacent to process launch.
	// The native resume protocol accepts a pathname, not an already-open file;
	// this is a drift check, not an atomic filesystem-to-native transaction.
	verification, cancel := context.WithTimeout(ctx, operationTimeout)
	defer cancel()
	if err := validateIdlePath(p.archivePath); err != nil {
		return nil, err
	}
	archive, err := p.source.verifyArchive(verification, p.archive.path)
	if err != nil {
		return nil, err
	}
	if archive.digest != p.archive.digest || !sameIdleFile(p.archive.info, archive.info) {
		return nil, ErrState
	}
	if err := validateIdlePath(p.archivePath); err != nil {
		return nil, err
	}
	if err := p.launch.validate(); err != nil {
		return nil, err
	}
	if err := verification.Err(); err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, ErrState
	}
	started = true
	process := &IdleProcess{idleProcessState: &idleProcessState{cmd: cmd, exited: make(chan struct{}), done: make(chan struct{}), stop: make(chan struct{})}}
	go func() {
		process.observationErr = observer.wait(cmd.Process.Pid)
		observer.close()
		close(process.exited)
	}()
	go process.run(ctx)
	if err := ctx.Err(); err != nil {
		return nil, errors.Join(err, process.Close())
	}
	return process, nil
}

// IdleProcess owns signaling and reaping of one resumed process and its group.
// It does not parse the TUI, send input, close caller files, or start replacements.
type IdleProcess struct {
	*idleProcessState
}

type idleProcessState struct {
	cmd            *exec.Cmd
	exited, done   chan struct{}
	stop           chan struct{}
	stopOnce       sync.Once
	observationErr error
	err            error // published by closing done
}

func (p *IdleProcess) Done() <-chan struct{} { return p.done }

// Wait waits for final group cleanup and reaping. Cancelling this wait also
// retires the owned process; it never detaches an unobserved live child.
func (p *IdleProcess) Wait(ctx context.Context) error {
	select {
	case <-p.done:
		return p.err
	case <-ctx.Done():
		return errors.Join(ctx.Err(), p.Close())
	}
}

// Close requests termination once and joins bounded cleanup (at most 2.5s of
// cleanup waits). Intentional termination is not an unsuccessful native exit;
// inability to confirm observation or reaping returns ErrCleanup.
func (p *IdleProcess) Close() error {
	p.stopOnce.Do(func() { close(p.stop) })
	<-p.done
	return p.err
}

func (p *IdleProcess) run(ctx context.Context) {
	stopped := false
	select {
	case <-ctx.Done():
		p.err = ctx.Err()
		stopped = true
	case <-p.stop:
		stopped = true
	case <-p.exited:
	}
	select {
	case <-p.exited:
	default:
		_ = signalProcessGroup(p.cmd, false)
		if !waitFor(p.exited, 500*time.Millisecond) {
			_ = signalProcessGroup(p.cmd, true)
		}
	}
	observed := waitFor(p.exited, time.Second)
	// No Wait has run: the unreaped child still reserves its PID/group identity.
	// Kill any remaining descendants before releasing that reservation.
	_ = signalProcessGroup(p.cmd, true)
	reaped := make(chan struct{})
	var waitErr error
	go func() { waitErr = p.cmd.Wait(); close(reaped) }()
	if !waitFor(reaped, time.Second) {
		p.err = errors.Join(p.err, ErrCleanup)
	} else if waitErr != nil {
		var exit *exec.ExitError
		if !errors.As(waitErr, &exit) {
			p.err = errors.Join(p.err, ErrCleanup)
		} else if !stopped {
			p.err = errors.Join(p.err, ErrIdleExit)
		}
	}
	if !observed || p.observationErr != nil {
		p.err = errors.Join(p.err, ErrCleanup)
	}
	close(p.done)
}
