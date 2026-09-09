package sender

import (
	"bytes"
	"flag"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"testing"
	"testing/fstest"

	"github.com/gokrazy/rsync/internal/log"
	"github.com/gokrazy/rsync/internal/protocol"
	"github.com/gokrazy/rsync/internal/rsyncopts"
	"github.com/gokrazy/rsync/internal/rsyncos"
	"github.com/gokrazy/rsync/internal/rsyncwire"
	"github.com/gokrazy/rsync/internal/testlogger"
)

var update = flag.Bool("update", false, "rewrite golden files")

// memWriteCloser turns a bytes.Buffer into an io.WriteCloser so it can back a
// MultiplexWriter in golden tests.
type memWriteCloser struct {
	bytes.Buffer
}

func (m *memWriteCloser) Close() error { return nil }

// nopWriteCloser discards output while satisfying io.WriteCloser.
type nopWriteCloser struct{}

func (nopWriteCloser) Write(p []byte) (int, error) { return len(p), nil }
func (nopWriteCloser) Close() error                { return nil }

// newGoldenTransfer builds a Transfer in server-sender mode over a MapFS,
// writing multiplexed DATA frames into the returned buffer. Options mirror
// the flag letters a real client sends (-logDtpre.iLsfxCIvu), with the
// protocol-32 session flags a Go↔C transfer negotiates.
func newGoldenTransfer(t *testing.T, mapfs fstest.MapFS) (*Transfer, *bytes.Buffer) {
	t.Helper()

	osenv := &rsyncos.Env{Stderr: nopWriteCloser{}}
	opts := rsyncopts.NewOptions(osenv)
	pc := rsyncopts.NewContext(opts)
	if err := pc.ParseArguments(osenv, []string{"--server", "--sender", "-logDtpre.iLsfxCIvu", "."}); err != nil {
		t.Fatalf("parsing server options: %v", err)
	}
	opts.SetProtocolVersion(32)

	buf := &memWriteCloser{}
	conn := &rsyncwire.Conn{
		Writer: &rsyncwire.MultiplexWriter{Writer: buf},
	}
	st := &Transfer{
		Logger: log.New(testlogger.New(t)),
		Opts:   opts,
		Session: &protocol.Session{
			Version:          32,
			IncRecurse:       true,
			VarintFlistFlags: true,
			SafeFlist:        true,
		},
		Conn:   conn,
		Source: NewFSSource(mapfs),
		Env:    osenv,
	}
	return st, &buf.Buffer
}

// compareGolden compares got against the named golden file, rewriting it when
// -update was passed. On mismatch it reports the first differing offset.
func compareGolden(t *testing.T, name string, got []byte) {
	t.Helper()
	path := filepath.Join("testdata", "golden", name+".golden")
	if *update {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, got, 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading golden (run with -update to create): %v", err)
	}
	if bytes.Equal(want, got) {
		return
	}
	off := 0
	for off < len(want) && off < len(got) && want[off] == got[off] {
		off++
	}
	t.Fatalf("golden mismatch in %s: got %d bytes, want %d bytes (first difference at offset %d: got %#02x, want %#02x)",
		name, len(got), len(want), off,
		got[min(off, len(got)-1)], want[min(off, len(want)-1)])
}

// TestSendFileListGolden pins the exact wire bytes of the initial file list,
// the lazily emitted per-directory segments, and the trailing NDX_FLIST_EOF,
// so the lazy per-directory scan refactor (C send1extra model) can be
// verified byte-identical against the pre-refactor implementation.
func TestSendFileListGolden(t *testing.T) {
	type testCase struct {
		name  string
		mapfs fstest.MapFS
		paths []string
	}
	for _, tc := range []testCase{
		{
			// dot-dir transfer: the initial list carries ".", "a", "top.txt";
			// segments for a, a/b follow in DFS order.
			name: "dotdir",
			mapfs: fstest.MapFS{
				"a/b/f":   {Data: []byte("f")},
				"a/c.txt": {Data: []byte("c")},
				"top.txt": {Data: []byte("t")},
			},
			paths: []string{"/"},
		},
		{
			// a sibling name sorting between a dir and its children pins the
			// DFS segment emission order (seg(a), seg(a/y), seg(a-x)).
			name: "siblingdash",
			mapfs: fstest.MapFS{
				"a/x":   {Data: []byte("x")},
				"a/y/z": {Data: []byte("z")},
				"a-x/w": {Data: []byte("w")},
			},
			paths: []string{"/"},
		},
		{
			// childless dirs still get an (empty) segment.
			name: "emptydir",
			mapfs: fstest.MapFS{
				"empty":   &fstest.MapFile{Mode: fs.ModeDir},
				"top.txt": {Data: []byte("t")},
			},
			paths: []string{"/"},
		},
		{
			// a single file gets only the initial list, then EOF.
			name:  "singlefile",
			mapfs: fstest.MapFS{"just-a-file": {Data: []byte("j")}},
			paths: []string{"/just-a-file"},
		},
		{
			// a non-dot dir argument: the dir itself is the only initial
			// entry (dirIdx 0), its children form the next segment.
			name: "subdirarg",
			mapfs: fstest.MapFS{
				"sub/x":   {Data: []byte("x")},
				"sub/y/z": {Data: []byte("z")},
			},
			paths: []string{"/sub"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st, buf := newGoldenTransfer(t, tc.mapfs)
			fl, err := st.SendFileList("/", tc.paths, &filterRuleList{})
			if err != nil {
				t.Fatalf("SendFileList: %v", err)
			}
			if fl.inc == nil {
				t.Fatal("SendFileList did not set up the incremental scheduler")
			}
			if err := fl.inc.topUp(); err != nil {
				t.Fatalf("topUp: %v", err)
			}
			compareGolden(t, tc.name, buf.Bytes())
		})
	}
}

// goldenNames decodes a captured byte stream just enough to log it on
// failure: it splits the multiplex frames and prints their payloads
// (segment headers, entry encodings, EOF marker).
func goldenNames(buf []byte) string {
	var out string
	for off := 0; off+4 <= len(buf); {
		header := uint32(buf[off]) | uint32(buf[off+1])<<8 | uint32(buf[off+2])<<16 | uint32(buf[off+3])<<24
		tag := uint8(header>>24) - 7
		length := int(header & 0x00ffffff)
		off += 4
		if off+length > len(buf) {
			out += fmt.Sprintf("<truncated frame tag=%d len=%d>", tag, length)
			break
		}
		out += fmt.Sprintf("<frame tag=%d len=%d %q>\n", tag, length, buf[off:off+length])
		off += length
	}
	return out
}
