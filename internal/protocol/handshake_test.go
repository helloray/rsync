package protocol

import (
	"bytes"
	"encoding/binary"
	"io"
	"reflect"
	"sync"
	"testing"
)

// memPipe is a buffered, non-blocking pipe (like a socket with a large
// buffer), so that both sides of the handshake can write-before-read without
// deadlocking.
type memPipe struct {
	mu     sync.Mutex
	cond   *sync.Cond
	buf    bytes.Buffer
	closed bool
}

func newMemPipe() *memPipe {
	p := &memPipe{}
	p.cond = sync.NewCond(&p.mu)
	return p
}

func (p *memPipe) Write(b []byte) (int, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.buf.Write(b)
	p.cond.Signal()
	return len(b), nil
}

func (p *memPipe) Read(b []byte) (int, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	for p.buf.Len() == 0 && !p.closed {
		p.cond.Wait()
	}
	return p.buf.Read(b)
}

func (p *memPipe) Close() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.closed = true
	p.cond.Broadcast()
	return nil
}

// wireEnd adapts a read- and a write-pipe to the wireConn interface used by
// the handshake.
type wireEnd struct {
	r *memPipe
	w *memPipe
}

func (e *wireEnd) Read(p []byte) (int, error)  { return e.r.Read(p) }
func (e *wireEnd) Write(p []byte) (int, error) { return e.w.Write(p) }

func (e *wireEnd) WriteInt32(v int32) error {
	return binary.Write(e.w, binary.LittleEndian, v)
}
func (e *wireEnd) ReadInt32() (int32, error) {
	var v int32
	err := binary.Read(e.r, binary.LittleEndian, &v)
	return v, err
}
func (e *wireEnd) WriteVString(s string) error {
	l := len(s)
	if err := e.WriteByte(byte(l)); err != nil {
		return err
	}
	_, err := e.w.Write([]byte(s))
	return err
}
func (e *wireEnd) ReadVString() (string, error) {
	b, err := e.ReadByte()
	if err != nil {
		return "", err
	}
	buf := make([]byte, int(b))
	if _, err := io.ReadFull(e.r, buf); err != nil {
		return "", err
	}
	return string(buf), nil
}
func (e *wireEnd) WriteByte(b byte) error {
	_, err := e.w.Write([]byte{b})
	return err
}
func (e *wireEnd) ReadByte() (byte, error) {
	buf := [1]byte{}
	if _, err := io.ReadFull(e.r, buf[:]); err != nil {
		return 0, err
	}
	return buf[0], nil
}

func TestHandshakeRoundtrip(t *testing.T) {
	tests := []struct {
		name         string
		clientInfo   string
		allowIncRecur bool
		version      int
	}{
		{"modern_full", ".LfsxCvIu", true, 30},
		{"modern_no_inc", ".LfsxCvIu", false, 30},
		{"modern_minimal", ".fCv", true, 30},
		{"legacy_29", ".LfsxCvIu", true, 29},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			// serverToClient and clientToServer are the two directions.
			serverToClient := newMemPipe()
			clientToServer := newMemPipe()
			defer serverToClient.Close()
			defer clientToServer.Close()
			serverEnd := &wireEnd{r: clientToServer, w: serverToClient}
			clientEnd := &wireEnd{r: serverToClient, w: clientToServer}

			const seed = uint32(0xDEADBEEF)
			var serverSess, clientSess *Session
			var serverErr, clientErr error
			done := make(chan struct{})
			go func() {
				defer close(done)
				serverSess, serverErr = ServerHandshake(serverEnd, HandshakeParams{
					Version:         tc.version,
					ClientInfo:      tc.clientInfo,
					AllowIncRecurse: tc.allowIncRecur,
				}, seed)
			}()
			clientSess, clientErr = ClientHandshake(clientEnd, HandshakeParams{
				Version: tc.version,
			})
			<-done

			if serverErr != nil {
				t.Fatalf("ServerHandshake: %v", serverErr)
			}
			if clientErr != nil {
				t.Fatalf("ClientHandshake: %v", clientErr)
			}
			if serverSess.Version != tc.version || clientSess.Version != tc.version {
				t.Fatalf("version = %d/%d, want %d", serverSess.Version, clientSess.Version, tc.version)
			}
			if serverSess.Version < 30 {
				if serverSess.ChecksumAlgo != "md4" || clientSess.ChecksumAlgo != "md4" {
					t.Fatalf("legacy checksum = %q/%q, want md4", serverSess.ChecksumAlgo, clientSess.ChecksumAlgo)
				}
				return
			}
			if serverSess.Compat != clientSess.Compat {
				t.Fatalf("compat mismatch: server=%#x client=%#x", serverSess.Compat.Bits(), clientSess.Compat.Bits())
			}
			if !reflect.DeepEqual(serverSess, clientSess) {
				t.Fatalf("sessions differ:\nserver=%+v\nclient=%+v", serverSess, clientSess)
			}
			if clientSess.ChecksumSeed != seed {
				t.Errorf("seed = %#x, want %#x", clientSess.ChecksumSeed, seed)
			}
			if !clientSess.Compat.Contains(CFVarintFlistFlags) {
				t.Errorf("client_info %q should set CFVarintFlistFlags", tc.clientInfo)
			}
		})
	}
}

func TestServerCompatFlagsMap(t *testing.T) {
	got := serverCompatFlags(HandshakeParams{
		ClientInfo:      ".LfsxCvIu",
		AllowIncRecurse: true,
	})
	want := CFIncRecurse | CFSymlinkTimes | CFSafeFlist | CFAvoidXattrOptim |
		CFChksumSeedFix | CFInplacePartialDir | CFVarintFlistFlags | CFId0Names
	if got != want {
		t.Errorf("serverCompatFlags = %#x, want %#x", got.Bits(), want.Bits())
	}
	// No inc-recurse means no CFIncRecurse bit.
	got2 := serverCompatFlags(HandshakeParams{ClientInfo: ".LfsxCvIu", AllowIncRecurse: false})
	if got2.Contains(CFIncRecurse) {
		t.Error("CFIncRecurse set without AllowIncRecurse")
	}
}