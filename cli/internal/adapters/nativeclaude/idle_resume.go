package nativeclaude

import (
	"context"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
)

type idlePathEntry struct {
	path string
	info os.FileInfo
}

type idleLaunch struct {
	executable, cwd, model string
	version                string
	env, configArgs        []string
	executablePath         []idlePathEntry
	cwdPath                []idlePathEntry
	authority              *idleResumeAuthority
}

type idleResumeAuthority struct {
	mu       sync.Mutex
	prepared bool
}

func freezeIdleLaunch(opts Options, env []string, cwd, version string) (idleLaunch, error) {
	launch := idleLaunch{executable: opts.Executable, cwd: cwd, model: opts.Model, version: version,
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
	if !validArgs(l.configArgs, l.version) || l.executable == "" || l.cwd == "" {
		return ErrState
	}
	if err := validateIdlePath(l.executablePath); err != nil {
		return err
	}
	return validateIdlePath(l.cwdPath)
}

func (l idleLaunch) resumeArgs(archivePath string) ([]string, error) {
	if !validArgs(l.configArgs, l.version) || strings.HasPrefix(l.model, "-") || strings.ContainsRune(l.model, 0) || len(l.model) > 256 || !validIdleArchivePath(archivePath) {
		return nil, ErrState
	}
	// A separated option-looking value can be interpreted by native as another
	// flag. Do not silently rewrite it to the equals form during handoff.
	for i := 0; i < len(l.configArgs); i++ {
		name, _, equal := strings.Cut(l.configArgs[i], "=")
		if equal || standaloneConfigFlag(name, l.version) {
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

// IdleResumePlan is an opaque, one-shot continuation of a retired session with
// a verified completed-exchange archive. StartSupervised transfers the TUI to
// the public delivery supervisor, which owns terminal signaling and reaping.
// Neither its launch state nor its archive identity can be changed by callers,
// and process creation is not evidence of native readiness.
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

// prepareIdleResume requires successful Close and a prior successful
// VerifyArchive for ownedPath. It freshly verifies the same raw archive,
// including metadata rows. One Session can issue at most one plan, even when
// preparation or the later launch fails. Archives are never rewritten here.
func (s *Session) prepareIdleResume(ctx context.Context, ownedPath string) (*IdleResumePlan, error) {
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
	if s.closeErr != nil || s.verifiedArchive == nil || s.verifiedArchive.path != ownedPath || s.verifiedArchive.exchange == nil || s.version != supportedVersion || s.launch.version != s.version {
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
	verified, err := s.verifyExchangeArchive(ctx, ownedPath)
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

// Claiming the supervised handoff plan consumes every attempt. The final archive
// read remains immediately before process creation in startResumeCommand.
func (p *IdleResumePlan) resumeCommand(ctx context.Context, stdin io.Reader, stdout, stderr io.Writer) (*exec.Cmd, error) {
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
	for _, stream := range []any{stdin, stdout, stderr} {
		if file, ok := stream.(*os.File); ok {
			if file == nil {
				return nil, ErrState
			}
			if _, err := file.Stat(); err != nil {
				return nil, ErrState
			}
		}
	}
	if err := p.launch.validate(); err != nil {
		return nil, err
	}
	args, err := p.launch.resumeArgs(p.archive.path)
	if err != nil {
		return nil, err
	}
	cmd := exec.Command(p.launch.executable, args...)
	cmd.Dir, cmd.Env = p.launch.cwd, append([]string{}, p.launch.env...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = stdin, stdout, stderr
	return cmd, nil
}

func (p *IdleResumePlan) startResumeCommand(ctx context.Context, cmd *exec.Cmd) error {
	// Keep the final bounded raw read immediately adjacent to process launch.
	// The native resume protocol accepts a pathname, not an already-open file;
	// this is a drift check, not an atomic filesystem-to-native transaction.
	verification, cancel := context.WithTimeout(ctx, operationTimeout)
	defer cancel()
	if err := validateIdlePath(p.archivePath); err != nil {
		return err
	}
	archive, err := p.source.verifyExchangeArchive(verification, p.archive.path)
	if err != nil {
		return err
	}
	if archive.digest != p.archive.digest || !sameIdleFile(p.archive.info, archive.info) {
		return ErrState
	}
	if err := validateIdlePath(p.archivePath); err != nil {
		return err
	}
	if err := p.launch.validate(); err != nil {
		return err
	}
	if err := verification.Err(); err != nil {
		return err
	}
	if err := cmd.Start(); err != nil {
		return ErrState
	}
	return nil
}
