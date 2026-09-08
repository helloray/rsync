package flist

import (
	"fmt"
	"io"

	"github.com/gokrazy/rsync"
	"github.com/gokrazy/rsync/internal/protocol"
)

// ioerrValidMask sanitizes the i/o-error word carried by a terminator
// (_c-rsync/rsync.h:IOERR_VALID_MASK).
const ioerrValidMask = 1<<0 | 1<<1 | 1<<2 // IOERR_GENERAL|VANISHED|DEL_LIMIT

// WriteFileList streams a complete, non-incremental file list to w: every entry
// in files, an end-of-list terminator, the uid/gid name lists, and the i/o
// error word. It mirrors the tail of _c-rsync/flist.c:send_file_list plus
// uidlist.c:send_id_lists.
//
// At protocol >= 30 inline uid/gid names ride inside the entries (when
// IncRecurse and not NumericIDs), so the trailing id lists carry only the
// remaining names; at < 30 they carry all of them.
func WriteFileList(w io.Writer, p Params, files []*FileEntry, uidNames, gidNames map[int32]string, ioErrors int32) error {
	c := &codec{params: p, haveLast: false}
	for _, f := range files {
		if err := c.Encode(w, f); err != nil {
			return err
		}
	}
	// end-of-list terminator: a zero flags frame, immediately followed (at
	// protocol >= 30) by the i/o error word. C recv_file_list consumes
	// [term varint(0)][io-word varint] before the id lists when
	// xfer_flags_as_varint (flist.c:2946-2949), but reads the io-word after
	// the id lists before 30 (flist.c:3067-3071). So the io-word must move in
	// front of the trailing id lists for protocol >= 30 to match.
	if err := writeEndOfFlist(w, p, ioErrors != 0, ioErrors); err != nil {
		return err
	}
	// trailing id lists. A codec is used for one direction at a time, so the
	// Write/Read halves must make the same decision.
	if p.PreserveUid && atProto30UsesIDList(p) {
		id0 := p.ID0Names && p.ProtocolVersion >= 30
		if err := WriteIDList(w, uidNames, p.ProtocolVersion, id0); err != nil {
			return err
		}
	}
	if p.PreserveGid && atProto30UsesIDList(p) {
		id0 := p.ID0Names && p.ProtocolVersion >= 30
		if err := WriteIDList(w, gidNames, p.ProtocolVersion, id0); err != nil {
			return err
		}
	}
	// i/o error word (protocol < 30 reads it after the id lists).
	if p.ProtocolVersion >= 30 {
		return nil
	}
	return writeInt32(w, ioErrors)
}

// writeEndOfFlist writes the end-of-list terminator of a (complete or
// incremental) file list, mirroring _c-rsync/flist.c:write_end_of_flist:
// in varint mode a zero varint followed by the i/o-error word, otherwise a
// zero byte — or, when an i/o error is to be reported and SafeFlist allows
// it, the XMIT_IO_ERROR_ENDLIST sentinel shortint followed by the error as a
// varint.
func writeEndOfFlist(w io.Writer, p Params, sendIOError bool, ioErrors int32) error {
	if p.ProtocolVersion >= 30 && p.VarintFlags {
		if err := protocol.WriteVarint(w, 0); err != nil {
			return err
		}
		v := int32(0)
		if sendIOError {
			v = ioErrors
		}
		return protocol.WriteVarint(w, v)
	}
	if sendIOError && p.SafeFlist && p.ProtocolVersion >= 30 {
		if err := writeInt16(w, uint16(rsync.XMIT_EXTENDED_FLAGS|rsync.XMIT_IO_ERROR_ENDLIST)); err != nil {
			return err
		}
		return protocol.WriteVarint(w, ioErrors)
	}
	return writeByte(w, 0)
}

