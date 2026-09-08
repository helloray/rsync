package flist

import (
	"encoding/binary"
	"fmt"
	"io"

	"github.com/gokrazy/rsync"
	"github.com/gokrazy/rsync/internal/protocol"
)

// Encoder writes file-list entries one at a time, maintaining the cross-entry
// compression state across calls. Use it from a sender that streams entries
// without buffering the whole list.
type Encoder struct{ c codec }

// NewEncoder returns an Encoder for the given negotiation parameters.
func NewEncoder(p Params) *Encoder { return &Encoder{c: codec{params: p}} }

// Encode writes one entry.
func (e *Encoder) Encode(w io.Writer, f *FileEntry) error { return e.c.Encode(w, f) }

// WriteEndOfFlist writes the end-of-list terminator for a file-list segment,
// mirroring _c-rsync/flist.c:write_end_of_flist. The compression state is
// untouched: the next Encode continues the cross-entry shorthand relative to
// the last entry before the terminator (C keeps lastname in function statics
// across flists).
func (e *Encoder) WriteEndOfFlist(w io.Writer, sendIOError bool, ioErrors int32) error {
	return writeEndOfFlist(w, e.c.params, sendIOError, ioErrors)
}

// Decoder reads file-list entries one at a time, maintaining the cross-entry
// compression state across calls. It returns io.EOF when the end-of-list
// terminator (a zero flags frame) is reached.
type Decoder struct {
	c codec
	// ioErrors accumulates the i/o-error words carried by end-of-list
	// terminators (varint mode or sentinel), mirroring the C receiver's
	// io_error |= err & IOERR_VALID_MASK per flist.
	ioErrors int32
}

// NewDecoder returns a Decoder for the given negotiation parameters.
func NewDecoder(p Params) *Decoder { return &Decoder{c: codec{params: p}} }

// IOError returns the accumulated i/o-error word from all end-of-list
// terminators consumed by Decode so far.
func (d *Decoder) IOError() int32 { return d.ioErrors }

// Decode reads the next entry, returning io.EOF at the end of the list.
func (d *Decoder) Decode(r io.Reader) (*FileEntry, error) {
	xflags, err := readFlags(r, d.c.params)
	if err != nil {
		return nil, err
	}
	if xflags == 0 {
		// varint-mode terminator: the i/o-error word follows and must be
		// consumed here so the caller stays framed for the next segment
		// header (flist.c:2946-2949).
		if d.c.params.ProtocolVersion >= 30 && d.c.params.VarintFlags {
			v, err := protocol.ReadVarint(r)
			if err != nil {
				return nil, err
			}
			d.ioErrors |= v & ioerrValidMask
		}
		return nil, io.EOF
	}
	// non-varint sentinel terminator carrying the i/o error word
	// (flist.c:2960-2971).
	if xflags == rsync.XMIT_EXTENDED_FLAGS|rsync.XMIT_IO_ERROR_ENDLIST {
		if !d.c.params.SafeFlist {
			return nil, fmt.Errorf("invalid flist flag: %x", xflags)
		}
		v, err := protocol.ReadVarint(r)
		if err != nil {
			return nil, err
		}
		d.ioErrors |= v & ioerrValidMask
		return nil, io.EOF
	}
	return d.c.decode(r, xflags)
}

// codec holds the per-direction cross-entry compression state plus the
// negotiation-derived parameters. A codec is stateful: it remembers the
// previous entry's name, mode, modtime, uid, gid, and rdev major so it can
// encode the SAME_* shorthand flags. It must be created fresh for one
// uninterrupted direction of a connection and shared across all entries.
type codec struct {
	params Params

	// compression state (mirrors the static variables in C flist.c)
	lastMode    int32
	lastModTime int32
	lastUid     int32
	lastGid     int32
	lastRdevMaj int32
	lastName    string
	haveLast    bool // no previous entry yet; the SAME_* shorthands cannot fire
}

