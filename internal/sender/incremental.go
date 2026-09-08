package sender

import (
	"bytes"
	"sort"
	"strings"

	"github.com/gokrazy/rsync/internal/flist"
	"github.com/gokrazy/rsync/internal/protocol"
)

// minFilecntLookahead mirrors rsync.h:MIN_FILECNT_LOOKAHEAD: the sender keeps
// emitting directory segments until this many entries sit in unconsumed
// file-lists ahead of the receiver's current position.
const minFilecntLookahead = 1000

// incSegment is one file-list segment under incremental recursion.
// Segment 0 is the initial list (no wire header); every other segment is
// preceded by a NDX_FLIST_OFFSET-dirIdx header naming the dir whose children
// it carries.
type incSegment struct {
	dirIdx   int32 // index into the receiver's dir list; -1 for the initial list
	ndxStart int32
	idxs     []int // indices into the flat sorted entry list
}

// incSched owns the sender side of incremental recursion: it partitions the
// globally sorted flat entry list into the initial list plus one segment per
// directory (mirroring C's dir_flist tree walk), lazily emits segments while
// the receiver's lookahead backlog is below minFilecntLookahead, and maps
// request ndx values back to flat entries.
type incSched struct {
	st       *Transfer
	enc      *flist.Encoder
	entries  []*flist.FileEntry // parallel to fileList.Files (global FNameCmp order)
	segs     []incSegment
	ioErrors int32

	sentUpTo  int
	curSeg    int
	freedUpTo int

	fileTotal    int
	fileOldTotal int
	eofSent      bool

	buf bytes.Buffer
}

// newIncSched partitions the sorted flat entries into segments and assigns
// ndx ranges. entries must be in global FNameCmp order, which is DFS
// pre-order: the parent dir sorts immediately before its descendants, and a
// dir's children form a sorted run.
//
// Dir indices follow the receiver's dir_flist numbering: dirs are numbered
// in the order their entries arrive on the wire — the initial list's dirs
// first (sorted), then each emitted segment's dirs (sorted) as segments are
// emitted in DFS order.
func newIncSched(st *Transfer, enc *flist.Encoder, entries []*flist.FileEntry, ioErrors int32) *incSched {
	s := &incSched{
		st:       st,
		enc:      enc,
		entries:  entries,
		ioErrors: ioErrors,
	}

	// bucket children by parent dir name; the initial list holds every
	// entry without a slash (the root dir entry itself plus top-level files
	// and dirs).
	children := map[string][]int{}
	var initial []int
	for i, e := range entries {
		if strings.IndexByte(e.Name, '/') < 0 {
			initial = append(initial, i)
			continue
		}
		p := flist.ParentPath(e.Name)
		children[p] = append(children[p], i)
	}

	// dir index space: initial dirs in initial (sorted) order, then dirs as
	// their segments are emitted (DFS), each run sorted.
	dirIndex := map[string]int32{}
	var topDirs []string
	for _, i := range initial {
		if entries[i].IsDir() {
			topDirs = append(topDirs, entries[i].Name)
		}
	}
	for i, n := range topDirs {
		dirIndex[n] = int32(i)
	}

	var emitOrder []string
	var walk func(dir string)
	walk = func(dir string) {
		emitOrder = append(emitOrder, dir)
		kids := children[dir]
		var childDirs []string
		for _, k := range kids {
			if entries[k].IsDir() {
				childDirs = append(childDirs, entries[k].Name)
			}
		}
		for _, cd := range childDirs {
			dirIndex[cd] = int32(len(dirIndex))
		}
		for _, cd := range childDirs {
			walk(cd)
		}
	}
	for _, n := range topDirs {
		if n == "." {
			// the dot-dir rides the initial list and never gets its own
			// segment (C add_dirs_to_tree skips basename ".")
			continue
		}
		walk(n)
	}

	s.segs = []incSegment{{dirIdx: -1, idxs: initial}}
	for _, dir := range emitOrder {
		s.segs = append(s.segs, incSegment{dirIdx: dirIndex[dir], idxs: children[dir]})
	}

	// ndx_start chain: the initial list starts at 1 under inc-recurse, and
	// each next list starts one past the previous list's last possible ndx
	// (the +1 gap is load-bearing, flist.c:3260-3268).
	s.segs[0].ndxStart = 1
	for i := 1; i < len(s.segs); i++ {
		prev := &s.segs[i-1]
		s.segs[i].ndxStart = prev.ndxStart + int32(len(prev.idxs)) + 1
	}
	s.fileTotal = len(initial)
	s.fileOldTotal = len(initial)
	return s
}

