package sender

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os"
	"sort"

	"github.com/gokrazy/rsync"
	"github.com/gokrazy/rsync/internal/protocol"
	"github.com/gokrazy/rsync/internal/rsyncchecksum"
	"github.com/gokrazy/rsync/internal/rsynccommon"
	"github.com/gokrazy/rsync/internal/rsyncopts"
	"github.com/gokrazy/rsync/internal/rsyncwire"
	"github.com/mmcloughlin/md4"
	"golang.org/x/sync/errgroup"
)

// ndxWrite returns the NDX writer codec, creating it from the negotiated
// protocol version on first use.
func (st *Transfer) ndxWrite() *protocol.NdxCodec {
	if st.ndxWriteC == nil {
		st.ndxWriteC = protocol.NewNdxCodec(st.Opts.ProtocolVersion())
	}
	return st.ndxWriteC
}

// ndxRead returns the NDX reader codec, creating it from the negotiated
// protocol version on first use.
func (st *Transfer) ndxRead() *protocol.NdxCodec {
	if st.ndxReadC == nil {
		st.ndxReadC = protocol.NewNdxCodec(st.Opts.ProtocolVersion())
	}
	return st.ndxReadC
}

// writeNdxTransfer sends a transfer request index to the receiver, like
// rsync/sender.c:write_ndx_and_attrs with iflags=ITEM_TRANSFER
// (byte-reduction encoding for protocol >= 30, a plain int32 below).
func (st *Transfer) writeNdxTransfer(fileIndex int32) error {
	if err := st.ndxWrite().WriteNdx(st.Conn, fileIndex); err != nil {
		return err
	}
	if !protocol.SupportsIFlags(st.Opts.ProtocolVersion()) {
		return nil
	}
	return st.Conn.WriteShortint(rsync.ITEM_TRANSFER)
}

// writeNdxAndAttrs echoes a file index and its itemize flags to the
// receiver, like rsync/sender.c:write_ndx_and_attrs (rsync/io.c:write_ndx
// falls back to a plain int32 for protocol < 30).
func (st *Transfer) writeNdxAndAttrs(fileIndex int32, iflags uint16, fnamecmpType byte) error {
	if err := st.ndxWrite().WriteNdx(st.Conn, fileIndex); err != nil {
		return err
	}
	if !protocol.SupportsIFlags(st.Opts.ProtocolVersion()) {
		return nil
	}
	if err := st.Conn.WriteShortint(iflags); err != nil {
		return err
	}
	if iflags&rsync.ITEM_BASIS_TYPE_FOLLOWS != 0 {
		if err := st.Conn.WriteByte(fnamecmpType); err != nil {
			return err
		}
	}
	if iflags&rsync.ITEM_XNAME_FOLLOWS != 0 {
		if err := st.Conn.WriteVString(""); err != nil {
			return err
		}
	}
	return nil
}

