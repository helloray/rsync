package flist

import (
	"bytes"
	"io"
	"reflect"
	"testing"
)

// TestSegmentTerminatorVarint checks the varint-mode end-of-list terminator
// (zero varint + i/o-error word) round trips through the streaming Decoder,
// which must consume the i/o word to stay framed for a following segment.
func TestSegmentTerminatorVarint(t *testing.T) {
	p := baseParams(30) // VarintFlags = true
	enc := NewEncoder(p)
	dec := NewDecoder(p)

	files := []*FileEntry{
		reg("one.txt"),
		reg("two.txt"),
	}
	var buf bytes.Buffer
	for _, f := range files {
		if err := enc.Encode(&buf, f); err != nil {
			t.Fatalf("encode: %v", err)
		}
	}
	if err := enc.WriteEndOfFlist(&buf, true, 3); err != nil {
		t.Fatalf("WriteEndOfFlist: %v", err)
	}

	var got []*FileEntry
	for {
		f, err := dec.Decode(&buf)
		if err == io.EOF {
			break
		} else if err != nil {
			t.Fatalf("decode: %v", err)
		}
		got = append(got, f)
	}
	if len(got) != len(files) {
		t.Fatalf("got %d entries, want %d", len(got), len(files))
	}
	if !reflect.DeepEqual(files, got) {
		t.Errorf("entries:\n want %+v\n  got %+v", files, got)
	}
	if got := dec.IOError(); got != 3 {
		t.Errorf("IOError = %d, want 3", got)
	}
}

// TestSegmentTerminatorSentinel checks the non-varint sentinel terminator
// (XMIT_EXTENDED_FLAGS|XMIT_IO_ERROR_ENDLIST + varint i/o word), permitted
// when SafeFlist is set.
func TestSegmentTerminatorSentinel(t *testing.T) {
	p := baseParams(30)
	p.VarintFlags = false
	p.SafeFlist = true
	enc := NewEncoder(p)
	dec := NewDecoder(p)

	var buf bytes.Buffer
	if err := enc.Encode(&buf, reg("a.txt")); err != nil {
		t.Fatalf("encode: %v", err)
	}
	if err := enc.WriteEndOfFlist(&buf, true, 2); err != nil {
		t.Fatalf("WriteEndOfFlist: %v", err)
	}

	if _, err := dec.Decode(&buf); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if _, err := dec.Decode(&buf); err != io.EOF {
		t.Fatalf("decode = %v, want io.EOF", err)
	}
	if got := dec.IOError(); got != 2 {
		t.Errorf("IOError = %d, want 2", got)
	}
}

// TestSegmentTerminatorPlainByte checks the plain zero-byte terminator in
// non-varint mode without SafeFlist.
func TestSegmentTerminatorPlainByte(t *testing.T) {
	p := baseParams(29) // non-varint, no SafeFlist
	enc := NewEncoder(p)
	dec := NewDecoder(p)

	var buf bytes.Buffer
	if err := enc.Encode(&buf, reg("a.txt")); err != nil {
		t.Fatalf("encode: %v", err)
	}
	if err := enc.WriteEndOfFlist(&buf, true, 9); err != nil {
		t.Fatalf("WriteEndOfFlist: %v", err)
	}
	// without SafeFlist the io error cannot ride the terminator: it must be
	// the plain zero byte.
	b := buf.Bytes()
	if n := len(b); n < 1 || b[n-1] != 0 {
		t.Fatalf("terminator must end in a zero byte, got %v", b[max(0, n-3):])
	}

	if _, err := dec.Decode(&buf); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if _, err := dec.Decode(&buf); err != io.EOF {
		t.Fatalf("decode = %v, want io.EOF", err)
	}
	if got := dec.IOError(); got != 0 {
		t.Errorf("IOError = %d, want 0", got)
	}
}