// emit writes segment i to the connection: the flist header (except for the
// initial list), the entries, and the end-of-list terminator.
func (s *incSched) emit(i int) error {
	seg := &s.segs[i]
	s.buf.Reset()
	if seg.dirIdx >= 0 {
		if err := s.st.ndxWrite().WriteNdx(s.st.Conn, protocol.NdxFlistOff-seg.dirIdx); err != nil {
			return err
		}
	}
	for _, idx := range seg.idxs {
		if err := s.enc.Encode(&s.buf, s.entries[idx]); err != nil {
			return err
		}
	}
	// Only the initial list reports walk-time i/o errors: the Go sender
	// discovers them all up front during the full walk, while C spreads them
	// across lazily scanned segments. Either way the receiver ORs the words.
	sendIOError := seg.dirIdx == -1 && s.ioErrors != 0
	if err := s.enc.WriteEndOfFlist(&s.buf, sendIOError, s.ioErrors); err != nil {
		return err
	}
	return s.st.Conn.WriteString(s.buf.String())
}

// topUp emits segments while the unconsumed backlog is below the lookahead
// window (rsync/sender.c:send_extra_file_list), and sends NDX_FLIST_EOF once
// the last segment has gone out.
func (s *incSched) topUp() error {
	if s.eofSent {
		return nil
	}
	for s.fileTotal-s.fileOldTotal < minFilecntLookahead {
		if s.sentUpTo >= len(s.segs) {
			if err := s.st.ndxWrite().WriteNdx(s.st.Conn, protocol.NdxFlistEOF); err != nil {
				return err
			}
			s.eofSent = true
			return nil
		}
		if err := s.emit(s.sentUpTo); err != nil {
			return err
		}
		s.fileTotal += len(s.segs[s.sentUpTo].idxs)
		s.sentUpTo++
	}
	return nil
}

// advance tracks the receiver's position: a request for an ndx in a later
// segment moves the current segment forward and frees lookahead budget
// (rsync.c:read_ndx_and_attrs recomputes file_old_total when cur_flist
// changes). It returns the flat index of the requested entry.
func (s *incSched) advance(ndx int32) (int, bool) {
	// segments have strictly increasing ndxStart (even empty ones advance by
	// the +1 gap), so binary search for the last seg with ndxStart <= ndx.
	k := sort.Search(len(s.segs), func(i int) bool {
		return s.segs[i].ndxStart > ndx
	}) - 1
	if k < 0 {
		return 0, false
	}
	seg := &s.segs[k]
	off := ndx - seg.ndxStart
	if off < 0 || int(off) >= len(seg.idxs) {
		return 0, false
	}
	if k != s.curSeg {
		s.curSeg = k
		total := 0
		for j := s.freedUpTo; j <= k; j++ {
			total += len(s.segs[j].idxs)
		}
		s.fileOldTotal = total
	}
	return seg.idxs[off], true
}

// releaseDone consumes one receiver file-list-done marker (the generator
// sends one NDX_DONE per completed flist): it frees the oldest unconsumed
// segment (rsync/sender.c:530-538). freed reports whether a list was freed;
// more reports whether lists remain — while they do, the sender echoes the
// DONE without counting a phase, but when the last list is freed the same
// marker also counts a phase (the fall-through at sender.c:540), so the
// caller must not double-echo.
func (s *incSched) releaseDone() (freed, more bool) {
	if s.freedUpTo < s.sentUpTo {
		s.fileOldTotal -= len(s.segs[s.freedUpTo].idxs)
		s.freedUpTo++
		return true, s.freedUpTo < s.sentUpTo
	}
	return false, false
}
