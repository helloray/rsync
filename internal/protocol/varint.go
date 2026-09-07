package protocol

import (
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

const varintMaxAllowed = uint32(0xFFFFFFFF)

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