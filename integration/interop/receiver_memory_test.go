package interop_test

import (
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"testing"
	"time"

	"github.com/gokrazy/rsync"
	"github.com/gokrazy/rsync/internal/maincmd"
	"github.com/gokrazy/rsync/internal/rsynctest"
	"github.com/gokrazy/rsync/internal/rsyncopts"
	"github.com/gokrazy/rsync/internal/rsyncos"
	"github.com/gokrazy/rsync/internal/testlogger"
)

// syncIncGoClientPullFromC is syncGoClientPullFromC with incremental
// recursion forced on. The Go client builds its server option string from the
// pre-negotiation protocol version (gokrazy default 27), so it omits the
// -e.<flags> capability block and a C sender falls back to the complete file
// list — set the version explicitly so the option string advertises 'i' and
// the transfer runs the incremental path this test measures.
func syncIncGoClientPullFromC(t *testing.T, source, dest string, extraArgs ...string) {
	t.Helper()
	env := &rsyncos.Env{Stdout: os.Stdout, Stderr: testlogger.New(t)}
	pc := rsyncopts.NewContext(rsyncopts.NewOptionsWithGokrazyDefaults(env))
	args := append([]string{"-a"}, extraArgs...)
	if err := pc.ParseArguments(env, args); err != nil {
		t.Fatal(err)
	}
	pc.Options.SetProtocolVersion(32)

	cmd := exec.Command(rsynctest.AnyRsync(t), pc.Options.CommandOptions(".")...)
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
	if _, err := maincmd.ClientRun(env, pc.Options, rw, []string{dest}, true); err != nil {
		t.Errorf("client.Run: %v", err)
	}
	if err := cmd.Wait(); err != nil {
		t.Errorf("rsync subprocess: %v", err)
	}
}

// createLargeTree builds a tree like createDeepTree, but with numTop
// top-level directories (45 files + 2 empty dirs each) so the receiver-side
// file list can be scaled into the tens of thousands of entries.
func createLargeTree(t *testing.T, numTop int) string {
	t.Helper()
	root := t.TempDir()
	mkdir := func(rel string) {
		if err := os.MkdirAll(root+"/"+rel, 0755); err != nil {
			t.Fatal(err)
		}
	}
	write := func(rel string) {
		if err := os.WriteFile(root+"/"+rel, []byte("content of "+rel+"\n"), 0644); err != nil {
			t.Fatal(err)
		}
	}
	for d := 0; d < numTop; d++ {
		top := fmt.Sprintf("d%04d", d)
		mkdir(top)
		for _, sub := range []string{"sub1", "sub2"} {
			mkdir(top + "/" + sub)
			for f := 0; f < 10; f++ {
				write(fmt.Sprintf("%s/%s/g%02d.txt", top, sub, f))
			}
		}
		for f := 0; f < 25; f++ {
			write(fmt.Sprintf("%s/f%02d.txt", top, f))
		}
	}
	return root
}

// sampleHeap runs until the stop channel closes. Every 200 ms it forces a GC
// cycle and records the live heap size (HeapAlloc right after runtime.GC()),
// returning the maximum observed. Transient garbage and GC-timing noise are
// factored out this way: the per-entry file-list retention inflates exactly
// the live set, while the raw allocation peak is dominated by short-lived
// decode/transfer buffers and does not discriminate.
func sampleHeap(stop <-chan struct{}) (maxLive uint64) {
	ticker := time.NewTicker(200 * time.Millisecond)
	defer ticker.Stop()
	var ms runtime.MemStats
	for {
		runtime.GC()
		runtime.ReadMemStats(&ms)
		if ms.HeapAlloc > maxLive {
			maxLive = ms.HeapAlloc
		}
		select {
		case <-stop:
			return maxLive
		case <-ticker.C:
		}
	}
}

// TestReceiverMemoryLargeTree pins the incremental-recursion receiver's
// memory footprint on a large tree (the per-entry File structs must be
// released as each file's data arrives, leaving only the names for --delete;
// see docs/receiver-memory.md). It runs the Go client as the receiver
// against a C rsync sender subprocess and asserts on the maximum live heap
// (post-GC) observed in this process during the transfer.
//
// This test must not call t.Parallel: it runs alone before the parallel
// tests start, so parallel package-internal allocations cannot pollute the
// measurement. Skip it with -short; expect it to take a few minutes.
func TestReceiverMemoryLargeTree(t *testing.T) {
	if testing.Short() {
		t.Skip("memory measurement needs a large tree")
	}
	rsynctest.TridgeOrGTFO(t, "receiver memory needs a C rsync sender")

	// 1200 top-level dirs × 45 files ≈ 54k files: above the 1000-entry
	// inc-recurse lookahead window, small enough to build in minutes.
	source := createLargeTree(t, 1200)
	dest := t.TempDir()

	// Collect the garbage of earlier tests so the measurement reflects this
	// transfer, not leftovers.
	runtime.GC()
	runtime.GC()
	stop := make(chan struct{})
	liveCh := make(chan uint64, 1)
	go func() { liveCh <- sampleHeap(stop) }()

	syncIncGoClientPullFromC(t, source, dest)

	close(stop)
	maxLive := <-liveCh

	verifySame(t, source, dest)

	// The pre-optimization receiver kept every received entry (File struct
	// + byNdx slot, ~200 B/entry) for the whole transfer: ~11 MB of live
	// retention at 54k entries on top of the names, the in-flight window
	// and process scaffolding. Post-optimization only the names
	// (~40 B/entry), the directories and the in-flight window stay. The
	// threshold sits between the two.
	if maxLive > 8<<20 {
		t.Errorf("receiver max live heap during transfer = %.1f MB, want <= 8 MB",
			float64(maxLive)/(1<<20))
	}
	t.Logf("receiver max live heap during transfer: %.1f MB",
		float64(maxLive)/(1<<20))

	// A re-sync over the just-built copy transfers no data: every entry is
	// skipped by the generator, and the skip releases must keep the live
	// heap flat. A released-too-soon bug would surface here as an "invalid
	// file index" receiver error (the generator dropped a slot the frame
	// loop still needed).
	stop2 := make(chan struct{})
	live2Ch := make(chan uint64, 1)
	go func() { live2Ch <- sampleHeap(stop2) }()

	syncIncGoClientPullFromC(t, source, dest)

	close(stop2)
	maxLive2 := <-live2Ch

	if maxLive2 > 8<<20 {
		t.Errorf("re-sync max live heap = %.1f MB, want <= 8 MB",
			float64(maxLive2)/(1<<20))
	}
	t.Logf("re-sync max live heap: %.1f MB", float64(maxLive2)/(1<<20))
}
