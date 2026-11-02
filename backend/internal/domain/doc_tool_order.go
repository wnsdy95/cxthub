package domain

import (
	"bytes"
	"context"
	"encoding/json"
)

const DocToolOrderProfile = "public-tool-child-order-v1"
const DocToolOrderPageEntries = 64
const MaxDocToolOrderPageBytes = 32 << 10

type DocToolOrderSource struct {
	Version int             `json:"version"`
	Profile string          `json:"profile"`
	Tree    ContentHash     `json:"tree"`
	Keys    ContentHash     `json:"keys"`
	Parent  DocToolTreeNode `json:"parent"`
	Count   int             `json:"count"`
}

func NewDocToolOrderSource(tree DocToolTreeSource, keys DocToolKeyMapSource, parent DocToolTreeNode, count int) (DocToolOrderSource, error) {
	treeHash, err := tree.Hash()
	if err != nil {
		return DocToolOrderSource{}, err
	}
	keyHash, err := keys.Hash(tree)
	if err != nil {
		return DocToolOrderSource{}, err
	}
	source := DocToolOrderSource{Version: 1, Profile: DocToolOrderProfile, Tree: treeHash, Keys: keyHash, Parent: cloneDocToolTreeNode(parent), Count: count}
	return source, source.Validate(tree, keys)
}

func (source DocToolOrderSource) Validate(tree DocToolTreeSource, keys DocToolKeyMapSource) error {
	treeHash, err := tree.Hash()
	keyHash, keyErr := keys.Hash(tree)
	if err != nil || keyErr != nil || source.Version != 1 || source.Profile != DocToolOrderProfile || source.Tree != treeHash || source.Keys != keyHash || source.Parent.Validate(tree) != nil || source.Parent.Type != DocToolValueObject && source.Parent.Type != DocToolValueArray || source.Count < 0 || source.Count > source.Parent.Children || source.Parent.Type == DocToolValueArray && source.Count != source.Parent.Children || source.Parent.Children > 0 && source.Parent.Type == DocToolValueObject && source.Count == 0 {
		return ErrIntegrity
	}
	return nil
}

func (source DocToolOrderSource) Encode(tree DocToolTreeSource, keys DocToolKeyMapSource) ([]byte, error) {
	if source.Validate(tree, keys) != nil {
		return nil, ErrIntegrity
	}
	return canonicalJSON(source)
}

func (source DocToolOrderSource) Hash(tree DocToolTreeSource, keys DocToolKeyMapSource) (ContentHash, error) {
	raw, err := source.Encode(tree, keys)
	if err != nil {
		return "", err
	}
	return HashContent(raw), nil
}

type DocToolOrderEntry struct {
	Node         DocToolTreeNode `json:"node"`
	First        int             `json:"first"`
	Digest       ContentHash     `json:"digest,omitempty"`
	IsArrayIndex bool            `json:"is_array_index,omitempty"`
	ArrayIndex   uint32          `json:"array_index,omitempty"`
}

func NewDocToolOrderMember(member DocToolKeyMember) DocToolOrderEntry {
	return DocToolOrderEntry{Node: cloneDocToolTreeNode(member.Last.Node), First: member.First, Digest: member.Last.Digest,
		IsArrayIndex: member.Last.IsArrayIndex, ArrayIndex: member.Last.ArrayIndex}
}

func (entry DocToolOrderEntry) Member() DocToolKeyMember {
	return DocToolKeyMember{First: entry.First, Last: DocToolKeyItem{Node: cloneDocToolTreeNode(entry.Node), Digest: entry.Digest,
		IsArrayIndex: entry.IsArrayIndex, ArrayIndex: entry.ArrayIndex}}
}

func (entry DocToolOrderEntry) Validate(tree DocToolTreeSource, keys DocToolKeyMapSource, source DocToolOrderSource) error {
	if source.Validate(tree, keys) != nil || entry.Node.Validate(tree) != nil || entry.Node.Parent != source.Parent.Value.From || entry.Node.Depth != source.Parent.Depth+1 || entry.Node.Value.To >= source.Parent.Value.To || entry.Node.Ordinal >= source.Parent.Children {
		return ErrIntegrity
	}
	if source.Parent.Type == DocToolValueObject {
		return entry.Member().Validate(tree)
	}
	if entry.Node.Key != nil || entry.Node.Ordinal >= source.Count || entry.First != entry.Node.Value.From || entry.Digest != "" || entry.IsArrayIndex || entry.ArrayIndex != 0 {
		return ErrIntegrity
	}
	return nil
}

func CompareDocToolOrderEntries(left, right DocToolOrderEntry, source DocToolOrderSource) int {
	if source.Parent.Type == DocToolValueObject {
		return CompareDocToolKeyMembers(left.Member(), right.Member())
	}
	return left.Node.Ordinal - right.Node.Ordinal
}

