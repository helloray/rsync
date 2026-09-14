package receiver

import (
	"sort"
	"testing"

	"github.com/gokrazy/rsync"
	"github.com/gokrazy/rsync/internal/flist"
)

func rdir(name string) *flist.FileEntry {
	return &flist.FileEntry{Name: name, Mode: 0o755 | rsync.S_IFDIR}
}
func rreg(name string) *flist.FileEntry {
	return &flist.FileEntry{Name: name, Mode: 0o644 | rsync.S_IFREG}
}

// drainNext runs one generator step, asserting the outcome kind.
func drainNext(t *testing.T, inc *incRecv, wantName string, wantRel, wantDone bool) {
	t.Helper()
	f, rel, done, err := inc.next()
	if err != nil {
		t.Fatalf("next: %v", err)
	}
	if rel != wantRel || done != wantDone {
		t.Fatalf("next = rel=%v done=%v, want rel=%v done=%v", rel, done, wantRel, wantDone)
	}
	if !wantRel && !wantDone {
		if f == nil || f.Name != wantName {
			t.Fatalf("next = %v, want %q", f, wantName)
		}
	}
}

// TestIncRecvSegmentFlow drives the receiver state machine through a three
// segment transfer (initial list + two dir segments), checking wire ndx
// assignment (including the +1 gaps), in-order queue drain, and the
// release-DONE points: a segment is released once fully processed and its
// successor has arrived; the last segment is freed by the phase-1 marker.
func TestIncRecvSegmentFlow(t *testing.T) {
	rt := &Transfer{Opts: &TransferOpts{DeleteMode: true}}
	inc := newIncRecv(nil)

	seg0 := []*flist.FileEntry{rdir("."), rdir("a"), rreg("top.txt")}
	seg1 := []*flist.FileEntry{rdir("a/b"), rreg("a/c.txt")}
	seg2 := []*flist.FileEntry{rreg("a/b/f")}
	for _, seg := range []struct {
		dirIdx int32
		fes    []*flist.FileEntry
	}{{-1, seg0}, {1, seg1}, {2, seg2}} {
		if _, err := inc.pushEntries(rt, seg.dirIdx, seg.fes); err != nil {
			t.Fatalf("pushEntries(dirIdx=%d): %v", seg.dirIdx, err)
		}
	}

	// ndx chain: 1..3, gap, 5..6, gap, 8. Each segment's ndx refers to the
	// FNameCmp-sorted positions (the C sender sorts after writing the wire
	// entries), so files precede dirs: seg0 sorts [".", "top.txt", "a"].
	for ndx, want := range map[int32]string{
		1: ".", 2: "top.txt", 3: "a",
		5: "a/c.txt", 6: "a/b",
		8: "a/b/f",
	} {
		if got := inc.lookup(ndx); got == nil || got.Name != want {
			t.Fatalf("lookup(%d) = %v, want %q", ndx, got, want)
		}
	}
	for _, ndx := range []int32{0, 4, 7, 9} {
		if inc.lookup(ndx) != nil {
			t.Errorf("lookup(%d) unexpectedly resolved (gap ndx)", ndx)
		}
	}

	drainNext(t, inc, ".", false, false)       // ndx 1
	drainNext(t, inc, "top.txt", false, false) // ndx 2
	drainNext(t, inc, "a", false, false)       // ndx 3
	// all of segment 0 processed and segment 1 has arrived → release
	drainNext(t, inc, "", true, false)
	drainNext(t, inc, "a/c.txt", false, false) // ndx 5
	drainNext(t, inc, "a/b", false, false)     // ndx 6
	// segment 1 done, segment 2 arrived → release
	drainNext(t, inc, "", true, false)
	drainNext(t, inc, "a/b/f", false, false) // ndx 8
	// last segment: no release, wait for listsDone
	inc.markListsDone()
	drainNext(t, inc, "", false, true)

	if got := len(inc.dirs); got != 3 {
		t.Errorf("dirs = %d entries, want 3", got)
	}
	del := inc.deleteList()
	if len(del) != 6 {
		t.Fatalf("deleteList = %d names, want 6", len(del))
	}
	if !sort.StringsAreSorted(del) {
		t.Errorf("deleteList not sorted by name: %v", del)
	}
}

// TestIncRecvEmptySegments checks that childless dirs (empty segments)
// release in sequence with no entries to process, and that their arrival
// wakes the generator (pushEntries broadcasts even for zero entries).
func TestIncRecvEmptySegments(t *testing.T) {
	rt := &Transfer{Opts: &TransferOpts{DeleteMode: true}}
	inc := newIncRecv(nil)

	seg0 := []*flist.FileEntry{rdir("."), rdir("a"), rdir("b")}
	if _, err := inc.pushEntries(rt, -1, seg0); err != nil {
		t.Fatalf("pushEntries: %v", err)
	}
	// segments for a (dirIdx 1) and b (dirIdx 2), both empty
	if _, err := inc.pushEntries(rt, 1, nil); err != nil {
		t.Fatalf("pushEntries(a): %v", err)
	}
	if _, err := inc.pushEntries(rt, 2, nil); err != nil {
		t.Fatalf("pushEntries(b): %v", err)
	}

	drainNext(t, inc, ".", false, false)
	drainNext(t, inc, "a", false, false)
	drainNext(t, inc, "b", false, false)
	// both empty segments release back-to-back once seg 0 is drained
	drainNext(t, inc, "", true, false)
	drainNext(t, inc, "", true, false)
	inc.markListsDone()
	drainNext(t, inc, "", false, true)
}