// ReadFileList reads a complete, non-incremental file list produced by
// WriteFileList. It returns the entries, the populated uid/gid name lists, and
// the i/o error word.
func ReadFileList(r io.Reader, p Params, uidNames, gidNames map[int32]string) ([]*FileEntry, int32, error) {
	c := &codec{params: p, haveLast: false}
	var files []*FileEntry
	var ioErrors int32
	for {
		xflags, err := readFlags(r, p)
		if err != nil {
			return nil, 0, err
		}
		if xflags == 0 {
			break
		}
		// non-varint sentinel terminator carrying the i/o error word
		// (flist.c:2960-2971).
		if xflags == rsync.XMIT_EXTENDED_FLAGS|rsync.XMIT_IO_ERROR_ENDLIST {
			if !p.SafeFlist {
				return nil, 0, fmt.Errorf("invalid flist flag: %x", xflags)
			}
			v, err := protocol.ReadVarint(r)
			if err != nil {
				return nil, 0, err
			}
			ioErrors |= v & ioerrValidMask
			break
		}
		f, err := c.decode(r, xflags)
		if err != nil {
			return nil, 0, err
		}
		files = append(files, f)
	}
	// i/o error word: at protocol >= 30 in varint mode it immediately follows
	// the terminator (C flist.c:2947-2949), before the id lists; below 30 it
	// comes after them. In non-varint mode it only rides the sentinel
	// terminator, already consumed above.
	if p.ProtocolVersion >= 30 && p.VarintFlags {
		v, err := protocol.ReadVarint(r)
		if err != nil {
			return nil, 0, err
		}
		ioErrors |= v & ioerrValidMask
	}
	// trailing id lists
	if p.PreserveUid && atProto30UsesIDList(p) {
		id0 := p.ID0Names && p.ProtocolVersion >= 30
		ids, err := ReadIDList(r, p.ProtocolVersion, id0)
		if err != nil {
			return nil, 0, err
		}
		for k, v := range ids {
			uidNames[k] = v
		}
	}
	if p.PreserveGid && atProto30UsesIDList(p) {
		id0 := p.ID0Names && p.ProtocolVersion >= 30
		ids, err := ReadIDList(r, p.ProtocolVersion, id0)
		if err != nil {
			return nil, 0, err
		}
		for k, v := range ids {
			gidNames[k] = v
		}
	}
	if p.ProtocolVersion < 30 {
		v, err := readInt32(r)
		if err != nil {
			return nil, 0, err
		}
		ioErrors = v
	}
	return files, ioErrors, nil
}

// atProto30UsesIDList reports whether a trailing uid/gid name list follows
// the file entries. With incremental recursion the names ride inline in the
// entries and there is no trailing list at all; with NumericIDs the peer
// asked for numeric-only transmission. This mirrors C's gate on
// send_id_lists (flist.c:2820: numeric_ids <= 0 && !inc_recurse); the
// decision must match between the send and receive halves.
func atProto30UsesIDList(p Params) bool {
	if p.ProtocolVersion >= 30 && p.IncRecurse {
		return false
	}
	return !p.NumericIDs
}

// writeInt16 writes a little-endian 16-bit word (C write_shortint).
func writeInt16(w io.Writer, v uint16) error {
	var b [2]byte
	b[0] = byte(v)
	b[1] = byte(v >> 8)
	_, err := w.Write(b[:])
	return err
}

// readFlags reads the flags word for one entry (or 0 for the terminator),
// handling the varint / extended-shortint / byte framing per protocol.
func readFlags(r io.Reader, p Params) (uint32, error) {
	const ext = rsync.XMIT_EXTENDED_FLAGS
	if p.ProtocolVersion >= 30 && p.VarintFlags {
		// The end of a file list is signaled by a bare 0 varint only. A real
		// entry whose xflags happen to be 0 is sent as the (meaningless-in-varint)
		// XMIT_EXTENDED_FLAGS filler (4) by the encoder, so it must NOT be taken
		// as the terminator here (C flist.c recv_file_list breaks only on 0).
		v, err := protocol.ReadVarint(r)
		if err != nil {
			return 0, err
		}
		if v == 0 {
			return 0, nil
		}
		return uint32(v), nil
	}
	b, err := readByte(r)
	if err != nil {
		return 0, err
	}
	if b == 0 {
		return 0, nil
	}
	flags := uint32(b)
	if p.ProtocolVersion >= 28 && flags&ext != 0 {
		eb, err := readByte(r)
		if err != nil {
			return 0, err
		}
		flags |= uint32(eb) << 8
	}
	return flags, nil
}