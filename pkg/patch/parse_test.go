package patch

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

// mixed.patch is produced by "git diff --binary --full-index" over a tree
// exercising every section shape nit has to handle: delete, binary, path with
// a space, plain modify, add, symlink creation, rename and mode change.
func loadMixed(t *testing.T) []byte {
	t.Helper()

	raw, err := os.ReadFile(filepath.Join("..", "..", "testdata", "patches", "mixed.patch"))
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}

	return raw
}

func TestParseMixed(t *testing.T) {
	set, err := Parse(loadMixed(t))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}

	if len(set.Preamble) != 0 {
		t.Errorf("expected empty preamble, got %q", set.Preamble)
	}

	want := []struct {
		oldPath string
		newPath string
		op      Op
		newKind EntryKind
		binary  bool
		modeChg bool
	}{
		{"docs/gone.md", "", OpDelete, KindUnknown, false, false},
		{"docs/logo.png", "docs/logo.png", OpModify, KindBlob, true, false},
		{"docs/my file.txt", "docs/my file.txt", OpModify, KindBlob, false, false},
		{"secrets/prod.env", "secrets/prod.env", OpModify, KindBlob, false, false},
		{"", "src/added.go", OpAdd, KindBlob, false, false},
		{"src/app.go", "src/app.go", OpModify, KindBlob, false, false},
		{"", "src/leak.link", OpAdd, KindSymlink, false, false},
		// A pure rename carries no mode line at all: the mode is unchanged,
		// whatever it was upstream. KindUnknown is the honest answer here, and
		// it is safe: a section that states no mode cannot introduce a symlink.
		{"src/old.txt", "src/new.txt", OpRename, KindUnknown, false, false},
		{"src/run.sh", "src/run.sh", OpModify, KindBlob, false, true},
	}

	if len(set.Changes) != len(want) {
		t.Fatalf("got %d changes, want %d", len(set.Changes), len(want))
	}

	for i, w := range want {
		c := set.Changes[i]

		if c.Index != i {
			t.Errorf("change %d: Index = %d", i, c.Index)
		}
		if c.OldPath != w.oldPath || c.NewPath != w.newPath {
			t.Errorf("change %d: paths = %q -> %q, want %q -> %q",
				i, c.OldPath, c.NewPath, w.oldPath, w.newPath)
		}
		if c.Op != w.op {
			t.Errorf("change %d (%s): Op = %s, want %s", i, c.DisplayPath(), c.Op, w.op)
		}
		if c.NewKind != w.newKind {
			t.Errorf("change %d (%s): NewKind = %s, want %s", i, c.DisplayPath(), c.NewKind, w.newKind)
		}
		if c.IsBinary != w.binary {
			t.Errorf("change %d (%s): IsBinary = %v, want %v", i, c.DisplayPath(), c.IsBinary, w.binary)
		}
		if c.ModeChanged() != w.modeChg {
			t.Errorf("change %d (%s): ModeChanged = %v (%o -> %o), want %v",
				i, c.DisplayPath(), c.ModeChanged(), c.OldMode, c.NewMode, w.modeChg)
		}
	}
}

// The whole point of the section-splitting approach: keeping every section must
// reproduce the input byte for byte.
func TestRenderKeepAllIsIdentity(t *testing.T) {
	raw := loadMixed(t)

	set, err := Parse(raw)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}

	got := set.Render(func(*Change) bool { return true })

	if !bytes.Equal(got, raw) {
		t.Fatalf("render is not byte-identical to input\n got %d bytes\nwant %d bytes", len(got), len(raw))
	}
}

// Sections must partition the input: concatenating them in order, after the
// preamble, must also reproduce it exactly. This is what guarantees no byte is
// silently dropped or duplicated at a boundary.
func TestSectionsPartitionInput(t *testing.T) {
	raw := loadMixed(t)

	set, err := Parse(raw)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}

	var buf bytes.Buffer
	buf.Write(set.Preamble)

	for _, c := range set.Changes {
		buf.Write(c.Raw())
	}

	if !bytes.Equal(buf.Bytes(), raw) {
		t.Fatal("sections do not partition the input")
	}
}