// Encode writes the wire encoding of f (one file-list entry) to w, updating
// the codec's compression state. It mirrors _c-rsync/flist.c:send_file_entry.
func (c *codec) Encode(w io.Writer, f *FileEntry) error {
	var xflags uint32

	// rsync/flist.c:send_file_entry: FLAG_TOP_DIR rides XMIT_TOP_DIR (same
	// bit value) at every protocol — the pre-29 receiver maps it back to
	// FLAG_TOP_DIR for the per-top-dir delete pass; at >= 30 the bit marks
	// the transfer roots among content dirs (a dir without content would
	// carry XMIT_NO_CONTENT_DIR, which this sender never emits since it
	// sends a segment for every dir).
	if f.isDir() && f.TopDir {
		xflags |= rsync.XMIT_TOP_DIR
	}

	if f.Mode == c.lastMode {
		xflags |= rsync.XMIT_SAME_MODE
	}
	if !c.haveLast {
		xflags &^= rsync.XMIT_SAME_MODE
	}
	if f.ModTime == c.lastModTime {
		xflags |= rsync.XMIT_SAME_TIME
	}
	if !c.haveLast {
		xflags &^= rsync.XMIT_SAME_TIME
	}
	if c.params.PreserveUid && f.Uid == c.lastUid {
		xflags |= rsync.XMIT_SAME_UID
	}
	if !c.haveLast {
		xflags &^= rsync.XMIT_SAME_UID
	}
	if c.params.PreserveGid && f.Gid == c.lastGid {
		xflags |= rsync.XMIT_SAME_GID
	}
	if !c.haveLast {
		xflags &^= rsync.XMIT_SAME_GID
	}

	// device/special rdev. protocol >= 28 splits into major/minor.
	sendRdev := (c.params.PreserveDevices && f.isDevice()) ||
		(c.params.PreserveSpecials && f.isSpecial() && c.params.ProtocolVersion < 31)
	minorIsSmall := f.isSpecial() || f.RdevMinor <= 0xFF
	if sendRdev && c.params.ProtocolVersion >= 28 {
		if c.params.ProtocolVersion >= 30 {
			if f.RdevMajor == c.lastRdevMaj {
				xflags |= rsync.XMIT_SAME_RDEV_MAJOR
			}
		} else {
			// legacy (28-29) marked minor-as-byte separately
			if f.RdevMajor == c.lastRdevMaj {
				xflags |= rsync.XMIT_SAME_RDEV_MAJOR
			}
			if minorIsSmall {
				xflags |= rsync.XMIT_RDEV_MINOR_8_pre30
			}
		}
	}

	// name-prefix compression: l1 = common prefix vs last name (cap 255),
	// l2 = remaining length.
	l1, l2 := nameSplit(f.Name, c.lastName)
	if l1 > 0 {
		xflags |= rsync.XMIT_SAME_NAME
	}
	if l2 > 255 {
		xflags |= rsync.XMIT_LONG_NAME
	}

	// inline uid/gid names (protocol >= 30, incremental recursion only). C sends
	// the name whenever the uid differs from the previous entry's and a name is
	// available (flist.c:558-577) — the first entry too — so no haveLast gate.
	userFollows, groupFollows := false, false
	if c.params.ProtocolVersion >= 30 {
		if c.params.PreserveUid && c.params.IncRecurse && !c.params.NumericIDs &&
			f.Uid != 0 && f.User != "" && f.Uid != c.lastUid {
			userFollows = true
			xflags |= rsync.XMIT_USER_NAME_FOLLOWS
		}
		if c.params.PreserveGid && c.params.IncRecurse && !c.params.NumericIDs &&
			f.Gid != 0 && f.Group != "" && f.Gid != c.lastGid {
			groupFollows = true
			xflags |= rsync.XMIT_GROUP_NAME_FOLLOWS
		}
	}

	// modtime nsec (protocol >= 31).
	modNsec := false
	if c.params.ModNsec && f.ModNsec != 0 {
		xflags |= rsync.XMIT_MOD_NSEC
		modNsec = true
	}

	// 1. flags.
	switch {
	case c.params.VarintFlags:
		// Never emit a zero value (would signal end of list); use the
		// meaningless-in-varint XMIT_EXTENDED_FLAGS bit as the non-zero fill.
		fv := int32(xflags & 0x3FFFF)
		if fv == 0 {
			fv = rsync.XMIT_EXTENDED_FLAGS
		}
		if err := protocol.WriteVarint(w, fv); err != nil {
			return err
		}
	case c.params.ProtocolVersion >= 28:
		x16 := uint16(xflags)
		if !f.isDir() && x16 == 0 {
			x16 |= rsync.XMIT_TOP_DIR
		}
		if x16&0xFF00 != 0 || x16 == 0 {
			x16 |= rsync.XMIT_EXTENDED_FLAGS
			if err := binary.Write(w, binary.LittleEndian, x16); err != nil {
				return err
			}
		} else {
			if err := writeByte(w, byte(x16)); err != nil {
				return err
			}
		}
	default:
		// A zero flags byte terminates the list, so a zero-flag entry needs a
		// filler: dirs use XMIT_LONG_NAME, non-dirs XMIT_TOP_DIR. The filler
		// must land in xflags (not just the written byte) so the LONG_NAME
		// name-length framing below stays in sync with what the peer reads.
		if xflags == 0 {
			if f.isDir() {
				xflags = rsync.XMIT_LONG_NAME
			} else {
				xflags = rsync.XMIT_TOP_DIR
			}
		}
		if err := writeByte(w, byte(xflags)); err != nil {
			return err
		}
	}

	// 2-4. name.
	if xflags&rsync.XMIT_SAME_NAME != 0 {
		if err := writeByte(w, byte(l1)); err != nil {
			return err
		}
	}
	if err := c.writeNameLen(w, int32(l2), xflags&rsync.XMIT_LONG_NAME != 0); err != nil {
		return err
	}
	if _, err := io.WriteString(w, f.Name[l1:]); err != nil {
		return err
	}

	// 5. file length: write_varlong30 — a varlong at protocol >= 30, the
	// fixed-width longint below 30 (rsync/io.h:write_varlong30).
	if c.params.ProtocolVersion >= 30 {
		if err := protocol.WriteVLong30(w, f.Length, 3); err != nil {
			return err
		}
	} else if err := protocol.WriteLongInt(w, f.Length); err != nil {
		return err
	}

	// 6. modtime.
	if xflags&rsync.XMIT_SAME_TIME == 0 {
		if c.params.ProtocolVersion >= 30 {
			if err := protocol.WriteVLong(w, int64(f.ModTime), 4); err != nil {
				return err
			}
		} else if err := writeInt32(w, f.ModTime); err != nil {
			return err
		}
	}
	if modNsec {
		if err := protocol.WriteVarint(w, f.ModNsec); err != nil {
			return err
		}
	}

	// 7. mode.
	if xflags&rsync.XMIT_SAME_MODE == 0 {
		if err := writeInt32(w, f.Mode); err != nil {
			return err
		}
	}

	// 8-9. uid/gid (+ inline names at >=30).
	if c.params.PreserveUid && xflags&rsync.XMIT_SAME_UID == 0 {
		if c.params.ProtocolVersion < 30 {
			if err := writeInt32(w, f.Uid); err != nil {
				return err
			}
		} else {
			if err := protocol.WriteVarint(w, f.Uid); err != nil {
				return err
			}
			if userFollows {
				if err := writeByte(w, byte(len(f.User))); err != nil {
					return err
				}
				if _, err := io.WriteString(w, f.User); err != nil {
					return err
				}
			}
		}
	}
	if c.params.PreserveGid && xflags&rsync.XMIT_SAME_GID == 0 {
		if c.params.ProtocolVersion < 30 {
			if err := writeInt32(w, f.Gid); err != nil {
				return err
			}
		} else {
			if err := protocol.WriteVarint(w, f.Gid); err != nil {
				return err
			}
			if groupFollows {
				if err := writeByte(w, byte(len(f.Group))); err != nil {
					return err
				}
				if _, err := io.WriteString(w, f.Group); err != nil {
					return err
				}
			}
		}
	}

	// 10. device/special rdev.
	if sendRdev {
		switch {
		case c.params.ProtocolVersion < 28:
			if xflags&rsync.XMIT_SAME_RDEV_pre28 == 0 {
				if err := writeInt32(w, f.Rdev()); err != nil {
					return err
				}
			}
		default:
			if xflags&rsync.XMIT_SAME_RDEV_MAJOR == 0 {
				if c.params.ProtocolVersion >= 30 {
					if err := protocol.WriteVarint(w, f.RdevMajor); err != nil {
						return err
					}
				} else if err := writeInt32(w, f.RdevMajor); err != nil {
					return err
				}
			}
			if c.params.ProtocolVersion >= 30 {
				if err := protocol.WriteVarint(w, f.RdevMinor); err != nil {
					return err
				}
			} else if xflags&rsync.XMIT_RDEV_MINOR_8_pre30 != 0 {
				if err := writeByte(w, byte(f.RdevMinor)); err != nil {
					return err
				}
			} else if err := writeInt32(w, f.RdevMinor); err != nil {
				return err
			}
		}
	}

	// 11-12. symlink target (varint30 length at >=30).
	if c.params.PreserveLinks && f.isLink() {
		if c.params.ProtocolVersion >= 30 {
			if err := protocol.WriteVarint(w, int32(len(f.LinkTarget))); err != nil {
				return err
			}
		} else if err := writeInt32(w, int32(len(f.LinkTarget))); err != nil {
			return err
		}
		if _, err := io.WriteString(w, f.LinkTarget); err != nil {
			return err
		}
	}

	// whole-file checksum (regular files only at >=28).
	if c.params.AlwaysChecksum && (f.isReg() || c.params.ProtocolVersion < 28) {
		if _, err := w.Write(f.Checksum[:]); err != nil {
			return err
		}
	}

	// commit state
	c.lastMode = f.Mode
	c.lastModTime = f.ModTime
	c.lastUid = f.Uid
	c.lastGid = f.Gid
	if sendRdev {
		c.lastRdevMaj = f.RdevMajor
	}
	c.lastName = f.Name
	c.haveLast = true
	return nil
}

