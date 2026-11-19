package sessionrecords

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"

	"github.com/wnsdy95/cxthub-cli/internal/adapters/capture"
	"github.com/wnsdy95/cxthub-cli/internal/adapters/codec"
	"github.com/wnsdy95/cxthub-cli/internal/adapters/providerfs"
	"github.com/wnsdy95/cxthub-cli/internal/domain"
)

// ClaudeContent reads the conversation of a Claude Code session's records
// back from its session file. It converts and masks them with the codec and
// the masking policy capture uses, so the same record gives the same events.
type ClaudeContent struct {
	codec *codec.ClaudeCodec
}

// NewClaudeContent creates the reader.
func NewClaudeContent() *ClaudeContent { return &ClaudeContent{codec: codec.NewClaudeCodec()} }

// ReadSessionContent reads the named records from records.Source. A record
// that is no longer in the file is listed as missing, never skipped silently.
func (c *ClaudeContent) ReadSessionContent(ctx context.Context, repoRoot string, records domain.SessionRecordRanges) (domain.SessionContent, error) {
	out := domain.SessionContent{Ranges: make([][]domain.Event, len(records.Ranges))}
	if records.Provider != domain.ProviderClaude || records.Source == "" {
		return out, fmt.Errorf("no Claude Code session file is recorded for session %s", records.ID)
	}
	wanted := map[string]struct{}{}
	for _, ids := range records.Ranges {
		for _, id := range ids {
			wanted[id] = struct{}{}
		}
	}
	lines, err := claudeRecordLines(ctx, records.Source, wanted)
	if err != nil {
		return out, err
	}
	envelope, events := domain.Envelope{}, 0
	for index, ids := range records.Ranges {
		var raw bytes.Buffer
		for _, id := range ids {
			line, ok := lines[id]
			if !ok {
				out.Missing = append(out.Missing, id)
				continue
			}
			raw.Write(line)
		}
		if err := ctx.Err(); err != nil {
			return out, err
		}
		// The same two masking steps capture applies: the repository's listed
		// secret values on the tool's bytes, then the pattern scrub on events.
		scrubbed, _ := capture.ScrubSecrets(raw.Bytes(), repoRoot)
		doc, err := c.codec.DecodeAppend(ctx, scrubbed, envelope, events)
		if err != nil {
			return out, fmt.Errorf("session %s: %w", records.ID, err)
		}
		doc = capture.ScrubDoc(doc, repoRoot)
		envelope = doc.Envelope
		events += len(doc.Events)
		out.Ranges[index] = doc.Events
	}
	out.Envelope = envelope
	return out, nil
}

// claudeRecordLines returns the complete lines of the wanted conversation
// records, keyed by record identifier. The first line under an identifier is
// the record, as in the reader that listed them.
func claudeRecordLines(ctx context.Context, path string, wanted map[string]struct{}) (map[string][]byte, error) {
	file, err := providerfs.OpenRegularFile(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	reader := bufio.NewReaderSize(file, 1<<16)
	lines := make(map[string][]byte, len(wanted))
	for number := 0; ; number++ {
		if number%4096 == 0 {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
		}
		line, err := reader.ReadBytes('\n')
		if err != nil {
			// A line without its newline is still being written.
			if err != io.EOF {
				return nil, err
			}
			return lines, nil
		}
		var entry claudeEntry
		if json.Unmarshal(line, &entry) != nil || !claudeConversation(entry) || entry.UUID == "" {
			continue
		}
		if _, want := wanted[entry.UUID]; !want {
			continue
		}
		if _, dup := lines[entry.UUID]; dup {
			continue
		}
		lines[entry.UUID] = line
	}
}