func TestRenderDropsSections(t *testing.T) {
	raw := loadMixed(t)

	set, err := Parse(raw)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}

	got := set.Render(func(c *Change) bool {
		return c.DisplayPath() != "secrets/prod.env"
	})

	// Only the *section* for the dropped path must disappear. The string may
	// legitimately survive elsewhere: the fixture also adds a symlink whose
	// target is "../secrets/prod.env", which is exactly the leak that
	// path-name-only filtering cannot see and that enforce must catch.
	if bytes.Contains(got, []byte("diff --git a/secrets/prod.env")) {
		t.Error("filtered patch still contains the dropped section")
	}
	if !bytes.Contains(got, []byte("src/app.go")) {
		t.Error("filtered patch lost a kept path")
	}

	// The result must still be a parseable patch with exactly one section less.
	refiltered, err := Parse(got)
	if err != nil {
		t.Fatalf("re-parse filtered patch: %v", err)
	}
	if len(refiltered.Changes) != len(set.Changes)-1 {
		t.Errorf("got %d changes after filtering, want %d",
			len(refiltered.Changes), len(set.Changes)-1)
	}
}

func TestRenderNothingKeptReturnsNil(t *testing.T) {
	set, err := Parse(loadMixed(t))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}

	if got := set.Render(func(*Change) bool { return false }); got != nil {
		t.Errorf("expected nil, got %d bytes", len(got))
	}
}

func TestPaths(t *testing.T) {
	set, err := Parse(loadMixed(t))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}

	paths := set.Paths()

	// The rename contributes both of its sides.
	var hasOld, hasNew bool
	for _, p := range paths {
		hasOld = hasOld || p == "src/old.txt"
		hasNew = hasNew || p == "src/new.txt"
	}

	if !hasOld || !hasNew {
		t.Errorf("rename sides missing from Paths(): %v", paths)
	}
}

// A hunk body line that looks like a section header must not split a section:
// content lines always carry a prefix character.
func TestContentLineIsNotASectionHeader(t *testing.T) {
	raw := []byte("diff --git a/a.txt b/a.txt\n" +
		"index 1111111..2222222 100644\n" +
		"--- a/a.txt\n" +
		"+++ b/a.txt\n" +
		"@@ -1,2 +1,2 @@\n" +
		"-diff --git a/x b/x\n" +
		"+diff --git a/y b/y\n" +
		" tail\n")

	set, err := Parse(raw)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}

	if len(set.Changes) != 1 {
		t.Fatalf("got %d sections, want 1", len(set.Changes))
	}
}

func TestParseRejectsCombinedDiff(t *testing.T) {
	raw := []byte("diff --cc file.txt\nindex 111,222..333\n")

	if _, err := Parse(raw); err != ErrCombinedDiff {
		t.Errorf("got %v, want ErrCombinedDiff", err)
	}
}

func TestParseEmpty(t *testing.T) {
	if _, err := Parse([]byte("not a patch at all\n")); err != ErrEmpty {
		t.Errorf("got %v, want ErrEmpty", err)
	}
}

func TestParsePreservesPreamble(t *testing.T) {
	preamble := "From 0000 Mon Sep 17 00:00:00 2001\nSubject: [PATCH] test\n\n"
	raw := []byte(preamble +
		"diff --git a/a.txt b/a.txt\n" +
		"index 1111111..2222222 100644\n" +
		"--- a/a.txt\n" +
		"+++ b/a.txt\n" +
		"@@ -1 +1 @@\n" +
		"-a\n" +
		"+b\n")

	set, err := Parse(raw)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}

	if string(set.Preamble) != preamble {
		t.Errorf("preamble = %q, want %q", set.Preamble, preamble)
	}
}
