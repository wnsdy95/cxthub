package domain

import (
	"crypto/sha256"
	"hash"
	"reflect"
	"strconv"
	"strings"
	"unicode/utf8"
)

const (
	memoryAccumulatorMaxEntries = 16384
	memoryAccumulatorEntryBytes = 64
	memoryAccumulatorBaseBytes  = 128
)

// MemoryAccumulator retains an ordered union of contributions, rather than a
// queue of complete rendered archives. maxBytes bounds logical retained JSON
// payload plus bookkeeping, and independently bounds the final wire JSON. It
// is not a measurement of Go heap usage. The zero value has a zero budget.
// Add and Reset are atomic on error; Digest returns an independently owned copy.
// Like the merge helpers, this type is not safe for concurrent use.
type MemoryAccumulator struct {
	maxBytes                       int
	single                         *MemoryDigest // Preserve the first digest exactly until a second Add.
	started                        bool
	latest                         MemoryDigest // Metadata, or the one remaining opaque legacy carry.
	fragments                      []MemoryFragment
	index                          map[[sha256.Size]byte][]int
	fragmentBytes, fragmentEntries int
}

func NewMemoryAccumulator(maxBytes int) *MemoryAccumulator {
	return &MemoryAccumulator{maxBytes: maxBytes}
}

// Reset starts a new sequence at an opaque replacement boundary. No memory
// from before that boundary contributes to subsequent additions.
func (a *MemoryAccumulator) Reset(d MemoryDigest) error {
	next := NewMemoryAccumulator(a.maxBytes)
	if err := next.Add(d); err != nil {
		return err
	}
	*a = *next
	return nil
}

func (a *MemoryAccumulator) Add(d MemoryDigest) error {
	if !a.started {
		if _, _, err := memoryAccumulatorSize(d, a.maxBytes, nil); err != nil {
			return err
		}
		copy := cloneAccumulatorDigest(d)
		a.single, a.started = &copy, true
		return nil
	}

	// Stage only new entries. Failed additions cannot change the prior union,
	// its metadata, or its budget. Existing entries and hash buckets are reused.
	base := a.fragments
	baseIndex := a.index
	used, entries := a.fragmentBytes, a.fragmentEntries
	var added []MemoryFragment
	staged := make(map[[sha256.Size]byte][]int)
	addFragments := func(d MemoryDigest) error {
		fragments := d.Fragments
		if len(fragments) == 0 {
			fragments = memoryFragments(d)
		}
		for _, f := range fragments {
			if f.SourceSnapshot == "" {
				continue
			}
			h := sha256.New()
			n, count, err := memoryAccumulatorSize(f, a.maxBytes, h)
			if err != nil {
				return err
			}
			var key [sha256.Size]byte
			h.Sum(key[:0])
			duplicate := false
			for _, i := range baseIndex[key] {
				if accumulatorFragmentsEqual(base[i], f) {
					duplicate = true
					break
				}
			}
			if !duplicate {
				for _, i := range staged[key] {
					if accumulatorFragmentsEqual(added[i], f) {
						duplicate = true
						break
					}
				}
			}
			if duplicate {
				continue
			}
			count++ // The standalone fragment is also a retained collection entry.
			if n > a.maxBytes-used || !accumulatorBudgetOK(used+n, entries+count, a.maxBytes) {
				return ErrMemoryProjectionLimit
			}
			used, entries = used+n, entries+count
			staged[key] = append(staged[key], len(added))
			added = append(added, cloneAccumulatorFragment(f))
		}
		return nil
	}

	prior := a.latest
	if a.single != nil {
		prior = *a.single
	}
	// A legacy fold can attach inherited opaque content to the latest input's
	// SnapshotID. On the next Add, the pairwise helper synthesizes a fragment
	// from that carry even though the original input had no attributed content.
	if a.single != nil || len(base) == 0 {
		if err := addFragments(prior); err != nil {
			return err
		}
	}
	if err := addFragments(d); err != nil {
		return err
	}
	latest := d
	latest.Summary, latest.KeyFacts, latest.OpenTasks, latest.Fragments = "", nil, nil, nil
	if prior.ClaimsVersion > latest.ClaimsVersion {
		latest.ClaimsVersion = prior.ClaimsVersion
	}
	// An attributed input, including a fragment with an empty source, switches
	// MergeDigests to rendering and discards any unattributed legacy prose.
	priorAttributed := len(base) > 0 || accumulatorHasFragments(prior)
	if len(base)+len(added) == 0 && !priorAttributed && !accumulatorHasFragments(d) {
		var err error
		latest, err = accumulatorLegacy(prior, d, a.maxBytes)
		if err != nil {
			return err
		}
		if prior.ClaimsVersion > latest.ClaimsVersion {
			latest.ClaimsVersion = prior.ClaimsVersion
		}
	}
	n, count, err := memoryAccumulatorSize(latest, a.maxBytes, nil)
	if err != nil || n > a.maxBytes-used || !accumulatorBudgetOK(used+n, entries+count, a.maxBytes) {
		return ErrMemoryProjectionLimit
	}

	latest = cloneAccumulatorDigest(latest)
	if a.index == nil {
		a.index = make(map[[sha256.Size]byte][]int)
	}
	for key, positions := range staged {
		for _, i := range positions {
			a.index[key] = append(a.index[key], len(base)+i)
		}
	}
	a.fragments = append(base, added...)
	a.fragmentBytes, a.fragmentEntries = used, entries
	a.single, a.latest = nil, latest
	return nil
}

