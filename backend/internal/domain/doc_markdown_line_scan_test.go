package domain

import (
	"encoding/json"
	"errors"
	"testing"
)

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
