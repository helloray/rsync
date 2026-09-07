package protocol

import (
	"io"
)

// CompatibilityFlags is the rsync "compat_flags" bitmask exchanged between
// sender and receiver during protocol >= 30 negotiation. Bit definitions
// follow rsync/compat.c.
type CompatibilityFlags uint32

const (
	// CFIncRecurse advertises incremental recursion support.
	CFIncRecurse CompatibilityFlags = 1 << 0
	// CFSymlinkTimes: the sender can set times on symlinks.
	CFSymlinkTimes CompatibilityFlags = 1 << 1
	// CFSymlinkIconv: symlink contents require charset conversion.
	CFSymlinkIconv CompatibilityFlags = 1 << 2
	// CFSafeFlist: safe (incremental) file lists are supported.
	CFSafeFlist CompatibilityFlags = 1 << 3
	// CFAvoidXattrOptim: do not attempt the xattr optimization.
	CFAvoidXattrOptim CompatibilityFlags = 1 << 4
	// CFChksumSeedFix: the "proper" checksum seed order is used.
	CFChksumSeedFix CompatibilityFlags = 1 << 5
	// CFInplacePartialDir: honor --inplace partial directories.
	CFInplacePartialDir CompatibilityFlags = 1 << 6
	// CFVarintFlistFlags: flist flags and itemize markers use varint encoding.
	CFVarintFlistFlags CompatibilityFlags = 1 << 7
	// CFId0Names: uid/gid names (ID0) are transmitted.
	CFId0Names CompatibilityFlags = 1 << 8

	// CFAllKnown is the union of all defined compatibility flag bits.
	CFAllKnown CompatibilityFlags = (1 << 9) - 1
)

// Bits returns the raw bitmask.
func (c CompatibilityFlags) Bits() uint32 { return uint32(c) }

// Contains reports whether all of the given flags are set.
func (c CompatibilityFlags) Contains(other CompatibilityFlags) bool {
	return c&other == other
}

// WriteVarint encodes the flags to w using rsync varint encoding
// (rsync/compat.c:write_varint floor).
func (c CompatibilityFlags) WriteVarint(w io.Writer) error {
	return WriteVarint(w, int32(c))
}

// ReadCompatibilityFlags decodes the flags from r.
func ReadCompatibilityFlags(r io.Reader) (CompatibilityFlags, error) {
	v, err := ReadVarint(r)
	if err != nil {
		return 0, err
	}
	return CompatibilityFlags(v), nil
}