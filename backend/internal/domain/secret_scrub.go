package domain

import "regexp"

// secretScrubRules are the known secret formats and what replaces each. They
// are the patterns the CLI applies to a captured session at its standard tier
// (cli/internal/adapters/capture/secret_scrubber.go), in the same order; a
// test in the CLI compares the two lists.
var secretScrubRules = [][2]string{
	{`-----BEGIN [A-Z ]*PRIVATE KEY-----[\s\S]+?-----END [A-Z ]*PRIVATE KEY-----`, "«redacted:private-key»"},
	{`\bAKIA[0-9A-Z]{16}\b`, "«redacted:aws-key»"},
	{`\bgithub_pat_[A-Za-z0-9_]{22,}\b`, "«redacted:github-token»"},
	{`\bgh[pousr]_[A-Za-z0-9]{36,}\b`, "«redacted:github-token»"},
	{`\bsk-[A-Za-z0-9_-]{20,}\b`, "«redacted:api-key»"},
	{`\bxox[baprs]-[A-Za-z0-9-]{10,}\b`, "«redacted:slack-token»"},
	{`\beyJ[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]{8,}\b`, "«redacted:jwt»"},
	{`(?i)\b(bearer\s+)[A-Za-z0-9._~+/=-]{16,}`, "${1}«redacted:bearer»"},
	{"\\b([a-z][a-z0-9+.-]*://[^/\\s:@\"'`]+:)[^@/\\s\"'`]+@", "${1}«redacted:password»@"},
}

var secretScrubPatterns = func() []*regexp.Regexp {
	patterns := make([]*regexp.Regexp, len(secretScrubRules))
	for index, rule := range secretScrubRules {
		patterns[index] = regexp.MustCompile(rule[0])
	}
	return patterns
}()

// ScrubSecretPatterns masks known secret formats in text a person typed into
// the product, the way a captured session is masked before it is stored.
//
// The CLI also replaces the literal values a repository lists in its own
// secrets file. Those values never reach the server, so only the patterns can
// be applied here.
func ScrubSecretPatterns(text string) string {
	for index, pattern := range secretScrubPatterns {
		text = pattern.ReplaceAllString(text, secretScrubRules[index][1])
	}
	return text
}
