package memory

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf16"

	"github.com/wnsdy95/cxthub/cli/internal/adapters/providerfs"
)

const maxClaudeSettingsBytes = 1 << 20

var (
	claudeProjectNamePattern  = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)
	claudeReservedProjectName = regexp.MustCompile(`(?i)^(?:con|prn|aux|nul|com[0-9]|lpt[0-9])$`)
	errClaudeSettingsTooLarge = errors.New("Claude settings exceed safe read limit")
)

type claudeMemorySettings struct {
	AutoMemoryDirectory *string         `json:"autoMemoryDirectory"`
	AutoMemoryEnabled   *bool           `json:"autoMemoryEnabled"`
	PolicyHelper        json.RawMessage `json:"policyHelper"`
}

type claudeMemoryResolution struct {
	memoryDir string
	enabled   bool
	proven    bool
}

// resolveClaudeAutoMemory mirrors Claude Code's repository-scoped auto-memory
// identity. The working-tree root remains separate because project/local
// settings are worktree-specific even though the memory directory is shared.
func resolveClaudeAutoMemory(ctx context.Context, cwd string) claudeMemoryResolution {
	identityRoot, worktreeRoot := claudeRepositoryRoots(ctx, cwd)
	configRoot, customConfig := claudeConfigRoot()
	if configRoot == "" {
		return claudeMemoryResolution{enabled: false, proven: false}
	}
	settings, proven := effectiveClaudeMemorySettings(ctx, configRoot, worktreeRoot)
	if !claudeAutoMemoryRuntimeProven() {
		proven = false
	}
	launchFingerprint := strings.TrimSpace(os.Getenv("CXT_CLAUDE_MEMORY_CONFIG_FINGERPRINT"))
	if launchFingerprint == "" || launchFingerprint != ClaudeMemoryConfigFingerprint(ctx, cwd) {
		proven = false
	}

	if claudeAutoMemoryDisabledByEnv() {
		return claudeMemoryResolution{enabled: false, proven: proven}
	}
	if settings.AutoMemoryEnabled != nil && !*settings.AutoMemoryEnabled {
		return claudeMemoryResolution{enabled: false, proven: proven}
	}

	memoryDir := ""
	if settings.AutoMemoryDirectory != nil {
		var ok bool
		memoryDir, ok = resolveClaudeMemoryDirectory(*settings.AutoMemoryDirectory)
		if !ok {
			return claudeMemoryResolution{enabled: false, proven: false}
		}
	} else {
		projectKey := claudeProjectKey(identityRoot)
		if customConfig {
			if override, ok := validClaudeProjectName(os.Getenv("CLAUDE_CODE_PROJECT_DIR_NAME")); ok {
				projectKey = override
			}
		}
		memoryDir = filepath.Join(configRoot, "projects", projectKey, "memory")
	}
	return claudeMemoryResolution{memoryDir: memoryDir, enabled: true, proven: proven}
}

