package domain

import (
	"regexp"
	"strings"
	"unicode/utf8"
)

var (
	// A query string may carry a remote URL with credentials or a signed
	// parameter. Paths stay: they identify the operation and hold only IDs.
	errorTextQuery = regexp.MustCompile(`\?[^\s"']*`)
	// Opaque runs (bearer tokens, cookies, challenge tokens, base64 bodies).
	errorTextOpaque = regexp.MustCompile(`[A-Za-z0-9_\-.+=~]{32,}`)
	errorTextHex    = regexp.MustCompile(`^[0-9a-f]+$`)
)

// SanitizeErrorText makes an error message safe to persist or print in
// queue status: it drops URL query strings, redacts opaque token-like runs
// (lowercase hex content hashes are kept), collapses whitespace and bounds
// the length. It is a display filter, never a classifier.
func SanitizeErrorText(text string, limit int) string {
	text = errorTextQuery.ReplaceAllString(text, "?[redacted]")
	text = errorTextOpaque.ReplaceAllStringFunc(text, func(run string) string {
		if errorTextHex.MatchString(run) {
			return run
		}
		return "[redacted]"
	})
	text = strings.Join(strings.Fields(text), " ")
	if limit > 0 && len(text) > limit {
		cut := limit
		for cut > 0 && !utf8.RuneStart(text[cut]) {
			cut--
		}
		text = text[:cut] + "…"
	}
	return text
}
