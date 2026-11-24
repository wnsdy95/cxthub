package domain

import (
	"strings"
	"unicode"
	"unicode/utf8"
)

// RecordedPush is a push this clone made, as Git recorded it for a branch of
// origin: the branch, the commit the branch was moved to, and when.
type RecordedPush struct {
	Branch string `json:"branch"`
	SHA    string `json:"sha"`
	AtSec  int64  `json:"at"`
}

// RecordedPushLimit is how many of a clone's pushes are read, the newest
// first. Git keeps these records for some months; what is older than the
// newest few hundred says nothing a reader still asks about.
const RecordedPushLimit = 200

// CollectorReport is what the collector of this computer tells the server
// about itself: the newest pushes this clone made, and what went wrong in its
// run, empty when nothing did. With it the server can say of a push that came
// without a session whether it was made where cxt runs.
type CollectorReport struct {
	Machine  string         `json:"machine,omitempty"`
	Version  string         `json:"cxt_version,omitempty"`
	RunError string         `json:"run_error,omitempty"`
	Pushes   []RecordedPush `json:"pushes"`
}

// The server takes a version of at most 64 bytes and an error of at most 512,
// each one line of plain text.
const (
	CollectorVersionLimit = 64
	CollectorErrorLimit   = 512
)

// CollectorLine makes a text one line the server takes: line breaks and other
// control characters become spaces, runs of spaces become one, and what is
// longer than limit bytes is cut between two characters.
func CollectorLine(text string, limit int) string {
	text = strings.Join(strings.FieldsFunc(text, func(r rune) bool { return unicode.IsSpace(r) || unicode.IsControl(r) }), " ")
	if len(text) <= limit {
		return text
	}
	cut := limit
	for cut > 0 && !utf8.RuneStart(text[cut]) {
		cut--
	}
	return strings.TrimRight(text[:cut], " ")
}

// CollectorRunError says what went wrong in a run, as the report carries it:
// nothing for a run that ended well, and never nothing for one that did not.
func CollectorRunError(err error) string {
	if err == nil {
		return ""
	}
	if text := CollectorLine(err.Error(), CollectorErrorLimit); text != "" {
		return text
	}
	return "the run failed"
}