// TestIncRecvParentValidation checks the segment header validation: the dir
// index must reference a previously received directory, and every entry must
// name that directory as its parent.
func TestIncRecvParentValidation(t *testing.T) {
	rt := &Transfer{Opts: &TransferOpts{DeleteMode: true}}
	inc := newIncRecv(nil)
	if _, err := inc.pushEntries(rt, -1, []*flist.FileEntry{rdir("."), rdir("a")}); err != nil {
		t.Fatalf("pushEntries: %v", err)
	}

	if _, err := inc.pushEntries(rt, 5, nil); err == nil {
		t.Error("out-of-range dir index accepted")
	}
	if _, err := inc.pushEntries(rt, 1, []*flist.FileEntry{rreg("b/x")}); err == nil {
		t.Error("entry with mismatched parent accepted")
	}
	if _, err := inc.pushEntries(rt, 1, []*flist.FileEntry{rreg("a/x")}); err != nil {
		t.Errorf("valid segment rejected: %v", err)
	}
}

// TestIncRecvFreeOne checks the frame loop's list-freeing counter: every DONE
// frees one unreleased list until none remain, mirroring receiver.c:842-862.
func TestIncRecvFreeOne(t *testing.T) {
	rt := &Transfer{Opts: &TransferOpts{DeleteMode: true}}
	inc := newIncRecv(nil)
	if _, err := inc.pushEntries(rt, -1, []*flist.FileEntry{rdir(".")}); err != nil {
		t.Fatalf("pushEntries: %v", err)
	}
	if inc.unfreedLists() != 1 {
		t.Fatalf("unfreed = %d, want 1", inc.unfreedLists())
	}
	if !inc.freeOne() {
		t.Fatal("freeOne = false with a list unfreed")
	}
	if inc.freeOne() {
		t.Fatal("freeOne = true with nothing unfreed")
	}
}

// TestIncRecvRelease checks the per-entry release: a file entry's routing
// slot goes away once its data has been received (releaseFile), while the
// name (for --delete) and the directory entry (for the permission touch-up)
// survive. Releasing twice, or a gap ndx, is a no-op.
func TestIncRecvRelease(t *testing.T) {
	rt := &Transfer{Opts: &TransferOpts{DeleteMode: true}}
	inc := newIncRecv(nil)
	files, err := inc.pushEntries(rt, -1, []*flist.FileEntry{rdir("."), rreg("top.txt")})
	if err != nil {
		t.Fatalf("pushEntries: %v", err)
	}
	if len(files) != 2 || files[0].Ndx != 1 || files[1].Ndx != 2 {
		t.Fatalf("pushEntries files = %+v, want ndx 1 and 2", files)
	}

	inc.releaseFile(2)
	if f := inc.lookup(2); f != nil {
		t.Errorf("released entry still routed: %+v", f)
	}
	inc.releaseFile(2) // idempotent
	inc.releaseFile(3) // gap ndx, no-op
	if inc.lookup(1) == nil {
		t.Error("unreleased entry no longer routed")
	}

	// The released entry's name stays in the delete list, the dir stays in
	// the touch-up list.
	if del := inc.deleteList(); len(del) != 2 || del[0] != "." || del[1] != "top.txt" {
		t.Errorf("deleteList = %v, want [. top.txt]", del)
	}
	if len(inc.dirs) != 1 || inc.dirs[0].Name != "." {
		t.Errorf("dirs = %v, want [.]", inc.dirs)
	}
}

// TestIncRecvSkipRelease checks the generator-side release of skipped entries:
// queued ndx values are applied by the frame loop's applySkipReleases, a full
// queue drops the release (memory cost only), and names/dirs survive.
func TestIncRecvSkipRelease(t *testing.T) {
	rt := &Transfer{Opts: &TransferOpts{DeleteMode: true}}
	inc := newIncRecv(nil)
	if _, err := inc.pushEntries(rt, -1, []*flist.FileEntry{rdir("."), rreg("a.txt"), rreg("b.txt")}); err != nil {
		t.Fatalf("pushEntries: %v", err)
	}

	inc.queueSkipRelease(2)
	inc.applySkipReleases()
	if f := inc.lookup(2); f != nil {
		t.Errorf("skip-released entry still routed: %+v", f)
	}
	if inc.lookup(1) == nil || inc.lookup(3) == nil {
		t.Error("unreleased entries no longer routed")
	}
	inc.queueSkipRelease(3)
	inc.applySkipReleases()
	if inc.lookup(3) != nil {
		t.Error("second skip release not applied")
	}

	// The released entries' names stay in the delete list, the dir stays in
	// the touch-up list.
	if del := inc.deleteList(); len(del) != 3 {
		t.Errorf("deleteList = %v, want 3 names", del)
	}
	if len(inc.dirs) != 1 || inc.dirs[0].Name != "." {
		t.Errorf("dirs = %v, want [.]", inc.dirs)
	}
}

// TestIncRecvNamesGatedOnDelete checks that the name list is only accumulated
// when --delete is in effect: without it, deleteList is never consulted and
// keeping the names would be the dominant per-entry retention left.
func TestIncRecvNamesGatedOnDelete(t *testing.T) {
	rt := &Transfer{Opts: &TransferOpts{}}
	inc := newIncRecv(nil)
	if _, err := inc.pushEntries(rt, -1, []*flist.FileEntry{rdir("."), rreg("top.txt")}); err != nil {
		t.Fatalf("pushEntries: %v", err)
	}
	if del := inc.deleteList(); len(del) != 0 {
		t.Errorf("deleteList = %v without --delete, want empty", del)
	}
}
