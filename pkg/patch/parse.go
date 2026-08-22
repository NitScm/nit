package patch

import (
	"bytes"
	"errors"
	"fmt"

	"github.com/bluekeyes/go-gitdiff/gitdiff"
)

var (
	// ErrEmpty is returned when the input contains no file section at all.
	ErrEmpty = errors.New("patch: no file section found")

	// ErrCombinedDiff is returned for merge ("diff --cc") diffs, which have
	// more than two sides and therefore no single authorization subject.
	ErrCombinedDiff = errors.New("patch: combined diffs are not supported")
)

var (
	sectionMarker  = []byte("diff --git ")
	combinedMarker = []byte("diff --cc ")
)

// Set is a parsed patch: an opaque preamble followed by independent per-file
// sections.
type Set struct {
	// Preamble is everything preceding the first file section, preserved
	// verbatim. For "git diff" output it is empty; for "git format-patch"
	// output it holds the commit headers and message.
	Preamble []byte

	// Changes are the file sections in their original order.
	Changes []*Change

	raw []byte
}

// Raw returns the original patch bytes.
func (s *Set) Raw() []byte { return s.raw }

// Parse splits raw into per-file sections and extracts the metadata of each.
// The returned Set aliases raw, which must not be modified afterwards.
func Parse(raw []byte) (*Set, error) {
	bounds, err := splitSections(raw)
	if err != nil {
		return nil, err
	}
	if len(bounds) == 0 {
		return nil, ErrEmpty
	}

	set := &Set{
		Preamble: raw[:bounds[0].start],
		Changes:  make([]*Change, 0, len(bounds)),
		raw:      raw,
	}

	for i, b := range bounds {
		section := raw[b.start:b.end]

		change, err := parseSection(section)
		if err != nil {
			return nil, fmt.Errorf("patch: section %d: %w", i, err)
		}

		change.Index = i
		change.raw = section

		set.Changes = append(set.Changes, change)
	}

	return set, nil
}

// Paths returns every path touched by the patch, without duplicates, in order
// of first appearance.
func (s *Set) Paths() []string {
	seen := make(map[string]struct{}, len(s.Changes))
	paths := make([]string, 0, len(s.Changes))

	for _, c := range s.Changes {
		for _, p := range c.Paths() {
			if _, ok := seen[p]; ok {
				continue
			}
			seen[p] = struct{}{}
			paths = append(paths, p)
		}
	}

	return paths
}

// Render re-emits the patch keeping only the sections for which keep returns
// true. The output is byte-identical to the input for the kept sections: no
// re-serialization happens, the original bytes are copied.
//
// Render returns nil when no section is kept, which callers must treat as "no
// change to apply" rather than as an empty but valid patch.
func (s *Set) Render(keep func(*Change) bool) []byte {
	kept := make([]*Change, 0, len(s.Changes))
	size := 0

	for _, c := range s.Changes {
		if keep == nil || keep(c) {
			kept = append(kept, c)
			size += len(c.raw)
		}
	}

	if len(kept) == 0 {
		return nil
	}

	out := make([]byte, 0, len(s.Preamble)+size)
	out = append(out, s.Preamble...)

	for _, c := range kept {
		out = append(out, c.raw...)
	}

	return out
}

type bound struct {
	start int
	end   int
}

// splitSections locates the byte range of every file section.
//
// A section starts at a line beginning with "diff --git " at column 0. Inside a
// hunk every line is prefixed by ' ', '+', '-' or '\', and binary chunks are
// base85 without spaces, so no line of patch *content* can be mistaken for a
// section header.
func splitSections(raw []byte) ([]bound, error) {
	var bounds []bound

	offset := 0
	for offset < len(raw) {
		lineEnd := bytes.IndexByte(raw[offset:], '\n')
		if lineEnd < 0 {
			lineEnd = len(raw)
		} else {
			lineEnd = offset + lineEnd + 1
		}

		line := raw[offset:lineEnd]

		switch {
		case bytes.HasPrefix(line, sectionMarker):
			if n := len(bounds); n > 0 {
				bounds[n-1].end = offset
			}
			bounds = append(bounds, bound{start: offset, end: len(raw)})

		case bytes.HasPrefix(line, combinedMarker):
			return nil, ErrCombinedDiff
		}

		offset = lineEnd
	}

	return bounds, nil
}

// parseSection extracts the metadata of a single file section.
func parseSection(section []byte) (*Change, error) {
	files, _, err := gitdiff.Parse(bytes.NewReader(section))
	if err != nil {
		return nil, err
	}
	if len(files) != 1 {
		return nil, fmt.Errorf("expected 1 file, got %d", len(files))
	}

	f := files[0]

	change := &Change{
		OldPath:  f.OldName,
		NewPath:  f.NewName,
		Op:       operationOf(f),
		OldMode:  uint32(f.OldMode),
		NewMode:  uint32(f.NewMode),
		IsBinary: f.IsBinary,
	}

	// A header that only carries an "index" line reports a single mode; both
	// sides share it. Normalizing here keeps ModeChanged honest.
	if change.OldMode == 0 && change.NewMode != 0 && !f.IsNew {
		change.OldMode = change.NewMode
	}
	if change.NewMode == 0 && change.OldMode != 0 && !f.IsDelete {
		change.NewMode = change.OldMode
	}

	change.OldKind = KindOfMode(change.OldMode)
	change.NewKind = KindOfMode(change.NewMode)

	for _, frag := range f.TextFragments {
		change.Additions += int(frag.LinesAdded)
		change.Deletions += int(frag.LinesDeleted)
	}

	return change, nil
}

func operationOf(f *gitdiff.File) Op {
	switch {
	case f.IsRename:
		return OpRename
	case f.IsCopy:
		return OpCopy
	case f.IsNew:
		return OpAdd
	case f.IsDelete:
		return OpDelete
	default:
		return OpModify
	}
}
