package errors_test

import (
	"bytes"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gokrazy/rsync/internal/rsynctest"
	"github.com/google/go-cmp/cmp"
)

func TestErrors(t *testing.T) {
	t.Parallel()

	tmp := t.TempDir()

	dest := filepath.Join(tmp, "dest")

	// We configure an rsync module with a non-existant path to trigger an
	// error. Removing read permission from a file is not sufficient because
	// that does not actually trigger an error! See TestNoReadPermission.
	nonExistant := filepath.Join(tmp, "non/existant")

	// start a server to sync from
	srv := rsynctest.New(t, rsynctest.InteropModule(nonExistant))

	// sync into dest dir
	var buf bytes.Buffer
	rsync := exec.Command(rsynctest.AnyRsync(t),
		//		"--debug=all4",
		"--archive",
		"-v", "-v", "-v", "-v",
		"--port="+srv.Port,
		"rsync://localhost/interop/", // copy contents of interop
		//source+"/", // sync from local directory
		filepath.Base(dest)) // directly into dest (relative to rsync.Dir)
	rsync.Dir = filepath.Dir(dest)
	rsync.Stdout = &buf
	rsync.Stderr = &buf
	if err := rsync.Run(); err == nil {
		t.Fatalf("rsync unexpectedly did not return with an error exit code, output:\n%s", buf.String())
	}

	output := buf.String()
	t.Logf("output:\n%s\n(end of output)", output)
	if want := "module path is not accessible"; !strings.Contains(output, want) {
		t.Fatalf("rsync output unexpectedly did not contain %q:\n%s", want, output)
	}
}

func TestNoSuchModule(t *testing.T) {
	t.Parallel()

	tmp := t.TempDir()

	dest := filepath.Join(tmp, "dest")

	// start a server to sync from
	srv := rsynctest.New(t, nil)

	// sync into dest dir
	var buf bytes.Buffer
	rsync := exec.Command(rsynctest.AnyRsync(t),
		//		"--debug=all4",
		"--archive",
		"-v", "-v", "-v", "-v",
		"--port="+srv.Port,
		"rsync://localhost/requesting-nonsense/", // copy contents of interop
		//source+"/", // sync from local directory
		filepath.Base(dest)) // directly into dest (relative to rsync.Dir)
	rsync.Dir = filepath.Dir(dest)
	rsync.Stdout = &buf
	rsync.Stderr = &buf
	if err := rsync.Run(); err == nil {
		t.Fatalf("rsync unexpectedly did not return with an error exit code, output:\n%s", buf.String())
	}

	output := buf.String()
	if want := "Unknown module"; !strings.Contains(output, want) {
		t.Fatalf("rsync output unexpectedly did not contain %q:\n%s", want, output)
	}
}

func TestNoReadPermission(t *testing.T) {
	t.Parallel()

	tmp := t.TempDir()
	source := filepath.Join(tmp, "source")
	dest := filepath.Join(tmp, "dest")

	// create files in source to be copied
	if err := os.MkdirAll(source, 0755); err != nil {
		t.Fatal(err)
	}
	dummy := filepath.Join(source, "dummy")
	if err := os.WriteFile(dummy, []byte("dummy"), 0644); err != nil {
		t.Fatal(err)
	}
	other := filepath.Join(source, "other")
	want := []byte("other file contents")
	if err := os.WriteFile(other, want, 0644); err != nil {
		t.Fatal(err)
	}

	// Remove read permission to trigger an error for one of the requested files.
	if err := os.Chmod(dummy, 0); err != nil {
		t.Fatal(err)
	}

	// start a server to sync from
	srv := rsynctest.New(t, rsynctest.InteropModule(source))

	// sync into dest dir
	var buf bytes.Buffer
	rsync := exec.Command(rsynctest.AnyRsync(t),
		//		"--debug=all4",
		"--archive",
		"-v", "-v", "-v", "-v",
		"--port="+srv.Port,
		"rsync://localhost/interop/", // copy contents of interop
		filepath.Base(dest))          // directly into dest (relative to rsync.Dir)
	rsync.Dir = filepath.Dir(dest)
	rsync.Stdout = &buf
	rsync.Stderr = &buf
	if err := rsync.Run(); err != nil {
		t.Fatalf("%v: %v", rsync.Args, err)
	}

	if os.Getuid() > 0 {
		// uid 0 can read the file despite chmod(0), so skip this check:

		if _, err := os.ReadFile(filepath.Join(dest, "dummy")); err == nil {
			t.Fatalf("dummy file unexpectedly created in the destination")
		}
	}

	got, err := os.ReadFile(filepath.Join(dest, "other"))
	if err != nil {
		t.Fatal(err)
	}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Fatalf("unexpected file contents: diff (-want +got):\n%s", diff)
	}
}

