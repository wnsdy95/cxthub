// Package sessionrecords reads the session records coding agents keep on this
// computer and turns them into tool-neutral records. It only reads: it never
// writes to a tool's files, and it keeps no state of its own.
//
// Reading is separate from the linking and cutting rules, which live in the
// domain and work on these records alone. A reader that resumes from where it
// last stopped can therefore replace these full reads without touching a rule.
package sessionrecords

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/wnsdy95/cxthub-cli/internal/adapters/providerfs"
	"github.com/wnsdy95/cxthub-cli/internal/domain"
)

// ClaudeRecords reads Claude Code session files:
//
//	<projects>/<folder>/<session>.jsonl
//	<projects>/<folder>/<session>/subagents/agent-<agent>.jsonl
//	<projects>/<folder>/<session>/subagents/workflows/<name>/agent-<agent>.jsonl
//
// Every project folder is read, because a session belongs to a repository by
// the working folders its records name, not by the folder its file sits in.
type ClaudeRecords struct{ projects string }

// NewClaudeRecords reads the session files under the given projects directory.
func NewClaudeRecords(projects string) *ClaudeRecords { return &ClaudeRecords{projects: projects} }

type claudeEntry struct {
	Type      string          `json:"type"`
	UUID      string          `json:"uuid"`
	SessionID string          `json:"sessionId"`
	Timestamp string          `json:"timestamp"`
	Cwd       string          `json:"cwd"`
	GitBranch string          `json:"gitBranch"`
	PromptID  string          `json:"promptId"`
	Message   json.RawMessage `json:"message"`
}

type claudeBlock struct {
	Type      string          `json:"type"`
	ID        string          `json:"id"`
	Name      string          `json:"name"`
	ToolUseID string          `json:"tool_use_id"`
	Content   json.RawMessage `json:"content"`
}

type claudeFile struct {
	path string
	// session is the session identifier the records themselves carry.
	session string
	items   []domain.RecordedItem
	// calls maps a wanted tool-call identifier to the record that made it.
	calls map[string]string
	// What could not be read. A last line still being written is neither.
	openErr, readErr       error
	badLines, firstBadLine int
	badEntries             int
}

type claudeSubFile struct {
	path, folderSession, agent string
}

// ReadSessionRecords returns the sessions whose records name a working folder
// inside the repository.
func (r *ClaudeRecords) ReadSessionRecords(ctx context.Context, scope domain.SessionRecordScope) (domain.SessionRecordBatch, error) {
	var batch domain.SessionRecordBatch
	sessions, subs, err := r.list()
	if err != nil {
		batch.Unread = append(batch.Unread, domain.UnreadSessionRecord{Provider: domain.ProviderClaude, Source: r.projects, Reason: "session folder could not be listed"})
		return batch, nil
	}

	// A sub-session file is copied along when its session is forked. The copy
	// keeps the records' own session identifier, so that identifier, not the
	// folder a file sits in, names the parent and tells copies apart.
	type subKey struct{ parent, agent string }
	type subGroup struct {
		file   claudeSubFile
		parent string
		copies int
	}
	groups := map[subKey]*subGroup{}
	var order []subKey
	for _, sub := range subs {
		if err := ctx.Err(); err != nil {
			return domain.SessionRecordBatch{}, err
		}
		parent := peekClaudeSession(sub.path)
		key := subKey{parent, sub.agent}
		if parent == "" {
			// Without a recorded parent nothing says two files are the same
			// session, so each one stands alone.
			key = subKey{"\x00" + sub.path, sub.agent}
		}
		group, seen := groups[key]
		switch {
		case !seen:
			groups[key] = &subGroup{file: sub, parent: parent}
			order = append(order, key)
		case sub.folderSession == parent && group.file.folderSession != parent:
			group.file = sub
			group.copies++
		default:
			group.copies++
		}
	}
	wanted := map[string]struct{}{}
	spawnCall := map[subKey]string{}
	for _, key := range order {
		if call := readClaudeSpawnCall(groups[key].file.path); call != "" {
			spawnCall[key] = call
			wanted[call] = struct{}{}
		}
	}

	inScope := newScopeMemo(scope)
	calls := map[string]map[string]string{}
	for _, path := range sessions {
		if err := ctx.Err(); err != nil {
			return domain.SessionRecordBatch{}, err
		}
		file := readClaudeFile(path, wanted)
		id := strings.TrimSuffix(filepath.Base(path), ".jsonl")
		calls[id] = file.calls
		session := domain.RecordedSession{Provider: domain.ProviderClaude, ID: id, Scope: domain.SessionScopeFolder, Items: file.items}
		addClaudeSession(&batch, session, file, inScope)
	}
	for _, key := range order {
		if err := ctx.Err(); err != nil {
			return domain.SessionRecordBatch{}, err
		}
		group := groups[key]
		file := readClaudeFile(group.file.path, nil)
		session := domain.RecordedSession{
			Provider: domain.ProviderClaude, ID: group.file.agent, Sub: true, Parent: group.parent,
			Scope: domain.SessionScopeFolder, FileCopies: group.copies, Items: file.items,
		}
		if call := spawnCall[key]; call != "" {
			session.SpawnItem = calls[group.parent][call]
		}
		addClaudeSession(&batch, session, file, inScope)
	}
	return batch, nil
}

