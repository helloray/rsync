package protocol

import (
	"encoding/binary"
	"fmt"
	"io"
)

// This file ports rsync/io.c:write_varint and read_varint. rsync's variable
// length integer encoding is NOT the same as Google's protobuf varint: the
// first byte begins with (N-1) leading 1-bits terminating in a 0, which
// encodes the total byte count N. The remaining low bits of the first byte
// hold the most significant data bits, and the following N-1 bytes hold the
// value's low bytes in little-endian order.
//
//	value range        N   first byte prefix   data bits
//	[0x00, 0x7F]       1   (none)              7
//	[0x80, 0x3FFF]     2   10                  6
//	[0x4000, 0x1FFFFF] 3   110                 5
//	[0x200000, 0x0FFF..] 4   1110             4
//	else (> 0x0FFFFFFF)  5   11110             3
//
// Negative values are encoded as their two's complement uint32, so they
// always occupy the full five-byte form.

// WriteVarint writes v to w using rsync's variable-length integer encoding.
func WriteVarint(w io.Writer, v int32) error {
	u := uint64(uint32(v))
	var n, shift int
	switch {
	case u < 0x80:
		_, err := w.Write([]byte{byte(u)})
		return err
	case u < 0x4000:
		n, shift = 2, 8
	case u < 0x200000:
		n, shift = 3, 16
	case u < 0x10000000:
		n, shift = 4, 24
	default:
		n, shift = 5, 32
	}
	// In the first byte, the (n-1) leading 1-bits plus a 0 form the count
	// prefix: ((1<<n)-2) shifted up by (8-n) data bits.
	leading := byte(u>>shift) | byte((1<<uint(n))-2)<<uint(8-n)
	buf := [5]byte{leading}
	for i := 0; i < n-1; i++ {
		buf[1+i] = byte(u >> uint(8*i))
	}
	_, err := w.Write(buf[:n])
	return err
}

// ReadVarint reads a variable-length integer encoded by WriteVarint.
func ReadVarint(r io.Reader) (int32, error) {
	var first [1]byte
	if _, err := io.ReadFull(r, first[:]); err != nil {
		return 0, err
	}
	b := first[0]
	var n int
	switch {
	case b < 0x80:
		return int32(b), nil
	case b < 0xC0:
		n = 2
	case b < 0xE0:
		n = 3
	case b < 0xF0:
		n = 4
	default:
		n = 5
	}
	dataMask := byte(1)<<uint(8-n) - 1
	v := uint64(b&dataMask) << uint(8*(n-1))
	buf := make([]byte, n-1)
	if _, err := io.ReadFull(r, buf); err != nil {
		return 0, err
	}
	for i := 0; i < n-1; i++ {
		v |= uint64(buf[i]) << uint(8*i)
	}
	return int32(uint32(v)), nil
}

// WriteVLong writes a variable-length 64-bit integer, mirroring
// _c-rsync/io.c:write_varlong (and oc-rsync encode.rs:write_varlong). The value
// is packed into the minimum number of bytes (at least min_bytes), with a
// leading tag byte that indicates how many bytes follow. Typical min_bytes
// values: 3 for file lengths (write_varlong30 F_LENGTH), 4 for modtimes.
func WriteVLong(w io.Writer, value int64, minBytes uint8) error {
	le := [8]byte(leBytes(value))
	cnt := 8
	for cnt > int(minBytes) && le[cnt-1] == 0 {
		cnt--
	}
	bit := byte(1) << uint((7+int(minBytes))-cnt)
	var leading byte
	switch {
	case le[cnt-1] >= bit:
		cnt++
		leading = ^(bit - 1)
	case cnt > int(minBytes):
		leading = le[cnt-1] | ^(bit*2 - 1)
	default:
		leading = le[cnt-1]
	}
	buf := make([]byte, 1+cnt-1)
	buf[0] = leading
	copy(buf[1:], le[:cnt-1])
	_, err := w.Write(buf)
	return err
}