// TestCrossSegmentCompression checks that the name-prefix (and mode/time/uid/gid)
// shorthands keep referring to the last entry before a terminator across
// segment boundaries, both ways (C keeps lastname in function statics across
// flists, sender and receiver alike).
func TestCrossSegmentCompression(t *testing.T) {
	p := baseParams(30)
	enc := NewEncoder(p)
	dec := NewDecoder(p)

	seg1 := []*FileEntry{
		dir("a/b"),
		reg("a/b/one.txt"),
	}
	seg2 := []*FileEntry{
		reg("a/b/two.txt"),
		dir("a/c"),
	}

	var buf bytes.Buffer
	writeSeg := func(entries []*FileEntry) {
		t.Helper()
		for _, f := range entries {
			if err := enc.Encode(&buf, f); err != nil {
				t.Fatalf("encode: %v", err)
			}
		}
		if err := enc.WriteEndOfFlist(&buf, false, 0); err != nil {
			t.Fatalf("WriteEndOfFlist: %v", err)
		}
	}
	writeSeg(seg1)
	writeSeg(seg2)

	var got []*FileEntry
	for {
		f, err := dec.Decode(&buf)
		if err == io.EOF {
			continue
		} else if err != nil {
			t.Fatalf("decode: %v", err)
		}
		got = append(got, f)
		if len(got) == len(seg1)+len(seg2) {
			break
		}
	}
	want := append(append([]*FileEntry{}, seg1...), seg2...)
	for i := range want {
		if got[i].Name != want[i].Name || got[i].Mode != want[i].Mode {
			t.Errorf("entry %d: want (%s, %o), got (%s, %o)",
				i, want[i].Name, want[i].Mode, got[i].Name, got[i].Mode)
		}
	}
}

// TestAtProto30UsesIDList pins the trailing-id-list decision: under
// incremental recursion there is never a trailing list at >= 30, matching
// flist.c:2820 (send_id_lists only when numeric_ids <= 0 && !inc_recurse).
func TestAtProto30UsesIDList(t *testing.T) {
	for _, tt := range []struct {
		version int
		numeric bool
		inc     bool
		want    bool
	}{
		{version: 29, numeric: true, inc: false, want: true},
		{version: 29, numeric: true, inc: true, want: true}, // inc unused < 30
		{version: 30, numeric: false, inc: false, want: true},
		{version: 30, numeric: true, inc: false, want: true},
		{version: 30, numeric: false, inc: true, want: false},
		{version: 30, numeric: true, inc: true, want: false},
		{version: 32, numeric: true, inc: true, want: false},
	} {
		p := Params{ProtocolVersion: tt.version, NumericIDs: tt.numeric, IncRecurse: tt.inc}
		if got := atProto30UsesIDList(p); got != tt.want {
			t.Errorf("v%d numeric=%v inc=%v: got %v, want %v",
				tt.version, tt.numeric, tt.inc, got, tt.want)
		}
	}
}

// TestWriteFileListNoIDListUnderInc checks that a complete write under
// IncRecurse emits no trailing id lists and that the sentinel terminator
// appears only when SafeFlist and an error are present.
func TestWriteFileListNoIDListUnderInc(t *testing.T) {
	p := baseParams(32)
	p.IncRecurse = true
	p.NumericIDs = false

	var buf bytes.Buffer
	uidNames := map[int32]string{0: "root"}
	gidNames := map[int32]string{0: "root"}
	if err := WriteFileList(&buf, p, []*FileEntry{reg("x")}, uidNames, gidNames, 0); err != nil {
		t.Fatalf("WriteFileList: %v", err)
	}
	// decoder must consume everything: entries + terminator, nothing else
	dec := NewDecoder(p)
	if _, err := dec.Decode(&buf); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if _, err := dec.Decode(&buf); err != io.EOF {
		t.Fatalf("decode = %v, want io.EOF", err)
	}
	if rest, err := io.ReadAll(&buf); err != nil || len(rest) != 0 {
		t.Fatalf("expected no trailing bytes, got %q (%v)", rest, err)
	}
}
