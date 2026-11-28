package capture

import "github.com/wnsdy95/cxthub-cli/internal/domain"

// ScrubNativeMemory returns the tool's own memory with secret values hidden
// by the two steps a capture applies to it: the repository's listed secret
// values, then the pattern scrub of the configured tier.
func ScrubNativeMemory(root string, native domain.NativeMemory) domain.NativeMemory {
	tier := LoadScrubOptions(root).Tier
	mask := func(value string) string {
		clean, _ := ScrubSecrets([]byte(value), root)
		if tier == ScrubOff {
			return string(clean)
		}
		return maskString(string(clean), tier)
	}
	out := native
	out.Text = mask(out.Text)
	out.AutoLoadedPrefix = mask(out.AutoLoadedPrefix)
	if native.Structured != nil {
		out.Structured = make(map[string]string, len(native.Structured))
		for key, value := range native.Structured {
			out.Structured[key] = mask(value)
		}
	}
	return out
}
