package protocol

import (
	"fmt"
	"io"
	"strings"
)

// wireConn is the minimal raw-stream interface used during the binary
// handshake, before multiplexing is engaged. *rsyncwire.Conn satisfies it.
type wireConn interface {
	io.Reader
	io.Writer
	WriteInt32(int32) error
	ReadInt32() (int32, error)
	WriteVString(string) error
	ReadVString() (string, error)
	ReadByte() (byte, error)
}

// HandshakeParams carries the options that shape one side of the binary
// handshake (rsync/compat.c:setup_protocol + negotiate_the_strings).
type HandshakeParams struct {
	// Version is the already-negotiated protocol version. For the command
	// (ssh) path this is produced by negotiateProtocolVersion; for the
	// daemon path the caller derives it from the @RSYNCD: greeting.
	Version int

	// ClientInfo is the content of the -e option received from the peer
	// (server side), e.g. ".LfsxCvIu". The leading 'e' is already stripped.
	ClientInfo string

	// AllowIncRecurse reports whether this side may use incremental
	// recursion (server sets CFIncRecurse only if both sides allow it).
	AllowIncRecurse bool

	// DoCompression mirrors rsync's -z: whether the compression algorithm
	// list is negotiated. gokrazy rsync does not support compression, so
	// this is always false in practice.
	DoCompression bool
}

// ChecksumList is the checksum algorithms we can produce, strongest first.
// The handshake negotiates the strongest mutual algorithm. Only md4 is
// currently implemented, which rsync continues to accept at protocol >= 30.
var ChecksumList = []string{"md4"}

// negotiateProtocolVersion performs the binary version exchange
// (rsync/compat.c:setup_protocol): both sides write their maximum version,
// then read the peer's, and the negotiated version is the lower of the two.
func negotiateProtocolVersion(c wireConn, ourVersion int) (int, error) {
	if err := c.WriteInt32(int32(ourVersion)); err != nil {
		return 0, err
	}
	remote, err := c.ReadInt32()
	if err != nil {
		return 0, err
	}
	if remote < 27 {
		return 0, fmt.Errorf("protocol version mismatch: remote %d is older than minimum 27", remote)
	}
	return int(min(int32(ourVersion), remote)), nil
}

// serverCompatFlags builds the compatibility-flags bitmask the server sends,
// from its own capabilities and the client's -e information
// (rsync/compat.c:setup_protocol, the protocol_version >= 30 branch).
func serverCompatFlags(p HandshakeParams) CompatibilityFlags {
	var compat CompatibilityFlags
	if p.AllowIncRecurse {
		compat |= CFIncRecurse
	}
	compat |= CFSymlinkTimes // the receiver can set symlink mtimes
	if strings.ContainsRune(p.ClientInfo, 'f') {
		compat |= CFSafeFlist
	}
	if strings.ContainsRune(p.ClientInfo, 'x') {
		compat |= CFAvoidXattrOptim
	}
	if strings.ContainsRune(p.ClientInfo, 'C') {
		compat |= CFChksumSeedFix
	}
	if strings.ContainsRune(p.ClientInfo, 'I') {
		compat |= CFInplacePartialDir
	}
	if strings.ContainsRune(p.ClientInfo, 'u') {
		compat |= CFId0Names
	}
	if strings.ContainsRune(p.ClientInfo, 'v') {
		compat |= CFVarintFlistFlags
	}
	return compat
}

// tryNegotiate picks the strongest algorithm from ourList that also appears
// in peerList: honest peers exchange strongest-first lists and each picks the
// strongest mutual one, converging on the same choice
// (rsync/compat.c:parse_negotiate_str).
func tryNegotiate(ourList, peerList []string) (string, bool) {
	peer := make(map[string]bool, len(peerList))
	for _, name := range peerList {
		peer[name] = true
	}
	for _, name := range ourList {
		if peer[name] {
			return name, true
		}
	}
	return "", false
}

// negotiateStrings performs the vstring capability exchange
// (rsync/compat.c:negotiate_the_strings). When the modern vstring path is
// active (CFVarintFlistFlags), each side sends its checksum list (and, with
// compression, its compression list) before reading the peer's; otherwise the
// non-negotiated default checksum applies and nothing is exchanged.
func negotiateStrings(c wireConn, p HandshakeParams, modern bool) (string, error) {
	if !modern {
		return defaultChecksum(p.Version), nil
	}

	// Send all our lists first, then read the peer's (helps slow startup).
	if err := c.WriteVString(strings.Join(ChecksumList, " ")); err != nil {
		return "", err
	}
	if p.DoCompression {
		if err := c.WriteVString("none"); err != nil {
			return "", err
		}
	}
	peerChecksum, err := c.ReadVString()
	if err != nil {
		return "", err
	}
	if p.DoCompression {
		if _, err := c.ReadVString(); err != nil {
			return "", err
		}
	}
	algo, ok := tryNegotiate(ChecksumList, strings.Fields(peerChecksum))
	if !ok {
		return "", fmt.Errorf("failed to negotiate a checksum: peer list %q does not overlap ours %q",
			peerChecksum, ChecksumList)
	}
	return algo, nil
}

// ServerHandshake runs the server side of the binary handshake on the raw
// stream. Version must already be the negotiated protocol version, and seed
// is the checksum seed this server will send (a deterministic value is fine:
// the peer only seeds its own checksums with whatever is sent).
func ServerHandshake(c wireConn, p HandshakeParams, seed uint32) (*Session, error) {
	s := &Session{Version: p.Version}
	if p.Version < 30 {
		s.ChecksumAlgo = defaultChecksum(p.Version)
		return s, exchangeSeed(c, s, seed)
	}

	compat := serverCompatFlags(p)
	if err := compat.WriteVarint(c); err != nil {
		return nil, err
	}
	*s = NewSession(p.Version, compat)

	algo, err := negotiateStrings(c, p, compat.Contains(CFVarintFlistFlags))
	if err != nil {
		return nil, err
	}
	s.ChecksumAlgo = algo

	return s, exchangeSeed(c, s, seed)
}

// ClientHandshake runs the client side of the binary handshake on the raw
// stream, mirroring ServerHandshake.
func ClientHandshake(c wireConn, p HandshakeParams) (*Session, error) {
	s := &Session{Version: p.Version}
	if p.Version < 30 {
		s.ChecksumAlgo = defaultChecksum(p.Version)
		return s, clientReadSeed(c, s)
	}

	compat, err := ReadCompatibilityFlags(c)
	if err != nil {
		return nil, err
	}
	*s = NewSession(p.Version, compat)

	algo, err := negotiateStrings(c, p, compat.Contains(CFVarintFlistFlags))
	if err != nil {
		return nil, err
	}
	s.ChecksumAlgo = algo

	return s, clientReadSeed(c, s)
}

// exchangeSeed writes the checksum seed on the server side. This happens at
// every protocol version (rsync/compat.c, outside the >= 30 block).
func exchangeSeed(c wireConn, s *Session, seed uint32) error {
	if err := c.WriteInt32(int32(seed)); err != nil {
		return err
	}
	s.ChecksumSeed = seed
	return nil
}

// clientReadSeed reads the checksum seed on the client side.
func clientReadSeed(c wireConn, s *Session) error {
	seed, err := c.ReadInt32()
	if err != nil {
		return err
	}
	s.ChecksumSeed = uint32(seed)
	return nil
}