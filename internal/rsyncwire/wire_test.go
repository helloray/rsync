package rsyncwire

import (
	"bytes"
	"encoding/binary"
	"errors"
	"io"
	"testing"

	"github.com/gokrazy/rsync/internal/rsyncos"
)

// discardWC is an io.WriteCloser that drops everything, satisfying Env.Stderr.
type discardWC struct{}

func (discardWC) Write(p []byte) (int, error) { return len(p), nil }
func (discardWC) Close() error                { return nil }

var _ io.WriteCloser = discardWC{}

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