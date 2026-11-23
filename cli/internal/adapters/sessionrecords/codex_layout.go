package sessionrecords

import (
	"context"
	"encoding/json"
	"path/filepath"
	"sort"
	"strings"
)

// codexTable is one table cxt reads from the databases Codex keeps, with the
// columns its queries name.
type codexTable struct {
	// File is the prefix of the database file: <prefix><n>.sqlite.
	File    string
	Table   string
	Columns []string
}

// codexLayout is everything cxt assumes about where Codex keeps its threads.
// It was read from the databases Codex 0.171 creates. Every table and column
// a query in this package names is listed here, and nothing else is assumed:
// Codex may add tables and columns freely.
var codexLayout = []codexTable{
	{File: "state_", Table: "threads", Columns: []string{"id", "cwd", "git_origin_url", "created_at_ms", "history_mode"}},
	{File: "state_", Table: "thread_spawn_edges", Columns: []string{"parent_thread_id", "child_thread_id"}},
	{File: "thread_history_", Table: "thread_items", Columns: []string{"thread_id", "turn_id", "item_id", "item_type", "item_json", "rollout_ordinal", "created_at_ms", "started_at_ms", "completed_at_ms"}},
}

// CodexLayoutDifferences compares the newest databases under the Codex home
// directory with what cxt reads from them and says each difference in words.
// No difference returns nothing, and so does a directory without the
// databases. It reads the databases' own description of their tables and no
// row.
func CodexLayoutDifferences(ctx context.Context, home string, rows SQLiteRows) []string {
	if rows == nil {
		rows = sqliteProgramRows
	}
	var differences []string
	for _, table := range codexLayout {
		database := latestVersionedFile(home, table.File, ".sqlite")
		if database == "" {
			// Without the file there is nothing to compare: Codex is not
			// used on this computer, or keeps only older rollout files,
			// which the readers say themselves.
			continue
		}
		present := map[string]bool{}
		err := rows(ctx, database, "SELECT name FROM pragma_table_info('"+table.Table+"')", func(raw json.RawMessage) error {
			var column struct {
				Name string `json:"name"`
			}
			if err := json.Unmarshal(raw, &column); err != nil {
				return err
			}
			present[column.Name] = true
			return nil
		})
		name := filepath.Base(database)
		switch {
		case err != nil:
			differences = append(differences, name+" could not be described: "+err.Error())
		case len(present) == 0:
			differences = append(differences, name+" has no table "+table.Table)
		default:
			var absent []string
			for _, column := range table.Columns {
				if !present[column] {
					absent = append(absent, column)
				}
			}
			sort.Strings(absent)
			if len(absent) > 0 {
				differences = append(differences, name+": table "+table.Table+" has no column "+strings.Join(absent, ", "))
			}
		}
	}
	return differences
}

// codexReadFailure says why a read of the Codex databases failed. When the
// databases are laid out another way than cxt reads them, that is the reason,
// with every difference named; otherwise it is the error itself.
func codexReadFailure(ctx context.Context, home string, rows SQLiteRows, err error) string {
	if differences := CodexLayoutDifferences(ctx, home, rows); len(differences) > 0 {
		return "Codex keeps its threads another way than this cxt reads them (" + strings.Join(differences, "; ") + "). A newer cxt is needed; the threads stay on this computer and are uploaded once it reads them"
	}
	return "thread database could not be read: " + err.Error()
}
