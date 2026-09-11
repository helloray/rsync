package receiver

import (
	"fmt"
	"io/fs"
	"os"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gokrazy/rsync/internal/protocol"
	"github.com/gokrazy/rsync/internal/rsyncopts"
	"github.com/gokrazy/rsync/internal/rsyncstats"
	"github.com/gokrazy/rsync/internal/rsyncwire"
	"golang.org/x/sync/errgroup"
)

func isTopDir(f *File) bool {
	// TODO: once we check the f.Flags:
	// if !f.FileMode().IsDir() {
	//    // non-directories can get the top_dir flag set,
	//    // but it must be ignored (only for protocol reasons).
	//   return false
	// }
	// return (f.Flags & TOP_DIR) != 0
	return f.Name == "."
}

func (rt *Transfer) deleteFiles(fileList []*File) error {
	if rt.IOErrors > 0 {
		rt.Logger.Printf("IO error encountered, skipping file deletion")
		return nil
	}

	for _, f := range fileList {
		if !isTopDir(f) {
			continue
		}
		rt.Logger.Printf("deleting in %s", f.Name)
		// Other rsync implementations generate a local file list and compare it
		// with the remote file list, we re-implement the path→name mapping part
		// of file list generation here. We could change it for consistency.
		err := fs.WalkDir(rt.DestRoot.FS(), ".", func(path string, info fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			rt.Logger.Printf("WalkDir(%q)", path)
			if findInFileList(fileList, path) {
				return nil
			}
			if rt.Opts.Verbose {
				rt.Logger.Printf("  deleting %s", path)
			}
			if rt.Opts.DryRun {
				return nil
			}
			if err := rt.DestRoot.RemoveAll(path); err != nil {
				rt.Logger.Printf("  deleting %s failed: %v", path, err)
				// keep going
			} else {
				rt.countDeletion(info)
				// Only skip the remainder when a directory was removed;
				// for a regular file, fs.SkipDir would abandon the rest
				// of this directory's scan (deletions after it would be
				// silently missed).
				if info.IsDir() {
					return fs.SkipDir
				}
			}
			return nil
		})
		if err != nil {
			if os.IsNotExist(err) {
				return nil // destination does not exist, nothing to do
			}
			return err
		}
	}
	return nil
}

// countDeletion classifies one removed entry for the NDX_DEL_STATS counters.
// RemoveAll() takes whole subtrees at once, so nested contents are not
// counted individually — the counters are informational (they feed the
// --stats summary), and the wire format does not depend on exact values.
func (rt *Transfer) countDeletion(info fs.DirEntry) {
	if info.IsDir() {
		rt.delStats.dirs++
		return
	}
	if info.Type()&fs.ModeSymlink != 0 {
		rt.delStats.symlinks++
		return
	}
	if info.Type()&(fs.ModeDevice|fs.ModeCharDevice) != 0 {
		rt.delStats.devices++
		return
	}
	if info.Type()&(fs.ModeNamedPipe|fs.ModeSocket) != 0 {
		rt.delStats.specials++
		return
	}
	rt.delStats.files++
}

// rsync/main.c:write_del_stats: report the delete counters as an
// NDX_DEL_STATS frame (protocol >= 31). C tracks deleted_files as the total
// and sends files = total - dirs - symlinks - devices - specials; the Go
// counters are already mutually exclusive buckets, so the regular-file count
// is just the remaining bucket.
func (rt *Transfer) writeDelStats() error {
	if err := rt.writeNdx(protocol.NdxDelStats, 0); err != nil {
		return err
	}
	s := &rt.delStats
	for _, v := range []int32{s.files, s.dirs, s.symlinks, s.devices, s.specials} {
		if err := protocol.WriteVarint(rt.Conn, v); err != nil {
			return err
		}
	}
	return nil
}

