package sessionrecords

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/wnsdy95/cxthub-cli/internal/domain"
)

// SQLiteRows runs one reading query against a database file and passes each
// result row, a JSON object keyed by column name, to row.
type SQLiteRows func(ctx context.Context, database, query string, row func(json.RawMessage) error) error

// errSQLiteMissing reports that the sqlite3 program is not installed.
var errSQLiteMissing = errors.New("sqlite3 program not found")

// CodexRecords reads the thread databases Codex keeps in its home directory:
//
//	state_<n>.sqlite            threads, and which thread spawned which
//	thread_history_<n>.sqlite   the items of each thread
//
// The number in a file name is the tool's format version. The newest of each
// is read; a query that no longer fits it is reported as an unread record.
type CodexRecords struct {
	home      string
	rows      SQLiteRows
	normalize func(string) string
	// cache is the directory of the read cache; empty reads the databases anew.
	cache string
}

// NewCodexRecords reads the databases under the Codex home directory with the
// sqlite3 program, as the other Codex readers do, so no database driver is
// built in. normalize turns a remote address into the form the scope's origin
// is in.
func NewCodexRecords(home string, normalize func(string) string) *CodexRecords {
	return &CodexRecords{home: home, rows: sqliteProgramRows, normalize: normalize}
}

// WithCache keeps what the databases gave for a repository under dir and
// reads them again only when one of the database files, or its write-ahead
// log, changed in size or modification time. Without it nothing is written
// anywhere.
func (r *CodexRecords) WithCache(dir string) *CodexRecords {
	r.cache = dir
	return r
}

// databaseStamps is what the database files and their write-ahead logs look
// like now. A log that does not exist is part of the answer.
func databaseStamps(paths ...string) []sourceStamp {
	var stamps []sourceStamp
	for _, path := range paths {
		if path == "" {
			continue
		}
		for _, file := range []string{path, path + "-wal"} {
			stamp, _ := stampOf(file)
			stamps = append(stamps, stamp)
		}
	}
	return stamps
}

// sqliteProgramRows opens the database read-only and reads content that is
// still in its write-ahead log. cxt writes nothing.
//
// SQLite itself creates lock files beside a write-ahead-logged database when it
// opens one that has none, even read-only. A database with nothing in its log
// is therefore opened as immutable, which reads the same content and creates no
// file. A log with content means a writer already keeps those files.
func sqliteProgramRows(ctx context.Context, database, query string, row func(json.RawMessage) error) error {
	program, err := exec.LookPath("sqlite3")
	if err != nil {
		return errSQLiteMissing
	}
	target := database
	if info, err := os.Stat(database + "-wal"); err != nil || info.Size() == 0 {
		target = (&url.URL{Scheme: "file", Path: database}).String() + "?immutable=1"
	}
	cmd := exec.CommandContext(ctx, program, "-readonly", "-json", target, query)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	if err := cmd.Start(); err != nil {
		return err
	}
	decodeErr := decodeJSONRows(stdout, row)
	// Drain what is left so the program can exit even when decoding stopped.
	_, _ = io.Copy(io.Discard, stdout)
	waitErr := cmd.Wait()
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if waitErr != nil {
		message := strings.TrimSpace(stderr.String())
		if len(message) > 200 {
			message = message[:200]
		}
		return fmt.Errorf("sqlite3: %s", message)
	}
	return decodeErr
}

// decodeJSONRows reads a JSON array of objects one row at a time. A query with
// no result prints nothing at all.
func decodeJSONRows(r io.Reader, row func(json.RawMessage) error) error {
	decoder := json.NewDecoder(r)
	if _, err := decoder.Token(); err != nil {
		if err == io.EOF {
			return nil
		}
		return err
	}
	for decoder.More() {
		var raw json.RawMessage
		if err := decoder.Decode(&raw); err != nil {
			return err
		}
		if err := row(raw); err != nil {
			return err
		}
	}
	return nil
}

var codexThreadID = regexp.MustCompile(`^[0-9A-Za-z_-]{1,128}$`)