// ClaudeMemoryConfigFingerprint binds a supervised Claude launch to every
// observable input that can change auto-memory enablement or location. It
// contains no settings values; only the SHA-256 digest crosses into the child
// environment. A later mismatch makes native-memory projection fail closed.
func ClaudeMemoryConfigFingerprint(ctx context.Context, cwd string) string {
	_, worktreeRoot := claudeRepositoryRoots(ctx, cwd)
	configRoot, _ := claudeConfigRoot()
	hash := sha256.New()
	write := func(value string) {
		_, _ = hash.Write([]byte(value))
		_, _ = hash.Write([]byte{0})
	}
	for _, name := range []string{
		"CLAUDE_CONFIG_DIR",
		"CLAUDE_CODE_PROJECT_DIR_NAME",
		"CLAUDE_CODE_DISABLE_AUTO_MEMORY",
		"CLAUDE_CODE_SIMPLE",
		"CLAUDE_CODE_SAFE_MODE",
		"CLAUDE_CODE_MANAGED_SETTINGS_PATH",
		"CLAUDE_CODE_REMOTE",
		"CLAUDE_CODE_REMOTE_MEMORY_DIR",
		"CLAUDE_COWORK_MEMORY_PATH_OVERRIDE",
		"CLAUDE_MEMORY_STORES",
	} {
		write(name + "=" + os.Getenv(name))
	}
	if _, _, mdm, err := readClaudeMDMMemorySettings(ctx); err != nil {
		write("mdm-error:" + err.Error())
	} else if len(mdm) > 0 {
		digest := sha256.Sum256(mdm)
		write(fmt.Sprintf("mdm-sha256:%x", digest[:]))
	}
	var paths []string
	if configRoot == "" {
		write("config-root-unavailable")
	} else {
		paths = append(paths,
			filepath.Join(configRoot, "settings.json"),
			filepath.Join(configRoot, "remote-settings.json"),
		)
	}
	if worktreeRoot != "" {
		paths = append(paths,
			filepath.Join(worktreeRoot, ".claude", "settings.json"),
			filepath.Join(worktreeRoot, ".claude", "settings.local.json"),
		)
	}
	if managed := knownClaudeManagedSettingsPath(); managed != "" {
		paths = append(paths, managed)
		dropinDir := filepath.Join(filepath.Dir(managed), "managed-settings.d")
		entries, err := os.ReadDir(dropinDir)
		if err != nil {
			write("dir:" + dropinDir + ":" + err.Error())
		} else {
			for _, entry := range entries {
				if strings.HasSuffix(entry.Name(), ".json") {
					paths = append(paths, filepath.Join(dropinDir, entry.Name()))
				}
			}
		}
	}
	sort.Strings(paths)
	for _, path := range paths {
		write("path:" + path)
		data, err := readClaudeRegularSettingsFile(path)
		if err != nil {
			write("error:" + err.Error())
			continue
		}
		digest := sha256.Sum256(data)
		write(fmt.Sprintf("sha256:%x", digest[:]))
	}
	return fmt.Sprintf("%x", hash.Sum(nil))
}

func claudeRepositoryRoots(ctx context.Context, cwd string) (identityRoot, worktreeRoot string) {
	canonical := canonicalClaudePath(cwd)
	identityRoot, worktreeRoot = canonical, canonical

	if top, err := gitPath(ctx, cwd, "rev-parse", "--show-toplevel"); err == nil && top != "" {
		worktreeRoot = canonicalClaudePath(top)
		identityRoot = worktreeRoot
	}
	if common, err := gitPath(ctx, cwd, "rev-parse", "--git-common-dir"); err == nil && common != "" {
		if !filepath.IsAbs(common) {
			common = filepath.Join(cwd, common)
		}
		common = canonicalClaudePath(common)
		if filepath.Base(common) == ".git" {
			identityRoot = filepath.Dir(common)
		} else {
			identityRoot = common
		}
	}
	return identityRoot, worktreeRoot
}

func gitPath(ctx context.Context, cwd string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", append([]string{"-C", cwd}, args...)...)
	out, err := cmd.Output()
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

func canonicalClaudePath(path string) string {
	abs, err := filepath.Abs(path)
	if err != nil {
		abs = path
	}
	if real, err := filepath.EvalSymlinks(abs); err == nil {
		abs = real
	}
	return filepath.Clean(abs)
}

func claudeConfigRoot() (string, bool) {
	if configured := strings.TrimSpace(os.Getenv("CLAUDE_CONFIG_DIR")); configured != "" {
		return canonicalClaudePath(configured), true
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", false
	}
	return filepath.Join(home, ".claude"), false
}

func claudeProjectKey(root string) string {
	encoded := claudeEncodeProjectRoot(root)
	if len(encoded) <= 200 {
		return encoded
	}
	return encoded[:200] + "-" + strconv.FormatUint(uint64(absJSStringHash(root)), 36)
}

func claudeEncodeProjectRoot(root string) string {
	units := utf16.Encode([]rune(root))
	var encoded strings.Builder
	encoded.Grow(len(units))
	for _, unit := range units {
		if (unit >= 'a' && unit <= 'z') || (unit >= 'A' && unit <= 'Z') || (unit >= '0' && unit <= '9') {
			encoded.WriteByte(byte(unit))
		} else {
			encoded.WriteByte('-')
		}
	}
	return encoded.String()
}

// absJSStringHash matches Claude Code's 32-bit JavaScript string hash. JS
// charCodeAt iterates UTF-16 code units, so non-BMP paths must not hash Go
// runes or UTF-8 bytes.
func absJSStringHash(value string) uint32 {
	var hash int32
	for _, unit := range utf16.Encode([]rune(value)) {
		hash = hash*31 + int32(unit)
	}
	if hash < 0 {
		return uint32(-int64(hash))
	}
	return uint32(hash)
}

func validClaudeProjectName(value string) (string, bool) {
	if !claudeProjectNamePattern.MatchString(value) || claudeReservedProjectName.MatchString(value) {
		return "", false
	}
	return value, true
}

func resolveClaudeMemoryDirectory(value string) (string, bool) {
	value = strings.TrimSpace(value)
	if strings.HasPrefix(value, "~/") {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", false
		}
		value = filepath.Join(home, strings.TrimPrefix(value, "~/"))
	}
	if !filepath.IsAbs(value) || strings.ContainsRune(value, 0) {
		return "", false
	}
	value = filepath.Clean(value)
	if value == string(filepath.Separator) || value == "." {
		return "", false
	}
	return value, true
}

