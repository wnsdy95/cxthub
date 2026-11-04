package domain

import "context"

func (plan DocToolStringPlan) ProjectionSource() (DocDisplayProjectionSource, error) {
	raw, err := plan.Encode()
	if err != nil {
		return DocDisplayProjectionSource{}, err
	}
	source := DocDisplayProjectionSource{Version: 1, Profile: DocDisplayProjectionProfile, EventHash: plan.Source.EventHash, EventBytes: plan.Source.EventBytes, Plan: HashContent(raw), TextBytes: plan.TextBytes}
	return source, source.Validate()
}

type docToolStringCursor = docPreparedPageCursor

func NewDocToolStringAssemblyInput(plan DocToolStringPlan) (DocDisplayAssemblyInput, error) {
	source, err := plan.ProjectionSource()
	if err != nil {
		return DocDisplayAssemblyInput{}, err
	}
	return newDocPreparedPageAssemblyInput(source, plan.Pages.Count, docToolStringPageMinRunes, func(ctx context.Context, index int, load func(context.Context, ContentHash) ([]byte, error)) (DocDisplayPage, error) {
		page, err := ReadDocToolStringPage(ctx, plan, index, load)
		return page.Display, err
	})
}
