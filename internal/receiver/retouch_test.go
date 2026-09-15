package receiver

import (
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/gokrazy/rsync/internal/log"
	"github.com/gokrazy/rsync/internal/rsyncopts"
)

// newRetouchTestTransfer builds a Transfer wired to a fresh temp destination
// with an incremental-recursion state, mirroring the recvGenerator test
// fixtures in saferoot_test.go.
func newRetouchTestTransfer(t *testing.T) *Transfer {
	t.Helper()
	dir := t.TempDir()
	root, err := NewSafeRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { root.Close() })
	return &Transfer{
		Logger: log.New(io.Discard),
		Opts: &TransferOpts{
			DebugGTE: func(rsyncopts.DebugLevel, uint16) bool { return false },
			InfoGTE:  func(rsyncopts.InfoLevel, uint16) bool { return false },
		},
		Dest:     dir,
		DestRoot: root,
		inc:      newIncRecv(nil),
	}
}

// TestRecvGeneratorRetouchCollection checks that the generator collects
// exactly the unwritable directories for the end-of-transfer touch-up: the
// collection condition is the same S_IWUSR test that hands the directory a
// temporary write bit, so the set replaces the former full directory list
// (docs/dirs-two-phase.md).
func TestRecvGeneratorRetouchCollection(t *testing.T) {
	rt := newRetouchTestTransfer(t)

	if err := rt.recvGenerator(&File{Name: "locked", Ndx: 1, Mode: 0o040500}); err != nil {
		t.Fatalf("recvGenerator(locked) = %v", err)
	}
	if !rt.retouchDirPerms {
		t.Error("retouchDirPerms not set for an unwritable directory")
	}
	if len(rt.inc.retouch) != 1 || rt.inc.retouch[0].Name != "locked" {
		t.Fatalf("retouch = %v, want [locked]", rt.inc.retouch)
	}
	if st, err := os.Stat(filepath.Join(rt.Dest, "locked")); err != nil || !st.IsDir() {
		t.Fatalf("locked should have been created, stat err = %v", err)
	}

	// A writable directory is never collected.
	if err := rt.recvGenerator(&File{Name: "open", Ndx: 2, Mode: 0o040755}); err != nil {
		t.Fatalf("recvGenerator(open) = %v", err)
	}
	if len(rt.inc.retouch) != 1 {
		t.Fatalf("retouch = %v, want [locked]", rt.inc.retouch)
	}

	// A second unwritable directory joins the set. Its writable parent is
	// processed first, like the wire order guarantees (dirs before
	// contents), so MkdirAll has somewhere to create it.
	if err := rt.recvGenerator(&File{Name: "sub", Ndx: 3, Mode: 0o040755}); err != nil {
		t.Fatalf("recvGenerator(sub) = %v", err)
	}
	if err := rt.recvGenerator(&File{Name: "sub/locked", Ndx: 4, Mode: 0o040000}); err != nil {
		t.Fatalf("recvGenerator(sub/locked) = %v", err)
	}
	if len(rt.inc.retouch) != 2 || rt.inc.retouch[1].Name != "sub/locked" {
		t.Fatalf("retouch = %v, want [locked sub/locked]", rt.inc.retouch)
	}
}

// TestRecvGeneratorRetouchDryRun checks the dry-run consistency: the
// directory branch returns before the S_IWUSR detection, so nothing is
// collected and the touch-up flag stays unset — matching touchUpDirs's
// DryRun skip in the pre-two-phase world.
func TestRecvGeneratorRetouchDryRun(t *testing.T) {
	rt := newRetouchTestTransfer(t)
	rt.Opts.DryRun = true

	if err := rt.recvGenerator(&File{Name: "locked", Ndx: 1, Mode: 0o040500}); err != nil {
		t.Fatalf("recvGenerator(locked, dry-run) = %v", err)
	}
	if rt.retouchDirPerms {
		t.Error("retouchDirPerms set during dry-run")
	}
	if len(rt.inc.retouch) != 0 {
		t.Errorf("retouch = %v, want empty", rt.inc.retouch)
	}
}

// TestTouchUpDirsRestoresMode drives the full cycle for one unwritable
// directory: created writeable by the generator, then restored to the
// source mode by the touch-up over the retouch set.
func TestTouchUpDirsRestoresMode(t *testing.T) {
	rt := newRetouchTestTransfer(t)

	if err := rt.recvGenerator(&File{Name: "locked", Ndx: 1, Mode: 0o040500}); err != nil {
		t.Fatalf("recvGenerator(locked) = %v", err)
	}
	if st, err := os.Stat(filepath.Join(rt.Dest, "locked")); err != nil {
		t.Fatal(err)
	} else if st.Mode().Perm()&0o200 == 0 {
		t.Fatalf("perm = %o, want the temporary write bit set while filling", st.Mode().Perm())
	}

	if err := rt.touchUpDirs(rt.inc.retouch); err != nil {
		t.Fatalf("touchUpDirs = %v", err)
	}
	st, err := os.Stat(filepath.Join(rt.Dest, "locked"))
	if err != nil {
		t.Fatal(err)
	}
	if st.Mode().Perm()&0o200 != 0 {
		t.Errorf("perm = %o, want the write bit cleared after touch-up", st.Mode().Perm())
	}
}
