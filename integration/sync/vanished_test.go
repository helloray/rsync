package sync_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/gokrazy/rsync/internal/rsynctest"
	"github.com/gokrazy/rsync/rsyncd"
)

// TestSyncVanishedSourceFile pulls from a module whose source tree contains
// a file the sender cannot open. The sender must replace that file's data
// block with an MSG_NO_SEND control frame and keep going (rsync/sender.c:
// 709-723), and the receiver must consume the frame without aborting — one
// missing file must not interrupt the sync.
func TestSyncVanishedSourceFile(t *testing.T) {
	tmp := t.TempDir()
	source := filepath.Join(tmp, "source")
	dest := filepath.Join(tmp, "dest")
	if err := os.MkdirAll(source, 0755); err != nil {
		t.Fatal(err)
	}
	const hello = "world"
	if err := os.WriteFile(filepath.Join(source, "hello"), []byte(hello), 0644); err != nil {
		t.Fatal(err)
	}
	const bye = "moon"
	if err := os.WriteFile(filepath.Join(source, "bye"), []byte(bye), 0644); err != nil {
		t.Fatal(err)
	}
	locked := filepath.Join(source, "locked")
	if err := os.WriteFile(locked, []byte("secret"), 0644); err != nil {
		t.Fatal(err)
	}
	unlock := lockFile(t, locked)
	defer unlock()

	srv := rsynctest.NewInMemory(t, rsyncd.Module{
		Name: "interop",
		Path: source,
	})
	srv.RunClient(t, []string{"-a"}, "./", []string{dest})

	for _, tc := range []struct {
		name string
		want string
	}{
		{"hello", hello},
		{"bye", bye},
	} {
		got, err := os.ReadFile(filepath.Join(dest, tc.name))
		if err != nil {
			t.Fatalf("file %q missing after sync: %v", tc.name, err)
		}
		if string(got) != tc.want {
			t.Errorf("file %q = %q, want %q", tc.name, got, tc.want)
		}
	}
	if _, err := os.Stat(filepath.Join(dest, "locked")); !os.IsNotExist(err) {
		t.Errorf("locked file should not have been transferred, stat err = %v", err)
	}
}
