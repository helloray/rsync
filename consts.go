package rsync

// rsync.h
const (
	XMIT_TOP_DIR            = (1 << 0)
	XMIT_SAME_MODE          = (1 << 1)
	XMIT_SAME_RDEV_pre28    = (1 << 2) /* protocols 20 - 27  */
	XMIT_EXTENDED_FLAGS     = (1 << 2) /* protocols 28 - now */
	XMIT_SAME_UID           = (1 << 3)
	XMIT_SAME_GID           = (1 << 4)
	XMIT_SAME_NAME          = (1 << 5)
	XMIT_LONG_NAME          = (1 << 6)
	XMIT_SAME_TIME          = (1 << 7)
	XMIT_SAME_RDEV_MAJOR    = (1 << 8) /* protocols 28 - now (devices only) */
	XMIT_NO_CONTENT_DIR     = (1 << 8) /* protocols 30 - now (dirs only) */
	XMIT_HLINKED            = (1 << 9) /* protocols 28 - now (non-dirs)  */
	XMIT_SAME_DEV_pre30     = (1 << 10) /* protocols 28 - 29 */
	XMIT_USER_NAME_FOLLOWS  = (1 << 10) /* protocols 30 - now */
	XMIT_GROUP_NAME_FOLLOWS = (1 << 11) /* protocols 30 - now */
	XMIT_RDEV_MINOR_8_pre30 = (1 << 11) /* protocols 28 - 29 */
	XMIT_HLINK_FIRST        = (1 << 12) /* protocols 30 - now (HLINKED only) */
	XMIT_IO_ERROR_ENDLIST   = (1 << 12) /* protocols 31+ (w/XMIT_EXTENDED_FLAGS) */
	XMIT_MOD_NSEC           = (1 << 13) /* protocols 31 - now */
	XMIT_SAME_ATIME         = (1 << 14) /* any protocol - restricted by option */
	XMIT_UNUSED_15          = (1 << 15) /* unused flag bit */
	XMIT_RESERVED_16        = (1 << 16) /* reserved for future fileflags */
	XMIT_CRTIME_EQ_MTIME    = (1 << 17) /* any protocol - restricted by option */
)

// rsync/rsync.h: itemize flags, exchanged as a shortint (2 bytes
// little-endian) after the file index when protocol_version >= 29.
const (
	ITEM_REPORT_ATIME      = (1 << 0)
	ITEM_REPORT_CHANGE     = (1 << 1)
	ITEM_REPORT_SIZE       = (1 << 2) /* regular files only */
	ITEM_REPORT_TIMEFAIL   = (1 << 2) /* symlinks only */
	ITEM_REPORT_TIME       = (1 << 3)
	ITEM_REPORT_PERMS      = (1 << 4)
	ITEM_REPORT_OWNER      = (1 << 5)
	ITEM_REPORT_GROUP      = (1 << 6)
	ITEM_REPORT_ACL        = (1 << 7)
	ITEM_REPORT_XATTR      = (1 << 8)
	ITEM_REPORT_CRTIME     = (1 << 10)
	ITEM_BASIS_TYPE_FOLLOWS = (1 << 11)
	ITEM_XNAME_FOLLOWS      = (1 << 12)
	ITEM_IS_NEW             = (1 << 13)
	ITEM_LOCAL_CHANGE       = (1 << 14)
	ITEM_TRANSFER           = (1 << 15)
)

// rsync/rsync.h:FNAMECMP_*
const (
	FNAMECMP_BASIS_DIR_LOW = 0x00 /* Must remain 0! */
	FNAMECMP_BASIS_DIR_HIGH = 0x7F
	FNAMECMP_FNAME          = 0x80
	FNAMECMP_PARTIAL_DIR    = 0x81
	FNAMECMP_BACKUP         = 0x82
	FNAMECMP_FUZZY          = 0x83
)

// as per /usr/include/bits/stat.h:
const (
	S_IFMT   = 0o0170000 // bits determining the file type
	S_IFDIR  = 0o0040000 // Directory
	S_IFCHR  = 0o0020000 // Character device
	S_IFBLK  = 0o0060000 // Block device
	S_IFREG  = 0o0100000 // Regular file
	S_IFIFO  = 0o0010000 // FIFO
	S_IFLNK  = 0o0120000 // Symbolic link
	S_IFSOCK = 0o0140000 // Socket
)

// ProtocolVersion defines the currently implemented rsync protocol
// version.
//
// History: this implementation originally spoke protocol 27 (rsync 2.6.0,
// released 2004). Protocol 29 (rsync 2.6.7) is the oldest version that
// tridge rsync 3.x still accepts. Protocol 30 (rsync 3.1.0) adds the varint
// flist/flags framing, the byte-reduction NDX channel and bidirectional
// multiplexing; the fork speaks it in complete-file-list mode (incremental
// recursion is not yet implemented, so CF_INC_RECURSE is never advertised).
const ProtocolVersion = 30

// ProtocolVersionMin is the oldest protocol version we are willing to
// speak, matching rsync’s MIN_PROTOCOL_VERSION.
const ProtocolVersionMin = 27
