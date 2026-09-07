package protocol

import (
	"bytes"
	"testing"
)

// Golden vectors transcribed from oc-rsync
// crates/protocol/tests/golden_handshakes.rs.
func TestCompatibilityFlagsGolden(t *testing.T) {
	tests := []struct {
		name  string
		flags CompatibilityFlags
		want  []byte
	}{
		{"empty", 0, []byte{0x00}},
		{"inc_recurse", CFIncRecurse, []byte{0x01}},
		{"safe_flist", CFSafeFlist, []byte{0x08}},
		{
			"typical_server",
			CFIncRecurse | CFSymlinkTimes | CFSafeFlist | CFChksumSeedFix, // 43
			[]byte{0x2B},
		},
		{
			"full_modern",
			CFIncRecurse | CFSymlinkTimes | CFSafeFlist | CFChksumSeedFix |
				CFVarintFlistFlags | CFId0Names, // 0x1AB = 427
			[]byte{0x81, 0xAB},
		},
		{"all_known", CFAllKnown, []byte{0x81, 0xFF}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var buf bytes.Buffer
			if err := tc.flags.WriteVarint(&buf); err != nil {
				t.Fatalf("WriteVarint: %v", err)
			}
			if !bytes.Equal(buf.Bytes(), tc.want) {
				t.Errorf("encode = %x, want %x", buf.Bytes(), tc.want)
			}
			got, err := ReadCompatibilityFlags(bytes.NewReader(buf.Bytes()))
			if err != nil {
				t.Fatalf("Read: %v", err)
			}
			if got != tc.flags {
				t.Errorf("decode = %#x, want %#x", got.Bits(), tc.flags.Bits())
			}
		})
	}
}

func TestCompatibilityFlagsBits(t *testing.T) {
	if got := (CFIncRecurse | CFSafeFlist).Bits(); got != 0b1001 {
		t.Errorf("bits = %#x, want 9", got)
	}
	if CFAllKnown.Bits() != 0x1FF {
		t.Errorf("all_known = %#x, want 0x1FF", CFAllKnown.Bits())
	}
	if !(CFIncRecurse | CFSafeFlist).Contains(CFSafeFlist) {
		t.Error("Contains failed")
	}
	if (CFIncRecurse).Contains(CFSafeFlist) {
		t.Error("Contains must reject unset bits")
	}
}