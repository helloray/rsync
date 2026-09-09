package sender

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"testing"
	"testing/fstest"

	"github.com/gokrazy/rsync"
	"github.com/gokrazy/rsync/internal/flist"
)

func sdir(name string) file { return file{Wpath: name, Mode: 0o755 | rsync.S_IFDIR} }
func sreg(name string) file { return file{Wpath: name, Mode: 0o644 | rsync.S_IFREG} }

func names(got []file) []string {
	var out []string
	for i := range got {
		out = append(out, got[i].Wpath)
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

// newFakeSched builds an incSched over a golden Transfer with an injected
// scanner: scan maps a directory's wire path to its fake children, so
// scheduler tests need no real file system.
func newFakeSched(t *testing.T, initial []file, locs []dirLoc, scan func(wpath string) ([]file, []dirLoc, error)) *incSched {
	t.Helper()
	st, _ := newGoldenTransfer(t, fstest.MapFS{})
	roots := buildIncNodes(st, initial, locs)
	s := newIncSched(st, flist.NewEncoder(st.flistParams()), initial, new(int32),
		make(map[int32]string), make(map[int32]string), roots)
	s.scanDir = func(loc dirLoc) ([]file, []dirLoc, error) {
		return scan(loc.wpath)
	}
	return s
}

// checkInvariants asserts the lookahead counters exactly match the retained
// segment headers (the recompute-based accounting that guards against the
// f79095d deadlock class of incremental drift).
func checkInvariants(t *testing.T, s *incSched) {
	t.Helper()
	total := 0
	for j := s.windowBase; j < len(s.segs); j++ {
		total += int(s.segs[j].n)
	}
	if s.fileTotal != total || s.fileTotal < 0 {
		t.Fatalf("fileTotal = %d, want sum(segs[%d:]) = %d", s.fileTotal, s.windowBase, total)
	}
	old := 0
	for j := s.windowBase; j <= s.curSeg && j < len(s.segs); j++ {
		old += int(s.segs[j].n)
	}
	if s.fileOldTotal != old || s.fileOldTotal < 0 {
		t.Fatalf("fileOldTotal = %d, want sum(segs[%d..%d]) = %d",
			s.fileOldTotal, s.windowBase, s.curSeg, old)
	}
}

// TestIncSchedDotDir checks the partition of a dot-dir transfer: the initial
// list carries the dot-dir itself plus top-level entries; every deeper dir
// gets its own segment in DFS emission order with C's dir index numbering.
func TestIncSchedDotDir(t *testing.T) {
	s := newFakeSched(t,
		[]file{sdir("."), sdir("a"), sreg("top.txt")},
		[]dirLoc{{wpath: "a"}},
		func(wpath string) ([]file, []dirLoc, error) {
			switch wpath {
			case "a":
				// dir "a/b" sorts after file "a/c.txt" (type-first)
				return []file{sdir("a/b"), sreg("a/c.txt")},
					[]dirLoc{{wpath: "a/b"}}, nil
			case "a/b":
				return []file{sreg("a/b/f")}, nil, nil
			}
			return nil, nil, nil
		})

	if err := s.topUp(); err != nil {
		t.Fatal(err)
	}
	if len(s.segs) != 3 {
		t.Fatalf("got %d segments, want 3", len(s.segs))
	}
	eq(t, names(s.window[1].files), []string{"a/b", "a/c.txt"})
	eq(t, names(s.window[2].files), []string{"a/b/f"})

	if got := s.segs[0].node; got != -1 {
		t.Errorf("initial node = %d, want -1", got)
	}
	// dir numbering: "." = 0, "a" = 1 (initial), "a/b" = 2 (seg a)
	if s.segs[1].node != 1 || s.segs[2].node != 2 {
		t.Errorf("seg nodes = %d, %d; want 1, 2", s.segs[1].node, s.segs[2].node)
	}
	// ndx_start chain: 1, then 1+3+1, then 5+2+1
	if s.segs[0].ndxStart != 1 || s.segs[1].ndxStart != 5 || s.segs[2].ndxStart != 8 {
		t.Errorf("ndxStart = %d, %d, %d; want 1, 5, 8",
			s.segs[0].ndxStart, s.segs[1].ndxStart, s.segs[2].ndxStart)
	}
	checkInvariants(t, s)

	// ndx → entry mapping across segments, including the +1 gaps
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
		f, ok := s.advance(tt.ndx)
		if !ok || f.Wpath != tt.want {
			t.Errorf("advance(%d) = (%v, %v), want %q", tt.ndx, f, ok, tt.want)
		}
	}
	// gap ndx values must not resolve
	for _, ndx := range []int32{0, 4, 7, 9} {
		if _, ok := s.advance(ndx); ok {
			t.Errorf("advance(%d) unexpectedly resolved (gap ndx)", ndx)
		}
	}

	// EOF is sent and a repeated topUp emits nothing further
	if !s.eofSent {
		t.Error("topUp did not send NDX_FLIST_EOF")
	}
	n := len(s.segs)
	if err := s.topUp(); err != nil {
		t.Fatal(err)
	}
	if len(s.segs) != n {
		t.Errorf("repeated topUp grew segments to %d", len(s.segs))
	}
}

// TestIncSchedDFSOrder checks dir numbering and emission order when a sibling
// name sorts between a dir and its children ("a-x" < "a/x"): segments follow
// the DFS tree (seg(a), seg(a/y), seg(a-x)), not the flat global order, and
// the dir index space follows the receiver's arrival order.
func TestIncSchedDFSOrder(t *testing.T) {
	s := newFakeSched(t,
		[]file{sdir("."), sdir("a"), sdir("a-x")},
		[]dirLoc{{wpath: "a"}, {wpath: "a-x"}},
		func(wpath string) ([]file, []dirLoc, error) {
			switch wpath {
			case "a":
				return []file{sreg("a/x"), sdir("a/y")}, []dirLoc{{wpath: "a/y"}}, nil
			case "a/y":
				return []file{sreg("a/y/z")}, nil, nil
			case "a-x":
				return nil, nil, nil
			}
			return nil, nil, nil
		})

	if err := s.topUp(); err != nil {
		t.Fatal(err)
	}
	// segments: initial [., a, a-x], seg(a) [a/x, a/y], seg(a/y) [a/y/z],
	// seg(a-x) [] (childless dirs still get a segment)
	if len(s.segs) != 4 {
		t.Fatalf("got %d segments, want 4", len(s.segs))
	}
	eq(t, names(s.window[1].files), []string{"a/x", "a/y"})
	eq(t, names(s.window[2].files), []string{"a/y/z"})
	eq(t, names(s.window[3].files), nil)
	// dir indices: "."=0, "a"=1, "a-x"=2 (initial), "a/y"=3 (seg a); the
	// emission order is DFS: a, a/y, then a-x
	if s.segs[1].node != 1 || s.segs[2].node != 3 || s.segs[3].node != 2 {
		t.Errorf("seg nodes = %d, %d, %d; want 1, 3, 2",
			s.segs[1].node, s.segs[2].node, s.segs[3].node)
	}
	// ndx chain: initial 1..3, seg(a) 5..6, seg(a/y) 8, seg(a-x) 10
	if s.segs[1].ndxStart != 5 || s.segs[2].ndxStart != 8 || s.segs[3].ndxStart != 10 {
		t.Errorf("ndxStart = %d, %d, %d; want 5, 8, 10",
			s.segs[1].ndxStart, s.segs[2].ndxStart, s.segs[3].ndxStart)
	}
	checkInvariants(t, s)
}

// TestIncSchedEmptyDir checks a childless dir still gets an (empty) segment,
// and a single-file transfer gets only the initial list.
func TestIncSchedEmptyDir(t *testing.T) {
	s := newFakeSched(t,
		[]file{sdir("."), sreg("top.txt"), sdir("empty")},
		[]dirLoc{{wpath: "empty"}},
		func(wpath string) ([]file, []dirLoc, error) { return nil, nil, nil })

	if err := s.topUp(); err != nil {
		t.Fatal(err)
	}
	if len(s.segs) != 2 {
		t.Fatalf("got %d segments, want 2", len(s.segs))
	}
	eq(t, names(s.window[1].files), nil)
	// dir indices: "."=0, "empty"=1; ndx chain: initial 1..3, seg(empty) 5
	if s.segs[1].node != 1 || s.segs[1].ndxStart != 5 {
		t.Errorf("seg(empty): node=%d ndxStart=%d, want 1/5", s.segs[1].node, s.segs[1].ndxStart)
	}
	checkInvariants(t, s)

	s2 := newFakeSched(t,
		[]file{sreg("just-a-file")},
		nil,
		func(wpath string) ([]file, []dirLoc, error) { return nil, nil, nil })
	if err := s2.topUp(); err != nil {
		t.Fatal(err)
	}
	if len(s2.segs) != 1 || s2.segs[0].node != -1 || s2.segs[0].ndxStart != 1 {
		t.Fatalf("single-file segs = %+v", s2.segs)
	}
	if !s2.eofSent {
		t.Error("single-file transfer did not send NDX_FLIST_EOF")
	}
}

// TestIncSchedMultiArgMerge checks that the same dir name discovered by two
// args merges into one node whose segment carries both scans' children in
// stable wire order.
func TestIncSchedMultiArgMerge(t *testing.T) {
	calls := 0
	s := newFakeSched(t,
		[]file{sdir("."), sdir("a"), sdir("a"), sreg("top.txt")},
		[]dirLoc{{wpath: "a"}, {wpath: "a"}},
		func(wpath string) ([]file, []dirLoc, error) {
			if wpath != "a" {
				return nil, nil, fmt.Errorf("unexpected scan of %q", wpath)
			}
			calls++
			switch calls {
			case 1:
				return []file{sreg("a/f2")}, nil, nil
			case 2:
				return []file{sreg("a/f1")}, nil, nil
			}
			return nil, nil, nil
		})

	if err := s.topUp(); err != nil {
		t.Fatal(err)
	}
	// one node for "." plus one merged node for "a" (two initial entries)
	if len(s.nodes) != 2 {
		t.Fatalf("got %d nodes, want 2", len(s.nodes))
	}
	if len(s.segs) != 2 {
		t.Fatalf("got %d segments, want 2", len(s.segs))
	}
	// both scans' children land in one stable-sorted segment
	eq(t, names(s.window[1].files), []string{"a/f1", "a/f2"})
	if s.segs[1].n != 2 || s.segs[1].node != 1 {
		t.Errorf("seg(a) = %+v, want node 1 n 2", s.segs[1])
	}
	checkInvariants(t, s)
}

// TestIncSchedBudgetTracking exercises the lookahead counters: requests
// advance the receiver's segment, release-done markers free segments.
func TestIncSchedBudgetTracking(t *testing.T) {
	kids := make([]file, 8)
	for i := range kids {
		kids[i] = sreg(fmt.Sprintf("d/%c", 'f'+byte(i)))
	}
	s := newFakeSched(t,
		[]file{sdir("."), sdir("d")},
		[]dirLoc{{wpath: "d"}},
		func(wpath string) ([]file, []dirLoc, error) {
			if wpath == "d" {
				return kids, nil, nil
			}
			return nil, nil, nil
		})

	if s.fileTotal != 2 || s.fileOldTotal != 2 {
		t.Fatalf("initial totals = %d/%d, want 2/2", s.fileTotal, s.fileOldTotal)
	}
	if err := s.topUp(); err != nil {
		t.Fatal(err)
	}
	// initial (2 entries) + one segment for d (8 entries)
	if len(s.segs) != 2 {
		t.Fatalf("got %d segments, want 2", len(s.segs))
	}
	checkInvariants(t, s)

	// request lands in seg(d): ndxStart = 1+2+1 = 4; cur moves to seg 1 and
	// file_old_total becomes sum(freed..cur) = 2+8 = 10
	if f, ok := s.advance(5); !ok || f.Wpath != "d/g" {
		t.Fatalf("advance(5) = %v, %v", f, ok)
	}
	if s.curSeg != 1 || s.fileOldTotal != 10 {
		t.Fatalf("after advance: cur=%d old=%d, want cur=1 old=10", s.curSeg, s.fileOldTotal)
	}
	checkInvariants(t, s)

	// the initial list is freed by the generator's first done marker
	freed, more := s.releaseDone()
	if !freed || !more {
		t.Fatalf("releaseDone = %v, %v; want true, true (lists remain)", freed, more)
	}
	if s.window[0].files != nil {
		t.Error("released segment still holds its files")
	}
	if s.fileOldTotal != 8 || s.fileTotal != 8 {
		t.Fatalf("after release: old=%d total=%d, want 8/8", s.fileOldTotal, s.fileTotal)
	}
	checkInvariants(t, s)

	// the last segment is freed by the receiver's first phase-done marker;
	// freeing the last list means no "more" (the marker also counts a phase)
	freed, more = s.releaseDone()
	if !freed || more {
		t.Fatalf("releaseDone = %v, %v; want true, false (last list)", freed, more)
	}
	if s.fileOldTotal != 0 || s.fileTotal != 0 {
		t.Fatalf("after final release: old=%d total=%d, want 0/0", s.fileOldTotal, s.fileTotal)
	}
	if freed, _ := s.releaseDone(); freed {
		t.Fatal("releaseDone = true with nothing to free")
	}
	checkInvariants(t, s)

	// freed segments no longer resolve
	if _, ok := s.advance(5); ok {
		t.Error("advance(5) resolved after its segment was freed")
	}
}

// TestIncSchedEOFTiming checks that EOF is deferred while the lookahead
// backlog covers the receiver (a tree bigger than the window is not fully
// emitted by one topUp), and that draining it produces EOF exactly once.
func TestIncSchedEOFTiming(t *testing.T) {
	const numDirs, filesPerDir = 150, 10
	// chain d0 → d1 → …, each holding filesPerDir files
	scan := func(wpath string) ([]file, []dirLoc, error) {
		var idx int
		if _, err := fmt.Sscanf(wpath, "d%d", &idx); err != nil {
			return nil, nil, fmt.Errorf("unexpected scan of %q", wpath)
		}
		kids := make([]file, filesPerDir)
		for i := range kids {
			kids[i] = sreg(fmt.Sprintf("d%d/f%d", idx, i))
		}
		var dirs []dirLoc
		if idx+1 < numDirs {
			dirs = []dirLoc{{wpath: fmt.Sprintf("d%d", idx+1)}}
		}
		return kids, dirs, nil
	}
	s := newFakeSched(t,
		[]file{sdir("."), sdir("d0")},
		[]dirLoc{{wpath: "d0"}},
		scan)

	if err := s.topUp(); err != nil {
		t.Fatal(err)
	}
	// the backlog covers the window: no EOF yet, not the whole tree sent
	if s.eofSent {
		t.Fatal("topUp sent EOF while the lookahead backlog was still full")
	}
	if s.fileTotal-s.fileOldTotal < minFilecntLookahead {
		t.Fatalf("backlog = %d, want >= %d", s.fileTotal-s.fileOldTotal, minFilecntLookahead)
	}
	checkInvariants(t, s)

	// drain like the generator: advance one entry, free one list, repeat
	nextNdx := int32(1)
	for i := 0; i < 100000; i++ {
		if err := s.topUp(); err != nil {
			t.Fatal(err)
		}
		if s.eofSent {
			break
		}
		s.advance(nextNdx)
		nextNdx++
		freed, _ := s.releaseDone()
		if !freed {
			t.Fatal("generator advanced but nothing was freeable")
		}
		checkInvariants(t, s)
	}
	if !s.eofSent {
		t.Fatal("draining the window never reached NDX_FLIST_EOF")
	}
	checkInvariants(t, s)
}

// TestIncSchedTrueFreeing checks that released segments drop their entries
// for real: window slots nil out and the total retained entry count tracks
// fileTotal down to zero after a full drain.
func TestIncSchedTrueFreeing(t *testing.T) {
	const numDirs, filesPerDir = 150, 10
	scan := func(wpath string) ([]file, []dirLoc, error) {
		var idx int
		if _, err := fmt.Sscanf(wpath, "d%d", &idx); err != nil {
			return nil, nil, fmt.Errorf("unexpected scan of %q", wpath)
		}
		kids := make([]file, filesPerDir)
		for i := range kids {
			kids[i] = sreg(fmt.Sprintf("d%d/f%d", idx, i))
		}
		var dirs []dirLoc
		if idx+1 < numDirs {
			dirs = []dirLoc{{wpath: fmt.Sprintf("d%d", idx+1)}}
		}
		return kids, dirs, nil
	}
	s := newFakeSched(t,
		[]file{sdir("."), sdir("d0")},
		[]dirLoc{{wpath: "d0"}},
		scan)

	retained := func() int {
		n := 0
		for _, w := range s.window {
			n += len(w.files)
		}
		return n
	}

	if err := s.topUp(); err != nil {
		t.Fatal(err)
	}
	if got := retained(); got != s.fileTotal {
		t.Fatalf("retained = %d, fileTotal = %d", got, s.fileTotal)
	}

	// fully drain: the entries are released and the retained count reaches 0
	nextNdx := int32(1)
	for i := 0; i < 100000 && retained() > 0; i++ {
		if err := s.topUp(); err != nil {
			t.Fatal(err)
		}
		s.advance(nextNdx)
		nextNdx++
		s.releaseDone()
		checkInvariants(t, s)
		if got := retained(); got != s.fileTotal {
			t.Fatalf("retained = %d, fileTotal = %d", got, s.fileTotal)
		}
	}
	if got := retained(); got != 0 {
		t.Fatalf("retained = %d after full drain, want 0", got)
	}
	if s.fileTotal != 0 || s.fileOldTotal != 0 {
		t.Fatalf("totals = %d/%d after full drain, want 0/0", s.fileTotal, s.fileOldTotal)
	}
}

// TestIncSchedAdversarialOrdering runs release-before-advance, multi-segment
// jumps, and interleavings that broke earlier lookahead accounting
// (f79095d): the counters must stay exact and the tree must still reach EOF.
func TestIncSchedAdversarialOrdering(t *testing.T) {
	// release every freeable list before advancing at all: curSeg ends up
	// below windowBase, which must not produce negative or stale counters
	t.Run("ReleaseBeforeAdvance", func(t *testing.T) {
		s := newFakeSched(t,
			[]file{sdir("."), sdir("d"), sreg("d/f")},
			[]dirLoc{{wpath: "d"}},
			func(wpath string) ([]file, []dirLoc, error) {
				if wpath == "d" {
					return []file{sreg("d/f")}, nil, nil
				}
				return nil, nil, nil
			})
		if err := s.topUp(); err != nil {
			t.Fatal(err)
		}
		// the whole tree fits the window: everything (incl. EOF) is on the
		// wire after this first topUp
		if !s.eofSent {
			t.Fatal("EOF not sent for a tree smaller than the lookahead window")
		}
		segs := len(s.segs)
		for {
			freed, more := s.releaseDone()
			checkInvariants(t, s)
			if !freed || !more {
				break
			}
		}
		if _, ok := s.advance(2); ok {
			t.Error("advance into the freed initial list resolved")
		}
		if _, ok := s.advance(5); ok {
			t.Error("advance into the freed segment resolved")
		}
		// the releases must not have emitted anything
		if len(s.segs) != segs {
			t.Errorf("releases grew segments to %d, want %d", len(s.segs), segs)
		}
		if err := s.topUp(); err != nil {
			t.Fatal(err)
		}
		if !s.eofSent || len(s.segs) != segs {
			t.Errorf("post-release topUp changed state: eofSent=%v segs=%d",
				s.eofSent, len(s.segs))
		}
	})

	// one advance skips several segments at once (a generator jumping to a
	// deep ndx), then releases catch up in bulk
	t.Run("MultiSegmentJump", func(t *testing.T) {
		s := newFakeSched(t,
			[]file{sdir("."), sdir("d0")},
			[]dirLoc{{wpath: "d0"}},
			func(wpath string) ([]file, []dirLoc, error) {
				var idx int
				if _, err := fmt.Sscanf(wpath, "d%d", &idx); err != nil {
					return nil, nil, fmt.Errorf("unexpected scan of %q", wpath)
				}
				kids := []file{sreg(fmt.Sprintf("d%d/f", idx))}
				var dirs []dirLoc
				if idx < 30 {
					dirs = []dirLoc{{wpath: fmt.Sprintf("d%d", idx+1)}}
				}
				return kids, dirs, nil
			})
		if err := s.topUp(); err != nil {
			t.Fatal(err)
		}
		// jump to the last emitted segment's ndx_start
		last := len(s.segs) - 1
		if _, ok := s.advance(s.segs[last].ndxStart); !ok {
			t.Fatalf("advance(%d) did not resolve", s.segs[last].ndxStart)
		}
		checkInvariants(t, s)
		for {
			freed, more := s.releaseDone()
			checkInvariants(t, s)
			if !freed || !more {
				break
			}
		}
		if s.fileTotal != 0 || s.fileOldTotal != 0 {
			t.Fatalf("totals = %d/%d, want 0/0", s.fileTotal, s.fileOldTotal)
		}
	})
}

// TestIncSchedIOErrorPerSegment checks the end-of-flist i/o error word rides
// the segment whose scan failed (C's send1extra), while the initial list of
// an error-free scanRoot carries none.
func TestIncSchedIOErrorPerSegment(t *testing.T) {
	st, buf := newGoldenTransfer(t, fstest.MapFS{})
	// the walker's ioError callback flips the same flag the scheduler sends
	// in the end-of-flist word, like SendFileList wires them up
	ioErrs := int32(0)
	sw := &scopedWalker{
		st:        st,
		excl:      &filterRuleList{},
		uidMap:    map[int32]string{},
		gidMap:    map[int32]string{},
		fileList:  &fileList{},
		source:    st.Source,
		ioError:   func(err error) { ioErrs = 1 },
		requested: "/",
		strip:     getStrip("/"),
		subdir:    ".",
	}
	roots := buildIncNodes(st,
		[]file{sdir("."), sdir("a")},
		[]dirLoc{{sw: sw, wpath: "a"}})
	s := newIncSched(st, flist.NewEncoder(st.flistParams()), []file{sdir("."), sdir("a")},
		&ioErrs, make(map[int32]string), make(map[int32]string), roots)
	if err := s.emitInitial(); err != nil {
		t.Fatal(err)
	}
	initialBytes := append([]byte(nil), buf.Bytes()...)

	// a scan that flags an i/o error through the walker, like scanDir does
	// on a ReadDir/stat failure
	s.scanDir = func(loc dirLoc) ([]file, []dirLoc, error) {
		loc.sw.ioError(errors.New("boom"))
		return []file{sreg("a/f")}, nil, nil
	}
	// emit the failing segment directly (topUp would append the EOF marker
	// after it, hiding the entries frame from lastFrame)
	if err := s.emitSegment(); err != nil {
		t.Fatal(err)
	}
	segBytes := append([]byte(nil), buf.Bytes()[len(initialBytes):]...)

	// lastFrame extracts the payload of the final multiplex DATA frame: the
	// entries plus the end-of-flist terminator of one segment
	lastFrame := func(b []byte) []byte {
		var last []byte
		for off := 0; off+4 <= len(b); {
			length := int(b[off]) | int(b[off+1])<<8 | int(b[off+2])<<16
			off += 4
			if off+length > len(b) {
				break
			}
			last = b[off : off+length]
			off += length
		}
		return last
	}

	dec := flist.NewDecoder(st.flistParams())
	r := bytes.NewReader(lastFrame(initialBytes))
	for {
		_, err := dec.Decode(r)
		if err != nil {
			if !errors.Is(err, io.EOF) {
				t.Fatalf("decoding initial list: %v", err)
			}
			break
		}
	}
	if got := dec.IOError(); got != 0 {
		t.Errorf("initial list carried i/o error word %d, want 0", got)
	}

	dec2 := flist.NewDecoder(st.flistParams())
	r2 := bytes.NewReader(lastFrame(segBytes))
	for {
		_, err := dec2.Decode(r2)
		if err != nil {
			if !errors.Is(err, io.EOF) {
				t.Fatalf("decoding segment: %v", err)
			}
			break
		}
	}
	if got := dec2.IOError(); got == 0 {
		t.Error("failing segment did not carry the i/o error word")
	}
}
