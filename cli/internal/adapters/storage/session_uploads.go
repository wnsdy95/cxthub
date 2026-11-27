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
	"strings"

	"github.com/wnsdy95/cxthub-cli/internal/adapters/providerfs"
	"github.com/wnsdy95/cxthub-cli/internal/domain"
)

// SessionUploadFiles keeps what was uploaded for each session in the
// repository's .cxt folder:
//
//	.cxt/sessions/<tool>/<session id>/report.json                    the report as it was last sent
//	.cxt/sessions/<tool>/<session id>/conversation/<document>.json.gz  each document of the conversation the report names
//	.cxt/sessions/<tool>/<session id>/log.jsonl                      one line per run that sent something or failed
//
// The conversation is kept in the documents it is sent in, named by their
// hash: a session that goes on adds a file and replaces its last one, and the
// files of the documents before stay as they are.
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
	sessionUploadConversation = "conversation"
	sessionUploadDocument     = ".json.gz"
	sessionUploadLog          = "log.jsonl"
	// sessionUploadWhole is the one file an earlier build kept the whole
	// conversation in. It is removed when the documents are kept.
	sessionUploadWhole = "conversation.json.gz"
)

// sessionUploadPart is where the copy of one document of a conversation is
// kept, by the hash the server knows the document by.
func sessionUploadPart(dir string, doc domain.ContentHash) (string, error) {
	if err := domain.ValidateContentHash(doc); err != nil {
		return "", err
	}
	return filepath.Join(dir, sessionUploadConversation, strings.TrimPrefix(string(doc), "sha256:")+sessionUploadDocument), nil
}

// keepSessionUploadParts makes the conversation folder hold the documents
// the kept report names: those of built that it names and that are not kept
// yet are written, and files of documents it does not name are removed. It
// reports whether a document was written, and how many of the documents the
// report names are not kept afterwards.
func keepSessionUploadParts(root, dir string, report *domain.AnchoredSessionBody, built []domain.SessionPartDocument) (written bool, lacking int, err error) {
	named := map[string]struct{}{}
	if report != nil && report.Content != nil {
		for _, part := range report.Content.Parts {
			relative, err := sessionUploadPart(dir, part.Doc)
			if err != nil {
				return false, 0, err
			}
			named[filepath.Base(relative)] = struct{}{}
		}
	}
	for _, part := range built {
		relative, err := sessionUploadPart(dir, part.Doc)
		if err != nil {
			return written, 0, err
		}
		if _, wanted := named[filepath.Base(relative)]; !wanted {
			continue
		}
		if _, found, err := readSessionUploadFile(root, relative); err != nil {
			return written, 0, err
		} else if found {
			continue
		}
		var packed bytes.Buffer
		writer := gzip.NewWriter(&packed)
		if err := json.NewEncoder(writer).Encode(part.Document); err != nil {
			return written, 0, err
		}
		if err := writer.Close(); err != nil {
			return written, 0, err
		}
		if err := writeSessionUploadFile(root, relative, packed.Bytes()); err != nil {
			return written, 0, err
		}
		written = true
	}
	folder, err := providerfs.EnsureRepoDir(root, filepath.Join(dir, sessionUploadConversation), 0o700)
	if err != nil {
		return written, 0, err
	}
	entries, err := os.ReadDir(folder)
	if err != nil {
		return written, 0, err
	}
	lacking = len(named)
	for _, entry := range entries {
		if _, wanted := named[entry.Name()]; wanted {
			lacking--
			continue
		}
		if err := os.Remove(filepath.Join(folder, entry.Name())); err != nil {
			return written, 0, err
		}
	}
	if path, err := providerfs.PrepareRepoFile(root, filepath.Join(dir, sessionUploadWhole), 0o700); err == nil {
		_ = os.Remove(path)
	}
	return written, lacking, nil
}

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

