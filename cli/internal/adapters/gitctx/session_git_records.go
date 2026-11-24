package gitctx

import (
	"bufio"
	"bytes"
	"context"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/wnsdy95/cxthub-cli/internal/adapters/providerfs"
	"github.com/wnsdy95/cxthub-cli/internal/domain"
)

// SessionGitRecords reads what Git recorded about a repository: its worktrees,
// the HEAD log of each one, and every commit object it still has. It runs only
// reading Git commands and reads HEAD log files; it changes nothing.
type SessionGitRecords struct{}

// NewSessionGitRecords creates the reader.
func NewSessionGitRecords() *SessionGitRecords { return &SessionGitRecords{} }

// gitRead runs a reading Git command. Optional locks are off so that a reading
// command never refreshes repository state on the side.
func gitRead(ctx context.Context, cwd string, stdin []byte, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, "git", append([]string{"-C", cwd}, args...)...)
	cmd.Env = append(os.Environ(), "GIT_OPTIONAL_LOCKS=0")
	if stdin != nil {
		cmd.Stdin = bytes.NewReader(stdin)
	}
	out, err := cmd.Output()
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	return out, err
}

// ReadGitRecords reads the repository that holds cwd.
func (*SessionGitRecords) ReadGitRecords(ctx context.Context, cwd string) (domain.GitRecords, []domain.UnreadSessionRecord, error) {
	roots, err := ResolveRepositoryRoots(ctx, cwd)
	if err != nil {
		return domain.GitRecords{}, nil, err
	}
	common, err := gitCommonDir(ctx, roots.WorktreeRoot)
	if err != nil {
		return domain.GitRecords{}, nil, err
	}
	records := domain.GitRecords{Root: roots.SharedRoot}
	if raw, err := gitRead(ctx, roots.WorktreeRoot, nil, "config", "--get", "remote."+FollowedRemote(ctx, roots.WorktreeRoot)+".url"); err == nil {
		records.Origin = NormalizeRemoteURL(string(raw))
	} else if ctx.Err() != nil {
		return domain.GitRecords{}, nil, ctx.Err()
	}

	listed, err := gitRead(ctx, roots.WorktreeRoot, nil, "worktree", "list", "--porcelain")
	if err != nil {
		return domain.GitRecords{}, nil, err
	}
	records.Worktrees = parseWorktreeList(string(listed))
	logs := headLogPaths(common, records.Worktrees)
	var unread []domain.UnreadSessionRecord
	if format, _ := gitRead(ctx, roots.WorktreeRoot, nil, "config", "--get", "extensions.refstorage"); strings.TrimSpace(string(format)) == "reftable" {
		// HEAD logs are files only in the files backend. Report instead of
		// silently reading no branch moves and no commits made here.
		unread = append(unread, domain.UnreadSessionRecord{Source: filepath.Join(common, "logs", "HEAD"), Reason: "Git HEAD logs are stored as reftable and are not read"})
		logs = nil
	}
	for index := range records.Worktrees {
		worktree := &records.Worktrees[index]
		if real, err := filepath.EvalSymlinks(worktree.Root); err == nil && real != worktree.Root {
			worktree.Aliases = append(worktree.Aliases, real)
		}
		path, ok := logs[worktree.Root]
		if !ok {
			continue
		}
		file, err := providerfs.OpenRegularFile(path)
		if err != nil {
			if !os.IsNotExist(err) {
				unread = append(unread, domain.UnreadSessionRecord{Source: path, Reason: "HEAD log could not be opened"})
			}
			continue
		}
		worktree.Before, worktree.Moves, worktree.Made, err = parseHeadLog(file)
		_ = file.Close()
		if err != nil {
			unread = append(unread, domain.UnreadSessionRecord{Source: path, Reason: "HEAD log could not be read to its end"})
		}
	}
	if err := ctx.Err(); err != nil {
		return domain.GitRecords{}, nil, err
	}

	records.Commits, err = readCommitObjects(ctx, roots.WorktreeRoot)
	if err != nil {
		return domain.GitRecords{}, nil, err
	}
	return records, unread, nil
}