func claudeAutoMemoryDisabledByEnv() bool {
	return envTruthy("CLAUDE_CODE_DISABLE_AUTO_MEMORY") || envTruthy("CLAUDE_CODE_SIMPLE") || envTruthy("CLAUDE_CODE_SAFE_MODE") ||
		envTruthy("CXT_CLAUDE_BARE") || envTruthy("CXT_CLAUDE_SAFE_MODE") ||
		(os.Getenv("CLAUDE_CODE_REMOTE") != "" && os.Getenv("CLAUDE_CODE_REMOTE_MEMORY_DIR") == "" && os.Getenv("CLAUDE_COWORK_MEMORY_PATH_OVERRIDE") == "")
}

func claudeAutoMemoryRuntimeProven() bool {
	return os.Getenv("CLAUDE_CODE_REMOTE_MEMORY_DIR") == "" &&
		os.Getenv("CLAUDE_COWORK_MEMORY_PATH_OVERRIDE") == "" &&
		os.Getenv("CLAUDE_MEMORY_STORES") == ""
}

func envTruthy(name string) bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv(name))) {
	case "1", "true", "yes", "on":
		return true
	default:
		return false
	}
}

// effectiveClaudeMemorySettings applies the documented user < project < local
// precedence. autoMemoryDirectory is intentionally accepted only from the
// trusted user layer; repository-controlled redirects are ignored.
func effectiveClaudeMemorySettings(ctx context.Context, configRoot, worktreeRoot string) (claudeMemorySettings, bool) {
	var effective claudeMemorySettings
	profile, proven := claudeMemoryLaunchProfile()
	if !proven {
		return effective, false
	}
	apply := func(path string, trustedDirectory bool, repoRelative bool) {
		settings, found, err := readClaudeMemorySettings(path, worktreeRoot, repoRelative)
		if err != nil {
			proven = false
			return
		}
		if !found {
			return
		}
		if trustedDirectory && settings.AutoMemoryDirectory != nil {
			effective.AutoMemoryDirectory = settings.AutoMemoryDirectory
		}
		if settings.AutoMemoryEnabled != nil {
			effective.AutoMemoryEnabled = settings.AutoMemoryEnabled
		}
	}
	if profile.sources["user"] {
		apply(filepath.Join(configRoot, "settings.json"), true, false)
	}
	if worktreeRoot != "" {
		if profile.sources["project"] {
			apply(filepath.Join(worktreeRoot, ".claude", "settings.json"), false, true)
		}
		if profile.sources["local"] {
			apply(filepath.Join(worktreeRoot, ".claude", "settings.local.json"), false, true)
		}
	}
	if profile.flag.AutoMemoryDirectory != nil {
		effective.AutoMemoryDirectory = profile.flag.AutoMemoryDirectory
	}
	if profile.flag.AutoMemoryEnabled != nil {
		effective.AutoMemoryEnabled = profile.flag.AutoMemoryEnabled
	}
	managed, found, err := readClaudeManagedMemorySettings(ctx, configRoot)
	if err != nil {
		proven = false
	} else if found {
		if managed.PolicyHelper != nil && string(managed.PolicyHelper) != "null" {
			// The helper's runtime output replaces file-based policy. cxt cannot
			// safely execute an administrator command while loading context.
			proven = false
		}
		if managed.AutoMemoryDirectory != nil {
			effective.AutoMemoryDirectory = managed.AutoMemoryDirectory
		}
		if managed.AutoMemoryEnabled != nil {
			effective.AutoMemoryEnabled = managed.AutoMemoryEnabled
		}
	}
	return effective, proven
}

type claudeLaunchProfile struct {
	sources map[string]bool
	flag    claudeMemorySettings
}

