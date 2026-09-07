package protocol

import (
	"bytes"
	"testing"
)

// Golden vectors transcribed from oc-rsync
// crates/protocol/tests/golden_handshakes.rs (verified against rsync.c io.c).
func TestWriteVarintGolden(t *testing.T) {
	tests := []struct {
		v    int32
		want []byte
	}{
		{0, []byte{0x00}},
		{1, []byte{0x01}},
		{127, []byte{0x7F}},
		{128, []byte{0x80, 0x80}},
		{255, []byte{0x80, 0xFF}},
		{256, []byte{0x81, 0x00}},
		{16383, []byte{0xBF, 0xFF}},
		{16384, []byte{0xC0, 0x00, 0x40}},
		{1_073_741_824, []byte{0xF0, 0x00, 0x00, 0x00, 0x40}},
		{-1, []byte{0xF0, 0xFF, 0xFF, 0xFF, 0xFF}},
		{-128, []byte{0xF0, 0x80, 0xFF, 0xFF, 0xFF}},
	}
	for _, tc := range tests {
		var buf bytes.Buffer
		if err := WriteVarint(&buf, tc.v); err != nil {
			t.Fatalf("WriteVarint(%d): %v", tc.v, err)
		}
		if !bytes.Equal(buf.Bytes(), tc.want) {
			t.Errorf("WriteVarint(%d) = %x, want %x", tc.v, buf.Bytes(), tc.want)
		}
		if got, err := ReadVarint(bytes.NewReader(buf.Bytes())); err != nil || got != tc.v {
			t.Errorf("ReadVarint(%x) = %d, %v; want %d", buf.Bytes(), got, err, tc.v)
		}
	}
}

func TestWriteVarintSequence(t *testing.T) {
	// Transcribed from golden_varint_sequence.
	values := []int32{0, 1, 127, 128, 255, 16384, -1}
	want := []byte{
		0x00, 0x01, 0x7F,
		0x80, 0x80,
		0x80, 0xFF,
		0xC0, 0x00, 0x40,
		0xF0, 0xFF, 0xFF, 0xFF, 0xFF,
	}
	var buf bytes.Buffer
	for _, v := range values {
		if err := WriteVarint(&buf, v); err != nil {
			t.Fatalf("WriteVarint(%d): %v", v, err)
		}
	}
	if !bytes.Equal(buf.Bytes(), want) {
		t.Fatalf("sequence = %x, want %x", buf.Bytes(), want)
	}
	r := bytes.NewReader(buf.Bytes())
	for _, v := range values {
		if got, err := ReadVarint(r); err != nil || got != v {
			t.Errorf("ReadVarint seq got %d, %v; want %d", got, err, v)
		}
	}
}