package receiver

import (
	"fmt"
	"io"
	"sort"
	"sync"

	"github.com/gokrazy/rsync"
	"github.com/gokrazy/rsync/internal/flist"
	"github.com/gokrazy/rsync/internal/protocol"
	"github.com/gokrazy/rsync/internal/rsyncopts"
)

// incRecv holds the receiver side of incremental recursion (protocol >= 30,
// INC_RECURSE): the persistent flist decoder, the received-entry queue the
// generator drains, and the wire-ndx → file mapping the frame loop needs to
// route incoming file data.
//
// File-list segments arrive strictly in order on the single sender stream
// (the sender emits them in DFS pre-order, and a directory's own entry rides
// its parent's segment), so a directory always precedes its children on the
// wire. The receiver validates that invariant per segment header (dir index
// bounds + parent-name match) and then needs only a FIFO queue — no
// pending-parent machinery.
type incRecv struct {
	dec *flist.Decoder

	mu    sync.Mutex
	cond  *sync.Cond
	queue []*File // ready entries in wire (arrival) order
	qhead int

	// byNdx, allFiles and dirs are written by the frame-loop goroutine only
	// (before Do's errgroup joins) and read by the frame loop, the deletion
	// goroutine (after listsDone) and the final touch-up (after both other
	// goroutines have finished).
	byNdx    map[int32]*File // wire ndx → file, for routing incoming data
	allFiles []*File         // every received entry, arrival order
	dirs     []*File         // directories in arrival order = the sender's dir index space

	// segCounts[i] is the entry count of received segment i (0 = initial
	// list); nextNdx is the wire ndx of the next arriving entry, following
	// the ndx_start chain with C's +1 gap between segments (flist.c:3260).
	segCounts []int
	nextNdx   int32

	// unfreed counts received-but-not-yet-released segments; it is used and
	// mutated only by the frame-loop goroutine (receiver.c:842-862).
	unfreed int

	listsDone bool

	// genCount is the number of entries handed to (and fully processed by)
	// the generator; lastReleased is the highest segment index for which a
	// release-DONE has been emitted (-1 = none). Both are generator-only.
	genCount     int
	lastReleased int

	err error
}

func newIncRecv(dec *flist.Decoder) *incRecv {
	inc := &incRecv{
		dec:          dec,
		byNdx:        map[int32]*File{},
		nextNdx:      1, // the initial list starts at ndx 1 under inc-recurse
		lastReleased: -1,
	}
	inc.cond = sync.NewCond(&inc.mu)
	return inc
}

func (inc *incRecv) fail(err error) {
	inc.mu.Lock()
	defer inc.mu.Unlock()
	if inc.err == nil {
		inc.err = err
	}
	inc.cond.Broadcast()
}

// pushEntries stores one decoded segment. dirIdx is the sender's directory
// index from the segment header (-1 for the initial list); every entry must
// name dirIdx's directory as its parent.
func (inc *incRecv) pushEntries(rt *Transfer, dirIdx int32, fes []*flist.FileEntry) error {
	var dirName string
	if dirIdx >= 0 {
		if int(dirIdx) >= len(inc.dirs) {
			return fmt.Errorf("invalid file-list dir index %d (have %d dirs)", dirIdx, len(inc.dirs))
		}
		dirName = inc.dirs[dirIdx].Name
	}

	inc.mu.Lock()
	defer inc.mu.Unlock()
	for _, fe := range fes {
		if dirIdx >= 0 && flist.ParentPath(fe.Name) != dirName {
			return fmt.Errorf("file-list entry %q does not belong to dir %q", fe.Name, dirName)
		}
		f := rt.toFile(fe)
		f.Ndx = inc.nextNdx
		inc.nextNdx++
		if f.isDir() {
			inc.dirs = append(inc.dirs, f)
		}
		inc.byNdx[f.Ndx] = f
		inc.allFiles = append(inc.allFiles, f)
		inc.queue = append(inc.queue, f)
	}
	inc.segCounts = append(inc.segCounts, len(fes))
	inc.nextNdx++ // the +1 gap between consecutive segments' ndx ranges
	inc.unfreed++
	inc.cond.Broadcast()
	return nil
}

