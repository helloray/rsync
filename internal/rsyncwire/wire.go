package rsyncwire

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"io"
	"time"

	"github.com/gokrazy/rsync/internal/rsyncos"
)

const (
	// Message codes mirror rsync.h: `enum msgcode` (MPLEX_BASE=7 is added
	// to these to form the wire tag byte).
	MsgData       uint8 = 0
	MsgErrorXfer  uint8 = 1
	MsgInfo       uint8 = 2
	MsgError      uint8 = 3
	MsgWarning    uint8 = 4
	MsgErrorSock  uint8 = 5
	MsgLog        uint8 = 6
	MsgClient     uint8 = 7
	MsgErrorUtf8  uint8 = 8
	MsgRedo       uint8 = 9
	MsgStats      uint8 = 10
	MsgIoError    uint8 = 22
	MsgIoTimeout  uint8 = 33
	MsgNoOp       uint8 = 42
	MsgErrorExit  uint8 = 86
	MsgSuccess    uint8 = 100
	MsgDeleted    uint8 = 101
	MsgNoSend     uint8 = 102
)

const mplexBase = 7

// ControlFrameError reports that a multiplex frame carrying a control message
// (per-file Success/Deleted/NoSend, ErrorExit, Stats, or similar) was read
// where DATA was expected. The receiver routing layer can inspect Tag and
// Payload to dispatch the message. At protocol < 30 these frames never appear
// where the io.Reader view is used, so plain < 30 transfers never see them.
type ControlFrameError struct {
	Tag     uint8
	Payload []byte
}

func (e *ControlFrameError) Error() string {
	return fmt.Sprintf("mplex control message tag %d (len %d)", e.Tag, len(e.Payload))
}

// IsSwallowable reports whether the frame carries no data of its own and
// should be consumed and dropped by the DATA reader: a keep-alive no-op, an
// informational message we already logged, or an empty DATA heartbeat.
func isSwallowable(tag uint8, payload []byte) bool {
	switch tag {
	case MsgInfo, MsgNoOp:
		return true
	case MsgIoTimeout:
		// A daemon announces its --timeout at protocol >= 31
		// (rsync/main.c:1304); C adopts it only as a stricter cap of the
		// client's own timeout. We run no idle watchdog, so there is nothing
		// to shorten — consume the frame so it never surfaces as a spurious
		// control error in the data stream.
		return true
	case MsgData:
		return len(payload) == 0
	}
	return false
}

type MultiplexWriter struct {
	Writer io.WriteCloser
}

func (w *MultiplexWriter) Write(p []byte) (n int, err error) {
	return w.WriteMsg(MsgData, p)
}

func (w *MultiplexWriter) WriteMsg(tag uint8, p []byte) (n int, err error) {
	header := uint32(mplexBase+tag)<<24 | uint32(len(p))
	// log.Printf("len %d (hex %x)", len(p), uint32(len(p)))
	// log.Printf("header=%v (%x)", header, header)
	if err := binary.Write(w.Writer, binary.LittleEndian, header); err != nil {
		return 0, err
	}
	return w.Writer.Write(p)
}

func (w *MultiplexWriter) Close() error { return w.Writer.Close() }

type MultiplexReader struct {
	Env    *rsyncos.Env
	Reader io.Reader
}

// rsync.h defines IO_BUFFER_SIZE as 32 * 1024, but gokr-rsyncd increases it to
// 256K. Since we use this as the maximum message size, too, we need to at least
// match it.
const ioBufferSize = 256 * 1024
const maxMessageSize = ioBufferSize

func (w *MultiplexReader) ReadMsg() (tag uint8, p []byte, err error) {
	var header uint32
	if err := binary.Read(w.Reader, binary.LittleEndian, &header); err != nil {
		return 0, nil, err
	}

	tag = uint8(header>>24) - mplexBase
	length := header & 0x00FFFFFF
	if length > maxMessageSize {
		// NOTE: if you run into this error, one alternative to bumping
		// maxMessageSize is to restructure the program to work with i/o buffer
		// windowing.
		return 0, nil, fmt.Errorf("length %d exceeds max message size (%d)", length, maxMessageSize)
	}
	p = make([]byte, int(length))
	if _, err := io.ReadFull(w.Reader, p); err != nil {
		return 0, nil, err
	}
	// log.Printf("header=%v (%x), tag=%v, length=%v", header, header, tag, length)
	// log.Printf("payload=%x / %q", p, p)
	return tag, p, nil
}

