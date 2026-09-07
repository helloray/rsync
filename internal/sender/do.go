package sender

import (
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

	// send statistics:
	// total bytes read (from network connection)
	if err := st.Conn.WriteInt64(crd.BytesRead); err != nil {
		return err
	}
	// total bytes written (to network connection)
	if err := st.Conn.WriteInt64(cwr.BytesWritten); err != nil {
		return err
	}
	// total size of files
	if err := st.Conn.WriteInt64(fileList.TotalSize); err != nil {
		return err
	}
	// rsync/main.c:handle_stats: with protocol >= 29, the file list build
	// and transfer times follow the three byte counters.
	if protocol.SupportsMultiPhase(st.Opts.ProtocolVersion()) {
		if err := st.Conn.WriteInt64(0); err != nil { // flist build time
			return err
		}
		if err := st.Conn.WriteInt64(0); err != nil { // flist transfer time
			return err
		}
	}
	return nil
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

	finish, err := st.Conn.ReadInt32()
	if err != nil {
		return nil, err
	}
	if finish != -1 {
		return nil, fmt.Errorf("protocol error: expected final -1, got %d", finish)
	}

	return &rsyncstats.TransferStats{
		Read:    crd.BytesRead,
		Written: cwr.BytesWritten,
		Size:    fileList.TotalSize,
	}, nil
}
