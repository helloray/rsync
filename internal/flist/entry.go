// Package flist implements the shared rsync file-list entry codec used by
// both the sender and the receiver. It supersedes the entry encoding/decoding
// previously duplicated across internal/receiver and internal/sender.
//
// It bridges the protocol 27-29 legacy encoding (integer frames, extended-flags
// shortint) and the protocol >= 30 encoding (varint frames for the flags, uid,
// gid, rdev, and symlink length; varlong for the file length and modtime;
// inline uid/gid names). The wire layout mirrors _c-rsync/flist.c
// send_file_entry (lines ~475-775) and recv_file_entry (lines ~777-1090).
package flist

import (
	"github.com/gokrazy/rsync"
)

// Params carries the command-line feature set and protocol version that gate
// which fields are present in an entry and how they are framed. Both peers of
// a connection must agree on these.
type Params struct {
	// ProtocolVersion is the negotiated rsync protocol version (27..=32).
	ProtocolVersion int

	// VarintFlags reports whether the xflags are sent as a varint (protocol >=
	// 30 with the CF_VARINT_FLIST_FLAGS compat flag), mirroring C's
	// xfer_flags_as_varint.
	VarintFlags bool

	// ModNsec reports support for the XMIT_MOD_NSEC field (protocol >= 31).
	ModNsec bool

	PreserveUid      bool
	PreserveGid      bool
	PreserveLinks    bool
	PreserveDevices  bool
	PreserveSpecials bool
	AlwaysChecksum   bool

	// NumericIDs disables inline uid/gid name transmission (the names are still
	// sent via the trailing id list instead).
	NumericIDs bool

	// IncRecurse enables inline uid/gid NAME_FOLLOWS only when set; without
	// incremental recursion the names travel in the trailing id list.
	IncRecurse bool

	// ID0Names reports whether the uid/gid id lists additionally carry a
	// terminator name for id 0 (CF_ID0_NAMES). When set, each trailing list is
	// followed by the id-0 name, and the sender must emit it or the receiver
	// blocks reading the name length byte (_c-rsync/uidlist.c:send_one_list).
	ID0Names bool
}

// FileEntry is the wire-model of a single file-list entry. It is the shared
// representation the receiver and the sender both operate on; it carries every
// field rsync can transfer, with the codec deciding which are actually written.
type FileEntry struct {
	Name       string
	Length     int64
	ModTime    int32 // unix seconds, matching rsync's time_t on the wire
	ModNsec    int32
	Mode       int32
	Uid        int32
	Gid        int32
	User       string // inline user name (protocol >= 30)
	Group      string // inline group name (protocol >= 30)
	RdevMajor  int32
	RdevMinor  int32
	LinkTarget string
	Checksum   [16]byte
}

// IS_* helpers mirror the S_IS* macros; FileEntry.Mode carries the Linux st_mode
// bits (type in the top bits, perms in the low 9).
func (f *FileEntry) isDir() bool    { return f.Mode&rsync.S_IFMT == rsync.S_IFDIR }
func (f *FileEntry) isReg() bool    { return f.Mode&rsync.S_IFMT == rsync.S_IFREG }
func (f *FileEntry) isLink() bool   { return f.Mode&rsync.S_IFMT == rsync.S_IFLNK }
func (f *FileEntry) isDevice() bool { m := f.Mode & rsync.S_IFMT; return m == rsync.S_IFCHR || m == rsync.S_IFBLK }
func (f *FileEntry) isSpecial() bool {
	m := f.Mode & rsync.S_IFMT
	return m == rsync.S_IFIFO || m == rsync.S_IFSOCK
}

// Rdev composes a device number from the major/minor parts using the glibc
// makedev() layout tridge rsync uses.
func (f *FileEntry) Rdev() int32 {
	major, minor := f.RdevMajor, f.RdevMinor
	dev := (uint32(major)&0xfff)<<8 |
		uint32(minor)&0xff |
		(uint32(minor)&^uint32(0xff))<<12
	return int32(dev)
}