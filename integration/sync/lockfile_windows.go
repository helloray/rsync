//go:build windows

package sync_test

import (
	"testing"

	"golang.org/x/sys/windows"
)

// lockFile holds an open handle with no sharing flags, so any other open of
// the file fails with a sharing violation — the sender's do_open fails and
// it must replace the file's data block with MSG_NO_SEND
// (rsync/sender.c:709-723).
func lockFile(t *testing.T, path string) (unlock func()) {
	t.Helper()
	p, err := windows.UTF16PtrFromString(path)
	if err != nil {
		t.Fatal(err)
	}
	h, err := windows.CreateFile(p, windows.GENERIC_READ, 0, nil,
		windows.OPEN_EXISTING, windows.FILE_ATTRIBUTE_NORMAL, 0)
	if err != nil {
		t.Fatal(err)
	}
	return func() { _ = windows.CloseHandle(h) }
}
