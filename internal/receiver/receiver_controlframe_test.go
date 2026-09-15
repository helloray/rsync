package receiver

import (
	"bufio"
	"crypto/md5"
	"encoding/binary"
	"io"
	"os"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gokrazy/rsync"
	"github.com/gokrazy/rsync/internal/progress"
	"github.com/gokrazy/rsync/internal/protocol"
	"github.com/gokrazy/rsync/internal/rsyncopts"
	"github.com/gokrazy/rsync/internal/rsyncwire"
)

// TestRecvFilesToleratesNoSend drives the frame loop past an MSG_NO_SEND
// control frame. A C sender replaces a file's whole data block with this
// frame when it fails to open the source (rsync/sender.c:722); the transfer
// must continue with the remaining files instead of aborting.
func TestRecvFilesToleratesNoSend(t *testing.T) {
	dir := t.TempDir()
	rt := newTestTransfer(t, dir)
	rt.Opts = &TransferOpts{
		ProtocolVersion: 30,
		DebugGTE:        func(rsyncopts.DebugLevel, uint16) bool { return false },
	}
	rt.Opts.DebugGTE = func(rsyncopts.DebugLevel, uint16) bool { return false }

	pr, pw := io.Pipe()
	w := &rsyncwire.MultiplexWriter{Writer: pw}
	// Mirror the daemon's 256K buffering (rsyncd/rsyncd.go): the
	// MultiplexReader must be read through a large window, never byte-wise.
	r := bufio.NewReaderSize(&rsyncwire.MultiplexReader{Reader: pr}, 256*1024)
	rt.Conn = &rsyncwire.Conn{
		Writer: w,
		Reader: struct {
			io.Reader
			io.Closer
		}{Reader: r, Closer: pr},
	}

	fileList := []*File{{Name: "a", Ndx: 0}, {Name: "b", Ndx: 1}}

	done := make(chan struct{})
	go func() {
		defer close(done)
		defer pw.Close()
		// ndx 0: the sender could not open the source; its data block is
		// replaced by the 4-byte MSG_NO_SEND(ndx) control frame.
		if _, err := w.WriteMsg(rsyncwire.MsgNoSend, []byte{0, 0, 0, 0}); err != nil {
			t.Errorf("writing NO_SEND frame: %v", err)
			return
		}
		// ndx 1: an attribute-only echo (iflags without ITEM_TRANSFER) —
		// proves the loop resynchronized after the control frame.
		ndxCodec := protocol.NewNdxCodec(30)
		if err := ndxCodec.WriteNdx(w, 1); err != nil {
			t.Errorf("writing ndx 1: %v", err)
			return
		}
		if err := binary.Write(w, binary.LittleEndian, uint16(0)); err != nil {
			t.Errorf("writing iflags: %v", err)
			return
		}
		// protocol 30 uses multi-phase: three DONE markers end the loop.
		for i := 0; i < 3; i++ {
			if err := ndxCodec.WriteNdx(w, protocol.NdxDone); err != nil {
				t.Errorf("writing DONE: %v", err)
				return
			}
		}
	}()

	if err := rt.RecvFiles(fileList); err != nil {
		t.Fatalf("RecvFiles aborted on control frame: %v", err)
	}
	<-done
}

// TestRecvFilesToleratesControlFramesMidStream drives the stream-level filter
// (controlframe.go): mplex control frames can interleave between any two DATA
// frames, including between a file's sum-head and its token stream — where no
// ndx reader is active and every reader (receiveData, recvToken, the trailing
// checksum read) would treat a ControlFrameError as fatal. The frames must be
// routed so the file still arrives, and MSG_IO_ERROR must be booked into
// IOErrors (rsync/io.c:1702-1711) instead of aborting the transfer.
func TestRecvFilesToleratesControlFramesMidStream(t *testing.T) {
	dir := t.TempDir()
	rt := newTestTransfer(t, dir)
	rt.Opts = &TransferOpts{
		ProtocolVersion: 30,
		DebugGTE:        func(rsyncopts.DebugLevel, uint16) bool { return false },
	}
	rt.Session = &protocol.Session{ChecksumAlgo: "md5"}
	rt.Progress = progress.NewPrinter(io.Discard, time.Now)

	pr, pw := io.Pipe()
	w := &rsyncwire.MultiplexWriter{Writer: pw}
	r := bufio.NewReaderSize(&rsyncwire.MultiplexReader{Reader: pr}, 256*1024)
	rt.Conn = &rsyncwire.Conn{
		Writer: w,
		Reader: struct {
			io.Reader
			io.Closer
		}{Reader: r, Closer: pr},
	}
	// Production installs this in ReceiveFileList, before any data is read.
	rt.filterControlFrames()

	content := []byte("hello world\n")
	fileList := []*File{{Name: "a", Ndx: 0}}

	done := make(chan struct{})
	go func() {
		defer close(done)
		defer pw.Close()
		fail := func(stage string, err error) {
			t.Errorf("%s: %v", stage, err)
		}
		// ndx 0 with ITEM_TRANSFER.
		ndxCodec := protocol.NewNdxCodec(30)
		if err := ndxCodec.WriteNdx(w, 0); err != nil {
			fail("writing ndx 0", err)
			return
		}
		if err := binary.Write(w, binary.LittleEndian, uint16(rsync.ITEM_TRANSFER)); err != nil {
			fail("writing iflags", err)
			return
		}
		// Empty sum-head: the receiver falls back to a whole-file transfer.
		var zero [4]byte
		for i := 0; i < 4; i++ {
			if _, err := w.Write(zero[:]); err != nil {
				fail("writing sum-head", err)
				return
			}
		}
		// Control frames interleaved mid-file, before any token data. This
		// is where the unpatched reader died with "mplex control message
		// tag 102 (len 4)".
		if _, err := w.WriteMsg(rsyncwire.MsgIoError, []byte{1, 0, 0, 0}); err != nil {
			fail("writing IO_ERROR", err)
			return
		}
		if _, err := w.WriteMsg(rsyncwire.MsgNoSend, []byte{1, 0, 0, 0}); err != nil {
			fail("writing NO_SEND", err)
			return
		}
		// The file's token stream: raw data token, end-of-file token,
		// then the whole-file checksum receiveData verifies.
		if err := binary.Write(w, binary.LittleEndian, int32(len(content))); err != nil {
			fail("writing token", err)
			return
		}
		if _, err := w.Write(content); err != nil {
			fail("writing token data", err)
			return
		}
		if err := binary.Write(w, binary.LittleEndian, int32(0)); err != nil {
			fail("writing end token", err)
			return
		}
		sum := md5.Sum(content)
		if _, err := w.Write(sum[:]); err != nil {
			fail("writing checksum", err)
			return
		}
		// Protocol 30 multi-phase: three DONE markers.
		for i := 0; i < 3; i++ {
			if err := ndxCodec.WriteNdx(w, protocol.NdxDone); err != nil {
				fail("writing DONE", err)
				return
			}
		}
	}()

	if err := rt.RecvFiles(fileList); err != nil {
		t.Fatalf("RecvFiles aborted on mid-stream control frames: %v", err)
	}
	<-done

	got, err := os.ReadFile(dir + "/a")
	if err != nil {
		t.Fatalf("reading transferred file: %v", err)
	}
	if string(got) != string(content) {
		t.Fatalf("file content mismatch: got %q, want %q", got, content)
	}
	if ioerrs := atomic.LoadInt32(&rt.IOErrors); ioerrs&1 == 0 {
		t.Fatalf("IOErrors = 0x%x, want the IO_ERROR mask (0x1) OR-ed in", ioerrs)
	}
}
