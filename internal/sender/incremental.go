package sender

import (
	"bytes"
	"sort"

	"github.com/gokrazy/rsync"
	"github.com/gokrazy/rsync/internal/flist"
	"github.com/gokrazy/rsync/internal/protocol"
)

// minFilecntLookahead mirrors rsync.h:MIN_FILECNT_LOOKAHEAD: the sender keeps
// emitting directory segments until this many entries sit in unconsumed
// file-lists ahead of the receiver's current position.
const minFilecntLookahead = 1000

// dirNode is the sender's dir_flist equivalent (rsync/flist.c:add_dirs_to_tree):
// one directory in the DFS tree, with C's DIR_PARENT/DIR_FIRST_CHILD/
// DIR_NEXT_SIBLING links as slice indices. The node's slice index is its dir
// index on the wire. Nodes are appended as segments are emitted, so the index
// space follows the receiver's arrival order.
type dirNode struct {
	name  string   // wire path of the directory
	locs  []dirLoc // per-arg scan locations (merged duplicate names)
	noSeg bool     // the "." entry: its children pre-sent in the initial list

	parent, firstChild, nextSibling int32 // -1 = none
}

// segHdr is the permanently retained record of one emitted file-list segment
// (rsync/flist.c:flist_new's ndx chain): node is the dir index the segment
// was scanned for (-1 for the initial list), n the entry count. The chain
// carries the +1 gap between segments (flist.c:3260-3268). Retaining only
// these headers — not the entries — is what bounds sender memory by the dir
// count (docs/sendextra.md).
type segHdr struct {
	node     int32
	ndxStart int32
	n        int32
}

// incSegmentLive holds the entries of an emitted segment until the receiver
// confirms the segment is done; releaseDone drops them for real
// (rsync/flist.c:flist_free via pool_free_old).
type incSegmentLive struct {
	files []file
}

// incSched owns the sender side of incremental recursion, mirroring C's
// send_extra_file_list (rsync/flist.c:2396-2497): it scans exactly one
// directory per segment on demand, following the dirNode tree in DFS order
// while the receiver's lookahead backlog is below minFilecntLookahead, and
// frees consumed segments for real.
type incSched struct {
	st       *Transfer
	enc      *flist.Encoder
	ioErrors *int32

	// scanDir resolves a dirLoc to its children; injectable for tests.
	scanDir func(dirLoc) ([]file, []dirLoc, error)

	nodes  []dirNode
	segs   []segHdr         // emission order; the ndx chain lives here
	window []incSegmentLive // parallel to segs, until entries are freed

	nextNdxStart int32
	curSeg       int   // segment holding the receiver's current position
	windowBase   int   // oldest segment not yet freed
	sendDirNdx   int32 // DFS cursor over nodes; -1 = tree exhausted

	fileTotal    int
	fileOldTotal int
	eofSent      bool

	buf bytes.Buffer

	p      flist.Params
	uidMap map[int32]string
	gidMap map[int32]string
}

// buildIncNodes creates the dirNode roots from the sorted initial list: dirs
// appear as nodes in wire order (their slice index = dir index), with the "."
// entry marked noSeg and parenting every other top-level dir, like C's
// add_dirs_to_tree(-1, first_flist) (rsync/flist.c:1962-2001). The locs (one
// per discovering arg, keyed by wire path) provide the scan locations;
// duplicate dir names across args merge into one node.
func buildIncNodes(st *Transfer, initial []file, locs []dirLoc) []dirNode {
	locByWpath := make(map[string][]dirLoc)
	for _, loc := range locs {
		locByWpath[loc.wpath] = append(locByWpath[loc.wpath], loc)
	}

	var nodes []dirNode
	nodeByName := make(map[string]int32)
	dot := int32(-1)
	var prevTop int32 = -1
	recurse := st.Opts.Recurse()
	for i := range initial {
		e := &initial[i]
		if !e.isDir() || !recurse {
			continue
		}
		if e.Wpath == "." {
			// the dot-dir rides the initial list and never gets its own
			// segment (C add_dirs_to_tree skips basename ".")
			if dot < 0 {
				dot = int32(len(nodes))
				nodes = append(nodes, dirNode{name: e.Wpath, noSeg: true, parent: -1, firstChild: -1, nextSibling: -1})
			}
			continue
		}
		if _, ok := nodeByName[e.Wpath]; ok {
			// duplicate dir name across args: one node, merged locs
			continue
		}
		idx := int32(len(nodes))
		nodeByName[e.Wpath] = idx
		if prevTop >= 0 {
			nodes[prevTop].nextSibling = idx
		} else if dot >= 0 {
			nodes[dot].firstChild = idx
		}
		nodes = append(nodes, dirNode{name: e.Wpath, locs: locByWpath[e.Wpath], parent: dot, firstChild: -1, nextSibling: -1})
		prevTop = idx
	}
	return nodes
}