func (a *MemoryAccumulator) Digest() (MemoryDigest, error) {
	var out MemoryDigest
	if a.single != nil {
		out = *a.single
	} else if a.started {
		out = a.latest
		if len(a.fragments) > 0 {
			out.Fragments = a.fragments
			if out.HasMemoryClaims() && out.ClaimsVersion == 0 {
				out.ClaimsVersion = MemoryClaimsVersion
			}
			// Retained summaries and per-entry bookkeeping already bound the
			// renderer's join (including separators and fallback headings). The
			// wire guard below also charges duplicated rendered text and escaping.
			renderMemoryFragments(&out)
		}
	}
	// Rendering can repeat fragment content at the top level. Count its exact
	// JSON size without allocating a second complete serialization.
	s := accumulatorJSON{limit: a.maxBytes, wireOnly: true}
	s.value(reflect.ValueOf(out))
	if s.failed {
		return MemoryDigest{}, ErrMemoryProjectionLimit
	}
	return cloneAccumulatorDigest(out), nil
}

func accumulatorHasFragments(d MemoryDigest) bool {
	return len(d.Fragments) > 0 || (d.SnapshotID != "" && (d.Summary != "" || len(d.KeyFacts) > 0 || len(d.OpenTasks) > 0))
}

func accumulatorBudgetOK(n, entries, limit int) bool {
	return limit >= memoryAccumulatorBaseBytes && n >= 0 && entries >= 0 &&
		entries <= memoryAccumulatorMaxEntries &&
		n <= limit-memoryAccumulatorBaseBytes &&
		entries <= (limit-memoryAccumulatorBaseBytes-n)/memoryAccumulatorEntryBytes
}

func memoryAccumulatorSize(v any, limit int, h hash.Hash) (int, int, error) {
	s := accumulatorJSON{limit: limit, hash: h}
	s.value(reflect.ValueOf(v))
	if s.failed || !accumulatorBudgetOK(s.n, s.entries, limit) {
		return 0, 0, ErrMemoryProjectionLimit
	}
	return s.n, s.entries, nil
}

// Compare canonical field values even for a hash collision, without allocating
// two serialized fragments. Empty omitted slices are equivalent; code.paths
// distinguishes null from []. Malformed UTF-8 follows encoding/json exactly.
func accumulatorFragmentsEqual(a, b MemoryFragment) bool {
	if !accumulatorStringEqual(string(a.SourceSnapshot), string(b.SourceSnapshot)) ||
		!accumulatorStringEqual(a.Summary, b.Summary) || a.TasksAuthoritative != b.TasksAuthoritative ||
		!accumulatorStringListEqual(a.KeyFacts, b.KeyFacts) || !accumulatorStringListEqual(a.OpenTasks, b.OpenTasks) ||
		len(a.Claims) != len(b.Claims) {
		return false
	}
	for i, x := range a.Claims {
		y := b.Claims[i]
		if !accumulatorStringEqual(x.Kind, y.Kind) || !accumulatorStringEqual(x.Text, y.Text) || (x.Code == nil) != (y.Code == nil) {
			return false
		}
		if x.Code != nil && (!accumulatorStringEqual(x.Code.Commit, y.Code.Commit) ||
			!accumulatorStringEqual(x.Code.Parent, y.Code.Parent) || (x.Code.Paths == nil) != (y.Code.Paths == nil) ||
			!accumulatorStringListEqual(x.Code.Paths, y.Code.Paths)) {
			return false
		}
	}
	return true
}

func accumulatorStringListEqual(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i, s := range a {
		if !accumulatorStringEqual(s, b[i]) {
			return false
		}
	}
	return true
}

