package sessionrecords

import (
	"bytes"
	"crypto/sha256"
	"encoding/gob"
	"encoding/hex"
	"os"
	"path/filepath"

	"github.com/wnsdy95/cxthub-cli/internal/domain"
)

// A read cache keeps what a reader made of a source that has not changed
// since, so that a run reads again only what changed. It holds what the
// tool-neutral records hold and nothing more: identifiers, times, folders,
// branch names and commit-number candidates. No conversation text is in it.
//
// It is only a shortcut. An entry is used when the source it was read from
// still has the same size and modification time, and a missing, unreadable or
// outdated entry means reading the source again.

// recordCacheVersion changes whenever a reader would make something else of
// the same source.
const recordCacheVersion = 2

// sourceStamp is what a source looked like when it was read.
type sourceStamp struct {
	Path      string
	Size      int64
	ModTimeNs int64
}

func stampOf(path string) (sourceStamp, bool) {
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() {
		return sourceStamp{Path: path}, false
	}
	return sourceStamp{Path: path, Size: info.Size(), ModTimeNs: info.ModTime().UnixNano()}, true
}

func cacheFile(dir, kind, key string) string {
	sum := sha256.Sum256([]byte(key))
	return filepath.Join(dir, kind+"-"+hex.EncodeToString(sum[:])+".gob")
}

func loadCache(path string, into any) bool {
	raw, err := os.ReadFile(path)
	if err != nil {
		return false
	}
	return gob.NewDecoder(bytes.NewReader(raw)).Decode(into) == nil
}

// storeCache writes an entry for this user only. A failure is not an error of
// the read: the next run reads the source again.
func storeCache(path string, value any) {
	var raw bytes.Buffer
	if gob.NewEncoder(&raw).Encode(value) != nil {
		return
	}
	dir := filepath.Dir(path)
	if os.MkdirAll(dir, 0o700) != nil {
		return
	}
	temp, err := os.CreateTemp(dir, ".tmp-")
	if err != nil {
		return
	}
	defer os.Remove(temp.Name())
	_, writeErr := temp.Write(raw.Bytes())
	chmodErr := temp.Chmod(0o600)
	if temp.Close() != nil || writeErr != nil || chmodErr != nil {
		return
	}
	_ = os.Rename(temp.Name(), path)
}

// claudeFileCache is a parsed session file with the stamp it was parsed at.
// Calls holds every tool call of the file, so that one entry answers a read
// for any set of wanted calls.
type claudeFileCache struct {
	Version      int
	Stamp        sourceStamp
	Session      string
	Items        []domain.RecordedItem
	Calls        map[string]string
	BadLines     int
	FirstBadLine int
	BadEntries   int
	// Where the read ended, for a later read of the same file to go on from
	// when the file only grew: how many bytes of whole lines were read and
	// what they hash to, how many lines they were, the turn the last record
	// belonged to, and the commands that had started and not ended.
	Offset     int64
	PrefixHash []byte
	Lines      int
	Turn       string
	Pending    map[string]claudeCachedCommand
}

// claudeCachedCommand is a command that had started when a read ended.
type claudeCachedCommand struct {
	StartedMs int64
	Folder    string
}

// codexBatchCache is what the Codex databases gave for one scope, with the
// stamps of every database file it was read from.
type codexBatchCache struct {
	Version int
	Stamps  []sourceStamp
	Scope   domain.SessionRecordScope
	Batch   domain.SessionRecordBatch
}