func claudeMemoryLaunchProfile() (claudeLaunchProfile, bool) {
	profile := claudeLaunchProfile{sources: map[string]bool{}}
	if os.Getenv("CXT_CLAUDE_MEMORY_PROFILE") != "v1" {
		return profile, false
	}
	sources, sourcesSet := os.LookupEnv("CXT_CLAUDE_SETTING_SOURCES")
	sources = strings.TrimSpace(sources)
	if !sourcesSet {
		sources = "user,project,local"
	}
	if sources == "none" {
		return profile, true
	}
	for _, source := range strings.Split(sources, ",") {
		source = strings.TrimSpace(source)
		switch source {
		case "user", "project", "local":
			profile.sources[source] = true
		case "":
		default:
			return profile, false
		}
	}
	if value := os.Getenv("CXT_CLAUDE_FLAG_AUTO_MEMORY_DIRECTORY"); value != "" {
		profile.flag.AutoMemoryDirectory = &value
	}
	if value := strings.TrimSpace(os.Getenv("CXT_CLAUDE_FLAG_AUTO_MEMORY_ENABLED")); value != "" {
		switch value {
		case "true":
			enabled := true
			profile.flag.AutoMemoryEnabled = &enabled
		case "false":
			enabled := false
			profile.flag.AutoMemoryEnabled = &enabled
		default:
			return profile, false
		}
	}
	return profile, true
}

func readClaudeMemorySettings(path, repoRoot string, repoRelative bool) (claudeMemorySettings, bool, error) {
	var (
		data []byte
		err  error
	)
	if repoRelative {
		rel, relErr := filepath.Rel(repoRoot, path)
		if relErr != nil {
			return claudeMemorySettings{}, false, relErr
		}
		data, err = readClaudeRepoSettingsFile(repoRoot, rel)
	} else {
		data, err = readClaudeRegularSettingsFile(path)
	}
	if errors.Is(err, os.ErrNotExist) {
		return claudeMemorySettings{}, false, nil
	}
	if err != nil {
		return claudeMemorySettings{}, false, err
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		return claudeMemorySettings{}, false, err
	}
	if err := validateClaudeMemoryFieldIsolation(raw, false); err != nil {
		return claudeMemorySettings{}, false, err
	}
	var settings claudeMemorySettings
	if err := json.Unmarshal(data, &settings); err != nil {
		return claudeMemorySettings{}, false, err
	}
	return settings, true, nil
}

// readClaudeManagedMemorySettings follows the managed-source ordering that is
// observable without executing administrator code: server cache first, then
// the selected file-based policy and its sorted drop-ins. A policyHelper makes
// the result unproven because its output replaces file policy at runtime.
func readClaudeManagedMemorySettings(ctx context.Context, configRoot string) (claudeMemorySettings, bool, error) {
	remotePath := filepath.Join(configRoot, "remote-settings.json")
	remote, active, err := readClaudeManagedDocument(remotePath)
	if errors.Is(err, os.ErrNotExist) {
		err = nil
	}
	if err != nil {
		return claudeMemorySettings{}, false, err
	}
	if active {
		return remote, true, nil
	}
	mdm, active, _, err := readClaudeMDMMemorySettings(ctx)
	if err != nil {
		return claudeMemorySettings{}, false, err
	}
	if active {
		return mdm, true, nil
	}

	base := knownClaudeManagedSettingsPath()
	if base == "" {
		return claudeMemorySettings{}, false, nil
	}
	var effective claudeMemorySettings
	found := false
	applyFile := func(path string, required bool) error {
		settings, active, readErr := readClaudeManagedDocument(path)
		if errors.Is(readErr, os.ErrNotExist) && !required {
			return nil
		}
		if readErr != nil {
			return readErr
		}
		if !active {
			return nil
		}
		found = true
		if settings.AutoMemoryDirectory != nil {
			effective.AutoMemoryDirectory = settings.AutoMemoryDirectory
		}
		if settings.AutoMemoryEnabled != nil {
			effective.AutoMemoryEnabled = settings.AutoMemoryEnabled
		}
		if settings.PolicyHelper != nil {
			effective.PolicyHelper = settings.PolicyHelper
		}
		return nil
	}
	required := strings.TrimSpace(os.Getenv("CLAUDE_CODE_MANAGED_SETTINGS_PATH")) != ""
	if err := applyFile(base, required); err != nil {
		return claudeMemorySettings{}, false, err
	}
	dropinDir := filepath.Join(filepath.Dir(base), "managed-settings.d")
	entries, err := os.ReadDir(dropinDir)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return claudeMemorySettings{}, false, err
	}
	var names []string
	for _, entry := range entries {
		if entry.Type().IsRegular() && strings.HasSuffix(entry.Name(), ".json") {
			names = append(names, entry.Name())
		}
	}
	sort.Strings(names)
	for _, name := range names {
		if err := applyFile(filepath.Join(dropinDir, name), true); err != nil {
			return claudeMemorySettings{}, false, err
		}
	}
	return effective, found, nil
}

