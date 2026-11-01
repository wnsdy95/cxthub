package domain

import (
	"context"
	"errors"
	"strings"
	"unicode/utf8"
)

const DocMarkdownReplayPlanPages = 5

var errDocMappedReplayBudget = errors.New("mapped display replay reached its object budget")

type docMarkdownReplayPolicy struct {
	MaxBytes    int
	MaxRuns     int
	SourceBytes int
	MapSource   func(context.Context, int, int) (DocFindSpan, error)
}

type DocMarkdownPlanCursor struct {
	Version   int         `json:"version"`
	Plan      ContentHash `json:"plan"`
	Page      int         `json:"page"`
	Operation int         `json:"operation"`
	Byte      int         `json:"byte"`
	TextBytes int         `json:"text_bytes"`
}

func NewDocMarkdownPlanCursor(plan DocMarkdownPlan) (DocMarkdownPlanCursor, error) {
	raw, err := plan.Encode()
	if err != nil {
		return DocMarkdownPlanCursor{}, err
	}
	return DocMarkdownPlanCursor{Version: 1, Plan: HashContent(raw)}, nil
}

func (cursor DocMarkdownPlanCursor) Validate(plan DocMarkdownPlan) error {
	raw, err := plan.Encode()
	if err != nil || cursor.Version != 1 || cursor.Plan != HashContent(raw) || cursor.Page < 0 || cursor.Page > plan.Pages.Count || cursor.Operation < 0 || cursor.Operation >= DocMarkdownPlanOperations || cursor.Byte < 0 || cursor.Byte > MaxConversationDocumentBytes || cursor.TextBytes < 0 || cursor.TextBytes > plan.TextBytes {
		return ErrIntegrity
	}
	if cursor.Page == plan.Pages.Count {
		if cursor.Operation != 0 || cursor.Byte != 0 || cursor.TextBytes != plan.TextBytes {
			return ErrIntegrity
		}
	} else if cursor.TextBytes == plan.TextBytes || cursor.Page*DocMarkdownPlanOperations+cursor.Operation >= plan.OperationCount {
		return ErrIntegrity
	}
	return nil
}

type DocMarkdownPlanFragment struct {
	page      DocDisplayPage
	next      DocMarkdownPlanCursor
	readBytes int
	planPages int
}

func (fragment DocMarkdownPlanFragment) Text() string { return fragment.page.Text }
func (fragment DocMarkdownPlanFragment) From() int    { return fragment.page.From }
func (fragment DocMarkdownPlanFragment) Next() DocMarkdownPlanCursor {
	return fragment.next
}
func (fragment DocMarkdownPlanFragment) ReadBytes() int { return fragment.readBytes }
func (fragment DocMarkdownPlanFragment) PlanPages() int { return fragment.planPages }
func (fragment DocMarkdownPlanFragment) SourceRange(offset, length int) (DocFindSpan, error) {
	return fragment.page.SourceRange(offset, length)
}

func ReadDocMarkdownPlanFragment(ctx context.Context, plan DocMarkdownPlan, cursor DocMarkdownPlanCursor, loadObject func(context.Context, ContentHash) ([]byte, error), loadText func(context.Context, int, int) ([]byte, error)) (result DocMarkdownPlanFragment, failure error) {
	return readDocMarkdownPlanFragment(ctx, plan, cursor, loadObject, loadText, docMarkdownReplayPolicy{})
}

