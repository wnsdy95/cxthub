package memoryquery

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

func EffectiveItems(digest domain.MemoryDigest, content string) ([]domain.EffectiveMemoryItem, error) {
	if content == "prompt" {
		return effectivePromptItems(digest)
	}
	fragments := digest.Fragments
	if len(fragments) == 0 {
		fragments = []domain.MemoryFragment{{SourceSnapshot: digest.SnapshotID, Summary: digest.Summary, KeyFacts: digest.KeyFacts, OpenTasks: digest.OpenTasks}}
	}
	out := []domain.EffectiveMemoryItem{}
	seen := map[domain.ContentHash]bool{}
	add := func(item domain.EffectiveMemoryItem) error {
		raw, err := json.Marshal(item)
		if err != nil {
			return err
		}
		item.ID = domain.HashContent(raw)
		if seen[item.ID] {
			return nil
		}
		seen[item.ID] = true
		if len(out) >= MaxItems {
			return fmt.Errorf("%w: memory is too large; select a narrower context or stored object", domain.ErrValidation)
		}
		out = append(out, item)
		return nil
	}
	for _, fragment := range fragments {
		for _, claim := range fragment.Claims {
			state := domain.AssessMemoryClaim(claim, nil, "unknown")
			if err := add(domain.EffectiveMemoryItem{SourceSnapshot: fragment.SourceSnapshot, Kind: claim.Kind, Text: claim.Text, Code: claim.Code, MemoryClaimAssessment: state}); err != nil {
				return nil, err
			}
		}
		if content == "claims" {
			continue
		}
		historical := []struct {
			kind  string
			texts []string
		}{{"legacy_summary", []string{fragment.Summary}}, {"legacy_fact", fragment.KeyFacts}, {"legacy_task", fragment.OpenTasks}}
		for _, section := range historical {
			for _, text := range section.texts {
				if !utf8.ValidString(text) {
					return nil, domain.ErrIntegrity
				}
				originalHash := domain.HashContent([]byte(text))
				part := 0
				for len(text) > 0 {
					length := len(text)
					if length > 8192 {
						length = 8192
						for length > 0 && !utf8.RuneStart(text[length]) {
							length--
						}
					}
					item := domain.EffectiveMemoryItem{SourceSnapshot: fragment.SourceSnapshot, Kind: section.kind, Text: text[:length], TextHash: originalHash, Part: part, MemoryClaimAssessment: domain.MemoryClaimAssessment{State: "review", Reason: "untyped_historical_text"}}
					if err := add(item); err != nil {
						return nil, err
					}
					text = text[length:]
					part++
				}
			}
		}
	}
	return out, nil
}

// Prompt excerpts are a bounded read projection, never a new saved digest.
// Claims retain their normal assessments; legacy text remains explicitly
// unverified. Newest contributions get space first, regardless of summary size.
func effectivePromptItems(digest domain.MemoryDigest) ([]domain.EffectiveMemoryItem, error) {
	items, err := EffectiveItems(digest, "claims")
	if err != nil {
		return nil, err
	}
	fragments := digest.Fragments
	if len(fragments) == 0 {
		fragments = []domain.MemoryFragment{{SourceSnapshot: digest.SnapshotID, Summary: digest.Summary, KeyFacts: digest.KeyFacts, OpenTasks: digest.OpenTasks}}
	}
	used := 0
	for index := len(fragments) - 1; index >= 0 && used < 24<<10; index-- {
		fragment := fragments[index]
		text := strings.TrimSpace(strings.Join(append(append([]string{fragment.Summary}, fragment.KeyFacts...), fragment.OpenTasks...), "\n"))
		if text == "" {
			continue
		}
		if !utf8.ValidString(text) {
			return nil, domain.ErrIntegrity
		}
		original := domain.HashContent([]byte(text))
		if len(text) > 768 {
			offset := len(text) - 768
			for offset < len(text) && !utf8.RuneStart(text[offset]) {
				offset++
			}
			text = "[excerpt; full source through MCP]\n" + text[offset:]
		}
		item := domain.EffectiveMemoryItem{SourceSnapshot: fragment.SourceSnapshot, Kind: "legacy_summary", Text: text, TextHash: original, MemoryClaimAssessment: domain.MemoryClaimAssessment{State: "review", Reason: "untyped_historical_text"}}
		raw, err := json.Marshal(item)
		if err != nil {
			return nil, err
		}
		if used+len(raw) > 24<<10 {
			break
		}
		item.ID = domain.HashContent(raw)
		items = append(items, item)
		used += len(raw)
	}
	if len(items) > MaxItems {
		return nil, domain.ErrValidation
	}
	return items, nil
}

// An author's malformed comparison parent is reviewable evidence, not a reason
// to hide every other contribution in the selected memory.
func isMemoryScopeError(err error) bool { return errors.Is(err, domain.ErrValidation) }