func (w *MultiplexReader) Read(p []byte) (n int, err error) {
	tag, payload, err := w.ReadMsg()
	if err != nil {
		return 0, err
	}

	// Fatal/error messages terminate the session; surface them. MSG_IO_ERROR
	// is deliberately NOT here: C carries a 4-byte io_error bitmask in it and
	// merely ORs it into the local io_error counter (rsync/io.c:1702-1711,
	// sent by the sender after a phase with failures, rsync/sender.c:809) —
	// treating it as fatal would abort every transfer in which the sender
	// logged a per-file error like "file has vanished".
	switch tag {
	case MsgError, MsgErrorXfer:
		return 0, fmt.Errorf("rsync error (msg tag %d): %s", tag, payload)
	case MsgErrorExit:
		// rsync/io.c:read_a_msg accepts a 4-byte exit code or an empty
		// payload; anything else is a framing error.
		if len(payload) == 4 {
			return 0, fmt.Errorf("rsync error: peer aborted the transfer (exit code %d)", binary.LittleEndian.Uint32(payload))
		}
		if len(payload) != 0 {
			return 0, fmt.Errorf("rsync error: invalid MSG_ERROR_EXIT payload (%d bytes)", len(payload))
		}
		return 0, fmt.Errorf("rsync error: peer aborted the transfer")
	}

	// Frames that carry no data of their own (keep-alive, info we logged, or
	// an empty DATA heartbeat) are consumed and dropped so the next Read
	// sees the following frame (io.ReadFull handles 0-,nil reads by retrying).
	if isSwallowable(tag, payload) {
		if tag == MsgInfo {
			w.Env.Logf("info: %s", payload)
		}
		return 0, nil
	}

	// Per-file control messages (Success/Deleted/NoSend), Stats, and any
	// other control frame carry data that must be routed to the correct
	// consumer; they cannot be silently discarded as DATA.
	switch tag {
	case MsgData:
		// continues below
	case MsgSuccess, MsgDeleted, MsgNoSend, MsgRedo, MsgStats, MsgIoError,
		MsgErrorSock, MsgLog, MsgClient, MsgErrorUtf8, MsgWarning, MsgIoTimeout:
		return 0, &ControlFrameError{Tag: tag, Payload: payload}
	default:
		return 0, fmt.Errorf("unexpected msg tag: got %v, want %v", tag, MsgData)
	}

	if len(p) < len(payload) {
		panic(fmt.Sprintf("not enough buffer space! %d < %d", len(p), len(payload)))
	}
	return copy(p, payload), nil
}

type Buffer struct {
	// buf.Write() never fails, making for a convenient API.
	buf bytes.Buffer
}

func (b *Buffer) WriteByte(data byte) error {
	binary.Write(&b.buf, binary.LittleEndian, data)
	return nil
}

func (b *Buffer) WriteInt32(data int32) {
	binary.Write(&b.buf, binary.LittleEndian, data)
}

// WriteShortint writes a 2-byte little-endian value,
// like rsync/io.c:write_shortint().
func (b *Buffer) WriteShortint(data uint16) {
	binary.Write(&b.buf, binary.LittleEndian, data)
}

func (b *Buffer) WriteInt64(data int64) {
	// send as a 32-bit integer if possible
	if data <= 0x7FFFFFFF && data >= 0 {
		b.WriteInt32(int32(data))
		return
	}
	// otherwise, send -1 followed by the 64-bit integer
	b.WriteInt32(-1)
	binary.Write(&b.buf, binary.LittleEndian, data)
}

func (b *Buffer) WriteString(data string) {
	io.WriteString(&b.buf, data)
}

func (b *Buffer) String() string {
	return b.buf.String()
}

func (b *Buffer) Reset() {
	b.buf.Reset()
}

type Conn struct {
	Writer io.WriteCloser
	Reader io.ReadCloser
}

func (c *Conn) Read(p []byte) (int, error)  { return c.Reader.Read(p) }
func (c *Conn) Write(p []byte) (int, error) { return c.Writer.Write(p) }

func (c *Conn) Close() error {
	wcErr := c.Writer.Close()
	rcErr := c.Reader.Close()
	if wcErr != nil {
		return wcErr
	}
	return rcErr
}

