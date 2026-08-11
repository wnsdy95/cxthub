// ledger.go — Session ledger (.cxt/session-ledger.json): Adjusting the assumption that "active session = latest mtime".
//
// Structuring capture pollution in two ways:
//   - Materialized: recovery/seed files written by cxt. Excludes from capture candidates at the size where it was growing (i.e., before the user actually resumed). At the moment of growth, the entry is deleted and the session returns to the official active session — the path for the recovery to hijack the live session is removed.
//   - Superseded: sessions isolated by context switch/checkout. Excludes from permanent capture — ongoing utterances from stale contexts are not recorded (checkpointed only up to the switch point).
package providerfs

import (
	"encoding/json"
	"os"
	"path/filepath"
	"time"
)

// LedgerEntry represents a session file record in the ledger.
type LedgerEntry struct {
	// Size is the file size immediately after materialization (growth criterion — using size instead of mtime: mtime can change due to copying/indexing, but the session file grows only during actual conversation).
	Size int64 `json:"size"`
	// Superseded true indicates a session isolated by context switch — excluded from permanent capture.
	Superseded bool   `json:"superseded,omitempty"`
	At         string `json:"at"`
}

type ledgerFile struct {
	Sessions map[string]LedgerEntry `json:"sessions"`
}

func ledgerPath(repoRoot string) string {
	return filepath.Join(repoRoot, ".cxt", "session-ledger.json")
}

// --- Ledger File Lock ---
// The ledger can suffer from lost update if load-modify-save operations (context switch isolation marking ↔ hook capture growth criterion) overlap (review #3 — violation of isolation invariant).
// Serializes using the O_EXCL + stale argument pattern like the .lock of the capture coordinator.

const ledgerLockStale = 30 * time.Second

func lockLedger(repoRoot string) (unlock func(), ok bool) {
	lock, err := PrepareRepoFile(repoRoot, filepath.Join(".cxt", "session-ledger.json.lock"), 0o755)
	if err != nil {
		return func() {}, false
	}
	deadline := time.Now().Add(3 * time.Second)
	for {
		f, err := os.OpenFile(lock, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
		if err == nil {
			_ = f.Close()
			return func() { _ = os.Remove(lock) }, true
		}
		if fi, serr := os.Stat(lock); serr == nil && time.Since(fi.ModTime()) > ledgerLockStale {
			_ = os.Remove(lock) // Remove crash residual argument
			continue
		}
		if time.Now().After(deadline) {
			return func() {}, false // Lock acquisition failed — proceed without lock (availability priority, hooks must not be blocked)
		}
		time.Sleep(25 * time.Millisecond)
	}
}

func loadLedger(repoRoot string) ledgerFile {
	var lf ledgerFile
	if b, err := ReadRepoFile(repoRoot, filepath.Join(".cxt", "session-ledger.json")); err == nil {
		_ = json.Unmarshal(b, &lf)
	}
	if lf.Sessions == nil {
		lf.Sessions = map[string]LedgerEntry{}
	}
	return lf
}

func saveLedger(repoRoot string, lf ledgerFile) error {
	b, err := json.MarshalIndent(lf, "", "  ")
	if err != nil {
		return err
	}
	return WriteRepoFileAtomic(repoRoot, filepath.Join(".cxt", "session-ledger.json"), b, 0o644)
}

// RecordMaterialized records a materialized session file in the ledger (based on current size).
func RecordMaterialized(repoRoot, path string) error {
	info, err := os.Stat(path)
	if err != nil {
		return err
	}
	unlock, _ := lockLedger(repoRoot)
	defer unlock()
	lf := loadLedger(repoRoot)
	lf.Sessions[path] = LedgerEntry{Size: info.Size(), At: time.Now().UTC().Format(time.RFC3339)}
	return saveLedger(repoRoot, lf)
}

// MarkSuperseded records session files in an isolated manner (excluding permanent captures).
// The exclusion persists even if the file is recreated (path reused).
func MarkSuperseded(repoRoot, path string) error {
	unlock, _ := lockLedger(repoRoot)
	defer unlock()
	lf := loadLedger(repoRoot)
	e := lf.Sessions[path]
	e.Superseded = true
	e.At = time.Now().UTC().Format(time.RFC3339)
	lf.Sessions[path] = e
	return saveLedger(repoRoot, lf)
}

// UnmarkSuperseded rolls back a transition that failed before its boundary was
// durably recorded. It removes only a superseded marker; materialization
// entries retain their size gate.
func UnmarkSuperseded(repoRoot, path string) error {
	unlock, _ := lockLedger(repoRoot)
	defer unlock()
	lf := loadLedger(repoRoot)
	e, ok := lf.Sessions[path]
	if !ok || !e.Superseded {
		return nil
	}
	if e.Size > 0 {
		e.Superseded = false
		e.At = time.Now().UTC().Format(time.RFC3339)
		lf.Sessions[path] = e
	} else {
		delete(lf.Sessions, path)
	}
	return saveLedger(repoRoot, lf)
}

// CaptureExcluded returns whether the path should be excluded from active session determination.
// If the materialized entry "grows" (size increases), the entry is deleted and false is returned — the resumed session starts from the moment it becomes officially active.
func CaptureExcluded(repoRoot, path string, size int64) bool {
	unlock, _ := lockLedger(repoRoot)
	defer unlock()
	lf := loadLedger(repoRoot)
	e, ok := lf.Sessions[path]
	if !ok {
		return false
	}
	if e.Superseded {
		return true
	}
	if size > e.Size {
		delete(lf.Sessions, path)
		_ = saveLedger(repoRoot, lf)
		return false
	}
	return true
}