// ReadVLong reads a variable-length 64-bit integer encoded by WriteVLong,
// mirroring _c-rsync/io.c:read_varlong (oc-rsync decode.rs:read_varlong).
func ReadVLong(r io.Reader, minBytes uint8) (int64, error) {
	if minBytes == 0 || minBytes > 8 {
		return 0, fmt.Errorf("invalid min_bytes in ReadVLong: %d", minBytes)
	}
	min := int(minBytes)
	initial := make([]byte, min)
	if _, err := io.ReadFull(r, initial); err != nil {
		return 0, err
	}
	leading := initial[0]

	// Place initial data bytes (after the leading tag) at result[0..min-1]; the
	// result spans 8 data bytes, matching upstream's 9-byte union.
	var result [9]byte
	copy(result[:min-1], initial[1:])

	extra := int(intByteExtra[leading>>2])
	if extra > 0 {
		if min+extra > 9 {
			return 0, fmt.Errorf("overflow in ReadVLong")
		}
		bit := byte(1) << uint(8-extra)
		if _, err := io.ReadFull(r, result[min-1:min-1+extra]); err != nil {
			return 0, err
		}
		result[min+extra-1] = leading & (bit - 1)
	} else {
		result[min-1] = leading
	}
	return int64(binary.LittleEndian.Uint64(result[:8])), nil
}

// WriteLongInt writes a 64-bit integer in the pre-30 "longint" encoding
// (io.c:write_longint): a 4-byte LE int when it fits 0..0x7FFFFFFF, otherwise
// 0xFFFFFFFF followed by the full 8 bytes.
func WriteLongInt(w io.Writer, value int64) error {
	if value >= 0 && value <= 0x7FFFFFFF {
		return binary.Write(w, binary.LittleEndian, int32(value))
	}
	if err := binary.Write(w, binary.LittleEndian, int32(-1)); err != nil {
		return err
	}
	return binary.Write(w, binary.LittleEndian, value)
}

// ReadLongInt reads a pre-30 "longint" (see WriteLongInt).
func ReadLongInt(r io.Reader) (int64, error) {
	var first int32
	if err := binary.Read(r, binary.LittleEndian, &first); err != nil {
		return 0, err
	}
	if first == -1 {
		var v int64
		if err := binary.Read(r, binary.LittleEndian, &v); err != nil {
			return 0, err
		}
		return v, nil
	}
	return int64(first), nil
}

// WriteVLong30 / ReadVLong30 are the protocol >= 30 varlong forms: they are
// identical to WriteVLong/ReadVLong, exposed for callers keyed by protocol.
func WriteVLong30(w io.Writer, value int64, minBytes uint8) error {
	return WriteVLong(w, value, minBytes)
}

func ReadVLong30(r io.Reader, minBytes uint8) (int64, error) {
	return ReadVLong(r, minBytes)
}

// intByteExtra mirrors upstream's INT_BYTE_EXTRA table, indexed by firstByte>>2
// (6 bits) and giving the number of extra wire bytes that follow the tag byte.
// Values 0..=6: 0x00-0x03 -> 0, ... 0xFC-0xFF -> 6 (the 5-byte full form needs
// 4 extra bytes and the table saturates).
// intByteExtra mirrors upstream's int_byte_extra table (io.c; via
// oc-rsync varint/table.rs), indexed by leadingByte>>2 (6 bits) and giving the
// number of extra wire bytes that follow the tag byte.
var intByteExtra = [64]uint8{
	0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, // (0x00-0x3F) >> 2
	0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, // (0x40-0x7F) >> 2
	1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, // (0x80-0xBF) >> 2
	2, 2, 2, 2, 2, 2, 2, 2, 3, 3, 3, 3, 4, 4, 5, 6, // (0xC0-0xFF) >> 2
}

func leBytes(v int64) [8]byte {
	return [8]byte{
		byte(v), byte(v >> 8), byte(v >> 16), byte(v >> 24),
		byte(v >> 32), byte(v >> 40), byte(v >> 48), byte(v >> 56),
	}
}