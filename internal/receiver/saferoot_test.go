package receiver

import (
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"testing"

	"github.com/gokrazy/rsync/internal/log"
	"github.com/gokrazy/rsync/internal/rsyncopts"
)

// TestSafeRootReservedDeviceNames checks that Windows reserved device names
// (AUX, NUL, CON, COM1… with any extension) can be synced — os.Root alone
// rejects them with "path escapes from parent".
func TestSafeRootReservedDeviceNames(t *testing.T) {
	dir := t.TempDir()
	root, err := NewSafeRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()

	const name = "work/dir/i2c/aux.c"
	if err := root.MkdirAll(filepath.Dir(name), 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}

	f, err := root.OpenFile(name, os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatalf("OpenFile(aux.c): %v", err)
	}
	if _, err := f.Write([]byte("hello")); err != nil {
		t.Fatal(err)
	}
	f.Close()

	st, err := root.Lstat(name)
	if err != nil {
		t.Fatalf("Lstat(aux.c): %v", err)
	}
	if st.Size() != 5 {
		t.Errorf("size = %d, want 5", st.Size())
	}
	if _, err := root.Stat(name); err != nil {
		t.Errorf("Stat(aux.c): %v", err)
	}

	// Atomic-replace rename, the receiver's write path.
	pf, err := newPendingFile(root, name, false)
	if err != nil {
		t.Fatalf("newPendingFile: %v", err)
	}
	if _, err := pf.Write([]byte("world")); err != nil {
		t.Fatal(err)
	}
	if err := pf.CloseAtomicallyReplace(); err != nil {
		t.Fatalf("CloseAtomicallyReplace: %v", err)
	}
	// Read back through SafeRoot — plain os.ReadFile on a normal Win32 path
	// would open the AUX device and block forever.
	rf, err := root.Open(name)
	if err != nil {
		t.Fatalf("Open(aux.c): %v", err)
	}
	got, err := io.ReadAll(rf)
	rf.Close()
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "world" {
		t.Errorf("content = %q, want %q", got, "world")
	}

	// The FS() view (used by the deletion pass) must also list reserved-name
	// directories without hitting the device namespace.
	if err := root.MkdirAll("work/aux", 0o755); err != nil {
		t.Fatalf("MkdirAll(work/aux): %v", err)
	}
	if cf, err := root.OpenFile("work/aux/com1.c", os.O_CREATE|os.O_WRONLY, 0o644); err != nil {
		t.Fatalf("OpenFile(com1.c): %v", err)
	} else {
		cf.WriteString("z")
		cf.Close()
	}
	if entries, err := fs.ReadDir(root.FS(), "work/aux"); err != nil {
		t.Errorf("fs.ReadDir(work/aux): %v", err)
	} else if len(entries) != 1 || entries[0].Name() != "com1.c" {
		t.Errorf("entries = %v, want [com1.c]", entries)
	}

	// Cleanup path.
	if err := root.Remove(name); err != nil {
		t.Errorf("Remove(aux.c): %v", err)
	}

	// ".." must still be rejected by os.Root.
	if _, err := root.Open("../outside"); err == nil {
		t.Error("Open(../outside) should fail")
	}
}

// TestRecvGeneratorSkipsUnrepresentable checks that names NTFS cannot
// represent (colons, e.g. Perl man pages) are skipped with an IO error
// instead of aborting the whole transfer.
func TestRecvGeneratorSkipsUnrepresentable(t *testing.T) {
	dir := t.TempDir()
	root, err := NewSafeRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()

	rt := &Transfer{
		Logger:   log.New(io.Discard),
		Opts: &TransferOpts{
			DebugGTE: func(rsyncopts.DebugLevel, uint16) bool { return false },
			InfoGTE:  func(rsyncopts.InfoLevel, uint16) bool { return false },
		},
		Dest:     dir,
		DestRoot: root,
	}
	if err := rt.recvGenerator(&File{Name: "man3/Convert::Binary::C.3pm", Mode: 0o100644}); err != nil {
		t.Fatalf("recvGenerator(colon name) = %v, want skip", err)
	}
	if rt.IOErrors == 0 || rt.skipCount != 1 {
		t.Errorf("IOErrors = %d, skipCount = %d, want both counted", rt.IOErrors, rt.skipCount)
	}
	if _, err := os.Stat(filepath.Join(dir, "man3")); !os.IsNotExist(err) {
		t.Errorf("nothing should have been created, stat err = %v", err)
	}

	// Representable names still work normally.
	if err := rt.recvGenerator(&File{Name: "okdir", Mode: 0o040755}); err != nil {
		t.Fatalf("recvGenerator(okdir) = %v", err)
	}
	if st, err := os.Stat(filepath.Join(dir, "okdir")); err != nil || !st.IsDir() {
		t.Errorf("okdir should have been created, stat err = %v", err)
	}
}
