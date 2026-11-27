package domain

import (
	"errors"
	"strings"
	"testing"
)

// A Git object ID names a commit in full: 40 or 64 hexadecimal digits in
// lower case, and not the all-zero number Git uses for "no commit".
func TestGitObjectIDIsAFullLowerCaseNumber(t *testing.T) {
	for _, valid := range []string{
		strings.Repeat("a", 40), strings.Repeat("f", 64), "0123456789abcdef0123456789abcdef01234567",
		strings.Repeat("0", 39) + "1", "1" + strings.Repeat("0", 63),
	} {
		if err := ValidateGitOID(valid); err != nil {
			t.Errorf("%q: %v", valid, err)
		}
	}
	for name, invalid := range map[string]string{
		"nothing":                     "",
		"a short number":              "abc1234",
		"a short number of even size": "abcd",
		"one digit short":             strings.Repeat("a", 39),
		"one digit long":              strings.Repeat("a", 41),
		"between the two sizes":       strings.Repeat("a", 52),
		"one digit short of the long": strings.Repeat("a", 63),
		"longer than the long size":   strings.Repeat("a", 66),
		"upper case":                  strings.Repeat("A", 40),
		"one upper-case digit":        strings.Repeat("a", 39) + "F",
		"no commit":                   strings.Repeat("0", 40),
		"no commit, in the long size": strings.Repeat("0", 64),
		"a letter that is no digit":   strings.Repeat("g", 40),
		"one letter that is no digit": strings.Repeat("a", 39) + "z",
		"a space inside":              strings.Repeat("a", 20) + " " + strings.Repeat("a", 19),
		"a branch name":               "refs/heads/main-branch-with-forty-characters",
	} {
		if err := ValidateGitOID(invalid); !errors.Is(err, ErrValidation) {
			t.Errorf("%s (%q): %v", name, invalid, err)
		}
	}
}
