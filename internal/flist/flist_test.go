package flist

import (
	"bytes"
	"io"
	"reflect"
	"strconv"
	"testing"

	"github.com/gokrazy/rsync"
)

func entry(name string, mode int32) *FileEntry {
	return &FileEntry{Name: name, Mode: mode, ModTime: 1700000000, Uid: 1000, Gid: 1000}
}

func baseParams(version int) Params {
	return Params{
		ProtocolVersion:  version,
		VarintFlags:      version >= 30,
		PreserveUid:      true,
		PreserveGid:      true,
		PreserveLinks:    true,
		PreserveDevices:  true,
		PreserveSpecials: true,
		AlwaysChecksum:   true,
		NumericIDs:       true,
	}
}

func longName() string {
	s := "long-name-"
	for len(s) < 260 {
		s += "x"
	}
	return s
}

// TestRoundtripAllVersions exercises every field path for protocols 27..=30:
// regular, dir, SAME_NAME prefix, symlink, char device, and a long name.
func TestRoundtripAllVersions(t *testing.T) {
	for _, version := range []int{27, 28, 29, 30} {
		version := version
		t.Run("proto"+strconv.Itoa(version), func(t *testing.T) {
			p := baseParams(version)
			p.IncRecurse = version >= 30

			files := []*FileEntry{
				entry("top.txt", 0o100644|rsync.S_IFREG),
				entry("sub", 0o40755|rsync.S_IFDIR),
				entry("sub/in", 0o100600|rsync.S_IFREG),
				// SAME_NAME: shares the "sub/in" prefix with the previous entry.
				entry("sub/inner.txt", 0o100444|rsync.S_IFREG),
				// symlink (PreserveLinks) exercises the link length + target.
				{Name: "sub/link", Mode: 0o0777 | rsync.S_IFLNK,
					ModTime: 1700000001, LinkTarget: "../sub/inner.txt"},
				// char device (PreserveDevices) exercises rdev major/minor.
				{Name: "tty0", Mode: 0o0660 | rsync.S_IFCHR,
					ModTime: 1700000002, RdevMajor: 4, RdevMinor: 0, Length: 0},
				// long name forces LONG_NAME framing.
				entry(longName(), 0o100644|rsync.S_IFREG),
			}

			uidNames := map[int32]string{1000: "alice", 33: "www-data"}
			gidNames := map[int32]string{1000: "staff"}

			for _, f := range files {
				if f.isDevice() {
					f.Length = 0 // C zeroes the device length
				}
			}

			var buf bytes.Buffer
			if err := WriteFileList(&buf, p, files, uidNames, gidNames, 0); err != nil {
				t.Fatalf("WriteFileList: %v", err)
			}
			got, ioErr, err := ReadFileList(bytes.NewReader(buf.Bytes()), p, map[int32]string{}, map[int32]string{})
			if err != nil {
				t.Fatalf("ReadFileList: %v", err)
			}
			if ioErr != 0 {
				t.Fatalf("ioErr = %d, want 0", ioErr)
			}
			if len(got) != len(files) {
				t.Fatalf("got %d entries, want %d", len(got), len(files))
			}
			for i := range files {
				have := *got[i]
				if version < 30 {
					have.User, have.Group = "", ""
				}
				if !reflect.DeepEqual(&have, files[i]) {
					t.Errorf("entry %d mismatch:\n want %+v\n  got %+v", i, files[i], &have)
				}
			}
		})
	}
}

// TestRoundtripIncRecurseNames exercises the inline uid/gid NAME_FOLLOWS path
// at protocol 30 under incremental recursion.
func TestRoundtripIncRecurseNames(t *testing.T) {
	p := baseParams(30)
	p.NumericIDs = false
	p.IncRecurse = true
	files := []*FileEntry{
		{Name: "a", Mode: 0o100644 | rsync.S_IFREG,
			ModTime: 1700000000, Uid: 1000, Gid: 1000, User: "alice", Group: "staff"},
		{Name: "b", Mode: 0o100644 | rsync.S_IFREG,
			ModTime: 1700000001, Uid: 1001, Gid: 1001, User: "bob", Group: "dev"},
	}
	var buf bytes.Buffer
	uidNames := map[int32]string{1000: "alice", 1001: "bob"}
	gidNames := map[int32]string{1000: "staff", 1001: "dev"}
	if err := WriteFileList(&buf, p, files, uidNames, gidNames, 0); err != nil {
		t.Fatalf("WriteFileList: %v", err)
	}
	got, _, err := ReadFileList(bytes.NewReader(buf.Bytes()), p, map[int32]string{}, map[int32]string{})
	if err != nil {
		t.Fatalf("ReadFileList: %v", err)
	}
	for i := range files {
		if !reflect.DeepEqual(files[i], got[i]) {
			t.Errorf("entry %d: want %+v got %+v", i, files[i], got[i])
		}
	}
}