// addClaudeSession keeps a session that worked inside the repository, with
// whatever of its file could not be read. A file that gave no record at all
// cannot be placed in any repository, so its unread parts are reported rather
// than dropped.
func addClaudeSession(b *domain.SessionRecordBatch, session domain.RecordedSession, file claudeFile, inScope *scopeMemo) {
	held := false
	for _, item := range session.Items {
		if inScope.holds(item.Folder) {
			held = true
			break
		}
	}
	if held {
		b.Sessions = append(b.Sessions, session)
	}
	if !held && len(session.Items) > 0 {
		return
	}
	for _, reason := range file.unread() {
		b.Unread = append(b.Unread, domain.UnreadSessionRecord{Provider: domain.ProviderClaude, Source: file.path, Reason: reason})
	}
}

func (f claudeFile) unread() []string {
	var reasons []string
	if f.openErr != nil {
		reasons = append(reasons, "session file could not be opened")
	}
	if f.readErr != nil {
		reasons = append(reasons, "session file could not be read to its end")
	}
	if f.badLines > 0 {
		reasons = append(reasons, fmt.Sprintf("%d line(s) could not be decoded as session records, first at line %d", f.badLines, f.firstBadLine))
	}
	if f.badEntries > 0 {
		reasons = append(reasons, fmt.Sprintf("%d conversation record(s) have no readable identifier or time", f.badEntries))
	}
	return reasons
}

// list returns the session files and sub-session files, each in name order.
func (r *ClaudeRecords) list() (sessions []string, subs []claudeSubFile, err error) {
	folders, err := os.ReadDir(r.projects)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil, nil
		}
		return nil, nil, err
	}
	for _, folder := range folders {
		if !folder.IsDir() {
			continue
		}
		dir := filepath.Join(r.projects, folder.Name())
		entries, err := os.ReadDir(dir)
		if err != nil {
			continue
		}
		for _, entry := range entries {
			switch {
			case entry.Type().IsRegular() && strings.HasSuffix(entry.Name(), ".jsonl"):
				sessions = append(sessions, filepath.Join(dir, entry.Name()))
			case entry.IsDir():
				root := filepath.Join(dir, entry.Name(), "subagents")
				subs = appendClaudeSubFiles(subs, root, entry.Name())
				workflows, _ := os.ReadDir(filepath.Join(root, "workflows"))
				for _, workflow := range workflows {
					if workflow.IsDir() {
						subs = appendClaudeSubFiles(subs, filepath.Join(root, "workflows", workflow.Name()), entry.Name())
					}
				}
			}
		}
	}
	sort.Strings(sessions)
	sort.Slice(subs, func(i, j int) bool { return subs[i].path < subs[j].path })
	return sessions, subs, nil
}

func appendClaudeSubFiles(subs []claudeSubFile, dir, folderSession string) []claudeSubFile {
	entries, _ := os.ReadDir(dir)
	for _, entry := range entries {
		name := entry.Name()
		if entry.Type().IsRegular() && strings.HasPrefix(name, "agent-") && strings.HasSuffix(name, ".jsonl") {
			subs = append(subs, claudeSubFile{path: filepath.Join(dir, name), folderSession: folderSession, agent: strings.TrimSuffix(strings.TrimPrefix(name, "agent-"), ".jsonl")})
		}
	}
	return subs
}

// readClaudeSpawnCall reads which tool call of the parent spawned a
// sub-session. The tool records it beside the file, and not for every one.
func readClaudeSpawnCall(subPath string) string {
	raw, err := providerfs.ReadRegularFile(strings.TrimSuffix(subPath, ".jsonl") + ".meta.json")
	if err != nil {
		return ""
	}
	var meta struct {
		ToolUseID string `json:"toolUseId"`
	}
	if json.Unmarshal(raw, &meta) != nil {
		return ""
	}
	return meta.ToolUseID
}

func claudeConversation(entry claudeEntry) bool {
	return entry.Type == "user" || entry.Type == "assistant"
}

