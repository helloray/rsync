package protocol

import (
	"encoding/binary"
	"io"
)

// NDX sentinel values, mirroring rsync.h:285-288.
const (
	NdxDone       = -1
	NdxFlistEOF   = -2
	NdxDelStats   = -3
	NdxFlistOff   = -101 // NDX_FLIST_OFFSET
)

// NdxCodec encodes/decodes file-list indices (NDX) over the wire. The wire
// format differs by protocol version: < 30 uses a fixed 4-byte little-endian
// integer; >= 30 uses a delta-encoded byte-reduction format.
//
// A codec is stateful: the modern codec tracks the previous positive and
// negative indices to compute deltas, so it must be created fresh and used for
// one uninterrupted direction of a connection.
type NdxCodec struct {
	modern        bool
	prevPositive  int32
	prevNegative  int32
}

// NewNdxCodec returns a codec suitable for the given protocol version.
func NewNdxCodec(version int) *NdxCodec {
	return &NdxCodec{
		modern:        version >= 30,
		prevPositive:  -1,
		prevNegative:  1,
	}
}

// WriteNdx encodes ndx to w using this codec's wire format.
func (c *NdxCodec) WriteNdx(w io.Writer, ndx int32) error {
	if !c.modern {
		return binary.Write(w, binary.LittleEndian, ndx)
	}

	// NDX_DONE (-1) is a single 0x00 byte (io.c:2259-2262).
	if ndx == NdxDone {
		return writeByte(w, 0x00)
	}

	var buf [6]byte
	var cnt int

	var diff, value int32
	if ndx >= 0 {
		diff = ndx - c.prevPositive
		c.prevPositive = ndx
		value = ndx
	} else {
		// All negative indices start with a 0xFF prefix (io.c:2263-2268).
		buf[cnt] = 0xFF
		cnt++
		abs := -ndx
		diff = abs - c.prevNegative
		c.prevNegative = abs
		value = abs
	}

	switch {
	case diff > 0 && diff < 0xFE:
		buf[cnt] = byte(diff) // single-byte short diff (1..=253)
		cnt++
	case diff < 0 || diff >= 0x7FFF:
		// Full value with high-bit tag (io.c:2275-2280).
		buf[cnt] = 0xFE
		cnt++
		buf[cnt] = byte(value>>24) | 0x80
		cnt++
		buf[cnt] = byte(value)
		cnt++
		buf[cnt] = byte(value >> 8)
		cnt++
		buf[cnt] = byte(value >> 16)
		cnt++
	default:
		// Two-byte diff (0xFE..=0x7FFF) (io.c:2281-2284).
		buf[cnt] = 0xFE
		cnt++
		buf[cnt] = byte(diff >> 8)
		cnt++
		buf[cnt] = byte(diff)
		cnt++
	}

	_, err := w.Write(buf[:cnt])
	return err
}

// ReadNdx decodes the next NDX value from r using this codec's wire format.
func (c *NdxCodec) ReadNdx(r io.Reader) (int32, error) {
	if !c.modern {
		var ndx int32
		if err := binary.Read(r, binary.LittleEndian, &ndx); err != nil {
			return 0, err
		}
		return ndx, nil
	}

	lead, err := readByte(r)
	if err != nil {
		return 0, err
	}

	// 0x00: NDX_DONE, no further bytes.
	if lead == 0x00 {
		return NdxDone, nil
	}

	isNegative := lead == 0xFF
	// For a negative index the tag/diff byte follows the 0xFF prefix.
	if isNegative {
		lead, err = readByte(r)
		if err != nil {
			return 0, err
		}
	}

	prev := c.prevPositive
	if isNegative {
		prev = c.prevNegative
	}

	var num int32
	if lead == 0xFE {
		// Extended encoding (io.c:2305-2314).
		b, err := readByte(r)
		if err != nil {
			return 0, err
		}
		if b&0x80 != 0 {
			// 4-byte full value (io.c:2307-2311).
			var rest [3]byte
			if _, err := io.ReadFull(r, rest[:]); err != nil {
				return 0, err
			}
			high := int32(b &^ 0x80)
			num = high<<24 | int32(rest[1])<<8 | int32(rest[2])<<16 | int32(rest[0])
		} else {
			// Two-byte diff (io.c:2312-2314).
			b2, err := readByte(r)
			if err != nil {
				return 0, err
			}
			diff := int32(b)<<8 | int32(b2)
			num = prev + diff
		}
	} else {
		// Single-byte short diff (io.c:2316).
		num = prev + int32(lead)
	}

	if isNegative {
		c.prevNegative = num
		return -num, nil
	}
	c.prevPositive = num
	return num, nil
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