// rsync/sender.c:send_files()
func (st *Transfer) SendFiles(fileList *fileList) error {
	phase := 0
	for {
		// receive data about receiver’s copy of the file list contents (not
		// ordered)
		// see (*rsync.Receiver).Generator()
		fileIndex, err := st.ndxRead().ReadNdx(st.Conn)
		if err != nil {
			// A multiplex control frame (per-file Success/NoSend/Deleted, stats,
			// ...) can surface here wherever DATA was expected. In the default
			// full-list path our peer never sends these, but tolerate them so a
			// stray frame can't abort the transfer: log and keep reading.
			var cfe *rsyncwire.ControlFrameError
			if errors.As(err, &cfe) {
				st.Logger.Printf("ignoring control frame tag %d while reading ndx", cfe.Tag)
				continue
			}
			return err
		}
		if fileIndex == -1 {
			phase++
			// rsync/sender.c:send_files: max_phase is 2 with protocol >= 29,
			// so the sender reads three phase-done markers in total,
			// acknowledging the first two with an echo and breaking on the
			// third (whose final acknowledgment is written below the loop).
			maxPhase := 1
			if protocol.SupportsMultiPhase(st.Opts.ProtocolVersion()) {
				maxPhase = 2
			}
			if phase > maxPhase {
				break
			}
			// acknowledge phase change by sending -1
			if err := st.ndxWrite().WriteNdx(st.Conn, protocol.NdxDone); err != nil {
				return err
			}
			continue
		}

		// rsync/rsync.c:read_ndx_and_attrs: with protocol >= 29, the file
		// index is followed by the itemize iflags shortint, optionally the
		// basis type byte and the xname vstring.
		iflags := uint16(rsync.ITEM_TRANSFER)
		fnamecmpType := byte(rsync.FNAMECMP_FNAME)
		if protocol.SupportsIFlags(st.Opts.ProtocolVersion()) {
			iflags, err = st.Conn.ReadShortint()
			if err != nil {
				return err
			}
			// rsync/rsync.c:read_ndx_and_attrs: support the protocol-29
			// keep-alive style (index == list length, iflags == ITEM_IS_NEW).
			if st.Opts.ProtocolVersion() < 30 &&
				int(fileIndex) == len(fileList.Files) &&
				iflags == rsync.ITEM_IS_NEW {
				continue
			}
			if iflags&rsync.ITEM_BASIS_TYPE_FOLLOWS != 0 {
				fnamecmpType, err = st.Conn.ReadByte()
				if err != nil {
					return err
				}
			}
			if iflags&rsync.ITEM_XNAME_FOLLOWS != 0 {
				if _, err := st.Conn.ReadVString(); err != nil {
					return err
				}
			}
		}
		if fileIndex < 0 || int(fileIndex) >= len(fileList.Files) {
			return fmt.Errorf("invalid file index %d (list has %d entries)",
				fileIndex, len(fileList.Files))
		}

		// rsync/sender.c:send_files: echo itemize messages that do not
		// carry data (e.g. attribute-only updates) back to the receiver.
		if iflags&rsync.ITEM_TRANSFER == 0 {
			if err := st.writeNdxAndAttrs(fileIndex, iflags, fnamecmpType); err != nil {
				return err
			}
			continue
		}

		if st.Opts.DryRun() {
			if err := st.writeNdxAndAttrs(fileIndex, iflags, fnamecmpType); err != nil {
				return err
			}
			continue
		}

		fl := fileList.Files[fileIndex]
		st.Progress.Reset(uint64(fl.Length))

		head, err := st.receiveSums()
		if err != nil {
			return err
		}

		// The following quotes are citations from
		// https://www.samba.org/~tridge/phd_thesis.pdf, section 3.2.6 The
		// signature search algorithm (PDF page 64).

		// rsync/match.c:build_hash_table
		targets := make([]target, len(head.Sums))
		tagTable := make(map[uint16]int) // TODO: or int32 more specifically?
		{
			// “The first step in the algorithm is to sort the received
			// signatures by a 16 bit hash of the fast signature.”
			for idx, sum := range head.Sums {
				targets[idx] = target{
					index: int32(idx),
					tag:   rsyncchecksum.Tag(sum.Sum1),
				}
			}
			sort.Slice(targets, func(i, j int) bool {
				return targets[i].tag < targets[j].tag
			})

			// “A 16 bit index table is then formed which takes a 16 bit hash
			// value and gives an index into the sorted signature table which
			// points to the first entry in the table which has a matching
			// hash.”
			for idx := len(head.Sums) - 1; idx >= 0; idx-- {
				tagTable[targets[idx].tag] = idx
			}
		}

		st.lastMatch = 0
		if len(head.Sums) == 0 {
			// fast path: send the whole file
			err = st.sendFile(fileIndex, fl)
		} else {
			err = st.hashSearch(targets, tagTable, head, fileIndex, fl)
		}
		if err != nil {
			if _, ok := err.(*os.PathError); ok {
				// OpenFile() failed. Log the error (server side only) and
				// proceed. Only starting with protocol 30, an I/O error flag is
				// sent after the file transfer phase.
				if os.IsNotExist(err) {
					st.Logger.Printf("file has vanished: %s", fl.path)
				} else {
					st.Logger.Printf("sendFiles: %v", err)
				}
				continue
			} else {
				return err
			}
		}
	}

	// phase done
	if err := st.ndxWrite().WriteNdx(st.Conn, protocol.NdxDone); err != nil {
		return err
	}

	return nil
}

