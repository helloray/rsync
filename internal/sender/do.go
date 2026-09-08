package sender

import (
	"bytes"
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
	var buf bytes.Buffer
	var syserr error
	write := func(v int64) {
		if syserr == nil {
			syserr = protocol.WriteVLong30(&buf, v, 3)
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
	finish, err := st.ndxRead().ReadNdx(st.Conn)
	if err != nil {
		return nil, err
	}
	if finish != protocol.NdxDone {
		return nil, fmt.Errorf("protocol error: expected final -1, got %d", finish)
	}

	return &rsyncstats.TransferStats{
		Read:    crd.BytesRead,
		Written: cwr.BytesWritten,
		Size:    fileList.TotalSize,
	}, nil
}