type codexThread struct {
	ID          string  `json:"id"`
	Cwd         string  `json:"cwd"`
	OriginURL   *string `json:"git_origin_url"`
	CreatedAtMs *int64  `json:"created_at_ms"`
	HistoryMode *string `json:"history_mode"`
}

type codexItemRow struct {
	ThreadID      string   `json:"thread_id"`
	TurnID        string   `json:"turn_id"`
	ItemID        string   `json:"item_id"`
	ItemType      string   `json:"item_type"`
	CreatedAtMs   int64    `json:"created_at_ms"`
	StartedAtMs   *int64   `json:"started_at_ms"`
	CompletedAtMs *int64   `json:"completed_at_ms"`
	Cwd           *string  `json:"cwd"`
	DurationMs    *float64 `json:"duration_ms"`
	Output        *string  `json:"output"`
	Tool          *string  `json:"tool"`
	Sender        *string  `json:"sender"`
	Receivers     *string  `json:"receivers"`
	Status        *string  `json:"status"`
}

const codexItemColumns = `thread_id, turn_id, item_id, item_type, created_at_ms, started_at_ms, completed_at_ms,
 CASE WHEN item_type='commandExecution' THEN json_extract(item_json,'$.cwd') END AS cwd,
 CASE WHEN item_type='commandExecution' THEN json_extract(item_json,'$.durationMs') END AS duration_ms,
 CASE WHEN item_type='commandExecution' THEN json_extract(item_json,'$.aggregatedOutput') END AS output,
 CASE WHEN item_type='collabAgentToolCall' THEN json_extract(item_json,'$.tool') END AS tool,
 CASE WHEN item_type='collabAgentToolCall' THEN json_extract(item_json,'$.senderThreadId') END AS sender,
 CASE WHEN item_type='collabAgentToolCall' THEN json_extract(item_json,'$.receiverThreadIds') END AS receivers,
 CASE WHEN item_type='collabAgentToolCall' THEN json_extract(item_json,'$.status') END AS status`

// codexItemBatch bounds how many threads one items query names.
const codexItemBatch = 100

// ReadSessionRecords returns the threads that worked inside the repository. A
// thread belongs to it when its starting folder or the folder of one of its
// commands is inside a worktree. The one fallback is the remote address the
// tool recorded for the thread, for a thread whose folder is no longer one.
func (r *CodexRecords) ReadSessionRecords(ctx context.Context, scope domain.SessionRecordScope) (domain.SessionRecordBatch, error) {
	if r.cache == "" {
		return r.readSessionRecords(ctx, scope)
	}
	state := latestVersionedFile(r.home, "state_", ".sqlite")
	history := latestVersionedFile(r.home, "thread_history_", ".sqlite")
	if state == "" {
		return r.readSessionRecords(ctx, scope)
	}
	before := databaseStamps(state, history)
	entry := cacheFile(r.cache, "codex", r.home+"\x00"+strings.Join(scope.Folders, "\x00")+"\x00"+scope.Origin)
	var cached codexBatchCache
	if loadCache(entry, &cached) && cached.Version == recordCacheVersion && reflect.DeepEqual(cached.Stamps, before) && reflect.DeepEqual(cached.Scope, scope) {
		return cached.Batch, nil
	}
	batch, err := r.readSessionRecords(ctx, scope)
	if err != nil {
		return batch, err
	}
	// What could not be read is read again next time, and so is a database
	// that changed while it was read.
	if len(batch.Unread) == 0 && reflect.DeepEqual(databaseStamps(state, history), before) {
		storeCache(entry, codexBatchCache{Version: recordCacheVersion, Stamps: before, Scope: scope, Batch: batch})
	}
	return batch, nil
}