func accumulatorStringEqual(a, b string) bool {
	if a == b {
		return true
	}
	if utf8.ValidString(a) && utf8.ValidString(b) {
		return false
	}
	for len(a) > 0 && len(b) > 0 {
		x, nx := utf8.DecodeRuneInString(a)
		y, ny := utf8.DecodeRuneInString(b)
		if x != y || nx != ny {
			return false
		}
		a, b = a[nx:], b[ny:]
	}
	return len(a) == len(b)
}

func accumulatorLegacy(prior, fresh MemoryDigest, limit int) (MemoryDigest, error) {
	out := fresh
	if len(fresh.Summary) > limit {
		return MemoryDigest{}, ErrMemoryProjectionLimit
	}
	if prior.Summary != "" && prior.Summary != fresh.Summary && !strings.Contains(strings.ToLower(fresh.Summary), strings.ToLower(prior.Summary)) {
		if fresh.Summary == "" {
			out.Summary = prior.Summary
		} else {
			if limit < 2 || len(prior.Summary) > limit-2-len(fresh.Summary) {
				return MemoryDigest{}, ErrMemoryProjectionLimit
			}
			out.Summary = prior.Summary + "\n\n" + fresh.Summary
		}
	}
	var err error
	out.KeyFacts, err = accumulatorStrings(limit, prior.KeyFacts, fresh.KeyFacts)
	if err != nil {
		return MemoryDigest{}, err
	}
	if fresh.TasksAuthoritative {
		out.OpenTasks, err = accumulatorStrings(limit, fresh.OpenTasks)
	} else {
		out.OpenTasks, err = accumulatorStrings(limit, prior.OpenTasks, fresh.OpenTasks)
	}
	return out, err
}

func accumulatorStrings(limit int, groups ...[]string) ([]string, error) {
	seen := make(map[string]bool)
	var out []string
	used := 0
	for _, group := range groups {
		for _, s := range group {
			if s == "" || seen[s] {
				continue
			}
			n, _, err := memoryAccumulatorSize(s, limit, nil)
			if err != nil || n > limit-used || !accumulatorBudgetOK(used+n, len(out)+1, limit) {
				return nil, ErrMemoryProjectionLimit
			}
			used += n
			seen[s] = true
			out = append(out, s)
		}
	}
	return out, nil
}

func cloneAccumulatorDigest(d MemoryDigest) MemoryDigest {
	d.SnapshotID = ContentHash(strings.Clone(string(d.SnapshotID)))
	d.PreviousMemoryHash = ContentHash(strings.Clone(string(d.PreviousMemoryHash)))
	d.Provider = ProviderKind(strings.Clone(string(d.Provider)))
	d.Summary = strings.Clone(d.Summary)
	d.KeyFacts = cloneAccumulatorStrings(d.KeyFacts)
	d.OpenTasks = cloneAccumulatorStrings(d.OpenTasks)
	if d.Fragments != nil {
		f := make([]MemoryFragment, len(d.Fragments))
		for i := range f {
			f[i] = cloneAccumulatorFragment(d.Fragments[i])
		}
		d.Fragments = f
	}
	if d.GraftCoverage != nil {
		c := *d.GraftCoverage
		c.LineageFingerprint = ContentHash(strings.Clone(string(c.LineageFingerprint)))
		cloneHashes := func(in []ContentHash) []ContentHash {
			if in == nil {
				return nil
			}
			out := make([]ContentHash, len(in))
			for i, s := range in {
				out[i] = ContentHash(strings.Clone(string(s)))
			}
			return out
		}
		c.GraftParents, c.PinnedSources = cloneHashes(c.GraftParents), cloneHashes(c.PinnedSources)
		d.GraftCoverage = &c
	}
	return d
}

func cloneAccumulatorFragment(f MemoryFragment) MemoryFragment {
	f.SourceSnapshot = ContentHash(strings.Clone(string(f.SourceSnapshot)))
	f.Summary = strings.Clone(f.Summary)
	f.KeyFacts, f.OpenTasks = cloneAccumulatorStrings(f.KeyFacts), cloneAccumulatorStrings(f.OpenTasks)
	if f.Claims != nil {
		claims := make([]MemoryClaim, len(f.Claims))
		for i, c := range f.Claims {
			c.Kind, c.Text = strings.Clone(c.Kind), strings.Clone(c.Text)
			if c.Code != nil {
				scope := *c.Code
				scope.Commit, scope.Parent = strings.Clone(scope.Commit), strings.Clone(scope.Parent)
				scope.Paths = cloneAccumulatorStrings(scope.Paths)
				c.Code = &scope
			}
			claims[i] = c
		}
		f.Claims = claims
	}
	return f
}

