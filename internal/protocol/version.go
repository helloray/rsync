// Package protocol implements rsync protocol version predicates and
// wire-level helpers shared between the sender and receiver sides.
//
// The predicates mirror oc-rsync’s
// crates/protocol/src/version/protocol_version/capabilities.rs, which in
// turn replaces the scattered protocol_version comparisons in tridge
// rsync.
package protocol

// SupportsExtendedFlags reports whether flist entries use the
// XMIT_EXTENDED_FLAGS continuation byte (rsync 2.6.7, protocol 28).
func SupportsExtendedFlags(protocolVersion int) bool {
	return protocolVersion >= 28
}

// SupportsIFlags reports whether file transfer requests and data messages
// carry the itemize iflags shortint (and optionally the basis type byte
// and xname vstring) after the file index (rsync 2.6.7+ / protocol 29).
func SupportsIFlags(protocolVersion int) bool {
	return protocolVersion >= 29
}

// SupportsSenderReceiverModifiers reports whether protocol 29 modifiers
// (multi-phase transfer, sender/receiver option negotiation) are active.
func SupportsSenderReceiverModifiers(protocolVersion int) bool {
	return protocolVersion >= 29
}

// SupportsMultiPhase reports whether the transfer uses up to 2 phases
// instead of 1 (protocol 29+).
func SupportsMultiPhase(protocolVersion int) bool {
	return protocolVersion >= 29
}

// UsesOldPrefixes reports whether the pre-29 name sort order (plain byte
// comparison) is used for file lists. With protocol 29 and newer, file
// lists are sorted with the f_name_cmp state machine, which orders
// directory contents after regular entries at each level.
func UsesOldPrefixes(protocolVersion int) bool {
	return protocolVersion < 29
}

// SupportsIncrementalRecursion reports whether this build implements
// incremental recursion (--inc-recursive, the protocol-30+ incremental file
// list exchange). It is false until Phase E lands, so both sides fall back to
// the complete file list even when a peer would accept incremental framing.
// Advertising CF_INC_RECURSE without incremental transfer support would make
// the peer's receiver skip the trailing id lists that we still write (we send
// numeric-only lists), desyncing the file list decode.
func SupportsIncrementalRecursion() bool {
	return false
}
