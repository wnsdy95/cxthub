package domain

import "context"

func (plan DocToolValuePlan) ProjectionSource() (DocDisplayProjectionSource, error) {
	hash, err := plan.Hash()
	if err != nil {
		return DocDisplayProjectionSource{}, err
	}
	source := DocDisplayProjectionSource{Version: 1, Profile: DocDisplayProjectionProfile, EventHash: plan.Source.Tree.EventHash, EventBytes: plan.Source.Tree.EventBytes, Plan: hash, TextBytes: plan.TextBytes}
	return source, source.Validate()
}

func NewDocToolValueAssemblyInput(plan DocToolValuePlan) (DocDisplayAssemblyInput, error) {
	plan.Source.Root = cloneDocToolTreeNode(plan.Source.Root)
	source, err := plan.ProjectionSource()
	if err != nil {
		return DocDisplayAssemblyInput{}, err
	}
	return newDocPreparedPageAssemblyInput(source, plan.Pages.Count, DocToolValuePageMinRunes, func(ctx context.Context, index int, load func(context.Context, ContentHash) ([]byte, error)) (DocDisplayPage, error) {
		page, err := ReadDocToolValuePage(ctx, plan, index, load)
		return page.Display, err
	})
}
