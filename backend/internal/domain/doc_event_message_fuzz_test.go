package domain

import (
	"encoding/json"
	"testing"
)

func FuzzDocEventMessagePreparationParity(fuzz *testing.F) {
	for _, summary := range []bool{false, true} {
		fuzz.Add("**left", "right**", "**literal**", summary)
		fuzz.Add("```\nraw", "code\n```", "[name](not-markdown)", summary)
		fuzz.Add("", "", "", summary)
		fuzz.Add("\ud55c\uae00🙂\n\n# title", "[label](https://hidden.test)", "\x00\n\t🙂", summary)
		fuzz.Add("[cxt seed] Branch-switch context: first", "last", "asset", summary)
	}
	fuzz.Fuzz(func(test *testing.T, left, right, name string, summary bool) {
		if len(left)+len(right)+len(name) > 1024 {
			return
		}
		raw, err := json.Marshal(map[string]any{
			"kind": EventMessage, "compact_summary": summary, "opaque": "small-private-state",
			"blocks": []any{
				map[string]any{"type": "text", "text": left},
				map[string]any{"type": "image", "name": name, "text": right},
				map[string]any{"type": "text", "text": right},
				map[string]any{"type": "text", "text": nil},
			},
		})
		if err != nil {
			test.Fatal(err)
		}
		fixture := newEventMessageFixture(test, raw)
		generation := fixture.finish(test)
		fixture.pages(test, generation, fixture.oracle(test))
	})
}
