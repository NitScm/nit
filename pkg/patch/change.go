package patch

import "fmt"

// Op is the kind of change a patch section applies to a single path.
type Op string

const (
	OpAdd    Op = "add"
	OpModify Op = "modify"
	OpDelete Op = "delete"
	OpRename Op = "rename"
	OpCopy   Op = "copy"
)

// EntryKind is what a tree entry actually is. A patch that only ever reports
// path names cannot distinguish a regular file from a symlink or a submodule
// pointer, and both of those are authorization escape hatches: a symlink can
// expose a file the author is not allowed to read, and a submodule pointer can
// pull in arbitrary external content.
type EntryKind string

const (
	KindBlob      EntryKind = "blob"
	KindSymlink   EntryKind = "symlink"
	KindSubmodule EntryKind = "submodule"
	KindUnknown   EntryKind = "unknown"
)

// Git tree entry modes, as they appear in patch headers.
const (
	ModeRegular    uint32 = 0o100644
	ModeExecutable uint32 = 0o100755
	ModeSymlink    uint32 = 0o120000
	ModeSubmodule  uint32 = 0o160000
)

// KindOfMode maps a git tree entry mode to the kind of object it designates.
func KindOfMode(mode uint32) EntryKind {
	switch mode {
	case 0:
		return KindUnknown
	case ModeSymlink:
		return KindSymlink
	case ModeSubmodule:
		return KindSubmodule
	default:
		return KindBlob
	}
}

// Change is a single per-file section of a patch, together with the exact byte
// range it occupies in the original patch.
type Change struct {
	// Index is the position of the section in the original patch, starting at 0.
	Index int

	// OldPath is the path before the change; empty for a pure addition.
	OldPath string
	// NewPath is the path after the change; empty for a deletion.
	NewPath string

	Op Op

	OldMode uint32
	NewMode uint32

	OldKind EntryKind
	NewKind EntryKind

	// IsBinary reports whether the section carries a binary delta rather than
	// text hunks.
	IsBinary bool

	// Additions and Deletions count changed lines; both are zero for binary
	// sections and for pure mode changes.
	Additions int
	Deletions int

	// raw is the section's byte range in the original patch, including its
	// "diff --git" header line and everything up to (but excluding) the next
	// section header.
	raw []byte
}

// Raw returns the original bytes of the section. The slice aliases the buffer
// passed to Parse and must not be modified.
func (c *Change) Raw() []byte {
	return c.raw
}

// Size returns the byte length of the section in the original patch.
func (c *Change) Size() int {
	return len(c.raw)
}

// ModeChanged reports whether the tree entry mode differs between the two
// sides. A mode change is a change in its own right: flipping 100644 to 120000
// turns a regular file into a symlink without touching any other path.
func (c *Change) ModeChanged() bool {
	if c.OldMode == 0 || c.NewMode == 0 {
		return false
	}
	return c.OldMode != c.NewMode
}

// Paths returns every path the section touches, without duplicates. A rename
// or a copy touches two paths, and authorization must hold for both of them.
func (c *Change) Paths() []string {
	switch {
	case c.OldPath == "" && c.NewPath == "":
		return nil
	case c.OldPath == "":
		return []string{c.NewPath}
	case c.NewPath == "":
		return []string{c.OldPath}
	case c.OldPath == c.NewPath:
		return []string{c.NewPath}
	default:
		return []string{c.OldPath, c.NewPath}
	}
}

// DisplayPath returns the single path best identifying the section in reports
// and log lines.
func (c *Change) DisplayPath() string {
	if c.NewPath != "" {
		return c.NewPath
	}
	return c.OldPath
}

// String renders a short human-readable summary, for logs and CLI output.
func (c *Change) String() string {
	if c.Op == OpRename || c.Op == OpCopy {
		return fmt.Sprintf("%s %s -> %s", c.Op, c.OldPath, c.NewPath)
	}
	return fmt.Sprintf("%s %s", c.Op, c.DisplayPath())
}