func (r *CodexRecords) readSessionRecords(ctx context.Context, scope domain.SessionRecordScope) (domain.SessionRecordBatch, error) {
	var batch domain.SessionRecordBatch
	unread := func(source, reason string) {
		batch.Unread = append(batch.Unread, domain.UnreadSessionRecord{Provider: domain.ProviderCodex, Source: source, Reason: reason})
	}
	failed := func(source string, err error) (domain.SessionRecordBatch, error) {
		if ctx.Err() != nil {
			return domain.SessionRecordBatch{}, ctx.Err()
		}
		if errors.Is(err, errSQLiteMissing) {
			unread(source, "the sqlite3 program is not installed, so the thread database was not read")
		} else {
			unread(source, "thread database could not be read: "+err.Error())
		}
		return batch, nil
	}

	state := latestVersionedFile(r.home, "state_", ".sqlite")
	history := latestVersionedFile(r.home, "thread_history_", ".sqlite")
	if state == "" {
		// Without the thread database there is no list of threads to place in a
		// repository. Older rollout files are a different format.
		if rollouts := filepath.Join(r.home, "sessions"); hasEntries(rollouts) {
			unread(rollouts, "Codex rollout files are not read by this command")
		}
		return batch, nil
	}

	var threads []codexThread
	err := r.rows(ctx, state, "SELECT id, cwd, git_origin_url, created_at_ms, history_mode FROM threads ORDER BY id", func(raw json.RawMessage) error {
		var thread codexThread
		if err := json.Unmarshal(raw, &thread); err != nil {
			return err
		}
		threads = append(threads, thread)
		return nil
	})
	if err != nil {
		return failed(state, err)
	}
	parents := map[string]string{}
	err = r.rows(ctx, state, "SELECT parent_thread_id, child_thread_id FROM thread_spawn_edges ORDER BY child_thread_id", func(raw json.RawMessage) error {
		var edge struct {
			Parent string `json:"parent_thread_id"`
			Child  string `json:"child_thread_id"`
		}
		if err := json.Unmarshal(raw, &edge); err != nil {
			return err
		}
		parents[edge.Child] = edge.Parent
		return nil
	})
	if err != nil {
		return failed(state, err)
	}

	inScope := newScopeMemo(scope)
	byFolder := map[string]bool{}
	if history != "" {
		err = r.rows(ctx, history, "SELECT DISTINCT thread_id, json_extract(item_json,'$.cwd') AS cwd FROM thread_items WHERE item_type='commandExecution'", func(raw json.RawMessage) error {
			var used struct {
				ThreadID string  `json:"thread_id"`
				Cwd      *string `json:"cwd"`
			}
			if err := json.Unmarshal(raw, &used); err != nil {
				return err
			}
			if used.Cwd != nil && inScope.holds(*used.Cwd) {
				byFolder[used.ThreadID] = true
			}
			return nil
		})
		if err != nil {
			return failed(history, err)
		}
	}

	sessions := map[string]*domain.RecordedSession{}
	legacy := map[string]bool{}
	var ids []string
	for _, thread := range threads {
		rule := domain.SessionScopeRule("")
		switch {
		case inScope.holds(thread.Cwd) || byFolder[thread.ID]:
			rule = domain.SessionScopeFolder
		case scope.Origin != "" && thread.OriginURL != nil && *thread.OriginURL != "" && r.normalize(*thread.OriginURL) == scope.Origin:
			rule = domain.SessionScopeOriginURL
		default:
			continue
		}
		if !codexThreadID.MatchString(thread.ID) {
			unread(state, "a thread identifier cannot be used in a query, so its items were not read")
			continue
		}
		session := &domain.RecordedSession{Provider: domain.ProviderCodex, ID: thread.ID, StartFolder: thread.Cwd, Scope: rule}
		if thread.CreatedAtMs != nil {
			session.StartedMs = *thread.CreatedAtMs
		}
		if parent, spawned := parents[thread.ID]; spawned {
			session.Sub, session.Parent = true, parent
		}
		sessions[thread.ID] = session
		legacy[thread.ID] = thread.HistoryMode != nil && *thread.HistoryMode == "legacy"
		ids = append(ids, thread.ID)
	}
	sort.Strings(ids)

	undecoded := 0
	for first := 0; history != "" && first < len(ids); first += codexItemBatch {
		last := min(first+codexItemBatch, len(ids))
		query := "SELECT " + codexItemColumns + " FROM thread_items WHERE thread_id IN ('" + strings.Join(ids[first:last], "','") + "') ORDER BY thread_id, rollout_ordinal"
		err := r.rows(ctx, history, query, func(raw json.RawMessage) error {
			var row codexItemRow
			if json.Unmarshal(raw, &row) != nil || row.ItemID == "" || sessions[row.ThreadID] == nil {
				undecoded++
				return nil
			}
			item, call := codexItem(row)
			session := sessions[row.ThreadID]
			session.Items = append(session.Items, item)
			if call != nil {
				batch.Calls = append(batch.Calls, *call)
				if call.Tool == "spawnAgent" {
					for _, child := range call.To {
						// The item that made the recorded spawning call marks
						// where in the parent the spawned thread begins.
						if spawned := sessions[child]; spawned != nil && spawned.Parent == row.ThreadID && spawned.SpawnItem == "" {
							spawned.SpawnItem = item.ID
						}
					}
				}
			}
			return nil
		})
		if err != nil {
			return failed(history, err)
		}
	}
	if undecoded > 0 {
		unread(history, strconv.Itoa(undecoded)+" thread item(s) could not be decoded")
	}
	for _, id := range ids {
		session := sessions[id]
		if len(session.Items) == 0 && legacy[id] {
			unread(state, "thread "+id+" keeps its history in an older rollout file, which this command does not read")
		}
		batch.Sessions = append(batch.Sessions, *session)
	}
	return batch, nil
}

