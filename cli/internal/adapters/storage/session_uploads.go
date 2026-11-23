package storage

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"

	"github.com/wnsdy95/cxthub-cli/internal/adapters/providerfs"
	"github.com/wnsdy95/cxthub-cli/internal/domain"
)

// SessionUploadFiles keeps what was uploaded for each session in the
// repository's .cxt folder:
//
//	.cxt/sessions/<tool>/<session id>/report.json           the report as it was last sent
//	.cxt/sessions/<tool>/<session id>/conversation.json.gz  the conversation document as it was last sent
//	.cxt/sessions/<tool>/<session id>/log.jsonl             one line per run that sent something or failed
//
// The files are the user's own, readable by the user alone. Nothing is
// computed from them: they are what the server can be compared with.
type SessionUploadFiles struct {
	// root gives the folder that holds the repository's .cxt folder, and
	// false where cxt is not set up.
	root func(ctx context.Context, cwd string) (string, bool)
}

// NewSessionUploadFiles keeps records under the folder root names.
func NewSessionUploadFiles(root func(ctx context.Context, cwd string) (string, bool)) *SessionUploadFiles {
	return &SessionUploadFiles{root: root}
}

const (
	sessionUploadReport       = "report.json"
	sessionUploadConversation = "conversation.json.gz"
	sessionUploadLog          = "log.jsonl"
)

// sessionUploadReportFile is report.json: the session's key and the report.
type sessionUploadReportFile struct {
	Provider  domain.ProviderKind        `json:"provider"`
	SessionID string                     `json:"session_id"`
	Body      domain.AnchoredSessionBody `json:"body"`
}

func sessionUploadDir(key domain.SessionUploadKey) (string, error) {
	if (key.Provider != domain.ProviderClaude && key.Provider != domain.ProviderCodex) || !domain.ValidAnchoredSessionID(key.SessionID) {
		return "", fmt.Errorf("session %s %s cannot name a folder", key.Provider, key.SessionID)
	}
	return filepath.Join(".cxt", "sessions", string(key.Provider), key.SessionID), nil
}

func (f *SessionUploadFiles) place(ctx context.Context, cwd string, key domain.SessionUploadKey) (root, dir string, ok bool, err error) {
	root, ok = f.root(ctx, cwd)
	if !ok {
		return "", "", false, nil
	}
	dir, err = sessionUploadDir(key)
	return root, dir, err == nil, err
}

func readSessionUploadFile(root, relative string) ([]byte, bool, error) {
	raw, err := providerfs.ReadRepoFile(root, relative)
	if errors.Is(err, os.ErrNotExist) {
		return nil, false, nil
	}
	return raw, err == nil, err
}

// readSessionUploadLog reads the log. A line that is not an entry, such as
// one cut short by a crash, is left out.
func readSessionUploadLog(root, dir string) ([]domain.SessionUploadLogEntry, error) {
	raw, found, err := readSessionUploadFile(root, filepath.Join(dir, sessionUploadLog))
	if err != nil || !found {
		return nil, err
	}
	var entries []domain.SessionUploadLogEntry
	scanner := bufio.NewScanner(bytes.NewReader(raw))
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for scanner.Scan() {
		var entry domain.SessionUploadLogEntry
		if json.Unmarshal(scanner.Bytes(), &entry) == nil && entry.At != "" {
			entries = append(entries, entry)
		}
	}
	return entries, nil
}

func appendSessionUploadLog(root, dir string, entry domain.SessionUploadLogEntry) error {
	path, err := providerfs.PrepareRepoFile(root, filepath.Join(dir, sessionUploadLog), 0o700)
	if err != nil {
		return err
	}
	line, err := json.Marshal(entry)
	if err != nil {
		return err
	}
	file, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return err
	}
	// A log that ends in a line cut short starts the new one on a line of
	// its own.
	if info, err := file.Stat(); err == nil && info.Size() > 0 {
		last := make([]byte, 1)
		if _, err := file.ReadAt(last, info.Size()-1); err == nil && last[0] != '\n' {
			line = append([]byte{'\n'}, line...)
		}
	}
	_, err = file.Write(append(line, '\n'))
	if closeErr := file.Close(); err == nil {
		err = closeErr
	}
	return err
}

// KeepSessionUpload stores the report and, when one is given, the
// conversation, and logs the run. Where cxt is not set up it keeps nothing.
func (f *SessionUploadFiles) KeepSessionUpload(ctx context.Context, cwd string, upload domain.AnchoredSessionUpload, conversation *domain.CIRDocument, entry domain.SessionUploadLogEntry) error {
	key := domain.SessionUploadKey{Provider: upload.Provider, SessionID: upload.SessionID}
	root, dir, ok, err := f.place(ctx, cwd, key)
	if err != nil || !ok {
		return err
	}
	changed := false
	// The report of a refused upload is not what the server holds; the copy
	// kept stays the one that was last sent and taken.
	if entry.Report != domain.SegmentPushRefused {
		report, err := json.MarshalIndent(sessionUploadReportFile{Provider: upload.Provider, SessionID: upload.SessionID, Body: upload.Body}, "", "  ")
		if err != nil {
			return err
		}
		report = append(report, '\n')
		kept, _, err := readSessionUploadFile(root, filepath.Join(dir, sessionUploadReport))
		if err != nil {
			return err
		}
		if !bytes.Equal(kept, report) {
			if err := writeSessionUploadFile(root, filepath.Join(dir, sessionUploadReport), report); err != nil {
				return err
			}
			changed = true
		}
	}
	log, err := readSessionUploadLog(root, dir)
	if err != nil {
		return err
	}
	if conversation != nil && entry.Content == domain.SegmentContentStored {
		// The copy kept is the document the last line names; another
		// document, or no copy at all, is written.
		_, found, err := readSessionUploadFile(root, filepath.Join(dir, sessionUploadConversation))
		if err != nil {
			return err
		}
		if !found || len(log) == 0 || lastSessionUploadDoc(log) != entry.Doc {
			var packed bytes.Buffer
			writer := gzip.NewWriter(&packed)
			if err := json.NewEncoder(writer).Encode(conversation); err != nil {
				return err
			}
			if err := writer.Close(); err != nil {
				return err
			}
			if err := writeSessionUploadFile(root, filepath.Join(dir, sessionUploadConversation), packed.Bytes()); err != nil {
				return err
			}
			changed = true
		}
	}
	if !changed && !entry.Eventful() && len(log) > 0 {
		return nil
	}
	if len(log) > 0 && log[len(log)-1].SameOutcome(entry) && !changed {
		return nil
	}
	return appendSessionUploadLog(root, dir, entry)
}

