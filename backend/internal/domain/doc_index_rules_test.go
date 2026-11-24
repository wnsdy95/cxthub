package domain

import (
	"errors"
	"testing"
)

// A unit of the visible index names a run of events and what of it a mode
// shows. It is read back from storage, so every rule it must keep is checked
// before it is used.
func TestVisibleUnitIsCheckedForItsMode(t *testing.T) {
	const total = 6
	// One event shown by itself, in a turn that began earlier.
	one := DocVisibleUnit{From: 2, To: 3, EventIndex: 2, TurnIndex: 1, RenderedBefore: 2}
	// Two events of which the last is shown and the first is folded.
	folded := DocVisibleUnit{From: 2, To: 4, EventIndex: 3, TurnIndex: 1, RenderedBefore: 2, FoldRows: 1}
	// A prompt and what follows it, folded.
	prompt := DocVisibleUnit{From: 2, To: 4, EventIndex: 2, TurnIndex: 2, RenderedBefore: 2, FoldRows: 1}
	// Events with nothing shown.
	hidden := DocVisibleUnit{From: 2, To: 4, EventIndex: -1, TurnIndex: -1}
	for name, valid := range map[string]struct {
		unit DocVisibleUnit
		mode string
	}{
		"one event in the list of all":       {one, "all"},
		"one event in the chat":              {one, "chat"},
		"the last of two in the chat":        {folded, "chat"},
		"a prompt with what follows it":      {prompt, "prompts"},
		"nothing shown, in the chat":         {hidden, "chat"},
		"nothing shown, among the prompts":   {hidden, "prompts"},
		"a unit that ends at the last event": {DocVisibleUnit{From: 5, To: 6, EventIndex: 5, TurnIndex: 0, RenderedBefore: 5}, "all"},
	} {
		if err := valid.unit.Validate(total, valid.mode); err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}

	change := func(unit DocVisibleUnit, edit func(*DocVisibleUnit)) DocVisibleUnit {
		edit(&unit)
		return unit
	}
	for name, invalid := range map[string]struct {
		unit  DocVisibleUnit
		total int
		mode  string
		want  error
	}{
		// What no mode allows.
		"a negative count of events":        {one, -1, "all", ErrIntegrity},
		"more events than a document holds": {one, MaxConversationDocumentBytes + 1, "all", ErrIntegrity},
		"a start before the first event":    {change(one, func(u *DocVisibleUnit) { u.From, u.RenderedBefore, u.TurnIndex = -1, 0, -1 }), total, "all", ErrIntegrity},
		"an end at its start":               {change(hidden, func(u *DocVisibleUnit) { u.To = u.From }), total, "chat", ErrIntegrity},
		"an end after the last event":       {change(hidden, func(u *DocVisibleUnit) { u.To = total + 1 }), total, "chat", ErrIntegrity},
		"a turn before none":                {change(one, func(u *DocVisibleUnit) { u.TurnIndex = -2 }), total, "all", ErrIntegrity},
		"a turn that begins after the unit": {change(one, func(u *DocVisibleUnit) { u.TurnIndex = 3 }), total, "all", ErrIntegrity},
		"a negative count of rows before":   {change(one, func(u *DocVisibleUnit) { u.RenderedBefore = -1 }), total, "all", ErrIntegrity},
		"more rows before than events":      {change(hidden, func(u *DocVisibleUnit) { u.To, u.RenderedBefore = 5, 3 }), total, "chat", ErrIntegrity},
		"a negative count of folded rows":   {change(folded, func(u *DocVisibleUnit) { u.FoldRows = -1 }), total, "chat", ErrIntegrity},
		"more folded rows than events":      {change(hidden, func(u *DocVisibleUnit) { u.FoldRows = 3 }), total, "chat", ErrIntegrity},
		"rows that end after the unit": {change(folded, func(u *DocVisibleUnit) {
			u.RenderedBefore = 3
			u.From, u.TurnIndex = 3, 1
			u.To = 5
			u.EventIndex = 4
			u.FoldRows = 1
			u.RenderedBefore = 4
		}), total, "chat", ErrIntegrity},
		"a shown event before none":          {change(hidden, func(u *DocVisibleUnit) { u.EventIndex = -2 }), total, "chat", ErrIntegrity},
		"a shown event before the unit":      {change(one, func(u *DocVisibleUnit) { u.EventIndex = 1 }), total, "all", ErrIntegrity},
		"a shown event at the unit's end":    {change(one, func(u *DocVisibleUnit) { u.EventIndex = 3 }), total, "all", ErrIntegrity},
		"every event folded and one shown":   {change(folded, func(u *DocVisibleUnit) { u.FoldRows, u.RenderedBefore = 2, 1 }), total, "chat", ErrIntegrity},
		"a turn that begins here, not shown": {change(folded, func(u *DocVisibleUnit) { u.TurnIndex = 2 }), total, "chat", ErrIntegrity},
		// What one mode does not allow.
		"two events as one row of all":         {hidden, total, "all", ErrIntegrity},
		"a folded row in the list of all":      {change(one, func(u *DocVisibleUnit) { u.EventIndex, u.FoldRows = -1, 1 }), total, "all", ErrIntegrity},
		"the chat shows another than the last": {change(folded, func(u *DocVisibleUnit) { u.EventIndex, u.FoldRows = 2, 0 }), total, "chat", ErrIntegrity},
		"a prompt that is not the first event": {change(prompt, func(u *DocVisibleUnit) { u.EventIndex, u.TurnIndex = 3, 1 }), total, "prompts", ErrIntegrity},
		"a prompt of an earlier turn":          {change(prompt, func(u *DocVisibleUnit) { u.TurnIndex = 1 }), total, "prompts", ErrIntegrity},
		// A mode that does not exist is the caller's mistake, not the store's.
		"no mode":         {one, total, "", ErrValidation},
		"an unknown mode": {one, total, "everything", ErrValidation},
	} {
		if err := invalid.unit.Validate(invalid.total, invalid.mode); !errors.Is(err, invalid.want) {
			t.Errorf("%s: %v, want %v (%+v)", name, err, invalid.want, invalid.unit)
		}
	}
}

