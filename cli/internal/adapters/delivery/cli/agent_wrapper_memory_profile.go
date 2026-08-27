package cli

import (
	"encoding/json"
	"strings"
)

const maxClaudeFlagSettingsBytes = 1 << 20

type claudeFlagMemorySettings struct {
	AutoMemoryDirectory *string `json:"autoMemoryDirectory"`
	AutoMemoryEnabled   *bool   `json:"autoMemoryEnabled"`
}

// claudeMemoryProfileEnv projects only auto-memory-relevant launch options
// into the supervised child. Hook subprocesses can then reproduce the exact
// settings sources without copying arbitrary --settings JSON (which may
// contain credentials) into their environment.
func claudeMemoryProfileEnv(cwd string, args []string) []string {
	unknown := func() []string {
		return []string{
			"CXT_CLAUDE_MEMORY_PROFILE=unknown",
			"CXT_CLAUDE_SETTING_SOURCES=",
			"CXT_CLAUDE_BARE=",
			"CXT_CLAUDE_SAFE_MODE=",
			"CXT_CLAUDE_FLAG_AUTO_MEMORY_DIRECTORY=",
			"CXT_CLAUDE_FLAG_AUTO_MEMORY_ENABLED=",
			"CXT_CLAUDE_MEMORY_CONFIG_FINGERPRINT=",
		}
	}
	profile := claudeFlagMemorySettings{}
	sources := "user,project,local"
	bare := false
	safeMode := false
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if arg == "--" {
			break
		}
		switch {
		case arg == "--bare":
			bare = true
		case arg == "--safe-mode":
			safeMode = true
		case arg == "--settings":
			if i+1 >= len(args) {
				return unknown()
			}
			i++
			settings, ok := readClaudeFlagMemorySettings(cwd, args[i])
			if !ok {
				return unknown()
			}
			mergeClaudeFlagMemorySettings(&profile, settings)
		case strings.HasPrefix(arg, "--settings="):
			settings, ok := readClaudeFlagMemorySettings(cwd, strings.TrimPrefix(arg, "--settings="))
			if !ok {
				return unknown()
			}
			mergeClaudeFlagMemorySettings(&profile, settings)
		case arg == "--setting-sources":
			if i+1 >= len(args) {
				return unknown()
			}
			i++
			var ok bool
			sources, ok = normalizeClaudeSettingSources(args[i])
			if !ok {
				return unknown()
			}
		case strings.HasPrefix(arg, "--setting-sources="):
			var ok bool
			sources, ok = normalizeClaudeSettingSources(strings.TrimPrefix(arg, "--setting-sources="))
			if !ok {
				return unknown()
			}
		}
	}

	directory := ""
	if profile.AutoMemoryDirectory != nil {
		directory = *profile.AutoMemoryDirectory
	}
	enabled := ""
	if profile.AutoMemoryEnabled != nil {
		enabled = "false"
		if *profile.AutoMemoryEnabled {
			enabled = "true"
		}
	}
	bareValue := ""
	if bare {
		bareValue = "true"
	}
	safeModeValue := ""
	if safeMode {
		safeModeValue = "true"
	}
	return []string{
		"CXT_CLAUDE_MEMORY_PROFILE=v1",
		"CXT_CLAUDE_SETTING_SOURCES=" + sources,
		"CXT_CLAUDE_BARE=" + bareValue,
		"CXT_CLAUDE_SAFE_MODE=" + safeModeValue,
		"CXT_CLAUDE_FLAG_AUTO_MEMORY_DIRECTORY=" + directory,
		"CXT_CLAUDE_FLAG_AUTO_MEMORY_ENABLED=" + enabled,
		"CXT_CLAUDE_MEMORY_CONFIG_FINGERPRINT=",
	}
}

func readClaudeFlagMemorySettings(_ string, value string) (claudeFlagMemorySettings, bool) {
	value = strings.TrimSpace(value)
	if value == "" {
		return claudeFlagMemorySettings{}, false
	}
	var data []byte
	if strings.HasPrefix(value, "{") {
		data = []byte(value)
	} else {
		// Claude reads external --settings files after cxt builds this
		// profile. Without a provider acknowledgement there is no way to
		// prove both processes observed the same bytes, so retain the portable
		// baseline instead of projecting a potentially stale path.
		return claudeFlagMemorySettings{}, false
	}
	if len(data) > maxClaudeFlagSettingsBytes {
		return claudeFlagMemorySettings{}, false
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		return claudeFlagMemorySettings{}, false
	}
	relevant := false
	unmodeled := false
	for key := range raw {
		switch key {
		case "autoMemoryDirectory", "autoMemoryEnabled":
			relevant = true
		default:
			if !strings.HasPrefix(key, "$") {
				unmodeled = true
			}
		}
	}
	if relevant && unmodeled {
		return claudeFlagMemorySettings{}, false
	}
	var settings claudeFlagMemorySettings
	if err := json.Unmarshal(data, &settings); err != nil {
		return claudeFlagMemorySettings{}, false
	}
	return settings, true
}

func mergeClaudeFlagMemorySettings(dst *claudeFlagMemorySettings, src claudeFlagMemorySettings) {
	if src.AutoMemoryDirectory != nil {
		dst.AutoMemoryDirectory = src.AutoMemoryDirectory
	}
	if src.AutoMemoryEnabled != nil {
		dst.AutoMemoryEnabled = src.AutoMemoryEnabled
	}
}

func normalizeClaudeSettingSources(value string) (string, bool) {
	if strings.TrimSpace(value) == "" {
		return "none", true
	}
	seen := map[string]bool{}
	var out []string
	for _, source := range strings.Split(value, ",") {
		source = strings.TrimSpace(source)
		switch source {
		case "user", "project", "local":
			if !seen[source] {
				seen[source] = true
				out = append(out, source)
			}
		default:
			return "", false
		}
	}
	return strings.Join(out, ","), true
}
