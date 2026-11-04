package http

import (
	"fmt"
	"os"
	"regexp"
	"strings"
	"testing"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

func docFindOpenAPIBlock(test *testing.T, source, name string) string {
	test.Helper()
	_, block, present := strings.Cut(source, "\n    "+name+":\n")
	if !present {
		test.Fatalf("missing schema %s", name)
	}
	if next := regexp.MustCompile(`(?m)^    [A-Za-z]\w*:`).FindStringIndex(block); next != nil {
		block = block[:next[0]]
	}
	return block
}

func TestDocFindPreparedOpenAPIProfileContract(test *testing.T) {
	raw, err := os.ReadFile("../../../../../schemas/openapi.yaml")
	if err != nil {
		test.Fatal(err)
	}
	source := string(raw)
	for _, fixture := range []struct{ name, position string }{{"DocFindSelector", "byte"}, {"DocLegacyFindSelector", "byte_offset"}} {
		test.Run(fixture.name, func(test *testing.T) {
			block := docFindOpenAPIBlock(test, source, fixture.name)
			for _, contract := range []string{
				"additionalProperties: false", "version: { type: integer, enum: [1, 2, 3] }",
				"if: { properties: { version: { const: 1 } } }\n          then: { not: { required: [representation] } }",
				"if: { properties: { version: { const: 2 } } }",
				fmt.Sprintf("then: { required: [representation], properties: { representation: { const: 'display-v1' }, field: { const: 0 }, %s: { maximum: 524288 } } }", fixture.position),
				"if: { properties: { version: { const: 3 } } }\n          then:\n            required: [representation, prepared]",
				"representation: { const: 'display-prepared-v1' }", "field: { const: 0 }",
				"else: { not: { required: [prepared] } }", "prepared: { $ref: '#/components/schemas/DocFindPreparedProgress' }",
				"if: { properties: { prepared: { properties: { generation: { const: '' } } } } }",
				fmt.Sprintf("then: { properties: { %s: { maximum: 524288 } } }", fixture.position),
				fmt.Sprintf("%s: { type: integer, minimum: 0, maximum: %d }", fixture.position, domain.MaxConversationDocumentBytes),
			} {
				if !strings.Contains(block, contract) {
					test.Errorf("missing selector profile rule %q", contract)
				}
			}
		})
	}
	progress := docFindOpenAPIBlock(test, source, "DocFindPreparedProgress")
	for _, contract := range []string{"additionalProperties: false", "required: [page, generation]",
		fmt.Sprintf("page: { type: integer, minimum: 0, maximum: %d }", domain.MaxDocRangePages-1),
		"if: { properties: { page: { minimum: 1 } } }", "then: { properties: { generation: { $ref: '#/components/schemas/ContentHash' } } }",
		"- { type: string, const: '' }"} {
		if !strings.Contains(progress, contract) {
			test.Errorf("missing prepared descriptor rule %q", contract)
		}
	}
	page := docFindOpenAPIBlock(test, source, "DocFindPage")
	for _, contract := range []string{
		"additionalProperties: false", "enum: ['find-v1', 'find-display-v1', 'find-display-prepared-v1']",
		"enum: ['find-display-v1', 'find-legacy-display-v1']", "byte_offset: { maximum: 524288 }",
		"if: { properties: { projection: { enum: ['find-display-prepared-v1', 'find-legacy-display-prepared-v1'] } } }\n          then:\n            required: [prepared]",
		"required: [source_range, prepared_page]", "field: { const: 0 }", "raw: { const: false }", "byte_offset: { maximum: 536870912 }",
		"else:\n            not: { required: [prepared] }\n            properties:\n              matches: { items: { not: { required: [prepared_page] } } }",
		"if: { properties: { next_cursor: { type: 'null' } } }", "byte_offset: { const: 0 }",
		"prepared: { properties: { page: { const: 0 }, generation: { const: '' } } }",
	} {
		if !strings.Contains(page, contract) {
			test.Errorf("missing response profile rule %q", contract)
		}
	}
	_, route, present := strings.Cut(source, "\n  /repos/{repoID}/docs/{hash}/find:\n")
	if !present {
		test.Fatal("missing find route")
	}
	route, _, _ = strings.Cut(route, "\n  /")
	for _, contract := range []string{"enum: ['display-v1', 'display-prepared-v1']", "doc_display_pending (503, Retry-After: 1)",
		"doc_find_display_unavailable (503, no Retry-After)", "256KiB canonical-source scan budget", "bounded prepared-page windows: at most 8 per",
		"whole event's displayed UTF-8 content, up to 512MiB", "Unknown profiles,\n        including display-v2"} {
		if !strings.Contains(route, contract) {
			test.Errorf("missing prepared route contract %q", contract)
		}
	}
}
