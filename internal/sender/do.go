package sender

import (
	"bytes"
	"errors"
	"fmt"
	"sort"

	"github.com/gokrazy/rsync/internal/protocol"
	"github.com/gokrazy/rsync/internal/rsyncopts"
	"github.com/gokrazy/rsync/internal/rsyncstats"
	"github.com/gokrazy/rsync/internal/rsyncwire"
)

// rsync/main.c:handle_stats
func (st *Transfer) handleStats(crd *rsyncwire.CountingReader, cwr *rsyncwire.CountingWriter, fileList *fileList) error {
	if !st.Opts.Server() || !st.Opts.Sender() {
		return nil
	}

	// send statistics as varlong30 values, coalesced into a single multiplex
	// DATA frame, exactly like _c-rsync/main.c:handle_stats does when
	// am_server && am_sender (write_varlong30 for each, one buffered flush).
	// The peer receiver reads them back as varlong30 (main.c:371-377); any
	// other encoding (fixed-width ints, per-value frames) desyncs its reader.
	// Below protocol 30, write_varlong30 falls back to the fixed-width
	// longint (rsync/io.h), so the writer must gate on the version too.
	var buf bytes.Buffer
	var syserr error
	write := func(v int64) {
		if syserr != nil {
			return
		}
		if st.Opts.ProtocolVersion() >= 30 {
			syserr = protocol.WriteVLong30(&buf, v, 3)
		} else {
			syserr = protocol.WriteLongInt(&buf, v)
		}
	}
	// total bytes read (from network connection)
	write(crd.BytesRead)
	// total bytes written (to network connection)
	write(cwr.BytesWritten)
	// total size of files
	write(fileList.TotalSize)
	// rsync/main.c:handle_stats: with protocol >= 29, the file list build
	// and transfer times follow the three byte counters.
	if protocol.SupportsMultiPhase(st.Opts.ProtocolVersion()) {
		write(0) // flist build time
		write(0) // flist transfer time
	}
	if syserr != nil {
		return syserr
	}
	_, err := st.Conn.Writer.Write(buf.Bytes())
	return err
}

// rsync/main.c:client_run am_sender
func (st *Transfer) Do(crd *rsyncwire.CountingReader, cwr *rsyncwire.CountingWriter, modPath string, paths []string, exclusionList *filterRuleList) (*rsyncstats.TransferStats, error) {
	if exclusionList == nil {
		exclusionList = &filterRuleList{}
	}

	// “Update exchange” as per
	// https://github.com/kristapsdz/openrsync/blob/master/rsync.5

	// send file list
	st.Logger.Printf("SendFileList(modPath=%q, paths=%q)", modPath, paths)
	fileList, err := st.SendFileList(modPath, paths, exclusionList)
	if err != nil {
		return nil, err
	}
	defer fileList.Close()

	if st.Opts.DebugGTE(rsyncopts.DEBUG_FLIST, 3) {
		st.Logger.Printf("file list sent")
	}

	// Sort the file list. The client sorts, so we need to sort, too (in the
	// same way!), otherwise our indices do not match what the client will
	// request.
	//
	// For protocol >= 29, SendFileList already ordered the files with
	// f_name_cmp (both C sides sort identically), so only the legacy
	// lexical order still needs to be applied here.
	if st.Opts.ProtocolVersion() < 29 {
		sort.Slice(fileList.Files, func(i, j int) bool {
			return fileList.Files[i].Wpath < fileList.Files[j].Wpath
		})
	}

	if err := st.SendFiles(fileList); err != nil {
		// rsync/io.c:send_msg(MSG_ERROR_EXIT): tell the peer the transfer is
		// aborting so it reports our error instead of waiting for data that
		// will never come.
		if serr := st.Conn.SendErrorExit(err.Error()); serr != nil {
			st.Logger.Printf("sending error-exit: %v", serr)
		}
		return nil, err
	}

	if err := st.handleStats(crd, cwr, fileList); err != nil {
		return nil, err
	}

	if st.Opts.DebugGTE(rsyncopts.DEBUG_PROTO, 1) {
		st.Logger.Printf("reading final int32")
	}

	// The receiver signals the end of the transfer with a final NDX_DONE.
	// At protocol >= 30 this is a single-byte byte-reduction marker (0x00),
	// at lower versions a plain int32 -1, so it must be read through the ndx
	// codec rather than as a fixed-width int (rsync/main.c:client_run).
	//
	// rsync/main.c:read_final_goodbye: at protocol >= 31 the goodbye is a
	// two-round exchange — read the first final NDX_DONE, echo it back
	// immediately, then read the second (the receiver process at main.c:1108
	// and the generator at main.c:1165 each contribute one). The echo must go
	// out before the second read: the receiver only sends the second goodbye
	// once its echo arrives. The generator's NDX_DEL_STATS and third
	// phase-done marker can interleave with these goodbyes because the two C
	// processes buffer their output independently, so drain DEL_STATS frames
	// here and tolerate control frames instead of assuming a fixed order
	// after the phase loop.
	if protocol.SupportsExtendedGoodbye(st.Opts.ProtocolVersion()) {
		echoed := false
		for {
			ndx, err := st.ndxRead().ReadNdx(st.Conn)
			if err != nil {
				var cfe *rsyncwire.ControlFrameError
				if errors.As(err, &cfe) {
					continue
				}
				return nil, err
			}
			if ndx == protocol.NdxDelStats {
				if err := st.drainDelStats(); err != nil {
					return nil, err
				}
				continue
			}
			if ndx != protocol.NdxDone {
				return nil, fmt.Errorf("protocol error: expected final -1, got %d", ndx)
			}
			if !echoed {
				if err := st.ndxWrite().WriteNdx(st.Conn, protocol.NdxDone); err != nil {
					return nil, err
				}
				echoed = true
				continue
			}
			break
		}
	} else {
		finish, err := st.ndxRead().ReadNdx(st.Conn)
		if err != nil {
			return nil, err
		}
		if finish != protocol.NdxDone {
			return nil, fmt.Errorf("protocol error: expected final -1, got %d", finish)
		}
	}

	return &rsyncstats.TransferStats{
		Read:    crd.BytesRead,
		Written: cwr.BytesWritten,
		Size:    fileList.TotalSize,
	}, nil
}
