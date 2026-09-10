package rsyncwire

import (
	"bytes"
	"encoding/binary"
	"errors"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gokrazy/rsync/internal/rsyncos"
)

// discardWC is an io.WriteCloser that drops everything, satisfying Env.Stderr.
type discardWC struct{}

func (discardWC) Write(p []byte) (int, error) { return len(p), nil }
func (discardWC) Close() error                { return nil }

var _ io.WriteCloser = discardWC{}

// nopWriteCloser turns an io.Writer into an io.WriteCloser.
type nopWriteCloser struct{ io.Writer }

func (nopWriteCloser) Close() error { return nil }

// encodeMsg builds a single multiplex frame: (mplexBase+tag)<<24 | len, LE,
// followed by the payload.
func encodeMsg(tag uint8, payload []byte) []byte {
	var b bytes.Buffer
	header := uint32(mplexBase+tag)<<24 | uint32(len(payload))
	binary.Write(&b, binary.LittleEndian, header)
	b.Write(payload)
	return b.Bytes()
}

func readAll(t *testing.T, mrd *MultiplexReader) (total []byte, control []*ControlFrameError) {
	t.Helper()
	buf := make([]byte, 4096)
	for {
		n, err := mrd.Read(buf)
		total = append(total, buf[:n]...)
		if err != nil {
			var cfe *ControlFrameError
			if errors.As(err, &cfe) {
				control = append(control, cfe)
				continue
			}
			return total, control
		}
	}
}

func TestMultiplexReadSwallowsHeartbeats(t *testing.T) {
	// Feed: empty DATA heartbeat, NoOp, Info, then DATA with payload.
	var stream bytes.Buffer
	stream.Write(encodeMsg(MsgData, nil))  // empty heartbeat
	stream.Write(encodeMsg(MsgNoOp, nil))  // keep-alive
	stream.Write(encodeMsg(MsgInfo, []byte("ping")))
	stream.Write(encodeMsg(MsgData, []byte("hello")))

	mrd := &MultiplexReader{Env: &rsyncos.Env{Stderr: discardWC{}}, Reader: &stream}
	total, control := readAll(t, mrd)
	if len(control) != 0 {
		t.Fatalf("unexpected control frames: %+v", control)
	}
	if string(total) != "hello" {
		t.Fatalf("read data = %q, want %q", total, "hello")
	}
}

func TestMultiplexReadRoutesControlFrames(t *testing.T) {
	var stream bytes.Buffer
	stream.Write(encodeMsg(MsgData, []byte("a")))
	stream.Write(encodeMsg(MsgSuccess, []byte{1, 0, 0, 0})) // per-file success ndx 1
	stream.Write(encodeMsg(MsgDeleted, []byte{2, 0, 0, 0}))
	stream.Write(encodeMsg(MsgData, []byte("b")))
	stream.Write(encodeMsg(MsgNoSend, []byte{3, 0, 0, 0}))

	mrd := &MultiplexReader{Env: &rsyncos.Env{Stderr: discardWC{}}, Reader: &stream}
	total, control := readAll(t, mrd)
	if string(total) != "ab" {
		t.Fatalf("read data = %q, want %q", total, "ab")
	}
	if len(control) != 3 {
		t.Fatalf("expected 3 control frames, got %d: %+v", len(control), control)
	}
	if control[0].Tag != MsgSuccess {
		t.Errorf("frame 0 tag = %d, want Success(%d)", control[0].Tag, MsgSuccess)
	}
	if control[1].Tag != MsgDeleted {
		t.Errorf("frame 1 tag = %d, want Deleted(%d)", control[1].Tag, MsgDeleted)
	}
	if control[2].Tag != MsgNoSend {
		t.Errorf("frame 2 tag = %d, want NoSend(%d)", control[2].Tag, MsgNoSend)
	}
}

func TestMultiplexReadErrorFrame(t *testing.T) {
	var stream bytes.Buffer
	stream.Write(encodeMsg(MsgData, []byte("x")))
	stream.Write(encodeMsg(MsgError, []byte("boom")))

	mrd := &MultiplexReader{Env: &rsyncos.Env{Stderr: discardWC{}}, Reader: &stream}
	total, control := readAll(t, mrd)
	if string(total) != "x" {
		t.Fatalf("read data = %q, want %q", total, "x")
	}
	if len(control) != 0 {
		t.Fatalf("expected no control frames, got %+v", control)
	}
}

// TestSendErrorExitWireFormat pins the abort-path framing to what C rsync
// accepts (rsync/io.c:read_a_msg): the reason rides an MSG_ERROR text frame,
// and MSG_ERROR_EXIT carries exactly a 4-byte exit code.
func TestSendErrorExitWireFormat(t *testing.T) {
	var stream bytes.Buffer
	conn := &Conn{
		Writer: &MultiplexWriter{Writer: nopWriteCloser{&stream}},
		Reader: io.NopCloser(&bytes.Reader{}),
	}
	if err := conn.SendError("receiver boom"); err != nil {
		t.Fatal(err)
	}
	if err := conn.SendErrorExit(RERRPartial); err != nil {
		t.Fatal(err)
	}

	tag, payload, err := (&MultiplexReader{Reader: &stream}).ReadMsg()
	if err != nil {
		t.Fatal(err)
	}
	if tag != MsgError || string(payload) != "receiver boom" {
		t.Fatalf("frame 0 = (tag %d, %q), want (Error(%d), \"receiver boom\")", tag, payload, MsgError)
	}
	tag, payload, err = (&MultiplexReader{Reader: &stream}).ReadMsg()
	if err != nil {
		t.Fatal(err)
	}
	if tag != MsgErrorExit {
		t.Fatalf("frame 1 tag = %d, want ErrorExit(%d)", tag, MsgErrorExit)
	}
	if len(payload) != 4 || binary.LittleEndian.Uint32(payload) != RERRPartial {
		t.Fatalf("frame 1 payload = %v, want 4-byte little-endian %d", payload, RERRPartial)
	}
}