// codexItem turns one thread item into a record. Its identifier joins the turn
// and item identifiers, which together identify an item within a thread.
func codexItem(row codexItemRow) (domain.RecordedItem, *domain.RecordedAgentCall) {
	item := domain.RecordedItem{ID: row.TurnID + "/" + row.ItemID, Turn: row.TurnID, AtMs: row.CreatedAtMs}
	switch row.ItemType {
	case "commandExecution":
		command := domain.RecordedCommand{StartedMs: row.CreatedAtMs}
		if row.StartedAtMs != nil {
			command.StartedMs = *row.StartedAtMs
		}
		command.EndedMs = command.StartedMs
		switch {
		case row.CompletedAtMs != nil:
			command.EndedMs = *row.CompletedAtMs
		case row.DurationMs != nil:
			command.EndedMs += int64(*row.DurationMs)
		}
		if row.Cwd != nil {
			command.Folder, item.Folder = *row.Cwd, *row.Cwd
		}
		if row.Output != nil {
			command.Tokens = domain.CommitNumberTokens(*row.Output)
		}
		item.Commands = []domain.RecordedCommand{command}
	case "collabAgentToolCall":
		call := &domain.RecordedAgentCall{Provider: domain.ProviderCodex, From: row.ThreadID, AtMs: row.CreatedAtMs, Item: item.ID}
		if row.Tool != nil {
			call.Tool = *row.Tool
		}
		if row.Sender != nil && *row.Sender != "" {
			call.From = *row.Sender
		}
		if row.Status != nil {
			call.Status = *row.Status
		}
		if row.Receivers != nil {
			_ = json.Unmarshal([]byte(*row.Receivers), &call.To)
		}
		return item, call
	}
	return item, nil
}

// latestVersionedFile returns the file <prefix><n><suffix> with the largest n.
func latestVersionedFile(dir, prefix, suffix string) string {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return ""
	}
	best, bestVersion := "", -1
	for _, entry := range entries {
		name := entry.Name()
		if !entry.Type().IsRegular() || !strings.HasPrefix(name, prefix) || !strings.HasSuffix(name, suffix) {
			continue
		}
		version, err := strconv.Atoi(strings.TrimSuffix(strings.TrimPrefix(name, prefix), suffix))
		if err == nil && version > bestVersion {
			best, bestVersion = filepath.Join(dir, name), version
		}
	}
	return best
}

func hasEntries(dir string) bool {
	entries, err := os.ReadDir(dir)
	return err == nil && len(entries) > 0
}
