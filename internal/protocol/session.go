package protocol

// Session carries the negotiated protocol state shared by the sender and
// receiver for the lifetime of a single rsync connection. It is produced by
// ServerHandshake / ClientHandshake and consumed by the transfer routines.
type Session struct {
	// Version is the negotiated protocol version (min of the two peers).
	Version int

	// Compat is the compatibility-flags bitmask as agreed during handshake.
	Compat CompatibilityFlags

	// ChecksumSeed is the 32-bit checksum seed sent by the server.
	ChecksumSeed uint32

	// ChecksumAlgo is the negotiated strong-checksum algorithm name
	// ("md5" at protocol >= 30 by default, "md4" otherwise).
	ChecksumAlgo string

	// NegotiatedStrings reports whether capability strings were negotiated
	// over vstrings (CFVarintFlistFlags). When false, the protocol uses the
	// non-negotiated default checksum.
	NegotiatedStrings bool

	// The following flags are derived from Compat (rsync/compat.c) and gate
	// per-transfer behavior. They mirror the corresponding C variables.

	// IncRecurse reports whether incremental recursion is active.
	IncRecurse bool
	// VarintFlistFlags reports whether flist flags and itemize markers use
	// varint encoding (CFVarintFlistFlags).
	VarintFlistFlags bool
	// SafeFlist reports whether safe (incremental) file lists are in effect:
	// (CFSafeFlist) || Version >= 31.
	SafeFlist bool
	// ID0Names reports whether uid/gid names are transmitted (CFId0Names).
	ID0Names bool
	// ProperSeedOrder reports whether the checksum seed order fix applies.
	ProperSeedOrder bool
}

// NewSession derives the negotiated behavior flags from version and compat
// flags, mirroring rsync/compat.c:setup_protocol post-read assignments.
func NewSession(version int, compat CompatibilityFlags) Session {
	return Session{
		Version:           version,
		Compat:            compat,
		IncRecurse:        compat.Contains(CFIncRecurse),
		VarintFlistFlags:  compat.Contains(CFVarintFlistFlags),
		SafeFlist:         compat.Contains(CFSafeFlist) || version >= 31,
		ID0Names:          compat.Contains(CFId0Names),
		ProperSeedOrder:   compat.Contains(CFChksumSeedFix),
		NegotiatedStrings: compat.Contains(CFVarintFlistFlags),
	}
}

// defaultChecksum returns the non-negotiated checksum algorithm name used
// when no vstring list is exchanged (rsync/compat.c:556): md5 at protocol
// >= 30, md4 otherwise.
func defaultChecksum(version int) string {
	if version >= 30 {
		return "md5"
	}
	return "md4"
}