package protocol

import (
	"bytes"
	"testing"
)

// modernEncode encodes vals with a fresh modern codec (protocol 30).
func modernEncode(t *testing.T, vals ...int32) []byte {
	t.Helper()
	c := NewNdxCodec(30)
	var buf bytes.Buffer
	for _, v := range vals {
		if err := c.WriteNdx(&buf, v); err != nil {
			t.Fatalf("WriteNdx(%d): %v", v, err)
		}
	}
	return buf.Bytes()
}

func TestModernNdxWriteGolden(t *testing.T) {
	// Sequential 0..9: every index is diff 1 from the previous => all 0x01.
	got := modernEncode(t, 0, 1, 2, 3, 4, 5, 6, 7, 8, 9)
	if want := bytes.Repeat([]byte{0x01}, 10); !bytes.Equal(got, want) {
		t.Errorf("0..9 = %x, want %x", got, want)
	}

	// NDX_DONE is a single 0x00 byte.
	if got := modernEncode(t, NdxDone); !bytes.Equal(got, []byte{0x00}) {
		t.Errorf("NDX_DONE = %x, want 00", got)
	}

	// NDX_FLIST_EOF (-2): 0xFF prefix; fresh codec abs=2, diff=2-1=1 => [FF 01].
	if got := modernEncode(t, NdxFlistEOF); !bytes.Equal(got, []byte{0xFF, 0x01}) {
		t.Errorf("NDX_FLIST_EOF = %x, want [FF 01]", got)
	}

	// First index 0: diff 1 from prev -1 => single byte [01].
	if got := modernEncode(t, 0); !bytes.Equal(got, []byte{0x01}) {
		t.Errorf("ndx 0 = %x, want [01]", got)
	}
}

func TestModernNdxRoundtrip(t *testing.T) {
	sequences := [][]int32{
		{0, 1, 2, 3, 4, 5, 6, 7, 8, 9},
		{NdxDone},
		{0, NdxDone},
		{NdxFlistEOF, NdxDelStats, NdxFlistOff},
		{0, 253, 254, 32767, 32768},
		{1 << 20, 1 << 24, 0x12345678},
		{0, NdxDelStats, 3, NdxDone},
	}
	for _, vals := range sequences {
		enc := NewNdxCodec(30)
		var buf bytes.Buffer
		for _, v := range vals {
			if err := enc.WriteNdx(&buf, v); err != nil {
				t.Fatalf("WriteNdx(%d): %v", v, err)
			}
		}
		dec := NewNdxCodec(30)
		r := bytes.NewReader(buf.Bytes())
		for _, want := range vals {
			got, err := dec.ReadNdx(r)
			if err != nil {
				t.Fatalf("ReadNdx (seq %v): %v", vals, err)
			}
			if got != want {
				t.Fatalf("ReadNdx seq %v: got %d want %d", vals, got, want)
			}
		}
	}
}

func TestLegacyNdxGolden(t *testing.T) {
	cases := []struct {
		ndx  int32
		want []byte
	}{
		{0, []byte{0x00, 0x00, 0x00, 0x00}},
		{0x12345678, []byte{0x78, 0x56, 0x34, 0x12}},
		{NdxDone, []byte{0xFF, 0xFF, 0xFF, 0xFF}},
		{NdxFlistEOF, []byte{0xFE, 0xFF, 0xFF, 0xFF}},
	}
	for _, c := range cases {
		codec := NewNdxCodec(29)
		var buf bytes.Buffer
		if err := codec.WriteNdx(&buf, c.ndx); err != nil {
			t.Fatalf("WriteNdx(%d): %v", c.ndx, err)
		}
		if !bytes.Equal(buf.Bytes(), c.want) {
			t.Errorf("legacy ndx %d = %x, want %x", c.ndx, buf.Bytes(), c.want)
		}
		got, err := codec.ReadNdx(bytes.NewReader(c.want))
		if err != nil || got != c.ndx {
			t.Errorf("legacy read %v = %d,%v want %d", c.want, got, err, c.ndx)
		}
	}
}

// TestModernNdxLargeBoundaries exercises the 2-byte (0xFE) diff form and the
// full 4-byte form by encoding values that force each branch.
func TestModernNdxLargeBoundaries(t *testing.T) {
	cases := [][]int32{
		{0, 253, 254},          // 254 is diff 254 => 2-byte form
		{0, 254, 32767, 32768}, // diff beyond 0x7FFF => full form
		{0, 1 << 24, 1 << 30},  // forces full 4-byte form
	}
	for _, vals := range cases {
		enc := NewNdxCodec(30)
		var buf bytes.Buffer
		for _, v := range vals {
			if err := enc.WriteNdx(&buf, v); err != nil {
				t.Fatalf("WriteNdx(%d): %v", v, err)
			}
		}
		dec := NewNdxCodec(30)
		r := bytes.NewReader(buf.Bytes())
		for _, want := range vals {
			got, err := dec.ReadNdx(r)
			if err != nil || got != want {
				t.Fatalf("seq %v: ReadNdx = %d,%v want %d", vals, got, err, want)
			}
		}
	}
}