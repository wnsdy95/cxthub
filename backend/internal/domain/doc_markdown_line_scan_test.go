package domain

import (
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"
	"unicode/utf8"
)

func docMarkdownLineScanReference(text string, from int) docMarkdownPlanLine {
	line := docMarkdownPlanLine{From: from, To: from + len(text), Next: from + len(text) + 1, ContentFrom: from}
	switch {
	case docMarkdownFence(text):
		line.Kind = docMarkdownLineFence
	default:
		if contentFrom, _, heading := docMarkdownHeading(text); heading {
			line.Kind, line.ContentFrom = docMarkdownLineHeading, from+contentFrom
		} else if contentFrom, _, list := docMarkdownList(text); list {
			line.Kind, line.ContentFrom = docMarkdownLineList, from+contentFrom
		} else if docMarkdownTrimStart(text) == len(text) {
			line.Kind = docMarkdownLineBlank
		}
	}
	return line
}

func assertDocMarkdownLineScan(test *testing.T, text string, from, fragment int) docMarkdownPlanLine {
	test.Helper()
	scan := newDocMarkdownLineScan(from)
	if err := scan.validate(from + len(text)); err != nil {
		test.Fatal(err)
	}
	consumed := 0
	for _, character := range text {
		width := utf8.RuneLen(character)
		if err := scan.consume(character, width); err != nil {
			test.Fatal("consume", scan, character, width, err)
		}
		consumed += width
		if fragment > 0 && (consumed/fragment != (consumed-width)/fragment || consumed == len(text)) {
			raw, err := json.Marshal(scan)
			if err != nil || len(raw) > 256 {
				test.Fatal("unbounded scan checkpoint", len(raw), err)
			}
			var resumed docMarkdownLineScan
			if json.Unmarshal(raw, &resumed) != nil || resumed != scan || resumed.validate(from+len(text)) != nil {
				test.Fatal("invalid resumed scan", string(raw), resumed)
			}
			scan = resumed
		}
	}
	line, err := scan.finish()
	want := docMarkdownLineScanReference(text, from)
	if err != nil || line != want {
		test.Fatalf("line = %+v, %v; want %+v for %q", line, err, want, text)
	}
	if again, err := scan.finish(); err != nil || again != line {
		test.Fatal("finish mutated scanner", again, err)
	}
	return line
}

func TestDocMarkdownLineScanSharedFixtureLines(test *testing.T) {
	raw, err := os.ReadFile("../../../schemas/fixtures/markdown-display-v1.json")
	if err != nil {
		test.Fatal(err)
	}
	var fixture struct {
		Version int
		Cases   []struct{ Name, Source string }
	}
	if json.Unmarshal(raw, &fixture) != nil || fixture.Version != DocMarkdownDisplayVersion || len(fixture.Cases) < 30 {
		test.Fatal("invalid shared fixture")
	}
	for _, scenario := range fixture.Cases {
		test.Run(scenario.Name, func(test *testing.T) {
			from := 0
			for _, text := range strings.Split(scenario.Source, "\n") {
				line := assertDocMarkdownLineScan(test, text, from, 1)
				from = line.Next
			}
		})
	}
}

