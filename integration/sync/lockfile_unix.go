//go:build !windows

package sync_test

import (
	"os"
	"testing"
)

// lockFile makes path unopenable: mode 000 blocks opens by non-root, so the
// sender's do_open fails and it must replace the file's data block with
// MSG_NO_SEND (rsync/sender.c:709-723). It skips the test when the lock
// cannot be established (running as root), since the path is then not
// exercised.
func lockFile(t *testing.T, path string) (unlock func()) {
	t.Helper()
	if err := os.Chmod(path, 0o000); err != nil {
		t.Fatal(err)
	}
	if f, err := os.Open(path); err == nil {
		f.Close()
		t.Skip("cannot make files unopenable on this system (running as root?)")
	}
	return func() { _ = os.Chmod(path, 0o644) }
}
