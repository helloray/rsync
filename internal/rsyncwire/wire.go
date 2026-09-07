package rsyncwire

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"io"

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

	// Fatal/error messages terminate the session; surface them.
	switch tag {
	case MsgError, MsgErrorXfer, MsgIoError, MsgErrorExit:
		return 0, fmt.Errorf("rsync error (msg tag %d): %s", tag, payload)
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
	case MsgSuccess, MsgDeleted, MsgNoSend, MsgRedo, MsgStats,
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

func (b *Buffer) WriteByte(data byte) {
	binary.Write(&b.buf, binary.LittleEndian, data)
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

func (w *CountingWriter) Close() error { return w.W.Close() }

func CounterPair(r io.ReadCloser, w io.WriteCloser) (*CountingReader, *CountingWriter) {
	crd := &CountingReader{R: r}
	cwr := &CountingWriter{W: w}
	return crd, cwr
}
