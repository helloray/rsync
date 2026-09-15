package receiver

import (
	"encoding/binary"
	"errors"
	"io"
	"sync/atomic"

	"github.com/gokrazy/rsync/internal/rsyncopts"
	"github.com/gokrazy/rsync/internal/rsyncwire"
)

// IOERR_VALID_MASK mirrors rsync.h:191-200: only the GENERAL, VANISHED and
// DEL_LIMIT bits are defined; anything else in an MSG_IO_ERROR payload is
// protocol noise and masked off like C does (rsync/io.c:1706).
const ioerrValidMask = (1 << 0) | (1 << 1) | (1 << 2)

// controlFrameReader is a stream-level tolerance layer around the multiplexed
// connection reader. The C sender emits multiplex control frames (per-file
// MSG_NO_SEND/MSG_SUCCESS, phase-end MSG_IO_ERROR, ...) which can interleave
// between ANY two DATA frames — mplex_send_msg just waits for a frame
// boundary, which includes the middle of a file's token stream and between
// file-list segment records (rsync/io.c:writefd). Only the readNdx sites
// tolerate them per-site; every other reader (recvToken, receiveData's
// SumHead.ReadFrom, the trailing checksum read, the flist decoder) would
// treat one as a fatal error and abort the transfer — exactly the
// "mplex control message tag 102 (len 4)" failure this guards against.
//
// The wrapper routes those frames before they surface. The MultiplexReader
// has already consumed the frame's bytes, so retrying the Read transparently
// continues with the next frame and the stream stays byte-consistent.
type controlFrameReader struct {
	r     io.Reader
	rt    *Transfer
	close func() error
}

func (cfr *controlFrameReader) Read(p []byte) (int, error) {
	for {
		n, err := cfr.r.Read(p)
		if n > 0 || err == nil {
			return n, err
		}
		var cfe *rsyncwire.ControlFrameError
		if errors.As(err, &cfe) {
			cfr.rt.routeControlFrame(cfe, nil)
			continue
		}
		return n, err
	}
}

func (cfr *controlFrameReader) Close() error { return cfr.close() }

// filterControlFrames installs the stream-level control-frame filter on the
// transfer's connection. Idempotent; must be called before any file data is
// read, because the filter has to be in place for every reader — not just
// the ndx loops.
func (rt *Transfer) filterControlFrames() {
	if rt.ctlFiltered {
		return
	}
	rt.ctlFiltered = true
	orig := rt.Conn.Reader
	rt.Conn.Reader = &controlFrameReader{
		r:     orig,
		rt:    rt,
		close: orig.Close,
	}
}

// routeControlFrame handles a control frame that surfaced where DATA was
// expected. The frame's payload has been fully consumed, so the stream stays
// in sync; like C's read_a_msg, which books these frames instead of aborting
// the stream (rsync/io.c:1695-1818), the transfer continues. fileList, when
// non-nil, resolves the frame's file index to a name for the log.
func (rt *Transfer) routeControlFrame(cfe *rsyncwire.ControlFrameError, fileList []*File) {
	ndx := int32(-1)
	if len(cfe.Payload) == 4 {
		ndx = int32(binary.LittleEndian.Uint32(cfe.Payload))
	}
	name := ""
	if ndx >= 0 {
		switch {
		case fileList != nil && int(ndx) < len(fileList):
			name = " (" + fileList[ndx].Name + ")"
		case rt.recvFileList != nil && int(ndx) < len(rt.recvFileList):
			name = " (" + rt.recvFileList[ndx].Name + ")"
		case rt.inc != nil:
			if f := rt.inc.lookup(ndx); f != nil {
				name = " (" + f.Name + ")"
			}
		}
	}
	switch cfe.Tag {
	case rsyncwire.MsgNoSend:
		// rsync/sender.c:709-723: the sender could not open the source file
		// (typically "file has vanished"), so no data follows for this
		// entry. C forwards the frame to the generator, which books the
		// entry as FES_NO_SEND (rsync/io.c:1809-1818); under incremental
		// recursion the routing slot is released here so the entry's File
		// does not pin its segment.
		if rt.Opts.DebugGTE(rsyncopts.DEBUG_RECV, 1) {
			rt.Logger.Printf("sender did not send file ndx=%d%s (vanished or failed to open); continuing", ndx, name)
		} else {
			rt.Logger.Printf("sender did not send file ndx=%d%s; continuing", ndx, name)
		}
		if rt.inc != nil && ndx >= 0 {
			rt.inc.releaseFile(ndx)
		}
	case rsyncwire.MsgIoError:
		// rsync/sender.c:809-810 sends the io-error bitmask after a phase
		// with per-file failures; C consumes it as io_error |= val
		// (rsync/io.c:1702-1711) — informational, never fatal. OR it into
		// the transfer's counter so deleteFiles skips deletion.
		val := uint32(0)
		if len(cfe.Payload) == 4 {
			val = binary.LittleEndian.Uint32(cfe.Payload)
		}
		atomic.OrInt32(&rt.IOErrors, int32(val&ioerrValidMask))
		rt.Logger.Printf("sender reported i/o errors (mask 0x%x); continuing", val&ioerrValidMask)
	case rsyncwire.MsgRedo:
		rt.Logger.Printf("sender asked to redo file ndx=%d%s; continuing without it", ndx, name)
	case rsyncwire.MsgSuccess, rsyncwire.MsgDeleted, rsyncwire.MsgStats:
		// Per-file completion notices and transfer stats: purely
		// informational in the fused Go receiver.
		if rt.Opts.DebugGTE(rsyncopts.DEBUG_RECV, 1) {
			rt.Logger.Printf("consumed control frame tag %d (ndx=%d%s)", cfe.Tag, ndx, name)
		}
	default:
		rt.Logger.Printf("ignoring control frame tag %d (len %d) in the data stream", cfe.Tag, len(cfe.Payload))
	}
}