// writeNameLen writes the remaining-name length, framed by whether LONG_NAME is
// set (C: write_varint30 at >=30 else write_int; otherwise a single byte).
func (c *codec) writeNameLen(w io.Writer, l2 int32, long bool) error {
	if !long {
		return writeByte(w, byte(l2))
	}
	if c.params.ProtocolVersion >= 30 {
		return protocol.WriteVarint(w, l2)
	}
	return writeInt32(w, l2)
}

// decode reads one file-list entry from r into f, updating the codec state.
// It mirrors _c-rsync/flist.c:recv_file_entry. xflags is the already-read flags
// word for this entry (framing handled by the caller / ReadFiles).
func (c *codec) decode(r io.Reader, xflags uint32) (*FileEntry, error) {
	f := &FileEntry{}

	var l1 int
	if xflags&rsync.XMIT_SAME_NAME != 0 {
		b, err := readByte(r)
		if err != nil {
			return nil, err
		}
		l1 = int(b)
	}

	var l2 int
	if xflags&rsync.XMIT_LONG_NAME != 0 {
		if c.params.ProtocolVersion >= 30 {
			v, err := protocol.ReadVarint(r)
			if err != nil {
				return nil, err
			}
			l2 = int(v)
		} else {
			v, err := readInt32(r)
			if err != nil {
				return nil, err
			}
			l2 = int(v)
		}
	} else {
		b, err := readByte(r)
		if err != nil {
			return nil, err
		}
		l2 = int(b)
	}

	const pathMax = 4096
	if l2 >= pathMax-l1 {
		return nil, fmt.Errorf("overflow: xflags=0x%x l1=%d l2=%d lastname=%s", xflags, l1, l2, c.lastName)
	}
	buf := make([]byte, l1+l2)
	readb := buf
	if l1 > 0 {
		copy(buf, c.lastName)
		readb = buf[l1:]
	}
	if _, err := io.ReadFull(r, readb); err != nil {
		return nil, err
	}
	f.Name = string(buf)

	// 5. file length: read_varlong30 — see Encode.
	var length int64
	var err error
	if c.params.ProtocolVersion >= 30 {
		length, err = protocol.ReadVLong30(r, 3)
	} else {
		length, err = protocol.ReadLongInt(r)
	}
	if err != nil {
		return nil, err
	}
	f.Length = length

	switch {
	case xflags&rsync.XMIT_SAME_TIME != 0:
		f.ModTime = c.lastModTime
	case c.params.ProtocolVersion >= 30:
		v, err := protocol.ReadVLong(r, 4)
		if err != nil {
			return nil, err
		}
		f.ModTime = int32(v)
	default:
		v, err := readInt32(r)
		if err != nil {
			return nil, err
		}
		f.ModTime = v
	}

	if xflags&rsync.XMIT_MOD_NSEC != 0 {
		v, err := protocol.ReadVarint(r)
		if err != nil {
			return nil, err
		}
		f.ModNsec = v
	}

	switch {
	case xflags&rsync.XMIT_SAME_MODE != 0:
		f.Mode = c.lastMode
	default:
		v, err := readInt32(r)
		if err != nil {
			return nil, err
		}
		f.Mode = v
	}

	if c.params.PreserveUid {
		if xflags&rsync.XMIT_SAME_UID != 0 {
			f.Uid = c.lastUid
		} else if c.params.ProtocolVersion < 30 {
			v, err := readInt32(r)
			if err != nil {
				return nil, err
			}
			f.Uid = v
		} else {
			v, err := protocol.ReadVarint(r)
			if err != nil {
				return nil, err
			}
			f.Uid = v
			if xflags&rsync.XMIT_USER_NAME_FOLLOWS != 0 {
				l, err := readByte(r)
				if err != nil {
					return nil, err
				}
				nm := make([]byte, int(l))
				if _, err := io.ReadFull(r, nm); err != nil {
					return nil, err
				}
				f.User = string(nm)
			}
		}
	}
	if c.params.PreserveGid {
		if xflags&rsync.XMIT_SAME_GID != 0 {
			f.Gid = c.lastGid
		} else if c.params.ProtocolVersion < 30 {
			v, err := readInt32(r)
			if err != nil {
				return nil, err
			}
			f.Gid = v
		} else {
			v, err := protocol.ReadVarint(r)
			if err != nil {
				return nil, err
			}
			f.Gid = v
			if xflags&rsync.XMIT_GROUP_NAME_FOLLOWS != 0 {
				l, err := readByte(r)
				if err != nil {
					return nil, err
				}
				nm := make([]byte, int(l))
				if _, err := io.ReadFull(r, nm); err != nil {
					return nil, err
				}
				f.Group = string(nm)
			}
		}
	}

	isDev := f.isDevice()
	isSpecial := f.isSpecial()
	sendRdev := (c.params.PreserveDevices && isDev) ||
		(c.params.PreserveSpecials && isSpecial && c.params.ProtocolVersion < 31)
	if sendRdev {
		switch {
		case c.params.ProtocolVersion < 28:
			var v int32
			if xflags&rsync.XMIT_SAME_RDEV_pre28 == 0 {
				vv, err := readInt32(r)
				if err != nil {
					return nil, err
				}
				v = vv
			} else {
				v = c.lastRdevMaj<<0 // not populated legacy; keep zero
			}
			maj, min := SplitRdev(v)
			f.RdevMajor, f.RdevMinor = maj, min
			c.lastRdevMaj = maj
		default:
			if xflags&rsync.XMIT_SAME_RDEV_MAJOR != 0 {
				f.RdevMajor = c.lastRdevMaj
			} else {
				v, err := c.readRdevMajor(r)
				if err != nil {
					return nil, err
				}
				f.RdevMajor = v
			}
			var minor int32
			switch {
			case c.params.ProtocolVersion >= 30:
				v, err := protocol.ReadVarint(r)
				if err != nil {
					return nil, err
				}
				minor = v
			case xflags&rsync.XMIT_RDEV_MINOR_8_pre30 != 0:
				b, err := readByte(r)
				if err != nil {
					return nil, err
				}
				minor = int32(b)
			default:
				v, err := readInt32(r)
				if err != nil {
					return nil, err
				}
				minor = v
			}
			f.RdevMinor = minor
			c.lastRdevMaj = f.RdevMajor
		}
		if isDev {
			// rsync forces device length to zero (the length frame is still
			// consumed, but the value is unused).
			f.Length = 0
		}
	}

	if c.params.PreserveLinks && f.isLink() {
		var linkLen int32
		if c.params.ProtocolVersion >= 30 {
			v, err := protocol.ReadVarint(r)
			if err != nil {
				return nil, err
			}
			linkLen = v
		} else {
			v, err := readInt32(r)
			if err != nil {
				return nil, err
			}
			linkLen = v
		}
		if linkLen < 0 || linkLen > pathMax {
			return nil, fmt.Errorf("overflow: linkname_len=%d", linkLen)
		}
		lb := make([]byte, int(linkLen))
		if _, err := io.ReadFull(r, lb); err != nil {
			return nil, err
		}
		f.LinkTarget = string(lb)
	}

	if c.params.AlwaysChecksum && (f.isReg() || c.params.ProtocolVersion < 28) {
		if _, err := io.ReadFull(r, f.Checksum[:]); err != nil {
			return nil, err
		}
	}

	// rsync/flist.c:recv_file_entry: below protocol 30, XMIT_TOP_DIR maps
	// back to FLAG_TOP_DIR on the decoded entry (see Encode). Non-dirs can
	// carry the bit as a zero-flags filler; C's generator ignores it there.
	if c.params.ProtocolVersion < 30 && f.isDir() && xflags&rsync.XMIT_TOP_DIR != 0 {
		f.TopDir = true
	}

	c.lastMode = f.Mode
	c.lastModTime = f.ModTime
	c.lastUid = f.Uid
	c.lastGid = f.Gid
	c.lastName = f.Name
	c.haveLast = true
	return f, nil
}