// decodeSegment reads one file-list segment from the sender stream and stores
// it. It runs on the frame-loop goroutine between file-data reads.
func (inc *incRecv) decodeSegment(rt *Transfer, dirIdx int32) error {
	var fes []*flist.FileEntry
	for {
		fe, err := inc.dec.Decode(rt.Conn)
		if err == io.EOF {
			break
		}
		if err != nil {
			return err
		}
		fes = append(fes, fe)
	}
	return inc.pushEntries(rt, dirIdx, fes)
}

// next blocks until one of: a release-DONE is due (rel=true), an entry is
// available (f), all lists are complete and drained (done), or an error
// occurred. It runs on the generator goroutine.
func (inc *incRecv) next() (f *File, rel, done bool, err error) {
	inc.mu.Lock()
	defer inc.mu.Unlock()
	for {
		if inc.err != nil {
			return nil, false, false, inc.err
		}
		// A completed segment whose successor has already arrived gets one
		// release-DONE (generator.c:2691-2698); the last segment is freed
		// by the phase-1 done marker instead. Checked before the queue so
		// the sender's lookahead budget frees up even when nothing is
		// consumable yet (e.g. an empty segment arriving alone).
		if inc.dueReleaseLocked() {
			return nil, true, false, nil
		}
		if inc.qhead < len(inc.queue) {
			f = inc.queue[inc.qhead]
			inc.queue[inc.qhead] = nil
			inc.qhead++
			if inc.qhead == len(inc.queue) {
				inc.queue = inc.queue[:0]
				inc.qhead = 0
			}
			inc.genCount++
			return f, false, false, nil
		}
		if inc.listsDone {
			return nil, false, true, nil
		}
		inc.cond.Wait()
	}
}

// dueReleaseLocked reports whether the next in-order segment release is due:
// all entries of segments 0..j are processed and segment j+1 has arrived.
func (inc *incRecv) dueReleaseLocked() bool {
	j := inc.lastReleased + 1
	if j+1 >= len(inc.segCounts) {
		return false
	}
	cum := 0
	for _, c := range inc.segCounts[:j+1] {
		cum += c
	}
	if inc.genCount >= cum {
		inc.lastReleased = j
		return true
	}
	return false
}

// markListsDone records NDX_FLIST_EOF: every segment has been received.
func (inc *incRecv) markListsDone() {
	inc.mu.Lock()
	defer inc.mu.Unlock()
	inc.listsDone = true
	inc.cond.Broadcast()
}

// waitListsDone blocks until all segments have been received (or the transfer
// failed). Used by the deletion goroutine, which needs the complete list.
func (inc *incRecv) waitListsDone() error {
	inc.mu.Lock()
	defer inc.mu.Unlock()
	for !inc.listsDone && inc.err == nil {
		inc.cond.Wait()
	}
	return inc.err
}

// freeOne frees the oldest received-but-unreleased segment (the receiver's
// mirror of the sender's list freeing, receiver.c:842-862). It reports
// whether a list was freed; when the last list is freed the caller must fall
// through to phase counting, since the same DONE marker carries both roles.
// Frame-loop goroutine only.
func (inc *incRecv) freeOne() bool {
	if inc.unfreed > 0 {
		inc.unfreed--
		return true
	}
	return false
}

// unfreedLists reports how many received segments are still unreleased.
// Frame-loop goroutine only.
func (inc *incRecv) unfreedLists() int { return inc.unfreed }

// lookup returns the file for a wire ndx, or nil. Frame-loop goroutine only.
func (inc *incRecv) lookup(ndx int32) *File { return inc.byNdx[ndx] }

// snapshot returns a copy of every received entry in arrival order.
func (inc *incRecv) snapshot() []*File {
	inc.mu.Lock()
	defer inc.mu.Unlock()
	out := make([]*File, len(inc.allFiles))
	copy(out, inc.allFiles)
	return out
}