// GracefulAbortClose drains the connection before closing it, for use on an
// abort path that has just sent MSG_ERROR/MSG_ERROR_EXIT frames. Closing a
// socket while the peer still has data in flight sends a RST, and a RST
// discards the peer's received-but-unread data — erasing the frames just sent,
// so the peer only sees "connection reset by peer" instead of the abort
// reason. (C rsync keeps its reader alive for the same reason via
// rsync/io.c:noop_io_until_death after cleanup.c sends MSG_ERROR_EXIT.)
//
// Reads are discarded until the peer closes its end (EOF) or drain elapses,
// whichever comes first; the watchdog Close unblocks a reader stuck on a peer
// that never finishes. Must not be called while another goroutine is still
// reading from the connection.
func (c *Conn) GracefulAbortClose(drain time.Duration) {
	done := make(chan struct{})
	go func() {
		defer close(done)
		buf := make([]byte, 32*1024)
		for {
			if _, err := c.Reader.Read(buf); err != nil {
				return
			}
		}
	}()
	timer := time.NewTimer(drain)
	defer timer.Stop()
	select {
	case <-done:
		c.Close()
	case <-timer.C:
		// The peer never finished: close to unblock the drain goroutine,
		// then wait for it to exit before returning.
		c.Close()
		<-done
	}
}

// msgWriter is implemented by writers that can carry multiplex control
// frames. CountingWriter forwards it to the underlying MultiplexWriter, so
// Conn helpers below work no matter how many wrappers surround it.
type msgWriter interface {
	WriteMsg(tag uint8, p []byte) (n int, err error)
}

// RERRPartial is the exit code (rsync/errcode.h) a side sends in its
// MSG_ERROR_EXIT frame when the transfer aborted mid-way because files could
// not be transferred ("some files/attrs were not transferred").
const RERRPartial = 23

// SendError best-effort sends the msg text as an MSG_ERROR control frame,
// mirroring C's rprintf(FERROR, ...) in a remote session (rsync/log.c): the
// text rides the message channel and the peer prints it. It is a no-op when
// the write direction is not multiplexed (raw streams below protocol 30
// cannot carry control frames).
func (c *Conn) SendError(msg string) error {
	return sendControlMsg(c.Writer, MsgError, []byte(msg))
}

// SendErrorExit best-effort sends an MSG_ERROR_EXIT control frame carrying
// the 4-byte exit code, mirroring C's cleanup.c send_msg_int(MSG_ERROR_EXIT,
// exit_code). rsync/io.c:read_a_msg accepts only a 4-byte or empty payload
// and rejects anything else as "invalid multi-message", so the abort reason
// must go out in a preceding SendError frame instead. The peer surfaces the
// frame as a fatal error instead of hanging or seeing a bare EOF. It is a
// no-op when the write direction is not multiplexed.
func (c *Conn) SendErrorExit(exitCode int) error {
	var b [4]byte
	binary.LittleEndian.PutUint32(b[:], uint32(exitCode))
	return sendControlMsg(c.Writer, MsgErrorExit, b[:])
}

// SendNoSend best-effort sends an MSG_NO_SEND control frame carrying the
// 4-byte flist index (rsync/sender.c:send_msg_int(MSG_NO_SEND, ndx)): the
// sender could not open the source file, so this frame replaces the file's
// data block and the peer's generator releases the entry
// (rsync/io.c:read_a_msg:1809). It is a no-op when the write direction is
// not multiplexed (raw streams below protocol 30 cannot carry it, matching
// C's `if (protocol_version >= 30)` guard).
func (c *Conn) SendNoSend(ndx int32) error {
	var b [4]byte
	binary.LittleEndian.PutUint32(b[:], uint32(ndx))
	return sendControlMsg(c.Writer, MsgNoSend, b[:])
}

// SendIoTimeout best-effort sends an MSG_IO_TIMEOUT control frame announcing
// the daemon's --timeout, mirroring rsync/main.c:1304 (am_daemon && io_timeout
// && protocol_version >= 31). The peer may adopt it only as a stricter cap of
// its own timeout.
func (c *Conn) SendIoTimeout(seconds int) error {
	var b [4]byte
	binary.LittleEndian.PutUint32(b[:], uint32(seconds))
	return sendControlMsg(c.Writer, MsgIoTimeout, b[:])
}

func sendControlMsg(w io.Writer, tag uint8, p []byte) error {
	mw, ok := w.(msgWriter)
	if !ok {
		return nil
	}
	_, err := mw.WriteMsg(tag, p)
	return err
}

func (c *Conn) WriteByte(data byte) error {
	return binary.Write(c.Writer, binary.LittleEndian, data)
}

func (c *Conn) WriteInt32(data int32) error {
	return binary.Write(c.Writer, binary.LittleEndian, data)
}

// WriteShortint writes a 2-byte little-endian value,
// like rsync/io.c:write_shortint().
func (c *Conn) WriteShortint(data uint16) error {
	return binary.Write(c.Writer, binary.LittleEndian, data)
}