// readRdevMajor reads a device major number in the framing of the protocol.
func (c *codec) readRdevMajor(r io.Reader) (int32, error) {
	if c.params.ProtocolVersion >= 30 {
		return protocol.ReadVarint(r)
	}
	return readInt32(r)
}

// SplitRdev decomposes a composed device number (glibc makedev layout) into
// major/minor, inverting FileEntry.Rdev.
func SplitRdev(rdev int32) (major, minor int32) {
	u := uint32(rdev)
	major = int32(u>>8) & 0xfff
	minor = int32(u)&0xff | int32(u>>12)&^0xff
	return major, minor
}

func writeByte(w io.Writer, b byte) error {
	_, err := w.Write([]byte{b})
	return err
}

func readByte(r io.Reader) (byte, error) {
	var b [1]byte
	if _, err := io.ReadFull(r, b[:]); err != nil {
		return 0, err
	}
	return b[0], nil
}

func readInt32(r io.Reader) (int32, error) {
	var v int32
	if err := binary.Read(r, binary.LittleEndian, &v); err != nil {
		return 0, err
	}
	return v, nil
}

func writeInt32(w io.Writer, v int32) error { return binary.Write(w, binary.LittleEndian, v) }

// nameSplit computes the common-prefix length (capped 255) and the remaining
// length of name relative to last.
func nameSplit(name, last string) (l1, l2 int) {
	limit := len(name)
	if len(last) < limit {
		limit = len(last)
	}
	l1 = 0
	for l1 < limit && l1 < 255 && name[l1] == last[l1] {
		l1++
	}
	return l1, len(name) - l1
}