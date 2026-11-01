package http

import (
	"os"
	"strings"
	"testing"
)

func TestOpenAPIPathsAndIdentityParametersStayInTheirBlocks(test *testing.T) {
	raw, err := os.ReadFile("../../../../../schemas/openapi.yaml")
	if err != nil {
		test.Fatal(err)
	}
	section, parametersIndent := "", -1
	for position, line := range strings.Split(string(raw), "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		indent := len(line) - len(strings.TrimLeft(line, " "))
		if indent == 0 {
			if key, _, found := strings.Cut(line, ":"); found {
				section = key
			}
		}
		if specPathRe.MatchString(line) && section != "paths" {
			test.Errorf("line %d: route declared outside paths: %s", position+1, line)
		}
		if strings.Contains(line, "#/components/parameters/DocIdentities") && (parametersIndent < 0 || indent != parametersIndent+2 || !strings.HasPrefix(trimmed, "- $ref:")) {
			test.Errorf("line %d: identity parameter is not an item in its parameters block: %s", position+1, line)
		}
		if trimmed == "parameters:" {
			parametersIndent = indent
		} else if parametersIndent >= 0 && indent <= parametersIndent {
			parametersIndent = -1
		}
	}
}