// lastSessionUploadDoc is the document the newest line with a stored
// conversation names.
func lastSessionUploadDoc(log []domain.SessionUploadLogEntry) domain.ContentHash {
	for index := len(log) - 1; index >= 0; index-- {
		if log[index].Content == domain.SegmentContentStored && log[index].Doc != "" {
			return log[index].Doc
		}
	}
	return ""
}

func writeSessionUploadFile(root, relative string, data []byte) error {
	path, err := providerfs.PrepareRepoFile(root, relative, 0o700)
	if err != nil {
		return err
	}
	return providerfs.WriteRegularFileAtomic(path, data, 0o600)
}

// LogSessionAction appends a line that is not an upload.
func (f *SessionUploadFiles) LogSessionAction(ctx context.Context, cwd string, key domain.SessionUploadKey, entry domain.SessionUploadLogEntry) error {
	root, dir, ok, err := f.place(ctx, cwd, key)
	if err != nil || !ok {
		return err
	}
	return appendSessionUploadLog(root, dir, entry)
}

// ListSessionUploads names every session a record is kept for, by tool then
// identifier.
func (f *SessionUploadFiles) ListSessionUploads(ctx context.Context, cwd string) ([]domain.SessionUploadKey, error) {
	root, ok := f.root(ctx, cwd)
	if !ok {
		return nil, nil
	}
	var keys []domain.SessionUploadKey
	for _, provider := range []domain.ProviderKind{domain.ProviderClaude, domain.ProviderCodex} {
		entries, err := os.ReadDir(filepath.Join(root, ".cxt", "sessions", string(provider)))
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, err
		}
		for _, entry := range entries {
			if entry.IsDir() && domain.ValidAnchoredSessionID(entry.Name()) {
				keys = append(keys, domain.SessionUploadKey{Provider: provider, SessionID: entry.Name()})
			}
		}
	}
	sort.Slice(keys, func(i, j int) bool {
		if keys[i].Provider != keys[j].Provider {
			return keys[i].Provider < keys[j].Provider
		}
		return keys[i].SessionID < keys[j].SessionID
	})
	return keys, nil
}

// ReadSessionUpload reads one session's record. A session nothing is kept
// for is ErrNotFound.
func (f *SessionUploadFiles) ReadSessionUpload(ctx context.Context, cwd string, key domain.SessionUploadKey, withConversation bool) (domain.SessionUploadRecord, error) {
	record := domain.SessionUploadRecord{Provider: key.Provider, SessionID: key.SessionID}
	root, dir, ok, err := f.place(ctx, cwd, key)
	if err != nil {
		return record, err
	}
	if !ok {
		return record, domain.ErrNotFound
	}
	raw, found, err := readSessionUploadFile(root, filepath.Join(dir, sessionUploadReport))
	if err != nil {
		return record, err
	}
	if found {
		var file sessionUploadReportFile
		if err := json.Unmarshal(raw, &file); err != nil {
			return record, fmt.Errorf("%s: %w", filepath.Join(dir, sessionUploadReport), err)
		}
		record.Report = &file.Body
	}
	if record.Log, err = readSessionUploadLog(root, dir); err != nil {
		return record, err
	}
	if !found && len(record.Log) == 0 {
		return record, domain.ErrNotFound
	}
	if path, err := providerfs.PrepareRepoFile(root, filepath.Join(dir, sessionUploadConversation), 0o700); err == nil {
		if info, err := os.Lstat(path); err == nil && info.Mode().IsRegular() {
			record.ConversationKept = true
		}
	}
	if withConversation {
		packed, found, err := readSessionUploadFile(root, filepath.Join(dir, sessionUploadConversation))
		if err != nil {
			return record, err
		}
		if found {
			reader, err := gzip.NewReader(bytes.NewReader(packed))
			if err != nil {
				return record, fmt.Errorf("%s: %w", filepath.Join(dir, sessionUploadConversation), err)
			}
			plain, err := io.ReadAll(reader)
			if err != nil {
				return record, fmt.Errorf("%s: %w", filepath.Join(dir, sessionUploadConversation), err)
			}
			var doc domain.CIRDocument
			if err := json.Unmarshal(plain, &doc); err != nil {
				return record, fmt.Errorf("%s: %w", filepath.Join(dir, sessionUploadConversation), err)
			}
			record.Conversation = &doc
		}
	}
	return record, nil
}
