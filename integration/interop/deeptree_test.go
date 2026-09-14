package interop_test

import (
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/gokrazy/rsync"
	"github.com/gokrazy/rsync/internal/rsynctest"
	"github.com/gokrazy/rsync/internal/testlogger"
	"github.com/gokrazy/rsync/rsyncclient"
	"github.com/gokrazy/rsync/rsyncd"
	"github.com/google/go-cmp/cmp"
)

// createDeepTree builds a >1000-entry tree (the incremental-recursion
// lookahead window size) with 3 directory levels, empty dirs and the
// FNameCmp boundary names a/a-x/a.x/a/x (rsync/flist.c:fNameCmp), which pin
// down the segment emission order. No symlinks, so the fixture works on
// Windows.
func createDeepTree(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	write := func(rel string) {
		path := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte("content of "+rel+"\n"), 0644); err != nil {
			t.Fatal(err)
		}
	}
	mkdir := func(rel string) {
		if err := os.MkdirAll(filepath.Join(root, filepath.FromSlash(rel)), 0755); err != nil {
			t.Fatal(err)
		}
	}
	const numTop = 40
	for d := 0; d < numTop; d++ {
		top := fmt.Sprintf("d%02d", d)
		for f := 0; f < 25; f++ {
			write(fmt.Sprintf("%s/f%02d.txt", top, f))
		}
		for _, sub := range []string{"sub1", "sub2"} {
			for f := 0; f < 10; f++ {
				write(fmt.Sprintf("%s/%s/g%02d.txt", top, sub, f))
			}
		}
		mkdir(top + "/empty")
	}
	// boundary names: "a-x" sorts between "a" and "a/x" ('-' < '/'),
	// "a.x" sorts before both ("." < "-")
	mkdir("a")
	write("a/w")
	write("a/x")
	mkdir("a-x")
	write("a-x/w")
	write("a.x")
	write("a-x.txt")
	return root
}

// treeContents walks root and returns the file contents and the dir set by
// slash-separated path relative to root.
func treeContents(t *testing.T, root string) (map[string]string, map[string]bool) {
	t.Helper()
	files := make(map[string]string)
	dirs := make(map[string]bool)
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		if rel == "." {
			return nil
		}
		rel = filepath.ToSlash(rel)
		if d.IsDir() {
			dirs[rel] = true
			return nil
		}
		b, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		files[rel] = string(b)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return files, dirs
}

// verifySame fails unless the two trees hold identical files (paths and
// contents) and identical directory sets (so empty dirs are checked, too).
func verifySame(t *testing.T, wantRoot, gotRoot string) {
	t.Helper()
	wantFiles, wantDirs := treeContents(t, wantRoot)
	gotFiles, gotDirs := treeContents(t, gotRoot)
	if diff := cmp.Diff(wantFiles, gotFiles); diff != "" {
		t.Errorf("file contents differ (-want +got):\n%s", diff)
	}
	if diff := cmp.Diff(wantDirs, gotDirs); diff != "" {
		t.Errorf("directory sets differ (-want +got):\n%s", diff)
	}
}

func runCRsync(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command(rsynctest.AnyRsync(t), args...)
	cmd.Dir = dir
	cmd.Stdout = testlogger.New(t)
	cmd.Stderr = testlogger.New(t)
	if err := cmd.Run(); err != nil {
		t.Fatalf("%v: %v", cmd.Args, err)
	}
}

// pullFromGoDaemon syncs the deep tree from a Go rsyncd daemon to a local
// destination using the C rsync client, with extra client args (e.g.
// --delete, --protocol=29).
func pullFromGoDaemon(t *testing.T, source, dest string, extraArgs ...string) {
	t.Helper()
	srv := rsynctest.New(t, rsynctest.InteropModule(source))
	args := append([]string{"--archive", "--port=" + srv.Port}, extraArgs...)
	runCRsync(t, filepath.Dir(dest), append(args, "rsync://localhost/interop/", filepath.Base(dest))...)
}

