package sender

import (
	"io"

	"github.com/gokrazy/rsync/internal/log"
	"github.com/gokrazy/rsync/internal/progress"
	"github.com/gokrazy/rsync/internal/protocol"
	"github.com/gokrazy/rsync/internal/rsyncopts"
	"github.com/gokrazy/rsync/internal/rsyncos"
	"github.com/gokrazy/rsync/internal/rsyncwire"
)

type Osenv struct {
	Stdin  io.Reader
	Stdout io.Writer
	Stderr io.Writer
}

// TransferOpts is a subset of Opts which is required for implementing a receiver.
type TransferOpts struct {
	Verbose bool
	DryRun  bool

	DeleteMode        bool
	PreserveGid       bool
	PreserveUid       bool
	PreserveLinks     bool
	PreservePerms     bool
	PreserveDevices   bool
	PreserveSpecials  bool
	PreserveTimes     bool
	PreserveHardlinks bool
}

type Transfer struct {
	// config
	// Opts *Opts
	Logger   log.Logger
	Opts     *rsyncopts.Options
	Env      *rsyncos.Env
	Progress progress.Printer
	Source   FileSource // for modules specifying a fs.FS

	// Session carries the negotiated protocol state (version, compatibility
	// flags, checksum contract) from the handshake. It drives the flist codec
	// and per-file message framing.
	Session *protocol.Session

	// Ndx codecs, one per wire direction. The modern (protocol >= 30) NDX
	// encoding is delta-encoded and stateful, so a single codec must be reused
	// for its direction across the whole transfer.
	ndxWriteC *protocol.NdxCodec
	ndxReadC  *protocol.NdxCodec

	// state
	Conn      *rsyncwire.Conn
	Seed      int32
	lastMatch int64
}

//func (rt *Transfer) listOnly() bool { return rt.Dest == "" }
