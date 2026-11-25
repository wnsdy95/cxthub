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
	// partBytes is where a part of a conversation is sealed.
	partBytes int
}

// NewClaudeContent creates the reader.
func NewClaudeContent() *ClaudeContent {
	return &ClaudeContent{codec: codec.NewClaudeCodec(), partBytes: domain.SessionPartBytes}
}

// claudeResume is where the records after a sealed part are looked for: the
// line of that part's last record, by where it begins, how long it is and
// which record it is.
type claudeResume struct {
	Offset int64  `json:"offset"`
	Length int    `json:"length"`
	ID     string `json:"id"`
}

// claudeRecordLine is the complete line of one record and where it begins.
type claudeRecordLine struct {
	raw    []byte
	offset int64
}

// after returns where the lines after the remembered record begin, when the
// record is still the line it was. Anything else, such as a file that was
// written anew, is no place to go on from.
func (r claudeResume) after(path string) (int64, bool) {
	file, err := providerfs.OpenRegularFile(path)
	if err != nil {
		return 0, false
	}
	defer file.Close()
	// Only what the file holds there is read, whatever length is remembered.
	line, err := io.ReadAll(io.NewSectionReader(file, r.Offset, int64(r.Length)))
	if err != nil {
		return 0, false
	}
	var entry claudeEntry
	if json.Unmarshal(line, &entry) != nil || !claudeConversation(entry) || entry.UUID != r.ID {
		return 0, false
	}
	return r.Offset + int64(r.Length), true
}

// ReadSessionParts converts the records named by ids, in order, into the
// parts of the session's conversation that follow what from stands for. A
// record that is not in the file is listed as missing, never skipped
// silently.
//
// The records are looked for after the place from.Token remembers, which is
// where they are when a session went on: the tool only adds to its file. When
// that place is not what it was, or the records are not found in order after
// it, the whole file is read instead; what is returned is the same.
func (c *ClaudeContent) ReadSessionParts(ctx context.Context, repoRoot string, records domain.SessionRecordRanges, ids []string, from domain.SessionPartResume) (domain.SessionPartsRead, error) {
	out := domain.SessionPartsRead{}
	if records.Provider != domain.ProviderClaude || records.Source == "" {
		return out, fmt.Errorf("no Claude Code session file is recorded for session %s", records.ID)
	}
	wanted := make(map[string]struct{}, len(ids))
	for _, id := range ids {
		wanted[id] = struct{}{}
	}
	start, resumed := int64(0), false
	var remembered claudeResume
	if from.Token != "" && json.Unmarshal([]byte(from.Token), &remembered) == nil {
		start, resumed = remembered.after(records.Source)
	}
	lines, ordered, err := c.linesInOrder(ctx, records.Source, start, wanted, ids)
	if err != nil {
		return out, err
	}
	if resumed && !ordered {
		// Not every record follows the remembered place in order.
		if lines, ordered, err = c.linesInOrder(ctx, records.Source, 0, wanted, ids); err != nil {
			return out, err
		}
	}
	sizes := make([]int, len(ids))
	for index, id := range ids {
		line, ok := lines[id]
		if !ok {
			out.Missing = append(out.Missing, id)
			continue
		}
		sizes[index] = len(line.raw)
	}
	if len(out.Missing) > 0 {
		return out, nil
	}
	cut, sealed := domain.CutSessionParts(sizes, c.partBytes)
	envelope, events, at := from.Envelope, from.Events, 0
	for index, held := range cut {
		if err := ctx.Err(); err != nil {
			return domain.SessionPartsRead{}, err
		}
		var raw bytes.Buffer
		for _, id := range ids[at : at+held] {
			raw.Write(lines[id].raw)
		}
		// The same two masking steps capture applies: the repository's listed
		// secret values on the tool's bytes, then the pattern scrub on events.
		scrubbed, _ := capture.ScrubSecrets(raw.Bytes(), repoRoot)
		doc, counts, err := c.codec.DecodeAppendCounted(ctx, scrubbed, envelope, events)
		if err != nil {
			return domain.SessionPartsRead{}, fmt.Errorf("session %s: %w", records.ID, err)
		}
		doc = capture.ScrubDoc(doc, repoRoot)
		envelope = doc.Envelope
		events += len(doc.Events)
		part := domain.SessionPartRead{Counts: counts, Events: doc.Events, Envelope: envelope, Sealed: sealed || index < len(cut)-1}
		// A place to go on from is remembered only where the records were
		// found in the order they are named in.
		if last := lines[ids[at+held-1]]; part.Sealed && ordered {
			token, _ := json.Marshal(claudeResume{Offset: last.offset, Length: len(last.raw), ID: ids[at+held-1]})
			part.Token = string(token)
		}
		out.Parts = append(out.Parts, part)
		at += held
	}
	return out, nil
}

// linesInOrder reads the lines of the wanted records at or after start and
// says whether every one of ids was found and each begins after the one
// before it.
func (c *ClaudeContent) linesInOrder(ctx context.Context, path string, start int64, wanted map[string]struct{}, ids []string) (map[string]claudeRecordLine, bool, error) {
	lines, err := claudeRecordLines(ctx, path, start, wanted)
	if err != nil {
		return nil, false, err
	}
	next := start
	for _, id := range ids {
		line, ok := lines[id]
		if !ok || line.offset < next {
			return lines, false, nil
		}
		next = line.offset + int64(len(line.raw))
	}
	return lines, true, nil
}

// claudeRecordLines returns the complete lines of the wanted conversation
// records that begin at or after start, keyed by record identifier. The first
// line under an identifier is the record, as in the reader that listed them.
func claudeRecordLines(ctx context.Context, path string, start int64, wanted map[string]struct{}) (map[string]claudeRecordLine, error) {
	file, err := providerfs.OpenRegularFile(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	if _, err := file.Seek(start, io.SeekStart); err != nil {
		return nil, err
	}
	reader := bufio.NewReaderSize(file, 1<<16)
	lines := make(map[string]claudeRecordLine, len(wanted))
	offset := start
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
		begins := offset
		offset += int64(len(line))
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
		lines[entry.UUID] = claudeRecordLine{raw: line, offset: begins}
	}
}