// The third version of a range generation holds its stream without chunks;
// every other version cuts it into chunks of one size, the last one shorter.
func TestRangeGenerationCountsItsChunks(t *testing.T) {
	for _, test := range []struct {
		version int
		bytes   int64
		want    int
	}{
		{3, 0, 0}, {3, 5 * ConversationManifestChunkBytes, 0},
		{4, 0, 0}, {4, 1, 1}, {4, ConversationManifestChunkBytes - 1, 1}, {4, ConversationManifestChunkBytes, 1},
		{4, ConversationManifestChunkBytes + 1, 2}, {2, 3 * ConversationManifestChunkBytes, 3}, {6, 3*ConversationManifestChunkBytes + 1, 4},
	} {
		if got := (DocRangeGeneration{Version: test.version, StreamBytes: test.bytes}).ChunkCount(); got != test.want {
			t.Errorf("version %d with %d bytes: %d chunks, want %d", test.version, test.bytes, got, test.want)
		}
	}
}

// A page of a range index is asked for by its position; a position the index
// does not have is not read.
func TestRangeIndexHasNoPageOutsideItsList(t *testing.T) {
	index := DocRangeIndex{Pages: []ContentHash{HashContent([]byte("first page")), HashContent([]byte("second page"))}}
	for _, position := range []int{-1, 2, 3, 1 << 30} {
		if entries, err := index.DecodePage(position, []byte("anything")); !errors.Is(err, ErrIntegrity) || entries != nil {
			t.Errorf("position %d: %v %v", position, entries, err)
		}
	}
	if entries, err := (DocRangeIndex{}).DecodePage(0, nil); !errors.Is(err, ErrIntegrity) || entries != nil {
		t.Errorf("an index without pages: %v %v", entries, err)
	}
}

// A document that was not verified gives no reference to store.
func TestUnverifiedDocumentHasNoReference(t *testing.T) {
	if reference := (VerifiedSessionDoc{}).Reference(); reference.Valid() || reference.Hash() != "" || reference != (VerifiedDocReference{}) {
		t.Fatalf("an unverified document gave the reference %+v", reference)
	}
}
