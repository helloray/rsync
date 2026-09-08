package sender

import (
	"testing"

	"github.com/gokrazy/rsync/internal/flist"
)

func sdir(name string) *flist.FileEntry { return &flist.FileEntry{Name: name, Mode: 0o755 | 0o040000} }
func sreg(name string) *flist.FileEntry { return &flist.FileEntry{Name: name, Mode: 0o644 | 0o100000} }

func names(segs []incSegment, entries []*flist.FileEntry, i int) []string {
	var out []string
	for _, idx := range segs[i].idxs {
		out = append(out, entries[idx].Name)
	}
	return out
}

func eq(t *testing.T, got, want []string) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for i := range got {
		if got[i] != want[i] {
			t.Fatalf("got %v, want %v", got, want)
		}
	}
}

// TestIncSchedDotDir checks the partition of a dot-dir transfer: the initial
// list carries the dot-dir itself plus top-level entries; every deeper dir
// gets its own segment in DFS emission order with C's dir index numbering.
func TestIncSchedDotDir(t *testing.T) {
	// global FNameCmp order (DFS pre-order)
	entries := []*flist.FileEntry{
		sdir("."),       // 0
		sdir("a"),       // 1
		sdir("a/b"),     // 2
		sreg("a/b/f"),   // 3
		sreg("a/c.txt"), // 4
		sreg("top.txt"), // 5
	}
	s := newIncSched(nil, nil, entries, 0)

	if len(s.segs) != 3 {
		t.Fatalf("got %d segments, want 3", len(s.segs))
	}
	eq(t, names(s.segs, entries, 0), []string{".", "a", "top.txt"})
	eq(t, names(s.segs, entries, 1), []string{"a/b", "a/c.txt"})
	eq(t, names(s.segs, entries, 2), []string{"a/b/f"})

	if got := s.segs[0].dirIdx; got != -1 {
		t.Errorf("initial dirIdx = %d, want -1", got)
	}
	// dir numbering: "." = 0, "a" = 1 (initial), "a/b" = 2 (seg a)
	if got := s.segs[1].dirIdx; got != 1 {
		t.Errorf("seg(a) dirIdx = %d, want 1", got)
	}
	if got := s.segs[2].dirIdx; got != 2 {
		t.Errorf("seg(a/b) dirIdx = %d, want 2", got)
	}

	// ndx_start chain: 1, then 1+3+1, then 5+2+1
	if s.segs[0].ndxStart != 1 || s.segs[1].ndxStart != 5 || s.segs[2].ndxStart != 8 {
		t.Errorf("ndxStart = %d, %d, %d; want 1, 5, 8",
			s.segs[0].ndxStart, s.segs[1].ndxStart, s.segs[2].ndxStart)
	}

	// ndx → flat mapping across segments, including the +1 gaps
	for _, tt := range []struct {
		ndx  int32
		want string
	}{
		{1, "."},
		{2, "a"},
		{3, "top.txt"},
		{5, "a/b"},
		{6, "a/c.txt"},
		{8, "a/b/f"},
	} {
		idx, ok := s.advance(tt.ndx)
		if !ok || entries[idx].Name != tt.want {
			t.Errorf("advance(%d) = (%d, %v), want %q", tt.ndx, idx, ok, tt.want)
		}
	}
	// gap ndx values must not resolve
	for _, ndx := range []int32{0, 4, 7, 9} {
		if _, ok := s.advance(ndx); ok {
			t.Errorf("advance(%d) unexpectedly resolved (gap ndx)", ndx)
		}
	}
}