func readClaudeMDMMemorySettings(ctx context.Context) (claudeMemorySettings, bool, []byte, error) {
	if runtime.GOOS != "darwin" {
		return claudeMemorySettings{}, false, nil, nil
	}
	mdmCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	exported, err := exec.CommandContext(mdmCtx, "/usr/bin/defaults", "export", "com.anthropic.claudecode", "-").Output()
	if err != nil {
		return claudeMemorySettings{}, false, nil, err
	}
	if len(exported) > maxClaudeSettingsBytes {
		return claudeMemorySettings{}, false, nil, errors.New("Claude MDM settings exceed safe read limit")
	}
	plutil := exec.CommandContext(mdmCtx, "/usr/bin/plutil", "-convert", "json", "-o", "-", "--", "-")
	plutil.Stdin = bytes.NewReader(exported)
	data, err := plutil.Output()
	if err != nil {
		return claudeMemorySettings{}, false, nil, err
	}
	settings, active, err := decodeClaudeManagedDocument(data)
	return settings, active, data, err
}

func readClaudeManagedDocument(path string) (claudeMemorySettings, bool, error) {
	data, err := readClaudeRegularSettingsFile(path)
	if err != nil {
		return claudeMemorySettings{}, false, err
	}
	return decodeClaudeManagedDocument(data)
}

func readClaudeRegularSettingsFile(path string) ([]byte, error) {
	f, err := providerfs.OpenRegularFile(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return readClaudeSettingsBytes(f)
}

func readClaudeRepoSettingsFile(repoRoot, relative string) ([]byte, error) {
	f, err := providerfs.OpenRepoFile(repoRoot, relative)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return readClaudeSettingsBytes(f)
}

func readClaudeSettingsBytes(r io.Reader) ([]byte, error) {
	data, err := io.ReadAll(io.LimitReader(r, maxClaudeSettingsBytes+1))
	if err != nil {
		return nil, err
	}
	if len(data) > maxClaudeSettingsBytes {
		return nil, errClaudeSettingsTooLarge
	}
	return data, nil
}

func decodeClaudeManagedDocument(data []byte) (claudeMemorySettings, bool, error) {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		return claudeMemorySettings{}, false, err
	}
	if err := validateClaudeMemoryFieldIsolation(raw, true); err != nil {
		return claudeMemorySettings{}, false, err
	}
	active := false
	for key := range raw {
		if !strings.HasPrefix(key, "$") || key == "$schema" {
			active = true
			break
		}
	}
	if !active {
		return claudeMemorySettings{}, false, nil
	}
	var settings claudeMemorySettings
	if err := json.Unmarshal(data, &settings); err != nil {
		return claudeMemorySettings{}, false, err
	}
	return settings, true, nil
}

func validateClaudeMemoryFieldIsolation(raw map[string]json.RawMessage, managed bool) error {
	relevant := false
	unmodeled := false
	for key := range raw {
		switch key {
		case "autoMemoryDirectory", "autoMemoryEnabled":
			relevant = true
		case "policyHelper":
			if managed {
				relevant = true
			} else {
				unmodeled = true
			}
		default:
			if !strings.HasPrefix(key, "$") {
				unmodeled = true
			}
		}
	}
	if relevant && unmodeled {
		return errors.New("Claude auto-memory settings share a document with fields requiring provider validation")
	}
	return nil
}

func knownClaudeManagedSettingsPath() string {
	if path := strings.TrimSpace(os.Getenv("CLAUDE_CODE_MANAGED_SETTINGS_PATH")); path != "" {
		return path
	}
	switch runtime.GOOS {
	case "darwin":
		return "/Library/Application Support/ClaudeCode/managed-settings.json"
	case "linux":
		return "/etc/claude-code/managed-settings.json"
	default:
		return ""
	}
}
