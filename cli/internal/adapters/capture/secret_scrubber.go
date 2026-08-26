package capture

import (
	"math"
	"regexp"

	"github.com/wnsdy95/cxthub/cli/internal/domain"
)

// ScrubTier is a secret scrub strength tier (capture path).
type ScrubTier string

const (
	// ScrubOff does not perform scrubbing (local trusted environment only).
	ScrubOff ScrubTier = "off"
	// ScrubStandard masks known key formats + URL qualifications + PEM/JWT/Bearer.
	// Default value for auto hooks.
	ScrubStandard ScrubTier = "standard"
	// ScrubStrict adds standard + entropy heuristic + secret env-var substitution masking.
	// Recommended for push target repos. Note: may also mask quoted normal base64 (e.g., signature samples) in conversation bodies, opt-in.
	ScrubStrict ScrubTier = "strict"
)

// Masking format: «redacted:<kind>» (capture path).
// Pattern uses value-type (prefix fixed) only — must not conflict with JSON structure/keys and is deterministic.
// (Same input → same output → content-addressed dedup stable).
var scrubStandardPatterns = []struct {
	re   *regexp.Regexp
	repl string
}{
	// PEM private key block (multi-line) — first (before partial matching by other patterns).
	{regexp.MustCompile(`-----BEGIN [A-Z ]*PRIVATE KEY-----[\s\S]+?-----END [A-Z ]*PRIVATE KEY-----`), "«redacted:private-key»"},
	{regexp.MustCompile(`\bAKIA[0-9A-Z]{16}\b`), "«redacted:aws-key»"},
	{regexp.MustCompile(`\bgithub_pat_[A-Za-z0-9_]{22,}\b`), "«redacted:github-token»"},
	{regexp.MustCompile(`\bgh[pousr]_[A-Za-z0-9]{36,}\b`), "«redacted:github-token»"},
	{regexp.MustCompile(`\bsk-[A-Za-z0-9_-]{20,}\b`), "«redacted:api-key»"},
	{regexp.MustCompile(`\bxox[baprs]-[A-Za-z0-9-]{10,}\b`), "«redacted:slack-token»"},
	{regexp.MustCompile(`\beyJ[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]{8,}\b`), "«redacted:jwt»"},
	{regexp.MustCompile(`(?i)\b(bearer\s+)[A-Za-z0-9._~+/=-]{16,}`), "${1}«redacted:bearer»"},
	// URL credentials: scheme://user:password@host → only masks password.
	{regexp.MustCompile("\\b([a-z][a-z0-9+.-]*://[^/\\s:@\"'`]+:)[^@/\\s\"'`]+@"), "${1}«redacted:password»@"},
}

var scrubStrictPatterns = []struct {
	re   *regexp.Regexp
	repl string
}{
	// Env-var substitution for secret names (SECRET/TOKEN/PASSWORD/…=value). Excludes general variables like PATH=.
	{regexp.MustCompile(`\b([A-Z0-9_]*(?:SECRET|TOKEN|PASSWORD|PASSWD|APIKEY|API_KEY|PRIVATE_KEY|CREDENTIAL)[A-Z0-9_]*=)(["']?)[^\s"']{6,}`), "${1}${2}«redacted:env»"},
}

// Entropy candidate: base64 series of 40 or more consecutive runs. Pure hex is excluded —
// Development conversations are filled with git sha/sha256 hashes, hex masking destroys the body.
var entropyRunRe = regexp.MustCompile(`[A-Za-z0-9+/_=-]{40,}`)