func (c *Conn) WriteInt64(data int64) error {
	// send as a 32-bit integer if possible
	if data <= 0x7FFFFFFF && data >= 0 {
		return c.WriteInt32(int32(data))
	}
	// otherwise, send -1 followed by the 64-bit integer
	if err := c.WriteInt32(-1); err != nil {
		return err
	}
	return binary.Write(c.Writer, binary.LittleEndian, data)
}

func (c *Conn) WriteString(data string) error {
	_, err := io.WriteString(c.Writer, data)
	return err
}

// WriteVString writes a length-prefixed string using the vstring encoding
// (rsync/io.c:write_vstring): a single length byte for lengths <= 0x7F,
// otherwise two bytes (0x80|len>>8, len&0xFF). Note that this encoding is
// different from (future) varint-prefixed strings.
func (c *Conn) WriteVString(data string) error {
	l := len(data)
	if l > 0x7FFF {
		return fmt.Errorf("vstring of length %d exceeds 0x7FFF", l)
	}
	if l <= 0x7F {
		if err := c.WriteByte(byte(l)); err != nil {
			return err
		}
	} else {
		if err := c.WriteByte(byte(0x80 | l>>8)); err != nil {
			return err
		}
		if err := c.WriteByte(byte(l & 0xFF)); err != nil {
			return err
		}
	}
	return c.WriteString(data)
}

// ReadVString reads a vstring-encoded string (see WriteVString).
func (c *Conn) ReadVString() (string, error) {
	b, err := c.ReadByte()
	if err != nil {
		return "", err
	}
	l := int(b)
	if b&0x80 != 0 {
		b2, err := c.ReadByte()
		if err != nil {
			return "", err
		}
		l = int(b&0x7F)<<8 | int(b2)
	}
	buf := make([]byte, l)
	if _, err := io.ReadFull(c.Reader, buf); err != nil {
		return "", err
	}
	return string(buf), nil
}

func (c *Conn) ReadByte() (byte, error) {
	var buf [1]byte
	if _, err := io.ReadFull(c.Reader, buf[:]); err != nil {
		return 0, err
	}
	return buf[0], nil
}

func (c *Conn) ReadInt32() (int32, error) {
	var buf [4]byte
	if _, err := io.ReadFull(c.Reader, buf[:]); err != nil {
		return 0, err
	}
	return int32(binary.LittleEndian.Uint32(buf[:])), nil
}

// ReadShortint reads a 2-byte little-endian value,
// like rsync/io.c:read_shortint().
func (c *Conn) ReadShortint() (uint16, error) {
	var buf [2]byte
	if _, err := io.ReadFull(c.Reader, buf[:]); err != nil {
		return 0, err
	}
	return binary.LittleEndian.Uint16(buf[:]), nil
}

func (c *Conn) ReadInt64() (int64, error) {
	{
		data, err := c.ReadInt32()
		if err != nil {
			return 0, err
		}
		if data != -1 {
			// The value was small enough to fit into a 32 bit int, so it was
			// transferred directly.
			return int64(data), nil
		}
		// Otherwise, -1 was transmitted, followed by the int64.
	}
	var data int64
	if err := binary.Read(c.Reader, binary.LittleEndian, &data); err != nil {
		return 0, err
	}
	return data, nil
}

type CountingReader struct {
	R         io.ReadCloser
	BytesRead int64
}

func (r *CountingReader) Read(p []byte) (n int, err error) {
	n, err = r.R.Read(p)
	r.BytesRead += int64(n)
	return n, err
}

func (r *CountingReader) Close() error { return r.R.Close() }

type CountingWriter struct {
	W            io.WriteCloser
	BytesWritten int64
}

func (w *CountingWriter) Write(p []byte) (n int, err error) {
	n, err = w.W.Write(p)
	w.BytesWritten += int64(n)
	return n, err
}

// WriteMsg forwards multiplex control frames to the underlying writer so
// Conn.SendErrorExit / Conn.SendIoTimeout reach the wire through this wrapper.
func (w *CountingWriter) WriteMsg(tag uint8, p []byte) (int, error) {
	mw, ok := w.W.(msgWriter)
	if !ok {
		return 0, nil
	}
	return mw.WriteMsg(tag, p)
}

func (w *CountingWriter) Close() error { return w.W.Close() }

func CounterPair(r io.ReadCloser, w io.WriteCloser) (*CountingReader, *CountingWriter) {
	crd := &CountingReader{R: r}
	cwr := &CountingWriter{W: w}
	return crd, cwr
}