func readDocMarkdownPlanFragment(ctx context.Context, plan DocMarkdownPlan, cursor DocMarkdownPlanCursor, loadObject func(context.Context, ContentHash) ([]byte, error), loadText func(context.Context, int, int) ([]byte, error), policy docMarkdownReplayPolicy) (result DocMarkdownPlanFragment, failure error) {
	defer func() {
		if err := ctx.Err(); err != nil {
			result, failure = DocMarkdownPlanFragment{}, err
		}
	}()
	if err := ctx.Err(); err != nil {
		return DocMarkdownPlanFragment{}, err
	}
	if cursor.Validate(plan) != nil || cursor.Page == plan.Pages.Count || loadObject == nil || loadText == nil || policy.MaxBytes < 0 || policy.MaxBytes > DocDisplayPageTextBytes || policy.MaxRuns < 0 || policy.MaxRuns > DocDisplayPageRuns || policy.SourceBytes < 0 || policy.SourceBytes > MaxConversationDocumentBytes {
		return DocMarkdownPlanFragment{}, ErrIntegrity
	}
	if policy.MaxBytes == 0 {
		policy.MaxBytes = DocDisplayPageTextBytes
	}
	if policy.MaxRuns == 0 {
		policy.MaxRuns = DocDisplayPageRuns
	}
	if policy.SourceBytes == 0 {
		policy.SourceBytes = plan.Input.Bytes
	}
	fragment := DocMarkdownPlanFragment{page: DocDisplayPage{Version: 2, Source: cursor.Plan, From: cursor.TextBytes}, next: cursor}
	var page DocMarkdownPlanPage
	var text strings.Builder
	var cached []byte
	cachedFrom, encodedText := 0, 0
	for fragment.next.Page < plan.Pages.Count {
		if err := ctx.Err(); err != nil {
			return DocMarkdownPlanFragment{}, err
		}
		if text.Len() == policy.MaxBytes {
			break
		}
		position := &fragment.next
		if len(page.Operations) == 0 || page.Index != position.Page {
			if fragment.planPages == DocMarkdownReplayPlanPages {
				break
			}
			var err error
			page, err = ReadDocMarkdownPlanPage(ctx, plan, position.Page, loadObject)
			if err != nil {
				if errors.Is(err, errDocMappedReplayBudget) && text.Len() > 0 {
					break
				}
				return DocMarkdownPlanFragment{}, err
			}
			fragment.planPages++
			if position.Operation >= len(page.Operations) {
				return DocMarkdownPlanFragment{}, ErrIntegrity
			}
			expected := page.From
			for _, operation := range page.Operations[:position.Operation] {
				expected += operation.TextBytes()
			}
			if position.Byte >= page.Operations[position.Operation].TextBytes() || position.Byte > plan.TextBytes-expected || expected+position.Byte != position.TextBytes {
				return DocMarkdownPlanFragment{}, ErrIntegrity
			}
		}
		operation := page.Operations[position.Operation]
		var character rune
		var width int
		from, to, linear := operation.From, operation.To, operation.linearCoordinates()
		if operation.Text == "" {
			from += position.Byte
			needed := min(utf8.UTFMax, operation.To-from)
			cachedOffset := from - cachedFrom
			if cachedOffset >= 0 && cachedOffset < len(cached) {
				available := min(needed, len(cached)-cachedOffset)
				if utf8.FullRune(cached[cachedOffset : cachedOffset+available]) {
					needed = available
				}
			}
			if from < cachedFrom || from-cachedFrom > len(cached)-needed {
				length := min(4096, max(needed, policy.MaxBytes-text.Len()), operation.To-from, DocDisplayStepSourceBytes-fragment.readBytes)
				if length < needed {
					break
				}
				raw, err := loadText(ctx, from, length)
				if err != nil {
					if errors.Is(err, errDocMappedReplayBudget) && text.Len() > 0 {
						break
					}
					return DocMarkdownPlanFragment{}, err
				}
				if len(raw) != length {
					return DocMarkdownPlanFragment{}, ErrIntegrity
				}
				cachedFrom, cached = from, append(cached[:0], raw...)
				fragment.readBytes += length
			}
			character, width = utf8.DecodeRune(cached[from-cachedFrom : from-cachedFrom+needed])
			if character == utf8.RuneError && width == 1 {
				return DocMarkdownPlanFragment{}, ErrIntegrity
			}
			to = from + width
		} else {
			character, width = utf8.DecodeRuneInString(operation.Text[position.Byte:])
			if linear {
				from += position.Byte
				to = from + width
			}
		}
		if text.Len()+width > policy.MaxBytes {
			break
		}
		if policy.MapSource != nil && len(fragment.page.Runs) == policy.MaxRuns && (!linear || !fragment.page.Runs[len(fragment.page.Runs)-1].Linear) {
			break
		}
		if policy.MapSource != nil {
			span, err := policy.MapSource(ctx, from, to-from)
			if err != nil {
				if errors.Is(err, errDocMappedReplayBudget) && text.Len() > 0 {
					break
				}
				return DocMarkdownPlanFragment{}, err
			}
			from, to = span.From, span.To
			linear = linear && to-from == width
		}
		runs := len(fragment.page.Runs)
		merge := runs > 0 && fragment.page.Runs[runs-1].Linear && linear && fragment.page.Runs[runs-1].SourceTo == from
		if !merge {
			runs++
		}
		if runs > policy.MaxRuns || encodedText+docDisplayEncodedRuneBytes(character)+docDisplayEncodedHeaderBytes+runs*docDisplayEncodedRunBytes > MaxDocDisplayPageBytes {
			break
		}
		text.WriteRune(character)
		encodedText += docDisplayEncodedRuneBytes(character)
		if merge {
			last := &fragment.page.Runs[len(fragment.page.Runs)-1]
			last.To, last.SourceTo = text.Len(), to
		} else {
			fragment.page.Runs = append(fragment.page.Runs, DocDisplayRun{To: text.Len(), SourceFrom: from, SourceTo: to, Linear: linear})
		}
		position.Byte += width
		position.TextBytes += width
		if position.Byte == operation.TextBytes() {
			position.Operation++
			position.Byte = 0
			if position.Operation == len(page.Operations) {
				position.Page++
				position.Operation = 0
			}
		}
	}
	fragment.page.Text = text.String()
	if fragment.page.validateCoordinates(policy.SourceBytes, false) != nil || fragment.next.Validate(plan) != nil {
		return DocMarkdownPlanFragment{}, ErrIntegrity
	}
	raw, err := canonicalJSON(fragment.page)
	if err != nil || len(raw) > MaxDocDisplayPageBytes {
		return DocMarkdownPlanFragment{}, ErrIntegrity
	}
	return fragment, nil
}