// Scrub returns a new CIRDocument with sensitive information masked from the original (capture path).
//
// Masked targets: message blocks, tool_call input, tool_result output, and reasoning
// redacted_summary. reasoning.locked.blob is not masked because it is already opaque
// signature/ciphertext data, and modifying it would break full-fidelity rehydration.
func Scrub(cir domain.CIRDocument, tier ScrubTier) domain.CIRDocument {
	switch tier {
	case ScrubOff:
		return cir
	case ScrubStandard, ScrubStrict:
	default:
		tier = ScrubStandard // Default to safe base value
	}
	mask := func(s string) string { return maskString(s, tier) }
	out := cir
	out.Events = scrubEvents(cir.Events, mask)
	return out
}

func scrubEvents(input []domain.Event, mask func(string) string) []domain.Event {
	evs := make([]domain.Event, len(input))
	copy(evs, input)
	for i := range evs {
		ev := &evs[i]
		if ev.Replacement != nil {
			ev.Replacement = scrubEvents(ev.Replacement, mask)
		}
		if len(ev.Blocks) > 0 {
			bs := make([]domain.ContentBlock, len(ev.Blocks))
			copy(bs, ev.Blocks)
			for j := range bs {
				bs[j].Text = mask(bs[j].Text)
			}
			ev.Blocks = bs
		}
		if ev.RedactedSummary != "" {
			ev.RedactedSummary = mask(ev.RedactedSummary)
		}
		if ev.Input != nil {
			ev.Input = maskValue(ev.Input, mask).(map[string]interface{})
		}
		if ev.Output != nil {
			ev.Output = maskValue(ev.Output, mask)
		}
	}
	return evs
}

// ScrubDoc is a common entry point for executing Scrub based on the repoRoot setting (secrets.scrub — off|standard|strict, default standard).
func ScrubDoc(cir domain.CIRDocument, repoRoot string) domain.CIRDocument {
	tier := LoadScrubOptions(repoRoot).Tier
	if tier == "" {
		tier = ScrubStandard
	}
	return Scrub(cir, tier)
}

func maskString(s string, tier ScrubTier) string {
	if s == "" {
		return s
	}
	for _, p := range scrubStandardPatterns {
		s = p.re.ReplaceAllString(s, p.repl)
	}
	if tier == ScrubStrict {
		for _, p := range scrubStrictPatterns {
			s = p.re.ReplaceAllString(s, p.repl)
		}
		s = entropyRunRe.ReplaceAllStringFunc(s, func(run string) string {
			if isLikelySecretRun(run) {
				return "«redacted:high-entropy»"
			}
			return run
		})
	}
	return s
}

// isLikelySecretRun determines the likelihood of a base64 run being a secret:
// Mixed case·numbers + not pure hex + Shannon entropy ≥ 4.2 bits/char.
func isLikelySecretRun(run string) bool {
	var hasUpper, hasLower, hasDigit, nonHex bool
	for _, r := range run {
		switch {
		case r >= 'A' && r <= 'Z':
			hasUpper = true
			if r > 'F' {
				nonHex = true
			}
		case r >= 'a' && r <= 'z':
			hasLower = true
			if r > 'f' {
				nonHex = true
			}
		case r >= '0' && r <= '9':
			hasDigit = true
		default:
			nonHex = true
		}
	}
	if !hasUpper || !hasLower || !hasDigit || !nonHex {
		return false
	}
	return shannonEntropy(run) >= 4.2
}

func shannonEntropy(s string) float64 {
	freq := map[rune]float64{}
	for _, r := range s {
		freq[r]++
	}
	n := float64(len([]rune(s)))
	e := 0.0
	for _, c := range freq {
		p := c / n
		e -= p * math.Log2(p)
	}
	return e
}

func maskValue(v interface{}, mask func(string) string) interface{} {
	switch t := v.(type) {
	case string:
		return mask(t)
	case map[string]interface{}:
		m := make(map[string]interface{}, len(t))
		for k, vv := range t {
			m[k] = maskValue(vv, mask)
		}
		return m
	case []interface{}:
		l := make([]interface{}, len(t))
		for i, vv := range t {
			l[i] = maskValue(vv, mask)
		}
		return l
	default:
		return v
	}
}