// syncGoGo pulls the deep tree from an in-memory Go daemon using the
// in-process Go client.
func syncGoGo(t *testing.T, source, dest string, extraArgs ...string) {
	t.Helper()
	srv := rsynctest.NewInMemory(t, rsyncd.Module{Name: "deep", Path: source})
	args := append([]string{"-a"}, extraArgs...)
	srv.RunClient(t, args, "./", []string{dest})
}

// syncGoClientPullFromC runs the Go rsync client as a pull receiver against
// a C rsync sender subprocess connected over stdin/stdout pipes
// (rsyncclient_test.go TestClientCommand pattern).
func syncGoClientPullFromC(t *testing.T, source, dest string, extraArgs ...string) {
	t.Helper()
	client, err := rsyncclient.New(append([]string{"-a"}, extraArgs...), rsyncclient.DontRestrict())
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(rsynctest.AnyRsync(t), client.ServerCommandOptions(".")...)
	cmd.Dir = source
	cmd.Stderr = testlogger.New(t)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	rw := &rsync.BothCloser{ReadCloser: stdout, WriteCloser: stdin}
	if _, err := client.Run(t.Context(), rw, []string{dest}); err != nil {
		t.Errorf("client.Run: %v", err)
	}
	if err := cmd.Wait(); err != nil {
		t.Errorf("rsync subprocess: %v", err)
	}
}

// syncGoClientPushToC runs the Go rsync client as a push sender against a C
// rsync receiver subprocess connected over stdin/stdout pipes
// (rsyncclient_test.go ExampleClient_Run_sendToGoroutine pattern).
func syncGoClientPushToC(t *testing.T, source, dest string, extraArgs ...string) {
	t.Helper()
	client, err := rsyncclient.New(append([]string{"-a"}, extraArgs...), rsyncclient.WithSender(), rsyncclient.DontRestrict())
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(rsynctest.AnyRsync(t), client.ServerCommandOptions(".")...)
	cmd.Dir = dest
	cmd.Stderr = testlogger.New(t)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	rw := &rsync.BothCloser{ReadCloser: stdout, WriteCloser: stdin}
	if _, err := client.Run(t.Context(), rw, []string{source}); err != nil {
		t.Errorf("client.Run: %v", err)
	}
	if err := cmd.Wait(); err != nil {
		t.Errorf("rsync subprocess: %v", err)
	}
}

// pushToGoDaemon pushes the deep tree from a local C rsync client INTO a Go
// rsyncd daemon — the receiver-memory critical direction, since the Go
// receiver holds the incoming file list — with extra client args (e.g.
// --delete).
func pushToGoDaemon(t *testing.T, source, dest string, extraArgs ...string) {
	t.Helper()
	srv := rsynctest.New(t, rsynctest.WritableInteropModule(dest))
	args := append([]string{"--archive", "--port=" + srv.Port}, extraArgs...)
	runCRsync(t, source, append(args, "./", "rsync://localhost/interop/")...)
}

// TestInteropDeepTreeCClient: Go daemon receiver ↔ C client sender, >1000
// entries, then a dry-run re-sync with --ignore-times (re-requests every
// entry without any data flow, exercising the frame loop's itemize-echo
// routing against released entries), then a --delete re-sync that must
// remove a deleted file and an entire deleted subtree.
func TestInteropDeepTreeCPushToGoDaemon(t *testing.T) {
	t.Parallel()
	source := createDeepTree(t)
	dest := t.TempDir()

	pushToGoDaemon(t, source, dest)
	verifySame(t, source, dest)

	pushToGoDaemon(t, source, dest, "--dry-run", "--ignore-times")
	verifySame(t, source, dest)

	if err := os.Remove(filepath.Join(source, "d05", "f01.txt")); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(filepath.Join(source, "d12", "sub2")); err != nil {
		t.Fatal(err)
	}
	pushToGoDaemon(t, source, dest, "--delete")
	verifySame(t, source, dest)
}