func TestDocMarkdownLineScanJavaScriptSemantics(test *testing.T) {
	for _, scenario := range []struct {
		text    string
		kind    byte
		content string
	}{
		{"", docMarkdownLineBlank, ""},
		{" \t\r\v\f\u00a0\u1680\u2000\u200a\u2028\u2029\u202f\u205f\u3000\ufeff", docMarkdownLineBlank, ""},
		{"\u0085", docMarkdownLinePlain, ""},
		{"\u200b", docMarkdownLinePlain, ""},
		{"# heading\r", docMarkdownLinePlain, ""},
		{"# heading\u2028", docMarkdownLinePlain, ""},
		{"# heading\u2029", docMarkdownLinePlain, ""},
		{"# before\rafter", docMarkdownLinePlain, ""},
		{"# before\u2028after", docMarkdownLinePlain, ""},
		{"# before\u2029after", docMarkdownLinePlain, ""},
		{"# \r\u2028\u2029", docMarkdownLineHeading, ""},
		{"### \t\u00a0\ufefftitle  ", docMarkdownLineHeading, "title  "},
		{"#\rtitle", docMarkdownLineHeading, "title"},
		{"# \u2028title", docMarkdownLineHeading, "title"},
		{"# title\f\vtext", docMarkdownLineHeading, "title\f\vtext"},
		{"# 🙂", docMarkdownLineHeading, "🙂"},
		{"#\u0085title", docMarkdownLinePlain, ""},
		{" # title", docMarkdownLinePlain, ""},
		{"#### title", docMarkdownLinePlain, ""},
		{"###", docMarkdownLinePlain, ""},
		{"#missing", docMarkdownLinePlain, ""},
		{"\ufeff\u2028  ````suffix\r", docMarkdownLineFence, ""},
		{"``", docMarkdownLinePlain, ""},
		{"`` x```", docMarkdownLinePlain, ""},
		{"- ```", docMarkdownLineList, "```"},
		{"# ```", docMarkdownLineHeading, "```"},
		{"- \r\u2028\ufeffbody\r\u2029", docMarkdownLineList, "body\r\u2029"},
		{"\u00a0* \t\u2029", docMarkdownLineList, ""},
		{"00123) \t\u00a0item", docMarkdownLineList, "item"},
		{"1. ", docMarkdownLineList, ""},
		{"1.", docMarkdownLinePlain, ""},
		{"1.. item", docMarkdownLinePlain, ""},
		{"1)missing", docMarkdownLinePlain, ""},
		{"١. item", docMarkdownLinePlain, ""},
		{"+ item", docMarkdownLinePlain, ""},
	} {
		test.Run(scenario.text, func(test *testing.T) {
			line := assertDocMarkdownLineScan(test, scenario.text, 17, 1)
			if line.Kind != scenario.kind || (line.Kind == docMarkdownLineHeading || line.Kind == docMarkdownLineList) && scenario.text[line.ContentFrom-line.From:] != scenario.content {
				test.Fatal("JavaScript semantics changed", line, scenario)
			}
		})
	}
}

func TestDocMarkdownLineScanFinitePrefixes(test *testing.T) {
	alphabet := []string{"#", "`", "*", "-", "0", ".", ")", " ", "\r", "\u2028", "a"}
	var visit func(string, int)
	visit = func(text string, remaining int) {
		assertDocMarkdownLineScan(test, text, 9, 0)
		if remaining > 0 {
			for _, character := range alphabet {
				visit(text+character, remaining-1)
			}
		}
	}
	visit("", 4)
}

func TestDocMarkdownLineScanLongLinesAndPrefixes(test *testing.T) {
	for _, text := range []string{
		strings.Repeat(" ", 256<<10),
		strings.Repeat("\ufeff\u2028\t", 32768) + "```suffix",
		strings.Repeat("9", 256<<10) + ") \u00a0item",
		strings.Repeat("9", 256<<10) + ".",
		"#" + strings.Repeat("\u2029 ", 65536) + "title",
		"# " + strings.Repeat("a", 256<<10) + "\r",
		"# " + strings.Repeat("🙂", 65536) + "\u2028tail",
		"  * " + strings.Repeat("\u00a0", 131072) + "item\r",
	} {
		assertDocMarkdownLineScan(test, text, 123, 4096)
	}
}