// peekClaudeSession returns the session identifier of the first conversation
// record that carries one.
func peekClaudeSession(path string) string {
	file, err := providerfs.OpenRegularFile(path)
	if err != nil {
		return ""
	}
	defer file.Close()
	reader := bufio.NewReaderSize(file, 1<<16)
	for {
		line, err := reader.ReadBytes('\n')
		if err != nil {
			return ""
		}
		var entry claudeEntry
		if json.Unmarshal(line, &entry) == nil && claudeConversation(entry) && entry.SessionID != "" {
			return entry.SessionID
		}
	}
}

type claudePending struct {
	startedMs int64
	folder    string
}

// readClaudeFile reads one session file. Only user and assistant records are
// conversation; other record types repeat tool calls and results and would
// count them twice.
func readClaudeFile(path string, wanted map[string]struct{}) claudeFile {
	out := claudeFile{path: path, calls: map[string]string{}}
	file, err := providerfs.OpenRegularFile(path)
	if err != nil {
		out.openErr = err
		return out
	}
	defer file.Close()
	reader := bufio.NewReaderSize(file, 1<<16)
	seen := map[string]struct{}{}
	pending := map[string]claudePending{}
	turn := ""
	for number := 1; ; number++ {
		line, err := reader.ReadBytes('\n')
		if err != nil {
			// A line without its newline is still being written. It is left
			// for the next read, as live capture does.
			if err != io.EOF {
				out.readErr = err
			}
			return out
		}
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		var entry claudeEntry
		if json.Unmarshal(line, &entry) != nil {
			if out.badLines == 0 {
				out.firstBadLine = number
			}
			out.badLines++
			continue
		}
		if !claudeConversation(entry) {
			continue
		}
		at, timeErr := time.Parse(time.RFC3339Nano, entry.Timestamp)
		if entry.UUID == "" || timeErr != nil {
			out.badEntries++
			continue
		}
		if out.session == "" {
			out.session = entry.SessionID
		}
		// The tool writes its turn identifier on user records only; the
		// assistant records that follow belong to the same turn.
		if entry.PromptID != "" {
			turn = entry.PromptID
		}
		if _, dup := seen[entry.UUID]; dup {
			continue
		}
		seen[entry.UUID] = struct{}{}
		item := domain.RecordedItem{ID: entry.UUID, Turn: turn, AtMs: at.UnixMilli(), Folder: entry.Cwd}
		if entry.GitBranch == "HEAD" {
			item.Detached = true
		} else {
			item.Branch = entry.GitBranch
		}
		for _, block := range claudeBlocks(entry.Message) {
			switch block.Type {
			case "tool_use":
				if block.Name == "Bash" && block.ID != "" {
					pending[block.ID] = claudePending{startedMs: item.AtMs, folder: entry.Cwd}
				}
				if _, want := wanted[block.ID]; want {
					out.calls[block.ID] = entry.UUID
				}
			case "tool_result":
				started, running := pending[block.ToolUseID]
				if !running {
					continue
				}
				delete(pending, block.ToolUseID)
				item.Commands = append(item.Commands, domain.RecordedCommand{
					StartedMs: started.startedMs, EndedMs: item.AtMs, Folder: started.folder, Tokens: claudeResultTokens(block.Content),
				})
			}
		}
		out.items = append(out.items, item)
	}
}

func claudeBlocks(message json.RawMessage) []claudeBlock {
	var body struct {
		Content json.RawMessage `json:"content"`
	}
	if json.Unmarshal(message, &body) != nil {
		return nil
	}
	var blocks []claudeBlock
	// A plain-text message has a string here and carries no tool blocks.
	_ = json.Unmarshal(body.Content, &blocks)
	return blocks
}

// claudeResultTokens reads the commit-number candidates of a tool result,
// whose content is either text or a list of blocks that may hold text.
func claudeResultTokens(content json.RawMessage) []string {
	var text string
	if json.Unmarshal(content, &text) == nil {
		return domain.CommitNumberTokens(text)
	}
	var blocks []struct {
		Text string `json:"text"`
	}
	if json.Unmarshal(content, &blocks) != nil {
		return nil
	}
	var parts []string
	for _, block := range blocks {
		parts = append(parts, block.Text)
	}
	return domain.CommitNumberTokens(strings.Join(parts, "\n"))
}

// scopeMemo remembers which folders are inside the repository: records name
// the same few folders over and over.
type scopeMemo struct {
	scope domain.SessionRecordScope
	known map[string]bool
}

func newScopeMemo(scope domain.SessionRecordScope) *scopeMemo {
	return &scopeMemo{scope: scope, known: map[string]bool{}}
}

func (m *scopeMemo) holds(folder string) bool {
	if folder == "" {
		return false
	}
	held, known := m.known[folder]
	if !known {
		held = m.scope.Holds(folder)
		m.known[folder] = held
	}
	return held
}