func cloneAccumulatorStrings(in []string) []string {
	if in == nil {
		return nil
	}
	out := make([]string, len(in))
	for i, s := range in {
		out[i] = strings.Clone(s)
	}
	return out
}

// accumulatorJSON streams exactly the encoding/json representation of the
// memory structs (strings, unsigned integers, booleans, slices and pointers).
// It stops before crossing the byte/entry bound, even for one huge string or
// slice. No full JSON buffer or variable-sized hash key is constructed.
type accumulatorJSON struct {
	limit, n, entries int
	failed            bool
	wireOnly          bool // Rendering repeats entries already charged on retention.
	hash              hash.Hash
	buffer            [4096]byte
}

func (s *accumulatorJSON) write(text string) {
	if s.failed {
		return
	}
	if len(text) > s.limit-s.n {
		s.failed = true
		return
	}
	s.n += len(text)
	if s.hash != nil {
		for len(text) > 0 {
			n := copy(s.buffer[:], text)
			s.hash.Write(s.buffer[:n])
			text = text[n:]
		}
	}
}

func (s *accumulatorJSON) quoted(text string) {
	if s.failed {
		return
	}
	if s.limit-s.n < 2 || len(text) > s.limit-s.n-2 {
		s.failed = true
		return
	}
	s.write("\"")
	start := 0
	for i := 0; i < len(text) && !s.failed; {
		var escape string
		n := 1
		b := text[i]
		switch b {
		case '\\':
			escape = "\\\\"
		case '"':
			escape = "\\\""
		case '\n':
			escape = "\\n"
		case '\r':
			escape = "\\r"
		case '\t':
			escape = "\\t"
		case '\b':
			escape = "\\b"
		case '\f':
			escape = "\\f"
		case '<':
			escape = "\\u003c"
		case '>':
			escape = "\\u003e"
		case '&':
			escape = "\\u0026"
		default:
			if b < 0x20 {
				const hex = "0123456789abcdef"
				s.write(text[start:i])
				s.write("\\u00")
				s.write(string([]byte{hex[b>>4], hex[b&15]}))
				start = i + 1
			} else if b >= utf8.RuneSelf {
				r, width := utf8.DecodeRuneInString(text[i:])
				n = width
				if r == utf8.RuneError && width == 1 {
					escape = "\\ufffd"
				}
				if r == '\u2028' {
					escape = "\\u2028"
				}
				if r == '\u2029' {
					escape = "\\u2029"
				}
			}
		}
		if escape != "" {
			s.write(text[start:i])
			s.write(escape)
			start = i + n
		}
		i += n
	}
	s.write(text[start:])
	s.write("\"")
}

func (s *accumulatorJSON) value(v reflect.Value) {
	if s.failed {
		return
	}
	switch v.Kind() {
	case reflect.Pointer:
		if v.IsNil() {
			s.write("null")
		} else {
			s.value(v.Elem())
		}
	case reflect.String:
		s.quoted(v.String())
	case reflect.Bool:
		s.write(strconv.FormatBool(v.Bool()))
	case reflect.Uint32, reflect.Uint64:
		s.write(strconv.FormatUint(v.Uint(), 10))
	case reflect.Slice:
		if v.IsNil() {
			s.write("null")
			return
		}
		if !s.wireOnly && v.Len() > memoryAccumulatorMaxEntries-s.entries {
			s.failed = true
			return
		}
		if !s.wireOnly {
			s.entries += v.Len()
		}
		s.write("[")
		for i := 0; i < v.Len() && !s.failed; i++ {
			if i > 0 {
				s.write(",")
			}
			s.value(v.Index(i))
		}
		s.write("]")
	case reflect.Struct:
		s.write("{")
		comma := false
		for i := 0; i < v.NumField() && !s.failed; i++ {
			field := v.Type().Field(i)
			name, options, _ := strings.Cut(field.Tag.Get("json"), ",")
			if name == "-" {
				continue
			}
			f := v.Field(i)
			if options == "omitempty" && (f.IsZero() || (f.Kind() == reflect.Slice && f.Len() == 0)) {
				continue
			}
			if comma {
				s.write(",")
			}
			comma = true
			s.quoted(name)
			s.write(":")
			s.value(f)
		}
		s.write("}")
	default:
		// Fail closed if the memory schema gains an unsupported field type.
		s.failed = true
	}
}