func TestDocMarkdownLineScanMalformedState(test *testing.T) {
	for _, scan := range []docMarkdownLineScan{
		{From: -1},
		{From: 2, Next: 1, ContentFrom: 2},
		{Next: 11},
		{From: 2, Next: 3, ContentFrom: 1},
		{Next: 1, ContentFrom: 2},
		{Stage: 255},
		{Next: 1, ContentFrom: 1},
		{Stage: docMarkdownScanTickOne},
		{Stage: docMarkdownScanTickTwo, Next: 1},
		{Stage: docMarkdownScanFence, Next: 2},
		{Stage: docMarkdownScanHashOne, Next: 2},
		{Stage: docMarkdownScanHashTwo, Next: 1},
		{Stage: docMarkdownScanHashThree, Next: 4},
		{Stage: docMarkdownScanHeadingSpace, Next: 1, ContentFrom: 1},
		{Stage: docMarkdownScanHeadingSpace, Next: 3, ContentFrom: 2},
		{Stage: docMarkdownScanHeadingBody, Next: 3, ContentFrom: 1},
		{Stage: docMarkdownScanHeadingBody, Next: 3, ContentFrom: 3},
		{Stage: docMarkdownScanDigits},
		{Stage: docMarkdownScanDigits, Next: 2, ContentFrom: 1},
		{Stage: docMarkdownScanListMarker},
		{Stage: docMarkdownScanListSpace, Next: 1, ContentFrom: 1},
		{Stage: docMarkdownScanListSpace, Next: 3, ContentFrom: 2},
		{Stage: docMarkdownScanListBody, Next: 3, ContentFrom: 1},
		{Stage: docMarkdownScanListBody, Next: 2, ContentFrom: 2},
		{Stage: docMarkdownScanPlain},
		{Stage: docMarkdownScanPlain, Next: 3, ContentFrom: 1},
		newDocMarkdownLineScan(int(^uint(0) >> 1)),
	} {
		if err := scan.validate(10); !errors.Is(err, ErrIntegrity) {
			test.Fatal("invalid checkpoint accepted", scan, err)
		}
		if scan.Next <= 10 {
			if line, err := scan.finish(); !errors.Is(err, ErrIntegrity) || line != (docMarkdownPlanLine{}) {
				test.Fatal("invalid checkpoint finished", scan, line, err)
			}
		}
	}
	for _, total := range []int{-1, MaxConversationDocumentBytes + 1, int(^uint(0) >> 1)} {
		if err := newDocMarkdownLineScan(0).validate(total); !errors.Is(err, ErrIntegrity) {
			test.Fatal("invalid total accepted", total, err)
		}
	}
}

func TestDocMarkdownLineScanRejectsInvalidConsumption(test *testing.T) {
	for _, scenario := range []struct {
		character rune
		width     int
	}{
		{'\n', 1}, {'a', 0}, {'a', -1}, {'a', 2}, {'a', 5},
		{'🙂', 3}, {utf8.RuneError, 1}, {-1, 1}, {0xd800, 3}, {0x110000, 4},
	} {
		scan := newDocMarkdownLineScan(3)
		before := scan
		if err := scan.consume(scenario.character, scenario.width); !errors.Is(err, ErrIntegrity) || scan != before {
			test.Fatal("invalid rune mutated scanner", scenario, scan, err)
		}
	}
	scan := newDocMarkdownLineScan(MaxConversationDocumentBytes)
	if err := scan.consume('a', 1); !errors.Is(err, ErrIntegrity) || scan.Next != MaxConversationDocumentBytes {
		test.Fatal("coordinate overflow accepted", scan, err)
	}
	line, err := scan.finish()
	if err != nil || line.To != MaxConversationDocumentBytes || line.Next != MaxConversationDocumentBytes+1 {
		test.Fatal("EOF sentinel rejected", line, err)
	}
	assertDocMarkdownLineScan(test, "\ufffd\u0000", 0, 1)
}

func TestDocMarkdownLineScanJSONFields(test *testing.T) {
	scan := newDocMarkdownLineScan(17)
	raw, err := json.Marshal(scan)
	if err != nil || string(raw) != `{"from":17,"next":17,"content_from":17,"stage":0}` {
		test.Fatal("scan JSON fields", string(raw), err)
	}
	line, err := scan.finish()
	if err != nil {
		test.Fatal(err)
	}
	raw, err = json.Marshal(line)
	if err != nil || string(raw) != `{"from":17,"to":17,"next":18,"content_from":17,"kind":1}` {
		test.Fatal("line JSON fields", string(raw), err)
	}
}
