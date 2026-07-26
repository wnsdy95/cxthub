package capture

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/wnsdy95/cxthub/cli/internal/adapters/providerfs"
)

// SecretsFile is a file in the repo root containing a list of secret values. One value per line (e.g., values from .env). Lines starting with '#' and empty lines are ignored. This file is excluded from git (.git/info/exclude) and from both sides of the context.
const SecretsFile = ".cxtsecrets"

// RedactedToken is the default string used to replace secrets (user policy text). It can be customized via repo settings (secrets.redact / secrets.minlen — cxt config).
const RedactedToken = "{this is deleted by security policy}"

// ScrubOptions is a masking policy. The loader (LoadScrubOptions) injects it from the side that knows remotecfg (main) — capture does not depend on remotecfg (to prevent circular dependencies).
type ScrubOptions struct {
	Redact string    // Replacement text ("": RedactedToken)
	MinLen int       // Minimum length (0 = 4)
	Tier   ScrubTier // Pattern scrub tier (off|standard|strict, "" = standard)
}

// LoadScrubOptions is a loader for the masking policy in repoRoot (default: always use the default value). It replaces the implementation that reads remotecfg with the composition root (cmd/cxt).
var LoadScrubOptions = func(repoRoot string) ScrubOptions { return ScrubOptions{} }

// LoadSecrets reads the list of secret values from repoRoot/.cxtsecrets. Returns nil if the file does not exist.
func LoadSecrets(repoRoot string) []string {
	b, err := providerfs.ReadRepoFile(repoRoot, SecretsFile)
	if err != nil {
		return nil
	}
	var out []string
	for _, line := range strings.Split(string(b), "\n") {
		v := strings.TrimSpace(line)
		if v == "" || strings.HasPrefix(v, "#") {
			continue
		}
		out = append(out, v)
	}
	return out
}

// ScrubSecrets replaces .cxtsecrets values in raw session bytes with RedactedToken. The local CLI applies
// this deterministically before storing context through snapshot, stash, or memorize. It is pure string
// replacement, not an AI decision. Escaped JSONL forms (json.Marshal output) are replaced too, covering
// secrets that contain special characters. It returns the scrubbed bytes and replacement count.
func ScrubSecrets(raw []byte, repoRoot string) ([]byte, int) {
	secrets := LoadSecrets(repoRoot)
	if len(secrets) == 0 {
		return raw, 0
	}
	opt := LoadScrubOptions(repoRoot)
	redact := opt.Redact
	if redact == "" {
		redact = RedactedToken
	}
	minLen := opt.MinLen
	if minLen <= 0 {
		minLen = 4
	}
	// Replace long values first — to prevent trailing parts from being exposed when shorter values are substrings of longer ones.
	// (e.g., "secret" should be replaced before "secretvalue" to avoid leaving "value").
	sort.Slice(secrets, func(i, j int) bool { return len(secrets[i]) > len(secrets[j]) })
	n := 0
	for _, v := range secrets {
		if len(v) < minLen {
			continue // Prevent excessive replacements (ignore too short values)
		}
		forms := []string{v}
		if enc, err := json.Marshal(v); err == nil {
			if e := string(enc[1 : len(enc)-1]); e != v { // Remove quotes from JSON escaped string
				forms = append(forms, e)
			}
		}
		for _, f := range forms {
			c := bytes.Count(raw, []byte(f))
			if c == 0 {
				continue
			}
			raw = bytes.ReplaceAll(raw, []byte(f), []byte(redact))
			n += c
		}
	}
	return raw, n
}

// GenerateFromEnv extracts values from repoRoot/.env (= after the = sign) to create .cxtsecrets.
// Does not modify if already exists (idempotent). Does not create if .env does not exist (prevents empty file — manually create or pull via web About ⚙ → cxt secrets pull).
// Returns: (number of extracted values, whether created).
func GenerateFromEnv(repoRoot string) (int, bool) {
	target := filepath.Join(repoRoot, SecretsFile)
	if _, err := os.Lstat(target); err == nil {
		return 0, false // Preserve
	}
	b, err := providerfs.ReadRepoFile(repoRoot, ".env")
	if err != nil {
		return 0, false // No .env — do not create
	}
	header := "# .cxtsecrets — List of secret values to mask before context is saved (one per line)\n" +
		"# This file does not upload to git or cxthub (plaintext prohibition).\n" +
		"# Team sharing is via web About ⚙ encrypted storage → cxt secrets pull.\n"
	var vals []string
	seen := map[string]bool{}
	for _, line := range strings.Split(string(b), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		eq := strings.Index(line, "=")
		if eq <= 0 {
			continue
		}
		v := strings.TrimSpace(line[eq+1:])
		v = strings.Trim(v, `"'`+"`")
		if len(v) < 4 || seen[v] { // Prevent excessive masking + dedup
			continue
		}
		seen[v] = true
		vals = append(vals, v)
	}
	content := header
	if len(vals) > 0 {
		content += "\n" + strings.Join(vals, "\n") + "\n"
	}
	if err := providerfs.WriteRepoFileAtomic(repoRoot, SecretsFile, []byte(content), 0o600); err != nil { // Owner-only permissions
		return 0, false
	}
	return len(vals), true
}