// deleteList returns every received entry sorted by plain name, the order
// findInFileList's binary search expects (rsync/receiver.c:delete_files).
func (inc *incRecv) deleteList() []*File {
	out := inc.snapshot()
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// rsync/flist.c:recv_file_list (initial list of an incremental transfer):
// only the initial segment is read here; the per-directory segments are
// decoded by the frame loop once the transfer goroutines are running.
func (rt *Transfer) receiveFileListInc(p flist.Params) ([]*File, error) {
	if rt.Opts.Progress {
		fmt.Fprintln(rt.Env.Stdout, "receiving file list...")
	}
	inc := newIncRecv(flist.NewDecoder(p))
	rt.inc = inc

	var fes []*flist.FileEntry
	for {
		fe, err := inc.dec.Decode(rt.Conn)
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, err
		}
		fes = append(fes, fe)
		if rt.Opts.DebugGTE(rsyncopts.DEBUG_FLIST, 1) {
			rt.Logger.Printf("[Receiver] i=%d ? %s mode=%o len=%d uid=%d gid=%d flags=?",
				len(fes)-1, fe.Name, fe.Mode, fe.Length, fe.Uid, fe.Gid)
		}
	}
	if err := inc.pushEntries(rt, -1, fes); err != nil {
		return nil, err
	}
	rt.IOErrors = inc.dec.IOError()

	if rt.Opts.Progress {
		fmt.Fprintf(rt.Env.Stdout, "\r%d files to consider\n", len(fes))
	}
	return inc.snapshot(), nil
}

// rsync/receiver.c:recv_files (incremental recursion): the frame loop is the
// single reader of the sender stream. It dispatches file-list segments into
// the shared queue, tracks the list-freeing done markers, and receives file
// data for the entries the generator has requested.
func (rt *Transfer) recvFilesInc() error {
	inc := rt.inc
	phase := 0
	maxPhase := 1
	if protocol.SupportsMultiPhase(rt.ProtocolVersion()) {
		maxPhase = 2
	}
	for {
		idx, err := rt.readNdx()
		if err != nil {
			inc.fail(err)
			return err
		}
		if idx == protocol.NdxFlistEOF {
			// sender.c:send_extra_file_list: all segments are on the wire.
			// The trailing i/o-error words were already consumed per segment
			// terminator and OR-ed into the decoder's accumulator.
			inc.markListsDone()
			rt.IOErrors |= inc.dec.IOError()
			continue
		}
		if idx == protocol.NdxDelStats {
			for i := 0; i < 5; i++ {
				if _, err := protocol.ReadVarint(rt.Conn); err != nil {
					inc.fail(err)
					return fmt.Errorf("reading delete stats: %v", err)
				}
			}
			continue
		}
		if idx == protocol.NdxDone {
			// receiver.c:842-862: free the oldest unreleased list and keep
			// going without counting a phase; when the last list is freed
			// the same marker also counts a phase.
			if inc.freeOne() && inc.unfreedLists() > 0 {
				continue
			}
			phase++
			if rt.Opts.DebugGTE(rsyncopts.DEBUG_RECV, 1) {
				rt.Logger.Printf("recvFiles phase=%d", phase)
			}
			if phase > maxPhase {
				break
			}
			continue
		}
		if idx <= protocol.NdxFlistOff {
			if err := inc.decodeSegment(rt, protocol.NdxFlistOff-idx); err != nil {
				inc.fail(err)
				return err
			}
			continue
		}

		// rsync/rsync.c:read_ndx_and_attrs: the file index is followed by the
		// itemize iflags shortint, optionally the basis type byte and the
		// xname vstring.
		iflags := uint16(rsync.ITEM_TRANSFER)
		if protocol.SupportsIFlags(rt.ProtocolVersion()) {
			iflags, err = rt.Conn.ReadShortint()
			if err != nil {
				inc.fail(err)
				return err
			}
			if iflags&rsync.ITEM_BASIS_TYPE_FOLLOWS != 0 {
				if _, err := rt.Conn.ReadByte(); err != nil {
					inc.fail(err)
					return err
				}
			}
			if iflags&rsync.ITEM_XNAME_FOLLOWS != 0 {
				if _, err := rt.Conn.ReadVString(); err != nil {
					inc.fail(err)
					return err
				}
			}
		}
		f := inc.lookup(idx)
		if f == nil {
			err := fmt.Errorf("invalid file index %d under incremental recursion", idx)
			inc.fail(err)
			return err
		}
		if iflags&rsync.ITEM_TRANSFER == 0 {
			// The sender echoes itemize messages for entries that do not
			// carry data (e.g. attribute-only updates).
			if rt.Opts.DebugGTE(rsyncopts.DEBUG_RECV, 1) {
				rt.Logger.Printf("recvFiles idx=%d iflags=0x%x (no transfer)", idx, iflags)
			}
			continue
		}
		if rt.Opts.DebugGTE(rsyncopts.DEBUG_RECV, 1) {
			rt.Logger.Printf("receiving file idx=%d iflags=0x%x: %+v", idx, iflags, f)
		}
		if rt.Opts.Progress {
			fmt.Fprintln(rt.Env.Stdout, f.Name)
		}
		if err := rt.recvFile1(f); err != nil {
			inc.fail(err)
			return err
		}
	}
	if rt.Opts.DebugGTE(rsyncopts.DEBUG_RECV, 1) {
		rt.Logger.Printf("recvFiles finished")
	}
	return nil
}