// newIncSched seeds the scheduler with the initial list; emitInitial puts it
// on the wire. initial must be in global FNameCmp order, which is DFS
// pre-order.
func newIncSched(st *Transfer, enc *flist.Encoder, initial []file, ioErrors *int32, uidMap, gidMap map[int32]string, roots []dirNode) *incSched {
	s := &incSched{
		st:       st,
		enc:      enc,
		ioErrors: ioErrors,
		scanDir: func(loc dirLoc) ([]file, []dirLoc, error) {
			return loc.sw.scanDir(loc.dpath)
		},
		nodes:  roots,
		p:      st.flistParams(),
		uidMap: uidMap,
		gidMap: gidMap,
	}
	switch {
	case len(s.nodes) > 0 && s.nodes[0].noSeg && s.nodes[0].firstChild >= 0:
		s.sendDirNdx = s.nodes[0].firstChild
	case len(s.nodes) > 0 && !s.nodes[0].noSeg:
		s.sendDirNdx = 0
	default:
		s.sendDirNdx = -1
	}
	s.segs = append(s.segs, segHdr{node: -1, ndxStart: 1, n: int32(len(initial))})
	s.window = append(s.window, incSegmentLive{files: initial})
	s.nextNdxStart = 1 + int32(len(initial)) + 1
	s.recomputeTotals()
	return s
}

// toFileEntry builds the wire entry for one file; called at emit time so the
// FileEntry copy is transient and only the file struct is retained until the
// segment is freed.
func (s *incSched) toFileEntry(f *file) *flist.FileEntry {
	fe := &flist.FileEntry{
		Name:       f.Wpath,
		Length:     f.Length,
		ModTime:    int32(f.ModTime.Unix()),
		Mode:       f.Mode,
		Uid:        f.Uid,
		Gid:        f.Gid,
		RdevMajor:  f.RdevMajor,
		RdevMinor:  f.RdevMinor,
		LinkTarget: f.LinkTarget,
		TopDir:     f.Flags&rsync.XMIT_TOP_DIR != 0,
	}
	if s.p.IncRecurse && !s.p.NumericIDs {
		// protocol >= 30 inc-recurse: uid/gid names ride inline in the
		// entries (XMIT_USER_NAME_FOLLOWS); there are no trailing lists.
		fe.User = s.uidMap[f.Uid]
		fe.Group = s.gidMap[f.Gid]
	}
	copy(fe.Checksum[:], f.Checksum[:])
	return fe
}

// emitInitial writes the initial file list: the entries and the end-of-list
// terminator, without a segment header (rsync/flist.c:send_file_list).
func (s *incSched) emitInitial() error {
	s.buf.Reset()
	for i := range s.window[0].files {
		if err := s.enc.Encode(&s.buf, s.toFileEntry(&s.window[0].files[i])); err != nil {
			return err
		}
	}
	if err := s.enc.WriteEndOfFlist(&s.buf, *s.ioErrors != 0, *s.ioErrors); err != nil {
		return err
	}
	return s.st.Conn.WriteString(s.buf.String())
}

// emitSegment scans the directory at the DFS cursor and writes its segment:
// the flist header, the entries, and the end-of-list terminator
// (rsync/flist.c:2412-2454). It appends discovered dirs to the node tree and
// advances the DFS cursor (flist.c:2473-2491).
func (s *incSched) emitSegment() error {
	ni := s.sendDirNdx
	node := &s.nodes[ni]

	var files []file
	var childDirs []dirLoc
	segIOError := false
	if !node.noSeg {
		for _, loc := range node.locs {
			// catch this segment's scan i/o errors: the end-of-flist word
			// rides the segment whose scan failed, like C's send1extra
			// (the receiver ORs the per-segment words)
			segErr := false
			if sw := loc.sw; sw != nil {
				saved := sw.ioError
				sw.ioError = func(err error) {
					segErr = true
					saved(err)
				}
				defer func() { sw.ioError = saved }()
			}
			f, d, err := s.scanDir(loc)
			if err != nil {
				return err
			}
			if segErr {
				segIOError = true
			}
			files = append(files, f...)
			childDirs = append(childDirs, d...)
		}
		if len(node.locs) > 1 {
			// merged duplicate args: restore wire order across the merged
			// scans (stable, like C's fsort in inc_recurse mode)
			sort.SliceStable(files, func(i, j int) bool {
				a, b := &files[i], &files[j]
				return protocol.FNameCmp(a.Wpath, a.isDir(), b.Wpath, b.isDir()) < 0
			})
		}
	}

	// add_dirs_to_tree: append discovered dirs in wire order; the first
	// child links the parent, subsequent ones chain as siblings
	// (rsync/flist.c:1993-2000)
	prevSibling := int32(-1)
	for _, d := range childDirs {
		n := int32(len(s.nodes))
		s.nodes = append(s.nodes, dirNode{name: d.wpath, locs: []dirLoc{d}, parent: ni, firstChild: -1, nextSibling: -1})
		if prevSibling < 0 {
			s.nodes[ni].firstChild = n
		} else {
			s.nodes[prevSibling].nextSibling = n
		}
		prevSibling = n
	}

	seg := segHdr{node: ni, ndxStart: s.nextNdxStart, n: int32(len(files))}
	s.nextNdxStart = seg.ndxStart + seg.n + 1

	s.buf.Reset()
	if seg.node >= 0 {
		if err := s.st.ndxWrite().WriteNdx(s.st.Conn, protocol.NdxFlistOff-seg.node); err != nil {
			return err
		}
	}
	for i := range files {
		if err := s.enc.Encode(&s.buf, s.toFileEntry(&files[i])); err != nil {
			return err
		}
	}
	// The i/o error word rides the segment whose scan failed (C's
	// send1extra); the initial list reports scanRoot failures. This is an
	// intentional divergence from the pre-refactor Go sender on error trees
	// only — it put every word on the initial list. The receiver ORs the
	// per-segment words either way.
	if err := s.enc.WriteEndOfFlist(&s.buf, segIOError, *s.ioErrors); err != nil {
		return err
	}
	if err := s.st.Conn.WriteString(s.buf.String()); err != nil {
		return err
	}

	s.segs = append(s.segs, seg)
	s.window = append(s.window, incSegmentLive{files: files})
	s.recomputeTotals()

	// advance the DFS cursor (flist.c:2473-2491)
	if s.nodes[ni].firstChild >= 0 {
		s.sendDirNdx = s.nodes[ni].firstChild
		return nil
	}
	for {
		if s.nodes[ni].nextSibling >= 0 {
			s.sendDirNdx = s.nodes[ni].nextSibling
			return nil
		}
		ni = s.nodes[ni].parent
		if ni < 0 {
			s.sendDirNdx = -1
			return nil
		}
	}
}