func gitCommonDir(ctx context.Context, worktree string) (string, error) {
	raw, err := gitRead(ctx, worktree, nil, "rev-parse", "--path-format=absolute", "--git-common-dir")
	if ctx.Err() != nil {
		return "", ctx.Err()
	}
	common := strings.TrimSpace(string(raw))
	if err != nil || common == "" {
		// Git before 2.31 has no --path-format; its answer is relative to the
		// worktree unless already absolute.
		raw, err = gitRead(ctx, worktree, nil, "rev-parse", "--git-common-dir")
		if ctx.Err() != nil {
			return "", ctx.Err()
		}
		common = strings.TrimSpace(string(raw))
		if err != nil || common == "" {
			return "", domain.ErrNotGitRepo
		}
		if !filepath.IsAbs(common) {
			common = filepath.Join(worktree, common)
		}
	}
	return filepath.Clean(common), nil
}

// parseWorktreeList reads `git worktree list --porcelain`. The first entry is
// the main worktree. A bare entry has no working folder and is left out.
func parseWorktreeList(listed string) []domain.RecordedWorktree {
	var worktrees []domain.RecordedWorktree
	var current *domain.RecordedWorktree
	bare, first := false, true
	flush := func() {
		if current != nil && !bare {
			worktrees = append(worktrees, *current)
		}
		current, bare = nil, false
	}
	for _, line := range strings.Split(listed, "\n") {
		line = strings.TrimRight(line, "\r")
		switch {
		case strings.HasPrefix(line, "worktree "):
			flush()
			current = &domain.RecordedWorktree{Root: filepath.Clean(strings.TrimPrefix(line, "worktree ")), Main: first}
			first = false
		case current == nil:
		case line == "bare":
			bare = true
		case strings.HasPrefix(line, "branch "):
			current.Current = strings.TrimPrefix(strings.TrimPrefix(line, "branch "), "refs/heads/")
		}
	}
	flush()
	return worktrees
}

// headLogPaths maps each worktree root to its HEAD log. The main worktree's log
// is in the common directory; a linked worktree's log is in its own
// administrative directory, whose gitdir file names the worktree's .git file.
func headLogPaths(common string, worktrees []domain.RecordedWorktree) map[string]string {
	known := map[string]bool{}
	logs := map[string]string{}
	for _, worktree := range worktrees {
		known[worktree.Root] = true
		if worktree.Main {
			logs[worktree.Root] = filepath.Join(common, "logs", "HEAD")
		}
	}
	entries, err := os.ReadDir(filepath.Join(common, "worktrees"))
	if err != nil {
		return logs
	}
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		admin := filepath.Join(common, "worktrees", entry.Name())
		raw, err := providerfs.ReadRegularFile(filepath.Join(admin, "gitdir"))
		if err != nil {
			continue
		}
		root := filepath.Clean(filepath.Dir(strings.TrimSpace(string(raw))))
		if known[root] {
			logs[root] = filepath.Join(admin, "logs", "HEAD")
		}
	}
	return logs
}

var (
	headLogCheckout = regexp.MustCompile(`^checkout: moving from (\S+) to (\S+)$`)
	headLogRebased  = regexp.MustCompile(`^rebase (?:\(finish\)|finished): returning to refs/heads/(\S+)$`)
	headLogDetached = regexp.MustCompile(`^[0-9a-f]{7,40}$`)
)

func headLogBranch(name string) string {
	if headLogDetached.MatchString(name) {
		return ""
	}
	return name
}

// headLogMadeCommit reports whether a HEAD log message records a commit that
// was created in this worktree at that moment: a commit of any kind, a
// cherry-pick, a revert, or a merge that made a merge commit. A fast-forward
// creates nothing. A rebase is left out on purpose: the commits it writes are
// copies of commits that were already made, and are found as copies.
func headLogMadeCommit(message string) bool {
	switch {
	case strings.HasPrefix(message, "commit:"), strings.HasPrefix(message, "commit ("):
		return true
	case strings.HasPrefix(message, "cherry-pick:"):
		return message != "cherry-pick: fast-forward"
	case strings.HasPrefix(message, "revert:"):
		return true
	case strings.HasPrefix(message, "merge "), strings.HasPrefix(message, "pull"):
		return strings.Contains(message, ": Merge made by ")
	}
	return false
}