func TestMultiplexReadErrorExit(t *testing.T) {
	for _, tc := range []struct {
		name    string
		payload []byte
		want    string
	}{
		{"exit code", encodeMsg(MsgErrorExit, []byte{23, 0, 0, 0}), "exit code 23"},
		{"empty", encodeMsg(MsgErrorExit, nil), "peer aborted"},
		{"invalid payload", encodeMsg(MsgErrorExit, []byte("109-byte error text would be rejected by C")), "invalid MSG_ERROR_EXIT payload"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var stream bytes.Buffer
			stream.Write(tc.payload)
			mrd := &MultiplexReader{Env: &rsyncos.Env{Stderr: discardWC{}}, Reader: &stream}
			_, err := mrd.Read(make([]byte, 16))
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("Read err = %v, want it to contain %q", err, tc.want)
			}
		})
	}
}

// blockingReadCloser simulates a peer that keeps the connection open: Read
// blocks until Close is called (like a net.Conn read unblocked by a
// concurrent Close), then reports EOF.
type blockingReadCloser struct {
	unblock chan struct{}
	closeFn sync.Once
}

func newBlockingReadCloser() *blockingReadCloser {
	return &blockingReadCloser{unblock: make(chan struct{})}
}

func (b *blockingReadCloser) Read(p []byte) (int, error) {
	<-b.unblock
	return 0, io.EOF
}

func (b *blockingReadCloser) Close() error {
	b.closeFn.Do(func() { close(b.unblock) })
	return nil
}

// eofReadCloser returns the given data once and then EOF, remembering
// whether Close was called.
type eofReadCloser struct {
	data   []byte
	off    int
	closed bool
}

func (e *eofReadCloser) Read(p []byte) (int, error) {
	if e.off < len(e.data) {
		n := copy(p, e.data[e.off:])
		e.off += n
		return n, nil
	}
	return 0, io.EOF
}

func (e *eofReadCloser) Close() error { e.closed = true; return nil }

// nopCloserWC records Close calls on the write side.
type recordingWC struct {
	io.Writer
	closed bool
}

func (r *recordingWC) Close() error { r.closed = true; return nil }

func TestGracefulAbortCloseDrainsThenCloses(t *testing.T) {
	var stream bytes.Buffer
	rwc := &recordingWC{Writer: &stream}
	rd := &eofReadCloser{data: []byte("in-flight peer data")}
	conn := &Conn{Writer: rwc, Reader: rd}

	start := time.Now()
	conn.GracefulAbortClose(5 * time.Second)
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("GracefulAbortClose took %v with an EOF peer, want immediate return", elapsed)
	}
	if !rd.closed || !rwc.closed {
		t.Fatalf("GracefulAbortClose did not close both directions (reader closed=%v, writer closed=%v)", rd.closed, rwc.closed)
	}
	if stream.Len() != 0 {
		// Nothing was ever written through the writer; this only guards that
		// Close ran on it.
		t.Fatalf("unexpected bytes written: %q", stream.String())
	}
}

func TestGracefulAbortCloseWatchdog(t *testing.T) {
	rd := newBlockingReadCloser()
	conn := &Conn{Writer: nopWriteCloser{io.Discard}, Reader: rd}

	start := time.Now()
	// Drain timeout far below the go test default panic timeout: a hang would
	// fail the whole run, this returns within ~100ms.
	conn.GracefulAbortClose(100 * time.Millisecond)
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Fatalf("GracefulAbortClose took %v against a stuck peer, want the watchdog to fire", elapsed)
	}
	select {
	case <-rd.unblock:
		// Close unblocked the reader, as intended.
	default:
		t.Fatalf("watchdog did not close the connection")
	}
}

func TestGracefulAbortCloseDrainsAllData(t *testing.T) {
	// A peer that sends a burst of data and then closes: the drain must
	// consume everything (discarded) and end on the peer's EOF, not the
	// watchdog.
	var stream bytes.Buffer
	payload := bytes.Repeat([]byte("x"), 128*1024)
	rd := &eofReadCloser{data: payload}
	conn := &Conn{Writer: nopWriteCloser{&stream}, Reader: rd}

	conn.GracefulAbortClose(5 * time.Second)
	if !rd.closed {
		t.Fatalf("connection was not closed after the peer EOFed")
	}
	if rd.off != len(rd.data) {
		t.Fatalf("drain read %d of %d bytes", rd.off, len(rd.data))
	}
}