// recomputeTotals derives the lookahead counters from the retained segment
// headers, exact by construction (the f79095d deadlock class came from
// incremental drift):
//   - fileOldTotal counts the entries from the oldest unfreed segment
//     through the receiver's current segment (rsync.c:read_ndx_and_attrs
//     recomputes file_old_total when cur_flist changes);
//   - fileTotal counts every unfreed, emitted entry (flist.c:3301 removes
//     freed entries from file_total).
func (s *incSched) recomputeTotals() {
	old := 0
	for j := s.windowBase; j <= s.curSeg && j < len(s.segs); j++ {
		old += int(s.segs[j].n)
	}
	s.fileOldTotal = old
	total := 0
	for j := s.windowBase; j < len(s.segs); j++ {
		total += int(s.segs[j].n)
	}
	s.fileTotal = total
}

// topUp emits segments while the unconsumed backlog is below the lookahead
// window (rsync/sender.c:send_extra_file_list), and sends NDX_FLIST_EOF once
// the last segment has gone out.
func (s *incSched) topUp() error {
	if s.eofSent {
		return nil
	}
	for s.fileTotal-s.fileOldTotal < minFilecntLookahead {
		if s.sendDirNdx < 0 {
			if err := s.st.ndxWrite().WriteNdx(s.st.Conn, protocol.NdxFlistEOF); err != nil {
				return err
			}
			s.eofSent = true
			return nil
		}
		if err := s.emitSegment(); err != nil {
			return err
		}
	}
	return nil
}

// advance tracks the receiver's position: a request for an ndx in a later
// segment moves the current segment forward and frees lookahead budget
// (rsync.c:read_ndx_and_attrs recomputes file_old_total when cur_flist
// changes). It returns the requested entry, or false for ndx values that do
// not resolve: the +1 gaps, already-freed segments, and the ndx_start-1
// parent itemizes.
func (s *incSched) advance(ndx int32) (*file, bool) {
	// segments have strictly increasing ndxStart (even empty ones advance by
	// the +1 gap), so binary search for the last seg with ndxStart <= ndx.
	k := sort.Search(len(s.segs), func(i int) bool {
		return s.segs[i].ndxStart > ndx
	}) - 1
	if k < s.windowBase {
		return nil, false
	}
	seg := &s.segs[k]
	off := ndx - seg.ndxStart
	if off < 0 || int(off) >= int(seg.n) {
		return nil, false
	}
	if k != s.curSeg {
		s.curSeg = k
		s.recomputeTotals()
	}
	return &s.window[k].files[off], true
}

// releaseDone consumes one receiver file-list-done marker (the generator
// sends one NDX_DONE per completed flist): it frees the oldest unconsumed
// segment, dropping its entries — the Go analogue of C's flist_free +
// pool_free_old (rsync/sender.c:530-538, flist.c:3301-3308). freed reports
// whether a list was freed; more reports whether lists remain — while they
// do, the sender echoes the DONE without counting a phase, but when the last
// list is freed the same marker also counts a phase (the fall-through at
// sender.c:540), so the caller must not double-echo.
func (s *incSched) releaseDone() (freed, more bool) {
	if s.windowBase < len(s.segs) {
		// true freeing: the retained entry structs go back to the garbage
		// collector; dir nodes and segment headers survive, like C's
		// dir_flist.
		s.window[s.windowBase].files = nil
		s.windowBase++
		s.recomputeTotals()
		return true, s.windowBase < len(s.segs)
	}
	return false, false
}