func cloneDocToolOrderEntry(entry DocToolOrderEntry) DocToolOrderEntry {
	entry.Node = cloneDocToolTreeNode(entry.Node)
	return entry
}

type DocToolOrderPage struct {
	Version int                 `json:"version"`
	Source  ContentHash         `json:"source"`
	Index   int                 `json:"index"`
	Entries []DocToolOrderEntry `json:"entries"`
}

func (page DocToolOrderPage) Encode(tree DocToolTreeSource, keys DocToolKeyMapSource, source DocToolOrderSource) ([]byte, error) {
	hash, err := source.Hash(tree, keys)
	if err != nil || page.Version != 1 || page.Source != hash || page.Index < 0 || page.Index >= (source.Count+DocToolOrderPageEntries-1)/DocToolOrderPageEntries || len(page.Entries) != min(DocToolOrderPageEntries, source.Count-page.Index*DocToolOrderPageEntries) {
		return nil, ErrIntegrity
	}
	for index, entry := range page.Entries {
		if entry.Validate(tree, keys, source) != nil || source.Parent.Type == DocToolValueArray && entry.Node.Ordinal != page.Index*DocToolOrderPageEntries+index || index > 0 && CompareDocToolOrderEntries(page.Entries[index-1], entry, source) >= 0 {
			return nil, ErrIntegrity
		}
	}
	raw, err := canonicalJSON(page)
	if err != nil || len(raw) > MaxDocToolOrderPageBytes {
		return nil, ErrIntegrity
	}
	return raw, nil
}

func DecodeDocToolOrderPage(tree DocToolTreeSource, keys DocToolKeyMapSource, source DocToolOrderSource, expected ContentHash, raw []byte) (DocToolOrderPage, error) {
	var page DocToolOrderPage
	if len(raw) == 0 || len(raw) > MaxDocToolOrderPageBytes || ValidateContentHash(expected) != nil || HashContent(raw) != expected || json.Unmarshal(raw, &page) != nil {
		return DocToolOrderPage{}, ErrIntegrity
	}
	canonical, err := page.Encode(tree, keys, source)
	if err != nil || !bytes.Equal(canonical, raw) {
		return DocToolOrderPage{}, ErrIntegrity
	}
	return page, nil
}

type DocToolOrderPlan struct {
	Version int                   `json:"version"`
	Source  DocToolOrderSource    `json:"source"`
	Pages   DocRangeDirectoryRoot `json:"pages"`
}

func (plan DocToolOrderPlan) Encode(tree DocToolTreeSource, keys DocToolKeyMapSource) ([]byte, error) {
	if plan.Version != 1 || plan.Source.Validate(tree, keys) != nil || plan.Pages.validate((plan.Source.Count+DocToolOrderPageEntries-1)/DocToolOrderPageEntries) != nil {
		return nil, ErrIntegrity
	}
	raw, err := canonicalJSON(plan)
	if err != nil || len(raw) > MaxDocDisplaySourceBytes {
		return nil, ErrIntegrity
	}
	return raw, nil
}

func DecodeDocToolOrderPlan(tree DocToolTreeSource, keys DocToolKeyMapSource, expected ContentHash, raw []byte) (DocToolOrderPlan, error) {
	var plan DocToolOrderPlan
	if len(raw) == 0 || len(raw) > MaxDocDisplaySourceBytes || ValidateContentHash(expected) != nil || HashContent(raw) != expected || json.Unmarshal(raw, &plan) != nil {
		return DocToolOrderPlan{}, ErrIntegrity
	}
	canonical, err := plan.Encode(tree, keys)
	if err != nil || !bytes.Equal(canonical, raw) {
		return DocToolOrderPlan{}, ErrIntegrity
	}
	return plan, nil
}

func ReadDocToolOrderPage(ctx context.Context, tree DocToolTreeSource, keys DocToolKeyMapSource, plan DocToolOrderPlan, index int, load func(context.Context, ContentHash) ([]byte, error)) (result DocToolOrderPage, failure error) {
	defer func() {
		if err := ctx.Err(); err != nil {
			failure = err
		}
		if failure != nil {
			result = DocToolOrderPage{}
		}
	}()
	if err := ctx.Err(); err != nil {
		return result, err
	}
	if _, err := plan.Encode(tree, keys); err != nil || index < 0 || index >= plan.Pages.Count || load == nil {
		return result, ErrIntegrity
	}
	hash, err := ResolveDocRangeLeaf(ctx, plan.Pages, index, load)
	if err != nil {
		return result, err
	}
	raw, err := load(ctx, hash)
	if err != nil {
		return result, err
	}
	page, err := DecodeDocToolOrderPage(tree, keys, plan.Source, hash, raw)
	if err != nil || page.Index != index {
		return result, ErrIntegrity
	}
	return page, nil
}
