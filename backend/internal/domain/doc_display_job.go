package domain

import (
	"bytes"
	"context"
)

type DocDisplayTarget struct {
	RepoID  ContentHash
	DocHash ContentHash
	Binding DocReadGenerationBinding
	Event   DocEventIndex
	Profile string
}

func (target DocDisplayTarget) DisplayProfile() string {
	if target.Profile == "" {
		return DocDisplayStringProfile
	}
	return target.Profile
}

func (target DocDisplayTarget) Validate() error {
	if target.DisplayProfile() != DocDisplayStringProfile && target.DisplayProfile() != DocDisplayProjectionProfile {
		return ErrValidation
	}
	if ValidateContentHash(target.RepoID) != nil || ValidateContentHash(target.DocHash) != nil || target.Binding.Validate() != nil || ValidateContentHash(target.Event.Hash) != nil || target.Event.Index < 0 || target.Event.Index >= MaxConversationDocumentBytes || target.Event.Offset < 0 || target.Event.Length <= 0 || target.Event.Length > MaxConversationDocumentBytes || target.Event.Offset > MaxConversationDocumentBytes-target.Event.Length || len(target.Event.Role) > 64 || target.Event.Text != "" {
		return ErrValidation
	}
	return nil
}

type DocDisplayJob struct {
	DocRangeIndexJob
	Target DocDisplayTarget
}

func (job DocDisplayJob) Validate() error {
	if job.DocRangeIndexJob.Validate() != nil || job.Target.Validate() != nil || job.RepoID != job.Target.RepoID || job.DocHash != job.Target.DocHash || job.BaseHash != "" {
		return ErrValidation
	}
	return nil
}

type DocDisplayPreparation struct {
	Discovery           *DocFieldDiscoveryState
	DiscoveryCheckpoint ContentHash
	Source              *DocDisplayStringSource
	State               DocDisplayStringState
	Checkpoint          ContentHash
}

func ValidateDocDisplayStringCheckpoint(ctx context.Context, source DocDisplayStringSource, state DocDisplayStringState, load func(context.Context, ContentHash) ([]byte, error)) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if state.Validate(source) != nil {
		return ErrIntegrity
	}
	if state.Directory.Count == 0 {
		return ctx.Err()
	}
	if load == nil {
		return ErrIntegrity
	}
	root, staged, err := state.Directory.Finish(ctx)
	if err != nil {
		return err
	}
	selected := func(ctx context.Context, hash ContentHash) ([]byte, error) {
		for _, object := range staged {
			if object.Hash == hash {
				return object.Bytes, nil
			}
		}
		return load(ctx, hash)
	}
	hash, err := ResolveDocRangeLeaf(ctx, root, state.Directory.Count-1, selected)
	if err != nil {
		return err
	}
	raw, err := selected(ctx, hash)
	if err != nil {
		return err
	}
	page, err := DecodeDocDisplayPage(source, hash, raw)
	if err != nil || page.Index != state.Directory.Count-1 || page.From+len(page.Text) != state.TextBytes {
		return ErrIntegrity
	}
	end := page.Runs[len(page.Runs)-1].SourceTo
	if state.Done {
		if end != source.Token.To-1 || state.Next != end+1 {
			return ErrIntegrity
		}
	} else if state.Next != end {
		return ErrIntegrity
	}
	return ctx.Err()
}

func ValidateDocDisplayStringStep(ctx context.Context, source DocDisplayStringSource, prior DocDisplayStringState, step DocDisplayStringStep) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if prior.Validate(source) != nil || prior.Done || step.State.Validate(source) != nil || step.State.Next <= prior.Next || step.ReadBytes <= 0 || step.ReadBytes > DocDisplayStepSourceBytes || step.State.Next-prior.Next > step.ReadBytes {
		return ErrIntegrity
	}
	expected := prior
	expected.Next, expected.Done = step.State.Next, step.State.Done
	var objects []DocRangeObject
	if step.Page == nil {
		if !expected.Done || prior.Next != source.Token.To-1 {
			return ErrIntegrity
		}
	} else {
		page := *step.Page
		encoded, err := page.Encode(source)
		if err != nil || page.Index != prior.Directory.Count || page.From != prior.TextBytes {
			return ErrIntegrity
		}
		start := prior.Next
		if start == source.Token.From {
			start++
		}
		end := page.Runs[len(page.Runs)-1].SourceTo
		if page.Runs[0].SourceFrom != start || expected.Done && (end != source.Token.To-1 || expected.Next != end+1) || !expected.Done && expected.Next != end {
			return ErrIntegrity
		}
		expected.Directory, objects, err = prior.Directory.Append(ctx, HashContent(encoded))
		if err != nil {
			return err
		}
		objects = append(objects, DocRangeObject{Hash: HashContent(encoded), Bytes: encoded})
		expected.TextBytes += len(page.Text)
		for _, character := range page.Text {
			if !docMarkdownWhitespace(character) {
				expected.HasContent = true
				break
			}
		}
	}
	expectedRaw, err := expected.Encode(source)
	if err != nil {
		return err
	}
	actualRaw, err := step.State.Encode(source)
	if err != nil || !bytes.Equal(expectedRaw, actualRaw) || len(objects) != len(step.Objects) {
		return ErrIntegrity
	}
	for index, object := range objects {
		if object.Hash != step.Objects[index].Hash || !bytes.Equal(object.Bytes, step.Objects[index].Bytes) {
			return ErrIntegrity
		}
	}
	return ctx.Err()
}
