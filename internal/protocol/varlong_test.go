package protocol

import (
	"bytes"
	"io"
	"testing"
)

// encodeVLong encodes value with minBytes into a fresh buffer.
func encodeVLong(t *testing.T, value int64, minBytes uint8) []byte {
	t.Helper()
	var buf bytes.Buffer
	if err := WriteVLong(&buf, value, minBytes); err != nil {
		t.Fatalf("WriteVLong(%d,%d): %v", value, minBytes, err)
	}
	return buf.Bytes()
}

func decodeVLong(t *testing.T, b []byte, minBytes uint8) int64 {
	t.Helper()
	v, err := ReadVLong(bytes.NewReader(b), minBytes)
	if err != nil {
		t.Fatalf("ReadVLong(%x,%d): %v", b, minBytes, err)
	}
	return v
}

// TestVLongRoundtrip pins the byte-level algorithm against manual expectations:
// file-length varlong uses min_bytes=3; modtime uses min_bytes=4.
func TestVLongRoundtrip(t *testing.T) {
	cases := []struct {
		value    int64
		minBytes uint8
	}{
		{0, 3}, {1, 3}, {127, 3}, {128, 3},
		{0x123456, 3},
		{0, 4}, {1234567890, 4},
		{1 << 40, 3}, {-1, 3},
	}
	for _, c := range cases {
		enc := encodeVLong(t, c.value, c.minBytes)
		if got := decodeVLong(t, enc, c.minBytes); got != c.value {
			t.Errorf("value %d minBytes %d: got %d want %d (enc %x)", c.value, c.minBytes, got, c.value, enc)
		}
	}
}

// TestVLongGolden checks a couple of concrete byte layouts.
func TestVLongGolden(t *testing.T) {
	// With minBytes=3, value 0 must occupy exactly 3 bytes (00 00 00).
	if got := encodeVLong(t, 0, 3); !bytes.Equal(got, []byte{0x00, 0x00, 0x00}) {
		t.Errorf("vlong(0,3) = %x, want [00 00 00]", got)
	}
	// value 1 with minBytes=3: the two high data bytes stay zero, leaving the
	// value byte at result[0]; tag 0x00, data [01,00,00].
	if got := encodeVLong(t, 1, 3); !bytes.Equal(got, []byte{0x00, 0x01, 0x00}) {
		t.Errorf("vlong(1,3) = %x, want [00 01 00]", got)
	}
}

// TestLongIntGolden validates the pre-30 longint encoding.
func TestLongIntGolden(t *testing.T) {
	var buf bytes.Buffer
	if err := WriteLongInt(&buf, 0x12345678); err != nil {
		t.Fatal(err)
	}
	if want := []byte{0x78, 0x56, 0x34, 0x12}; !bytes.Equal(buf.Bytes(), want) {
		t.Errorf("longint(0x12345678) = %x, want %x", buf.Bytes(), want)
	}
	buf.Reset()
	if err := WriteLongInt(&buf, 1<<40); err != nil {
		t.Fatal(err)
	}
	// 1<<40 > 0x7FFFFFFF => 0xFFFFFFFF prefix then full 8 bytes.
	if want := []byte{0xFF, 0xFF, 0xFF, 0xFF, 0x00, 0x00, 0x00, 0x00, 0x00, 0x01, 0x00, 0x00}; !bytes.Equal(buf.Bytes(), want) {
		t.Errorf("longint(1<<40) = %x, want %x", buf.Bytes(), want)
	}
	v, err := ReadLongInt(bytes.NewReader(buf.Bytes()))
	if err != nil || v != 1<<40 {
		t.Errorf("read longint = %d,%v want %d", v, err, int64(1)<<40)
	}
}

// TestReadVLongTruncated ensures the reader surfaces EOF on truncated input
// rather than panicking.
func TestReadVLongTruncated(t *testing.T) {
	if _, err := ReadVLong(bytes.NewReader([]byte{0x00}), 3); err != io.ErrUnexpectedEOF {
		t.Errorf("truncated vlong: got %v, want io.ErrUnexpectedEOF", err)
	}
}