// rsync/sender.c:receive_sums()
func (st *Transfer) receiveSums() (rsync.SumHead, error) {
	var head rsync.SumHead
	if err := head.ReadFrom(st.Conn); err != nil {
		return head, err
	}
	var offset int64
	head.Sums = make([]rsync.SumBuf, int(head.ChecksumCount))
	for i := int32(0); i < head.ChecksumCount; i++ {
		shortChecksum, err := st.Conn.ReadInt32()
		if err != nil {
			return head, err
		}
		sb := rsync.SumBuf{
			Index:  i,
			Offset: offset,
			Sum1:   uint32(shortChecksum),
		}
		if i == head.ChecksumCount-1 && head.RemainderLength != 0 {
			sb.Len = int64(head.RemainderLength)
		} else {
			sb.Len = int64(head.BlockLength)
		}
		offset += sb.Len
		n, err := io.ReadFull(st.Conn.Reader, sb.Sum2[:head.ChecksumLength])
		if err != nil {
			return head, err
		}
		_ = n
		// st.logger.Printf("chunk[%d] len=%d offset=%.0f sum1=%08x, sum2=%x",
		// 	i, sb.len, float64(sb.offset), sb.sum1, sb.sum2[:n])
		head.Sums[i] = sb
	}
	return head, nil
}

func (st *Transfer) sendFile(fileIndex int32, fl file) error {
	// rsync/rsync.h defines chunkSize as 32 * 1024. C rsync rejects a
	// single uncompressed token longer than 32 KiB ("invalid uncompressed
	// token length"), so we must not exceed this when sending the whole
	// file to a tridge rsync receiver.
	const chunkSize = 32 * 1024

	f, err := fl.source.Open(fl.path)
	if err != nil {
		return err
	}
	defer f.Close()

	fi, err := f.Stat()
	if err != nil {
		return err
	}

	if err := st.writeNdxTransfer(fileIndex); err != nil {
		return err
	}

	sh := rsynccommon.SumSizesSqroot(fi.Size())
	if err := sh.WriteTo(st.Conn); err != nil {
		return err
	}

	if !st.Opts.Server() &&
		st.Opts.InfoGTE(rsyncopts.INFO_NAME, 1) &&
		st.Opts.InfoGTE(rsyncopts.INFO_PROGRESS, 1) {
		fmt.Fprintln(st.Env.Stdout, fl.path)
	}

	h := md4.New()
	// Mirror C's sum_init seed gating (see match.go): protocol < 30 folds the
	// checksum seed, >= 30 does not. Must match the receiver and C counterpart.
	if st.Opts.ProtocolVersion() < 30 {
		binary.Write(h, binary.LittleEndian, st.Seed)
	}

	// Calculate the md4 hash in a goroutine.
	//
	// This allows an rsync connection to benefit from more than 1 core!
	//
	// We calculate the hash by opening the same file again and reading
	// independently. This keeps the hot loop below focused on shoveling data
	// into the network socket as quickly as possible.
	var eg errgroup.Group
	eg.Go(func() error {
		f, err := fl.source.Open(fl.path)
		if err != nil {
			return err
		}
		defer f.Close()
		var buf [chunkSize]byte
		if _, err := io.CopyBuffer(h, f, buf[:]); err != nil {
			return err
		}
		return nil
	})

	offset := 0
	buf := make([]byte, chunkSize)
	for {
		if st.Opts.InfoGTE(rsyncopts.INFO_PROGRESS, 1) {
			st.Progress.MaybeShow(uint64(offset), false)
		}
		n, err := f.Read(buf)
		if err != nil {
			if err == io.EOF {
				break
			}
			return err
		}
		chunk := buf[:n]
		// chunk size (“rawtok” variable in openrsync)
		if err := st.Conn.WriteInt32(int32(len(chunk))); err != nil {
			return err
		}
		n, err = st.Conn.Writer.Write(chunk)
		if err != nil {
			return err
		}
		offset += n
	}
	if st.Opts.InfoGTE(rsyncopts.INFO_PROGRESS, 1) {
		st.Progress.Show(uint64(offset), true)
	}
	// transfer finished:
	if err := st.Conn.WriteInt32(0); err != nil {
		return err
	}

	// whole file long checksum (16 bytes)
	if err := eg.Wait(); err != nil {
		return err
	}
	// The whole-file checksum used for cross-checking is computed over the file
	// data only (C's sum_init for modern CSUM_MD4, checksum.c, does not feed the
	// checksum seed), so the seed must not be folded into this hash.
	sum := h.Sum(nil)
	// st.logger.Printf("sum: %x (len = %d)", sum, len(sum))
	if _, err := st.Conn.Writer.Write(sum); err != nil {
		return err
	}
	return nil
}