// KeepSessionUpload stores the report and the documents of its conversation
// that this run built, and logs the run. Where cxt is not set up it keeps
// nothing. A copy that lacks a document its report names is an error, said
// after everything else was kept.
func (f *SessionUploadFiles) KeepSessionUpload(ctx context.Context, cwd string, upload domain.AnchoredSessionUpload, built []domain.SessionPartDocument, entry domain.SessionUploadLogEntry) error {
	key := domain.SessionUploadKey{Provider: upload.Provider, SessionID: upload.SessionID}
	root, dir, ok, err := f.place(ctx, cwd, key)
	if err != nil || !ok {
		return err
	}
	changed, lacking := false, 0
	kept, found, err := readSessionUploadFile(root, filepath.Join(dir, sessionUploadReport))
	if err != nil {
		return err
	}
	// The report of a refused upload is not what the server holds; the copy
	// kept stays the one that was last sent and taken.
	if entry.Report != domain.SegmentPushRefused {
		report, err := json.MarshalIndent(sessionUploadReportFile{Provider: upload.Provider, SessionID: upload.SessionID, Body: upload.Body}, "", "  ")
		if err != nil {
			return err
		}
		report = append(report, '\n')
		if !bytes.Equal(kept, report) {
			if err := writeSessionUploadFile(root, filepath.Join(dir, sessionUploadReport), report); err != nil {
				return err
			}
			changed = true
		}
		kept, found = report, true
	}
	// The documents kept are those of the report that is kept.
	if found {
		var file sessionUploadReportFile
		if err := json.Unmarshal(kept, &file); err != nil {
			return fmt.Errorf("%s: %w", filepath.Join(dir, sessionUploadReport), err)
		}
		// A document that was written is a change to the copy, and is logged.
		written, missing, err := keepSessionUploadParts(root, dir, &file.Body, built)
		if err != nil {
			return err
		}
		changed, lacking = changed || written, missing
	}
	log, err := readSessionUploadLog(root, dir)
	if err != nil {
		return err
	}
	repeated := len(log) > 0 && !changed && (!entry.Eventful() || log[len(log)-1].SameOutcome(entry))
	if !repeated {
		if err := appendSessionUploadLog(root, dir, entry); err != nil {
			return err
		}
	}
	if lacking > 0 {
		return fmt.Errorf("the copy of the conversation lacks %d of the documents its report names", lacking)
	}
	return nil
}

func writeSessionUploadFile(root, relative string, data []byte) error {
	path, err := providerfs.PrepareRepoFile(root, relative, 0o700)
	if err != nil {
		return err
	}
	return providerfs.WriteRegularFileAtomic(path, data, 0o600)
}

// LogSessionAction appends a line that is not an upload, unless it only
// repeats the last line.
func (f *SessionUploadFiles) LogSessionAction(ctx context.Context, cwd string, key domain.SessionUploadKey, entry domain.SessionUploadLogEntry) error {
	root, dir, ok, err := f.place(ctx, cwd, key)
	if err != nil || !ok {
		return err
	}
	log, err := readSessionUploadLog(root, dir)
	if err != nil {
		return err
	}
	if len(log) > 0 && log[len(log)-1].SameOutcome(entry) {
		return nil
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
	// The conversation is kept when every document the report names is.
	if record.Report == nil || record.Report.Content == nil || len(record.Report.Content.Parts) == 0 {
		return record, nil
	}
	var parts []domain.SessionPartDocument
	for _, part := range record.Report.Content.Parts {
		relative, err := sessionUploadPart(dir, part.Doc)
		if err != nil {
			return record, fmt.Errorf("%s: %w", filepath.Join(dir, sessionUploadReport), err)
		}
		if !withConversation {
			path, err := providerfs.PrepareRepoFile(root, relative, 0o700)
			if err != nil {
				return record, err
			}
			if info, err := os.Lstat(path); err != nil || !info.Mode().IsRegular() {
				return record, nil
			}
			continue
		}
		packed, found, err := readSessionUploadFile(root, relative)
		if err != nil {
			return record, err
		}
		if !found {
			return record, nil
		}
		reader, err := gzip.NewReader(bytes.NewReader(packed))
		if err != nil {
			return record, fmt.Errorf("%s: %w", relative, err)
		}
		plain, err := io.ReadAll(reader)
		if err != nil {
			return record, fmt.Errorf("%s: %w", relative, err)
		}
		var doc domain.CIRDocument
		if err := json.Unmarshal(plain, &doc); err != nil {
			return record, fmt.Errorf("%s: %w", relative, err)
		}
		parts = append(parts, domain.SessionPartDocument{Doc: part.Doc, Document: doc})
	}
	record.ConversationKept, record.Parts = true, parts
	return record, nil
}