func TestIDListRoundtrip(t *testing.T) {
	for _, version := range []int{27, 30} {
		names := map[int32]string{1000: "alice", 55: "daemon"}
		var buf bytes.Buffer
		if err := WriteIDList(&buf, names, version, false); err != nil {
			t.Fatalf("WriteIDList(%d): %v", version, err)
		}
		got, err := ReadIDList(bytes.NewReader(buf.Bytes()), version, false)
		if err != nil {
			t.Fatalf("ReadIDList(%d): %v", version, err)
		}
		if !reflect.DeepEqual(got, names) {
			t.Errorf("version %d: got %v want %v", version, got, names)
		}
	}
}

// TestCompleteReaderRoundtrip drives NewCompleteReader over a list produced by
// WriteFileList, exercising the Segment/Reader abstraction that the receiver
// consumes and that incremental recursion (Phase E) swaps underneath.
func TestCompleteReaderRoundtrip(t *testing.T) {
	for _, version := range []int{29, 30} {
		p := baseParams(version)
		p.IncRecurse = version >= 30
		files := []*FileEntry{
			entry("top.txt", 0o100644|rsync.S_IFREG),
			entry("sub", 0o40755|rsync.S_IFDIR),
			entry("sub/in", 0o100600|rsync.S_IFREG),
			entry(longName(), 0o100644|rsync.S_IFREG),
		}
		uidNames := map[int32]string{1000: "alice"}
		gidNames := map[int32]string{1000: "staff"}
		var buf bytes.Buffer
		if err := WriteFileList(&buf, p, files, uidNames, gidNames, 3); err != nil {
			t.Fatalf("v%d WriteFileList: %v", version, err)
		}
		r := NewCompleteReader(bytes.NewReader(buf.Bytes()), p)
		seg, err := r.Next()
		if err != nil {
			t.Fatalf("v%d Next: %v", version, err)
		}
		if !seg.EOF || seg.IOError != 3 {
			t.Errorf("v%d seg EOF=%v IOError=%d, want EOF=true IOError=3", version, seg.EOF, seg.IOError)
		}
		if seg.NdxStart != 0 || len(seg.Entries) != len(files) {
			t.Fatalf("v%d NdxStart=%d n=%d, want 0/%d", version, seg.NdxStart, len(seg.Entries), len(files))
		}
		for i := range files {
			have := *seg.Entries[i]
			if version < 30 {
				have.User, have.Group = "", ""
			}
			if !reflect.DeepEqual(&have, files[i]) {
				t.Errorf("v%d entry %d:\n want %+v\n  got %+v", version, i, files[i], &have)
			}
		}
		// the reader is drained: a second Next returns io.EOF.
		if _, err := r.Next(); err != io.EOF {
			t.Errorf("v%d second Next = %v, want io.EOF", version, err)
		}
	}
}

// TestEncoderDecoderStream checks the incremental entry API used by the future
// Segment reader (C3): entries streamed without a whole-list buffer roundtrip.
func TestEncoderDecoderStream(t *testing.T) {
	for _, version := range []int{29, 30} {
		p := baseParams(version)
		enc := NewEncoder(p)
		dec := NewDecoder(p)
		files := []*FileEntry{
			entry("x.txt", 0o100644|rsync.S_IFREG),
			entry("sub", 0o40755|rsync.S_IFDIR),
			entry("sub/y.txt", 0o100600|rsync.S_IFREG),
			{Name: "sub/l", Mode: 0o0777 | rsync.S_IFLNK, ModTime: 1700000000, LinkTarget: "y.txt"},
		}
		var buf bytes.Buffer
		for _, f := range files {
			if err := enc.Encode(&buf, f); err != nil {
				t.Fatalf("v%d encode: %v", version, err)
			}
		}
		if err := enc.WriteEndOfFlist(&buf, false, 0); err != nil { // terminator
			t.Fatal(err)
		}
		var got []*FileEntry
		for {
			f, err := dec.Decode(&buf)
			if err == io.EOF {
				break
			} else if err != nil {
				t.Fatalf("v%d decode: %v", version, err)
			}
			got = append(got, f)
		}
		if len(got) != len(files) {
			t.Fatalf("v%d got %d entries, want %d", version, len(got), len(files))
		}
		for i := range files {
			if !reflect.DeepEqual(files[i], got[i]) {
				t.Errorf("v%d entry %d: want %+v got %+v", version, i, files[i], got[i])
			}
		}
	}
}