// rsync/main.c:do_recv
func (rt *Transfer) Do(c *rsyncwire.Conn, fileList []*File, noReport bool) (*rsyncstats.TransferStats, error) {
	if rt.inc == nil && rt.Opts.DeleteMode {
		if err := rt.deleteFiles(fileList); err != nil {
			return nil, err
		}
	}

	var eg errgroup.Group
	// Wrap both, the generator and the receiver goroutine, in waitFor() calls
	// to ensure we don’t block on the generator when the receiver returns an
	// error, or vice versa (instead, return and let the goroutine finish in the
	// background).
	// waitFor calls f and waits for it to complete, but only until the specified
	// context is cancelled.
	var closeOnce sync.Once
	closeOnErr := func(err error) error {
		if err != nil {
			closeOnce.Do(func() {
				// rsync/log.c rides the abort reason in MSG_ERROR frames and
				// cleanup.c follows with the 4-byte exit code in
				// MSG_ERROR_EXIT (rsync/io.c:read_a_msg rejects any other
				// payload as "invalid multi-message"): tell the peer the
				// transfer is aborting so it reports our error instead of
				// hanging or dying on a bare EOF.
				if serr := c.SendError(err.Error()); serr != nil {
					rt.Logger.Printf("sending error: %v", serr)
				}
				if serr := c.SendErrorExit(rsyncwire.RERRPartial); serr != nil {
					rt.Logger.Printf("sending error-exit: %v", serr)
				}
				// Deliberately no Close here: the other goroutines still
				// read the connection, and closing while the peer still has
				// file data in flight sends a RST that discards the peer's
				// received-but-unread data — including the frames just sent.
				// The drain-and-close after eg.Wait() handles the shutdown.
			})
		}
		return err
	}
	if rt.inc != nil && rt.Opts.DeleteMode {
		// Incremental recursion: the complete list only exists once every
		// segment has arrived, so the deletion pass waits for that and then
		// runs concurrently with the generator (C's delete-during).
		eg.Go(func() error { return closeOnErr(rt.deleteIncWhenReady()) })
	}
	eg.Go(func() error { return closeOnErr(rt.GenerateFiles(fileList)) })
	eg.Go(func() error { return closeOnErr(rt.RecvFiles(fileList)) })
	if err := eg.Wait(); err != nil {
		// All goroutines are done, so nobody else reads the connection: the
		// peer exits once it processes the abort frames, and draining its
		// remaining in-flight data lets the final Close produce a FIN (a RST
		// would discard the frames on the peer side, see GracefulAbortClose).
		c.GracefulAbortClose(5 * time.Second)
		return nil, err
	}
	if rt.inc != nil {
		fileList = rt.inc.snapshot()
	}
	if rt.retouchDirPerms /* || rt.retouchDirTimes */ {
		if err := rt.touchUpDirs(fileList); err != nil {
			return nil, err
		}
	}

	var stats *rsyncstats.TransferStats
	if !noReport {
		var err error
		stats, err = rt.report(c)
		if err != nil {
			return nil, err
		}
	}

	// rsync/cleanup.c:exit_cleanup: a receiver with per-entry IO errors ends
	// the session with MSG_ERROR_EXIT(RERR 23, "some files/attrs were not
	// transferred") BEFORE the goodbye exchange — the peer is still reading
	// here and _exit_cleanup()s on the spot, so a later error-exit would
	// never be seen. The peer is gone after this, so the goodbye exchange
	// is skipped.
	if n := atomic.LoadInt32(&rt.skipCount); n > 0 {
		if err := c.SendError(fmt.Sprintf("%d files were skipped (see the server log for the reasons)", n)); err != nil {
			return nil, err
		}
		if err := c.SendErrorExit(rsyncwire.RERRPartial); err != nil {
			return nil, err
		}
		return stats, nil
	}

	// send final goodbye message
	if err := rt.writeNdx(protocol.NdxDone, 0); err != nil {
		return nil, err
	}
	// rsync/main.c:read_final_goodbye: at protocol >= 31 the goodbye is a
	// two-round exchange. The sender (main.c:1007) and the receiver process
	// (main.c:1118) both call it: the sender reads the receiver's first
	// goodbye, echoes one back, and reads the second; the receiver reads that
	// echo before sending the second goodbye (the receiver process at
	// main.c:1108 and the generator at main.c:1166 each contribute one). The
	// read is load-bearing for Go↔Go transfers: the transport pipe is
	// unbuffered, so writing both goodbyes back-to-back deadlocks against the
	// sender's echo write.
	if protocol.SupportsExtendedGoodbye(rt.ProtocolVersion()) {
		finish, err := rt.ndxRead().ReadNdx(rt.Conn)
		if err != nil {
			return nil, err
		}
		if finish != protocol.NdxDone {
			return nil, fmt.Errorf("protocol error: expected goodbye echo, got %d", finish)
		}
		if err := rt.writeNdx(protocol.NdxDone, 0); err != nil {
			return nil, err
		}
	}

	return stats, nil
}

// rsync/main.c:report
func (rt *Transfer) report(c *rsyncwire.Conn) (*rsyncstats.TransferStats, error) {
	// Read the first two in opposite order (compared to the sender’s
	// handle_stats) because the meaning of read/write swaps when switching
	// from sender to receiver (rsync/main.c:handle_stats).
	//
	// The sender encodes each value with write_varlong30 (rsync/main.c:353),
	// which is a varint at protocol >= 30 and falls back to the fixed-width
	// longint below 30 (rsync/io.c:read_varlong30) — the reader must mirror
	// that split or the byte stream desyncs and both sides deadlock.
	readStat := func() (int64, error) {
		if rt.ProtocolVersion() >= 30 {
			return protocol.ReadVLong30(c, 3)
		}
		return protocol.ReadLongInt(c)
	}
	// total bytes written (to network connection)
	written, err := readStat()
	if err != nil {
		return nil, err
	}
	// total bytes read (from network connection)
	read, err := readStat()
	if err != nil {
		return nil, err
	}
	// total size of files
	size, err := readStat()
	if err != nil {
		return nil, err
	}
	// rsync/main.c:handle_stats: with protocol >= 29, the file list build
	// and transfer times follow the three byte counters.
	if protocol.SupportsMultiPhase(rt.ProtocolVersion()) {
		if _, err := readStat(); err != nil { // flist build time
			return nil, err
		}
		if _, err := readStat(); err != nil { // flist transfer time
			return nil, err
		}
	}
	if rt.Opts.InfoGTE(rsyncopts.INFO_STATS, 1) {
		rt.Logger.Printf("server sent stats: read=%d, written=%d, size=%d", read, written, size)
	}

	return &rsyncstats.TransferStats{
		Read:    read,
		Written: written,
		Size:    size,
	}, nil
}