// rsync/generator.c:generate_files (incremental recursion): the generator
// drains the shared entry queue (directories always precede their contents),
// releases completed segments with done markers, and finishes with the usual
// phase-done sequence.
func (rt *Transfer) generateFilesInc() error {
	for {
		f, rel, done, err := rt.inc.next()
		if err != nil {
			return err
		}
		switch {
		case rel:
			// generator.c:2698: release the oldest completed file-list.
			if err := rt.writeNdx(protocol.NdxDone, 0); err != nil {
				return err
			}
		case done:
			return rt.writePhaseDones()
		default:
			if err := rt.recvGenerator(f); err != nil {
				return err
			}
		}
	}
}

// writePhaseDones sends the generator's closing done-marker sequence,
// identical for the complete-list and incremental paths (generator.c:2846+).
func (rt *Transfer) writePhaseDones() error {
	if rt.Opts.DebugGTE(rsyncopts.DEBUG_GENR, 1) {
		rt.Logger.Printf("generateFiles phase=1")
	}
	if err := rt.writeNdx(protocol.NdxDone, 0); err != nil {
		return err
	}

	if rt.Opts.DebugGTE(rsyncopts.DEBUG_GENR, 1) {
		rt.Logger.Printf("generateFiles phase=2")
	}
	if err := rt.writeNdx(protocol.NdxDone, 0); err != nil {
		return err
	}

	// rsync/generator.c:2873-2877: at protocol >= 31 the generator reports its
	// delete counters between the second and third phase-done markers.
	if protocol.SupportsDeleteStats(rt.ProtocolVersion()) && rt.Opts.DeleteMode {
		if err := rt.writeDelStats(); err != nil {
			return err
		}
	}

	// rsync/generator.c:2882-2890: with protocol >= 29, the generator
	// closes a third (delay-updates) phase.
	if protocol.SupportsMultiPhase(rt.ProtocolVersion()) {
		if rt.Opts.DebugGTE(rsyncopts.DEBUG_GENR, 1) {
			rt.Logger.Printf("generateFiles phase=3")
		}
		if err := rt.writeNdx(protocol.NdxDone, 0); err != nil {
			return err
		}
	}
	return nil
}

// deleteIncWhenReady blocks until the complete file list has arrived, then
// runs the deletion pass (C's delete-during: under incremental recursion the
// list is only complete partway through the transfer, so deletion cannot run
// up front). It runs on its own goroutine and never blocks the generator.
func (rt *Transfer) deleteIncWhenReady() error {
	if err := rt.inc.waitListsDone(); err != nil {
		return err
	}
	return rt.deleteFiles(rt.inc.deleteList())
}
