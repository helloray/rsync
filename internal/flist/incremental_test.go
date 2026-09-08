package flist

import (
	"testing"
)

func dir(name string) *FileEntry {
	return &FileEntry{Name: name, Mode: 0o755 | 0o040000} // S_IFDIR
}

func reg(name string) *FileEntry {
	return &FileEntry{Name: name, Mode: 0o644 | 0o100000} // S_IFREG
}

func drain(l *IncrementalFileList) []string {
	var names []string
	for {
		f := l.Pop()
		if f == nil {
			return names
		}
		names = append(names, f.Name)
	}
}

func TestParentPath(t *testing.T) {
	for _, tt := range []struct{ in, want string }{
		{".", ""},
		{"", ""},
		{"file", "."},
		{"sub/file", "sub"},
		{"a/b/c", "a/b"},
	} {
		if got := ParentPath(tt.in); got != tt.want {
			t.Errorf("ParentPath(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

func TestIncrementalPushPop(t *testing.T) {
	l := NewIncrementalFileList()

	// A file whose parent (root) exists becomes ready immediately.
	if !l.Push(reg("a.txt")) {
		t.Fatal("root-level file should be ready immediately")
	}
	// A child of a not-yet-seen directory is held pending.
	if l.Push(reg("sub/b.txt")) {
		t.Fatal("child of unseen dir must be pending")
	}
	if got := l.PendingCount(); got != 1 {
		t.Fatalf("pending = %d, want 1", got)
	}
	// The directory releases the pending child.
	if !l.Push(dir("sub")) {
		t.Fatal("dir under root should be ready immediately")
	}
	want := []string{"a.txt", "sub", "sub/b.txt"}
	if got := drain(l); len(got) != len(want) {
		t.Fatalf("drain = %v, want %v", got, want)
	}
}

func TestIncrementalNestedPending(t *testing.T) {
	l := NewIncrementalFileList()

	// Deeply nested entries arriving before any parent.
	l.Push(reg("a/b/c.txt"))
	if got := drain(l); len(got) != 0 {
		t.Fatalf("expected nothing ready, got %v", got)
	}
	l.Push(dir("a"))
	if got := drain(l); len(got) != 1 || got[0] != "a" {
		t.Fatalf("expected only [a] ready, got %v", got)
	}
	l.Push(dir("a/b"))
	got := drain(l)
	want := []string{"a/b", "a/b/c.txt"}
	if len(got) != len(want) {
		t.Fatalf("drain = %v, want %v", got, want)
	}
	if l.PendingCount() != 0 {
		t.Fatalf("pending = %d, want 0", l.PendingCount())
	}
}

func TestIncrementalMarkDirCreated(t *testing.T) {
	l := NewIncrementalFileList()
	l.Push(reg("existing/f.txt"))
	if got := drain(l); len(got) != 0 {
		t.Fatalf("expected pending, got %v", got)
	}
	l.MarkDirCreated("existing")
	got := drain(l)
	if len(got) != 1 || got[0] != "existing/f.txt" {
		t.Fatalf("drain = %v, want [existing/f.txt]", got)
	}
}

func TestIncrementalFinalize(t *testing.T) {
	l := NewIncrementalFileList()

	// Orphans: parents "x" and "x/y" never arrive.
	l.Push(reg("x/y/z.txt"))
	l.Push(reg("w.txt")) // ready immediately
	l.Pop()
	l.Finalize()

	got := drain(l)
	if len(got) != 1 || got[0] != "x/y/z.txt" {
		t.Fatalf("after finalize drain = %v, want [x/y/z.txt]", got)
	}
	if l.PendingCount() != 0 {
		t.Fatalf("pending = %d, want 0", l.PendingCount())
	}
	if l.finalizedPlaceholders != 2 { // "x" and "x/y"
		t.Fatalf("placeholders = %d, want 2", l.finalizedPlaceholders)
	}
}

func TestIncrementalFinalizeNoOrphans(t *testing.T) {
	l := NewIncrementalFileList()
	l.Push(reg("top.txt"))
	l.Finalize()
	if l.finalizedPlaceholders != 0 {
		t.Fatalf("placeholders = %d, want 0", l.finalizedPlaceholders)
	}
}