// TestInteropDeepTreeGoClientPullFromCDelete: C rsync sender → Go client
// receiver with --delete, against a destination seeded with stale
// destination-only entries that must be removed.
func TestInteropDeepTreeGoClientPullFromCDelete(t *testing.T) {
	t.Parallel()
	source := createDeepTree(t)
	dest := t.TempDir()

	syncGoClientPullFromC(t, source, dest)
	verifySame(t, source, dest)

	if err := os.Remove(filepath.Join(source, "d20", "f07.txt")); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(filepath.Join(source, "d02", "sub1")); err != nil {
		t.Fatal(err)
	}
	staleFile := filepath.Join(dest, "stale.txt")
	if err := os.WriteFile(staleFile, []byte("stale"), 0644); err != nil {
		t.Fatal(err)
	}
	staleDir := filepath.Join(dest, "d99")
	if err := os.Mkdir(staleDir, 0755); err != nil {
		t.Fatal(err)
	}

	syncGoClientPullFromC(t, source, dest, "--delete")
	verifySame(t, source, dest)
}

// TestInteropDeepTreeCCClient: Go daemon (lazy inc-recurse sender) ↔ C
// client, >1000 entries, then a --checksum re-sync.
func TestInteropDeepTreeCCClient(t *testing.T) {
	t.Parallel()
	source := createDeepTree(t)
	dest := t.TempDir()

	pullFromGoDaemon(t, source, dest)
	verifySame(t, source, dest)

	pullFromGoDaemon(t, source, dest, "--ignore-times", "--checksum")
	verifySame(t, source, dest)
}

// TestInteropDeepTreeCCClientDelete: --delete must remove a deleted file and
// an entire deleted subtree (including the empty dir) from the destination.
func TestInteropDeepTreeCCClientDelete(t *testing.T) {
	t.Parallel()
	source := createDeepTree(t)
	dest := t.TempDir()

	pullFromGoDaemon(t, source, dest)
	verifySame(t, source, dest)

	if err := os.Remove(filepath.Join(source, "d07", "f13.txt")); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(filepath.Join(source, "d31", "sub1")); err != nil {
		t.Fatal(err)
	}
	pullFromGoDaemon(t, source, dest, "--delete")
	verifySame(t, source, dest)
}

// TestInteropDeepTreeProto29 pins the non-incremental complete-list sender
// path against the C client on the same tree.
func TestInteropDeepTreeProto29(t *testing.T) {
	t.Parallel()
	source := createDeepTree(t)
	dest := t.TempDir()

	pullFromGoDaemon(t, source, dest, "--protocol=29")
	verifySame(t, source, dest)
}

// TestInteropDeepTreeGoGo: Go sender ↔ Go receiver (in-process), including a
// --delete re-sync.
func TestInteropDeepTreeGoGo(t *testing.T) {
	t.Parallel()
	source := createDeepTree(t)
	dest := t.TempDir()

	syncGoGo(t, source, dest)
	verifySame(t, source, dest)

	if err := os.Remove(filepath.Join(source, "d00", "sub2", "g09.txt")); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(filepath.Join(source, "d39")); err != nil {
		t.Fatal(err)
	}
	syncGoGo(t, source, dest, "--delete")
	verifySame(t, source, dest)
}

// TestInteropDeepTreeGoClientPullFromC: C rsync sender ↔ Go client receiver.
func TestInteropDeepTreeGoClientPullFromC(t *testing.T) {
	t.Parallel()
	source := createDeepTree(t)
	dest := t.TempDir()

	syncGoClientPullFromC(t, source, dest)
	verifySame(t, source, dest)
}

// TestInteropDeepTreeGoClientPushToC: Go client sender ↔ C rsync receiver.
// Pushing a directory arg creates the source basename below the destination
// (like `rsync -a src/001 host:dest`), so the synced tree lands in
// dest/<basename>.
func TestInteropDeepTreeGoClientPushToC(t *testing.T) {
	t.Parallel()
	source := createDeepTree(t)
	dest := t.TempDir()

	syncGoClientPushToC(t, source, dest)
	verifySame(t, source, filepath.Join(dest, filepath.Base(source)))
}