// TestIncSchedSiblingDash checks dir numbering when a sibling name sorts
// between a dir and its children ("a-x" < "a/x" in ASCII): the dir index
// space must follow the receiver's append order (initial dirs first), not
// plain flat order of dirs.
func TestIncSchedSiblingDash(t *testing.T) {
	entries := []*flist.FileEntry{
		sdir("."),     // 0
		sdir("a"),     // 1
		sdir("a-x"),   // 2  ("a-x" < "a/x")
		sreg("a/x"),   // 3
		sdir("a/y"),   // 4
		sreg("a/y/z"), // 5
	}
	// verify the assumed sort order
	for i := 1; i < len(entries); i++ {
		if entries[i-1].Name >= entries[i].Name {
			t.Fatalf("test setup: entries not sorted: %q >= %q", entries[i-1].Name, entries[i].Name)
		}
	}
	s := newIncSched(nil, nil, entries, 0)

	// segments: initial [., a, a-x], seg(a) [a/x, a/y], seg(a/y) [a/y/z],
	// seg(a-x) [] (childless dirs still get a segment)
	if len(s.segs) != 4 {
		t.Fatalf("got %d segments, want 4", len(s.segs))
	}
	eq(t, names(s.segs, entries, 0), []string{".", "a", "a-x"})
	eq(t, names(s.segs, entries, 1), []string{"a/x", "a/y"})
	eq(t, names(s.segs, entries, 2), []string{"a/y/z"})
	eq(t, names(s.segs, entries, 3), nil)
	// dir indices: "."=0, "a"=1, "a-x"=2 (initial), "a/y"=3 (seg a)
	if s.segs[1].dirIdx != 1 || s.segs[2].dirIdx != 3 || s.segs[3].dirIdx != 2 {
		t.Errorf("dirIdx = %d, %d, %d; want 1, 3, 2", s.segs[1].dirIdx, s.segs[2].dirIdx, s.segs[3].dirIdx)
	}
	// ndx chain: initial 1..3, seg(a) 5..6, seg(a/y) 8, seg(a-x) 10
	if s.segs[1].ndxStart != 5 || s.segs[2].ndxStart != 8 || s.segs[3].ndxStart != 10 {
		t.Errorf("ndxStart = %d, %d, %d; want 5, 8, 10", s.segs[1].ndxStart, s.segs[2].ndxStart, s.segs[3].ndxStart)
	}
}

// TestIncSchedEmptyDir checks a childless dir still gets an (empty) segment,
// and a single-file transfer gets only the initial list.
func TestIncSchedEmptyDir(t *testing.T) {
	entries := []*flist.FileEntry{
		sdir("."),       // 0
		sdir("empty"),   // 1
		sreg("top.txt"), // 2
	}
	s := newIncSched(nil, nil, entries, 0)
	if len(s.segs) != 2 {
		t.Fatalf("got %d segments, want 2", len(s.segs))
	}
	eq(t, names(s.segs, entries, 1), nil)
	if s.segs[1].dirIdx != 1 || s.segs[1].ndxStart != 5 {
		t.Errorf("seg(empty): dirIdx=%d ndxStart=%d, want 1/5", s.segs[1].dirIdx, s.segs[1].ndxStart)
	}

	single := []*flist.FileEntry{sreg("just-a-file")}
	s2 := newIncSched(nil, nil, single, 0)
	if len(s2.segs) != 1 || s2.segs[0].dirIdx != -1 || s2.segs[0].ndxStart != 1 {
		t.Fatalf("single-file segs = %+v", s2.segs)
	}
}

// TestIncSchedBudgetTracking exercises the lookahead counters: requests
// advance file_old_total, release-done markers free segments.
func TestIncSchedBudgetTracking(t *testing.T) {
	entries := []*flist.FileEntry{
		sdir("."),   // 0
		sdir("d"),   // 1
		sreg("d/f"), // 2
		sreg("d/g"), // 3
		sreg("d/h"), // 4
		sreg("d/i"), // 5
		sreg("d/j"), // 6
		sreg("d/k"), // 7
		sreg("d/l"), // 8
		sreg("d/m"), // 9
	}
	s := newIncSched(nil, nil, entries, 0)
	// initial (2 entries) + one segment for d (8 entries)
	if s.fileTotal != 2 || s.fileOldTotal != 2 {
		t.Fatalf("initial totals = %d/%d, want 2/2", s.fileTotal, s.fileOldTotal)
	}
	// request lands in seg(d): ndxStart = 1+2+1 = 4; cur moves to seg 1 and
	// file_old_total becomes sum(freed..cur) = 2+8 = 10
	if idx, ok := s.advance(5); !ok || entries[idx].Name != "d/g" {
		t.Fatalf("advance(5) = %d, %v", idx, ok)
	}
	if s.curSeg != 1 || s.fileOldTotal != 10 {
		t.Fatalf("after advance: cur=%d old=%d, want cur=1 old=10", s.curSeg, s.fileOldTotal)
	}
	// release the initial list (both segments were sent in this simulation)
	s.sentUpTo = 2
	freed, more := s.releaseDone()
	if !freed || !more {
		t.Fatalf("releaseDone = %v, %v; want true, true (lists remain)", freed, more)
	}
	if s.fileOldTotal != 8 {
		t.Fatalf("after release: old=%d, want 8", s.fileOldTotal)
	}
	// the last segment is freed by the receiver's first phase-done marker;
	// freeing the last list means no "more" (the marker also counts a phase)
	freed, more = s.releaseDone()
	if !freed || more {
		t.Fatalf("releaseDone = %v, %v; want true, false (last list)", freed, more)
	}
	if s.fileOldTotal != 0 {
		t.Fatalf("after final release: old=%d, want 0", s.fileOldTotal)
	}
	if freed, _ := s.releaseDone(); freed {
		t.Fatal("releaseDone = true with nothing to free")
	}
}
