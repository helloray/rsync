package receiver

import (
	"os"

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

type Transfer struct {
	// config
	Logger   log.Logger
	Opts     *TransferOpts
	Dest     string
	DestRoot *os.Root
	Env      *rsyncos.Env
	Progress progress.Printer

	// state
	Conn            *rsyncwire.Conn
	Seed            int32
	IOErrors        int32
	Users           map[int32]mapping
	Groups          map[int32]mapping
	retouchDirPerms bool

	// rdevMajor mirrors the static rdev_major in
	// rsync/flist.c:recv_file_entry: it persists across entries and is
	// only updated when a device entry carries a new major number.
	rdevMajor int32
}

func (rt *Transfer) listOnly() bool { return rt.Dest == "" }

// writeNdx sends a file index to the sender, like rsync/io.c:write_ndx
// (which falls back to a plain int32 for protocol < 30) followed, for
// protocol >= 29, by the itemize iflags shortint
// (rsync/generator.c:itemize). Negative indices are phase markers
// (e.g. NDX_DONE) and never carry iflags.
func (rt *Transfer) writeNdx(idx int32, iflags uint16) error {
	if err := rt.Conn.WriteInt32(idx); err != nil {
		return err
	}
	if idx < 0 || !protocol.SupportsIFlags(rt.ProtocolVersion()) {
		return nil
	}
	return rt.Conn.WriteShortint(iflags)
}