// parseHeadLog reads a HEAD log. before is the branch the first recorded move
// left. A line that is not a HEAD log line is passed over: the log is Git's
// own file and has one fixed layout.
func parseHeadLog(r io.Reader) (before string, moves []domain.HeadMove, made []domain.MadeCommit, err error) {
	reader := bufio.NewReader(r)
	sawMove := false
	for {
		line, readErr := reader.ReadString('\n')
		if line != "" {
			head, message, _ := strings.Cut(strings.TrimRight(line, "\r\n"), "\t")
			fields := strings.Fields(head)
			if len(fields) >= 5 {
				if atSec, parseErr := strconv.ParseInt(fields[len(fields)-2], 10, 64); parseErr == nil {
					if match := headLogCheckout.FindStringSubmatch(message); match != nil {
						if !sawMove {
							before, sawMove = headLogBranch(match[1]), true
						}
						moves = append(moves, domain.HeadMove{AtSec: atSec, Branch: headLogBranch(match[2])})
					} else if match := headLogRebased.FindStringSubmatch(message); match != nil {
						moves = append(moves, domain.HeadMove{AtSec: atSec, Branch: match[1]})
					} else if headLogMadeCommit(message) {
						made = append(made, domain.MadeCommit{AtSec: atSec, SHA: fields[1]})
					}
				}
			}
		}
		if readErr == io.EOF {
			break
		}
		if readErr != nil {
			err = readErr
			break
		}
	}
	sort.SliceStable(moves, func(i, j int) bool { return moves[i].AtSec < moves[j].AtSec })
	return before, moves, made, err
}

// readCommitObjects lists every commit object in the object database, whether
// or not a ref still names it, and marks the ones a local or remote-tracking
// branch reaches.
func readCommitObjects(ctx context.Context, worktree string) ([]domain.RecordedCommit, error) {
	listed, err := gitRead(ctx, worktree, nil, "cat-file", "--batch-all-objects", "--batch-check=%(objecttype) %(objectname)")
	if err != nil {
		return nil, err
	}
	var shas bytes.Buffer
	for _, line := range strings.Split(string(listed), "\n") {
		if sha, ok := strings.CutPrefix(line, "commit "); ok {
			shas.WriteString(sha)
			shas.WriteByte('\n')
		}
	}
	if shas.Len() == 0 {
		return nil, nil
	}
	onBranch := map[string]struct{}{}
	// An empty repository has no branch to walk; Git then reports an error and
	// nothing is reachable.
	if reached, err := gitRead(ctx, worktree, nil, "rev-list", "--branches", "--remotes"); err == nil {
		for _, sha := range strings.Fields(string(reached)) {
			onBranch[sha] = struct{}{}
		}
	} else if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	described, err := gitRead(ctx, worktree, shas.Bytes(), "log", "--no-walk=unsorted", "--stdin", "--format=%H%x1f%ct%x1f%at%x1f%ae%x1f%s")
	if err != nil {
		return nil, err
	}
	var commits []domain.RecordedCommit
	for _, line := range strings.Split(string(described), "\n") {
		fields := strings.SplitN(line, "\x1f", 5)
		if len(fields) != 5 {
			continue
		}
		created, createdErr := strconv.ParseInt(fields[1], 10, 64)
		authored, authoredErr := strconv.ParseInt(fields[2], 10, 64)
		if createdErr != nil || authoredErr != nil {
			continue
		}
		_, reached := onBranch[fields[0]]
		commits = append(commits, domain.RecordedCommit{SHA: fields[0], CreatedSec: created, AuthoredSec: authored, AuthorEmail: fields[3], Subject: fields[4], OnBranch: reached})
	}
	sort.Slice(commits, func(i, j int) bool { return commits[i].SHA < commits[j].SHA })
	return commits, nil
}