// TestReceiverErrorSurfacedToClient pins the abort-path framing to what C
// rsync accepts: the receiver's error rides an MSG_ERROR text frame followed
// by an MSG_ERROR_EXIT frame with a 4-byte exit code (rsync/io.c:read_a_msg
// rejects any other MSG_ERROR_EXIT payload as "invalid multi-message"). A
// regular file in the source collides with a non-empty directory of the same
// name in the destination, so the receiver aborts mid-transfer.
func TestReceiverErrorSurfacedToClient(t *testing.T) {
	t.Parallel()

	tmp := t.TempDir()
	source := filepath.Join(tmp, "source")
	if err := os.MkdirAll(source, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(source, "sub"), []byte("file contents"), 0644); err != nil {
		t.Fatal(err)
	}

	modRoot := filepath.Join(tmp, "modroot")
	if err := os.MkdirAll(filepath.Join(modRoot, "sub"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(modRoot, "sub", "blocked.txt"), []byte("x"), 0644); err != nil {
		t.Fatal(err)
	}

	srv := rsynctest.New(t, rsynctest.WritableInteropModule(modRoot))

	var buf bytes.Buffer
	rsync := exec.Command(rsynctest.AnyRsync(t),
		"--archive",
		"--port="+srv.Port,
		"source/", "rsync://localhost/interop/")
	rsync.Dir = tmp
	rsync.Stdout = &buf
	rsync.Stderr = &buf
	err := rsync.Run()
	if err == nil {
		t.Fatalf("rsync unexpectedly succeeded, output:\n%s", buf.String())
	}
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) || exitErr.ExitCode() != 23 {
		t.Fatalf("rsync exit = %v, want exit code 23 (RERR_PARTIAL): %v", err, err)
	}

	output := buf.String()
	t.Logf("output:\n%s\n(end of output)", output)
	if want := "make room for regular file"; !strings.Contains(output, want) {
		t.Fatalf("output unexpectedly did not contain the receiver error %q:\n%s", want, output)
	}
	if got := "invalid multi-message"; strings.Contains(output, got) {
		t.Fatalf("output unexpectedly contains %q — MSG_ERROR_EXIT framing regressed:\n%s", got, output)
	}
}

// TestReceiverErrorSurfacedWithInFlightData pins the abort-path shutdown: the
// receiver aborts while the C sender is still streaming a large file, so the
// error frames race in-flight data. Closing the socket while the peer has
// unread data sends a RST that discards the frames, and the client only sees
// "connection reset by peer" instead of the abort reason. The large file is
// ordered before the colliding name (f_name_cmp) so the generator hits the
// error mid-stream.
func TestReceiverErrorSurfacedWithInFlightData(t *testing.T) {
	if testing.Short() {
		t.Skip("transfers 16 MB to reproduce in-flight data at abort time")
	}
	t.Parallel()

	tmp := t.TempDir()
	source := filepath.Join(tmp, "source")
	if err := os.MkdirAll(source, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(source, "aaa"), bytes.Repeat([]byte("a"), 16<<20), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(source, "sub"), []byte("file contents"), 0644); err != nil {
		t.Fatal(err)
	}

	modRoot := filepath.Join(tmp, "modroot")
	if err := os.MkdirAll(filepath.Join(modRoot, "sub"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(modRoot, "sub", "blocked.txt"), []byte("x"), 0644); err != nil {
		t.Fatal(err)
	}

	srv := rsynctest.New(t, rsynctest.WritableInteropModule(modRoot))

	var buf bytes.Buffer
	rsync := exec.Command(rsynctest.AnyRsync(t),
		"--archive",
		"--port="+srv.Port,
		"source/", "rsync://localhost/interop/")
	rsync.Dir = tmp
	rsync.Stdout = &buf
	rsync.Stderr = &buf
	err := rsync.Run()
	if err == nil {
		t.Fatalf("rsync unexpectedly succeeded, output:\n%s", buf.String())
	}
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) || exitErr.ExitCode() != 23 {
		t.Fatalf("rsync exit = %v, want exit code 23 (RERR_PARTIAL): %v", err, err)
	}

	output := buf.String()
	t.Logf("output:\n%s\n(end of output)", output)
	if want := "make room for regular file"; !strings.Contains(output, want) {
		t.Fatalf("output unexpectedly did not contain the receiver error %q:\n%s", want, output)
	}
	if got := "connection reset"; strings.Contains(strings.ToLower(output), got) {
		t.Fatalf("output unexpectedly contains %q — abort path closes the socket with in-flight data:\n%s", got, output)
	}
}
