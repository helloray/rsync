package receiver

import (
	"github.com/gokrazy/rsync/internal/log"
	"github.com/gokrazy/rsync/internal/progress"
	"github.com/gokrazy/rsync/internal/protocol"
	"github.com/gokrazy/rsync/internal/rsyncopts"
	"github.com/gokrazy/rsync/internal/rsyncos"
	"github.com/gokrazy/rsync/internal/rsyncwire"
)

// TransferOpts is a subset of Opts which is required for implementing a receiver.
type TransferOpts struct {
	Verbose  bool
	DryRun   bool
	Server   bool
	Progress bool

	DeleteMode        bool
	PreserveGid       bool
	PreserveUid       bool
	PreserveLinks     bool
	PreservePerms     bool
	PreserveDevices   bool
	PreserveSpecials  bool
	PreserveTimes     bool
	PreserveHardlinks bool
	IgnoreTimes       bool
	AlwaysChecksum    bool
	DoFsync           bool

	// NumericIds mirrors C's numeric_ids: the peer asked for numeric-only
	// uid/gid transmission (no names inline or in trailing id lists).
	NumericIds bool

	InfoGTE  func(rsyncopts.InfoLevel, uint16) bool
	DebugGTE func(rsyncopts.DebugLevel, uint16) bool

	KeepPartial bool

	// ProtocolVersion is the negotiated rsync protocol version.
	// When zero, the transfer falls back to protocol 27.
	ProtocolVersion int
}

// ProtocolVersion returns the negotiated rsync protocol version for this
// transfer, defaulting to 27 when unset.
func (rt *Transfer) ProtocolVersion() int {
	if rt.Opts.ProtocolVersion == 0 {
		return 27
	}
	return rt.Opts.ProtocolVersion
}

// checksumAlgo returns the negotiated strong-checksum algorithm name,
// defaulting to md4 for legacy sessions without a negotiated Session.
func (rt *Transfer) checksumAlgo() string {
	if rt.Session != nil && rt.Session.ChecksumAlgo != "" {
		return rt.Session.ChecksumAlgo
	}
	return "md4"
}

// properSeedOrder reports whether the checksum seed-order fix was negotiated
// (CF_CHKSUM_SEED_FIX): md5 block checksums feed the seed before the data.
func (rt *Transfer) properSeedOrder() bool {
	return rt.Session != nil && rt.Session.ProperSeedOrder
}

type Transfer struct {
	// config
	Logger   log.Logger
	Opts     *TransferOpts
	Dest     string
	DestRoot *SafeRoot
	Env      *rsyncos.Env
	Progress progress.Printer

	// Session carries the negotiated protocol state (version, compatibility
	// flags, checksum contract) from the handshake. It drives the flist codec
	// and per-file message framing.
	Session *protocol.Session

	// skipCount tracks entries skipped as unrepresentable (e.g. NTFS cannot
	// store "Convert::Binary::C.3pm"); written atomically from the generator,
	// read by Do after both goroutines finish.
	skipCount int32

	// Ndx codecs, one per wire direction. The modern (protocol >= 30) NDX
	// encoding is delta-encoded and stateful, so a single codec must be reused
	// for its direction across the whole transfer. The writer codec is used
	// only from the generator goroutine, the reader codec only from the
	// receiver goroutine, so the two never contend.
	ndxWriteC *protocol.NdxCodec
	ndxReadC  *protocol.NdxCodec

	// state
	Conn            *rsyncwire.Conn
	Seed            int32
	IOErrors        int32
	Users           map[int32]mapping
	Groups          map[int32]mapping
	retouchDirPerms bool

	// inc holds the incremental-recursion receiver state; nil when the
	// transfer uses a complete file list. Set by ReceiveFileList before the
	// transfer goroutines start.
	inc *incRecv

	// delStats counts what deleteFiles removed, reported to the sender as
	// NDX_DEL_STATS at protocol >= 31 (rsync/main.c:write_del_stats).
	delStats delStats
}

// delStats mirrors rsync's stats.deleted_* counters as mutually exclusive
// buckets (rsync tracks a running total plus per-kind counters; the Go
// walk classifies each removed entry into exactly one bucket).
type delStats struct {
	files    int32
	dirs     int32
	symlinks int32
	devices  int32
	specials int32
}

func (rt *Transfer) listOnly() bool { return rt.Dest == "" }

// ndxWrite returns the NDX writer codec, creating it from the negotiated
// protocol version on first use.
func (rt *Transfer) ndxWrite() *protocol.NdxCodec {
	if rt.ndxWriteC == nil {
		rt.ndxWriteC = protocol.NewNdxCodec(rt.ProtocolVersion())
	}
	return rt.ndxWriteC
}

// ndxRead returns the NDX reader codec, creating it from the negotiated
// protocol version on first use.
func (rt *Transfer) ndxRead() *protocol.NdxCodec {
	if rt.ndxReadC == nil {
		rt.ndxReadC = protocol.NewNdxCodec(rt.ProtocolVersion())
	}
	return rt.ndxReadC
}

// writeNdx sends a file index to the sender, like rsync/io.c:write_ndx
// (byte-reduction encoding for protocol >= 30, a plain int32 below) followed,
// for protocol >= 29, by the itemize iflags shortint
// (rsync/generator.c:itemize). Negative indices are phase markers
// (e.g. NDX_DONE) and never carry iflags.
func (rt *Transfer) writeNdx(idx int32, iflags uint16) error {
	if err := rt.ndxWrite().WriteNdx(rt.Conn, idx); err != nil {
		return err
	}
	if idx < 0 || !protocol.SupportsIFlags(rt.ProtocolVersion()) {
		return nil
	}
	return rt.Conn.WriteShortint(iflags)
}

// readNdx reads a file index from the sender, like rsync/io.c:read_ndx
// (byte-reduction decoding for protocol >= 30, a plain int32 below).
func (rt *Transfer) readNdx() (int32, error) {
	return rt.ndxRead().ReadNdx(rt.Conn)
}
