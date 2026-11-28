package sessionrecords

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/wnsdy95/cxthub-cli/internal/adapters/capture"
	"github.com/wnsdy95/cxthub-cli/internal/adapters/codec"
	"github.com/wnsdy95/cxthub-cli/internal/domain"
)

// CodexContent reads the conversation of a Codex thread's records. A record
// is one item of the thread; the items are converted by the codec that reads
// the shape Codex publishes for them, and masked as capture masks.
//
// The items are read from the thread history database, the place the records
// themselves were listed from, so that the conversation and the report it
// belongs to cannot disagree.
type CodexContent struct {
	home string
	rows SQLiteRows
	// partBytes is where a part of a conversation is sealed.
	partBytes int
}

// NewCodexContent reads the databases under the Codex home directory.
func NewCodexContent(home string) *CodexContent {
	return &CodexContent{home: home, rows: sqliteProgramRows, partBytes: domain.SessionPartBytes}
}

// ReadSessionParts converts the records named by ids, in order, into the
// parts of the thread's conversation that follow what from stands for. A
// record that is no longer among the thread's items is listed as missing,
// never skipped silently. The thread's items are asked for anew every time:
// no place to go on from is remembered.
func (c *CodexContent) ReadSessionParts(ctx context.Context, repoRoot string, records domain.SessionRecordRanges, ids []string, from domain.SessionPartResume) (domain.SessionPartsRead, error) {
	out := domain.SessionPartsRead{}
	if records.Provider != domain.ProviderCodex || !codexThreadID.MatchString(records.ID) {
		return out, fmt.Errorf("no Codex thread is recorded for session %s", records.ID)
	}
	items, err := c.threadItems(ctx, records.ID)
	if err != nil {
		if ctx.Err() != nil {
			return out, ctx.Err()
		}
		if errors.Is(err, errSQLiteMissing) {
			return out, fmt.Errorf("session %s: the sqlite3 program is not installed, so the thread's items were not read", records.ID)
		}
		if errors.Is(err, errCodexHistoryMissing) {
			return out, fmt.Errorf("session %s: %w", records.ID, err)
		}
		return out, fmt.Errorf("session %s: %s", records.ID, codexReadFailure(ctx, c.home, c.rows, err))
	}
	sizes := make([]int, len(ids))
	for index, id := range ids {
		item, ok := items[id]
		if !ok {
			out.Missing = append(out.Missing, id)
			continue
		}
		sizes[index] = len(item.JSON)
	}
	if len(out.Missing) > 0 {
		return out, nil
	}
	cut, sealed := domain.CutSessionParts(sizes, c.partBytes)
	envelope, events, at := from.Envelope, from.Events, 0
	// The thread is named by the report's own identifier.
	envelope.SessionOriginID = records.ID
	for index, held := range cut {
		if err := ctx.Err(); err != nil {
			return domain.SessionPartsRead{}, err
		}
		chosen := make([]codec.CodexItem, 0, held)
		for _, id := range ids[at : at+held] {
			item := items[id]
			// The same two masking steps capture applies: the repository's
			// listed secret values on the tool's bytes, then the pattern scrub
			// on events.
			item.JSON, _ = capture.ScrubSecrets(item.JSON, repoRoot)
			chosen = append(chosen, item)
		}
		doc, counts, err := codec.DecodeCodexItemsCounted(chosen, envelope, events)
		if err != nil {
			return domain.SessionPartsRead{}, fmt.Errorf("session %s: %w", records.ID, err)
		}
		doc = capture.ScrubDoc(doc, repoRoot)
		envelope = doc.Envelope
		events += len(doc.Events)
		out.Parts = append(out.Parts, domain.SessionPartRead{Counts: counts, Events: doc.Events, Envelope: envelope, Sealed: sealed || index < len(cut)-1})
		at += held
	}
	return out, nil
}

// errCodexHistoryMissing reports that Codex keeps no thread history database
// under its home directory.
var errCodexHistoryMissing = errors.New("no Codex thread history database was found")

type codexContentRow struct {
	TurnID      string `json:"turn_id"`
	ItemID      string `json:"item_id"`
	CreatedAtMs int64  `json:"created_at_ms"`
	ItemJSON    string `json:"item_json"`
}

// threadItems returns the items of one thread by record identifier.
func (c *CodexContent) threadItems(ctx context.Context, threadID string) (map[string]codec.CodexItem, error) {
	history := latestVersionedFile(c.home, "thread_history_", ".sqlite")
	if history == "" {
		return nil, errCodexHistoryMissing
	}
	items := map[string]codec.CodexItem{}
	query := "SELECT turn_id, item_id, created_at_ms, item_json FROM thread_items WHERE thread_id='" + threadID + "' ORDER BY rollout_ordinal"
	err := c.rows(ctx, history, query, func(raw json.RawMessage) error {
		var row codexContentRow
		if err := json.Unmarshal(raw, &row); err != nil {
			return err
		}
		record := row.TurnID + "/" + row.ItemID
		if _, dup := items[record]; !dup {
			items[record] = codec.CodexItem{TurnID: row.TurnID, CreatedAtMs: row.CreatedAtMs, JSON: json.RawMessage(row.ItemJSON)}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return items, nil
}
