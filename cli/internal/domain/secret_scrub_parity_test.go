//go:build monorepo

package domain

import (
	"go/ast"
	"go/parser"
	"go/token"
	"reflect"
	"strconv"
	"testing"

	"github.com/wnsdy95/cxthub-cli/internal/testutil"
)

// The server masks text typed into the product with the patterns the CLI
// applies to a captured session at its standard tier. The two modules share
// no code, so each keeps its own list; this test fails when the lists differ.
func TestServerMasksWithTheStandardTierPatterns(t *testing.T) {
	// Both lists are a composite literal of (pattern, replacement) pairs. The
	// CLI wraps each pattern in regexp.MustCompile; the server compiles later.
	rules := func(path, name string) [][2]string {
		file, err := parser.ParseFile(token.NewFileSet(), path, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		var found [][2]string
		ast.Inspect(file, func(node ast.Node) bool {
			spec, ok := node.(*ast.ValueSpec)
			if !ok || len(spec.Names) != 1 || spec.Names[0].Name != name || len(spec.Values) != 1 {
				return true
			}
			for _, element := range spec.Values[0].(*ast.CompositeLit).Elts {
				pair := element.(*ast.CompositeLit).Elts
				if len(pair) != 2 {
					t.Fatalf("%s: a rule has %d parts", name, len(pair))
				}
				var rule [2]string
				for index, part := range pair {
					if call, wrapped := part.(*ast.CallExpr); wrapped {
						part = call.Args[0]
					}
					value, err := strconv.Unquote(part.(*ast.BasicLit).Value)
					if err != nil {
						t.Fatal(err)
					}
					rule[index] = value
				}
				found = append(found, rule)
			}
			return false
		})
		return found
	}
	cli := rules(testutil.RepositoryPath(t, "cli", "internal", "adapters", "capture", "secret_scrubber.go"), "scrubStandardPatterns")
	server := rules(testutil.RepositoryPath(t, "backend", "internal", "domain", "secret_scrub.go"), "secretScrubRules")
	if len(cli) == 0 || !reflect.DeepEqual(cli, server) {
		t.Fatalf("the lists differ:\nstandard tier %q\nserver        %q", cli, server)
	}
}
