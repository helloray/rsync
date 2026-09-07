package receiver

import (
	"fmt"
	"io"
	"io/fs"
	"path"
	"sort"
	"time"

	"github.com/gokrazy/rsync"
	"github.com/gokrazy/rsync/internal/protocol"
	"github.com/gokrazy/rsync/internal/rsyncchecksum"
	"github.com/gokrazy/rsync/internal/rsyncopts"
)

// rsync/flist.c:flist_sort_and_clean
//
// For protocol 29 and newer, the sender sorts file lists with
// f_name_cmp (t_PATH semantics), which places directory contents after
// plain entries at each level. The receiver must mirror that comparator
// to keep the file indices it sends back to the sender aligned with the
// sender’s file list.
func (rt *Transfer) sortFileList(fileList []*File) {
	if protocol.UsesOldPrefixes(rt.ProtocolVersion()) {
		sort.Slice(fileList, func(i, j int) bool {
			return fileList[i].Name < fileList[j].Name
		})
		return
	}
	sort.Slice(fileList, func(i, j int) bool {
		a, b := fileList[i], fileList[j]
		return protocol.FNameCmp(a.Name, a.isDir(), b.Name, b.isDir()) < 0
	})
}

func (f *File) isDir() bool {
	return f.Mode&rsync.S_IFMT == rsync.S_IFDIR
}

// makedev composes a device number from major/minor parts, matching the
// glibc makedev() macro that tridge rsync uses for local device nodes.
// Device numbers with a major >= 4096 exceed int32 and are not supported.
func makedev(major, minor int32) int32 {
	dev := (uint32(major)&0xfff)<<8 |
		uint32(minor)&0xff |
		(uint32(minor)&^uint32(0xff))<<12
	return int32(dev)
}

// rsync/receiver.c:delete_files
func findInFileList(fileList []*File, name string) bool {
	i := sort.Search(len(fileList), func(i int) bool {
		return fileList[i].Name >= name
	})
	return i < len(fileList) && fileList[i].Name == name
}

type File struct {
	Name       string
	Length     int64
	ModTime    time.Time
	Mode       int32
	Uid        int32
	Gid        int32
	LinkTarget string
	Rdev       int32
	RdevMajor  int32
	RdevMinor  int32
	Checksum   [rsyncchecksum.Size]byte
}

// FileMode converts from the Linux permission bits to Go’s permission bits.
func (f *File) FileMode() fs.FileMode {
	ret := fs.FileMode(f.Mode) & fs.ModePerm

	mode := f.Mode & rsync.S_IFMT
	switch mode {
	case rsync.S_IFCHR:
		ret |= fs.ModeCharDevice
	case rsync.S_IFBLK:
		ret |= fs.ModeDevice
	case rsync.S_IFIFO:
		ret |= fs.ModeNamedPipe
	case rsync.S_IFSOCK:
		ret |= fs.ModeSocket
	case rsync.S_IFLNK:
		ret |= fs.ModeSymlink
	case rsync.S_IFDIR:
		ret |= fs.ModeDir
	}

	return ret
}

// rsync/flist.c:receive_file_entry
func (rt *Transfer) receiveFileEntry(flags uint16, last *File) (*File, error) {
	f := &File{}

	var l1 int
	if flags&rsync.XMIT_SAME_NAME != 0 {
		l, err := rt.Conn.ReadByte()
		if err != nil {
			return nil, err
		}
		l1 = int(l)
	}

	var l2 int
	if flags&rsync.XMIT_LONG_NAME != 0 {
		l, err := rt.Conn.ReadInt32()
		if err != nil {
			return nil, err
		}
		l2 = int(l)
	} else {
		l, err := rt.Conn.ReadByte()
		if err != nil {
			return nil, err
		}
		l2 = int(l)
	}
	// linux/limits.h
	const PATH_MAX = 4096
	if l2 >= PATH_MAX-l1 {
		const lastname = ""
		return nil, fmt.Errorf("overflow: flags=0x%x l1=%d l2=%d lastname=%s",
			flags, l1, l2, lastname)
	}
	b := make([]byte, l1+l2)
	readb := b
	if l1 > 0 {
		copy(b, []byte(last.Name))
		readb = b[l1:]
	}
	if _, err := io.ReadFull(rt.Conn.Reader, readb); err != nil {
		return nil, err
	}
	// TODO: does rsync’s clean_fname() and sanitize_path() combination do
	// anything more than Go’s path.Clean()?
	// Use path.Clean (not filepath.Clean) to keep forward slashes on Windows,
	// so that the sort order matches the client’s sort order. filepath.Clean
	// converts ‘/’ to ‘\\’ on Windows, which changes the byte-wise sort order
	// (e.g. ‘\\’ > ‘0’ but ‘/’ < ‘0’), causing index mismatches when the
	// server sends file indices back to the client.
	f.Name = path.Clean(string(b))
	rt.Logger.Printf("[flist] receiveFileEntry: l1=%d l2=%d last.Name=%q → name=%q", l1, l2, last.Name, f.Name)

	length, err := rt.Conn.ReadInt64()
	if err != nil {
		return nil, err
	}
	f.Length = length

	if flags&rsync.XMIT_SAME_TIME != 0 {
		f.ModTime = last.ModTime
	} else {
		modTime, err := rt.Conn.ReadInt32()
		if err != nil {
			return nil, err
		}
		f.ModTime = time.Unix(int64(modTime), 0)
	}

	if flags&rsync.XMIT_SAME_MODE != 0 {
		f.Mode = last.Mode
	} else {
		mode, err := rt.Conn.ReadInt32()
		if err != nil {
			return nil, err
		}
		f.Mode = mode
	}

	if rt.Opts.PreserveUid {
		if flags&rsync.XMIT_SAME_UID != 0 {
			f.Uid = last.Uid
		} else {
			uid, err := rt.Conn.ReadInt32()
			if err != nil {
				return nil, err
			}
			f.Uid = uid
		}
	}

	if rt.Opts.PreserveGid {
		if flags&rsync.XMIT_SAME_GID != 0 {
			f.Gid = last.Gid
		} else {
			gid, err := rt.Conn.ReadInt32()
			if err != nil {
				return nil, err
			}
			f.Gid = gid
		}
	}

	mode := f.Mode & rsync.S_IFMT
	isDev := mode == rsync.S_IFCHR || mode == rsync.S_IFBLK
	isSpecial := mode == rsync.S_IFIFO || mode == rsync.S_IFSOCK
	isLink := mode == rsync.S_IFLNK

	if (rt.Opts.PreserveDevices && isDev) ||
		(rt.Opts.PreserveSpecials && isSpecial && rt.ProtocolVersion() < 31) {
		if rt.ProtocolVersion() < 28 {
			if flags&rsync.XMIT_SAME_RDEV_pre28 != 0 {
				f.Rdev = last.Rdev
			} else {
				rdev, err := rt.Conn.ReadInt32()
				if err != nil {
					return nil, err
				}
				f.Rdev = rdev
			}
		} else {
			// rsync/flist.c:recv_file_entry, protocol >= 28: the device
			// number is sent as separate major/minor parts.
			if flags&rsync.XMIT_SAME_RDEV_MAJOR == 0 {
				if rt.ProtocolVersion() < 30 {
					major, err := rt.Conn.ReadInt32()
					if err != nil {
						return nil, err
					}
					rt.rdevMajor = major
				} else {
					// TODO(protocol >= 30): varint
					major, err := rt.Conn.ReadInt32()
					if err != nil {
						return nil, err
					}
					rt.rdevMajor = major
				}
			}
			f.RdevMajor = rt.rdevMajor
			switch {
			case rt.ProtocolVersion() >= 30:
				// TODO(protocol >= 30): varint
				minor, err := rt.Conn.ReadInt32()
				if err != nil {
					return nil, err
				}
				f.RdevMinor = minor
			case flags&rsync.XMIT_RDEV_MINOR_8_pre30 != 0:
				minor, err := rt.Conn.ReadByte()
				if err != nil {
					return nil, err
				}
				f.RdevMinor = int32(minor)
			default:
				minor, err := rt.Conn.ReadInt32()
				if err != nil {
					return nil, err
				}
				f.RdevMinor = minor
			}
			f.Rdev = makedev(f.RdevMajor, f.RdevMinor)
		}
	}

	if rt.Opts.PreserveLinks && isLink {
		length, err := rt.Conn.ReadInt32()
		if err != nil {
			return nil, err
		}
		b := make([]byte, length)
		if _, err := io.ReadFull(rt.Conn.Reader, b); err != nil {
			return nil, err
		}
		f.LinkTarget = string(b)
	}

	// rsync/flist.c:recv_file_entry: with protocol >= 28, the whole-file
	// checksum is only transmitted for regular files (other entry types
	// carry an empty checksum).
	if rt.Opts.AlwaysChecksum &&
		(mode == rsync.S_IFREG || rt.ProtocolVersion() < 28) {
		if _, err := io.ReadFull(rt.Conn.Reader, f.Checksum[:]); err != nil {
			return nil, err
		}
	}

	return f, nil
}

// rsync/flist.c:recv_file_list
func (rt *Transfer) ReceiveFileList() ([]*File, error) {
	if rt.Opts.Progress {
		fmt.Fprintln(rt.Env.Stdout, "receiving file list...")
		fmt.Fprint(rt.Env.Stdout, "0 files to consider")
	}
	lastFileEntry := new(File)
	var fileList []*File
	for {
		b, err := rt.Conn.ReadByte()
		if err != nil {
			return nil, err
		}
		if b == 0 {
			break
		}
		flags := uint16(b)
		// rsync/flist.c:recv_file_list: with protocol >= 28, the extended
		// flags byte follows when XMIT_EXTENDED_FLAGS is set.
		if rt.ProtocolVersion() >= 28 && flags&rsync.XMIT_EXTENDED_FLAGS != 0 {
			ext, err := rt.Conn.ReadByte()
			if err != nil {
				return nil, err
			}
			flags |= uint16(ext) << 8
		}

		f, err := rt.receiveFileEntry(flags, lastFileEntry)
		if err != nil {
			return nil, err
		}
		rt.Logger.Printf("[flist] entry %d: flags=0x%x name=%q mode=%o len=%d (last=%q)",
			len(fileList), flags, f.Name, f.Mode, f.Length, lastFileEntry.Name)
		lastFileEntry = f
		// TODO: include depth in output?
		if rt.Opts.DebugGTE(rsyncopts.DEBUG_FLIST, 1) {
			rt.Logger.Printf("[Receiver] i=%d ? %s mode=%o len=%d uid=%d gid=%d flags=?",
				len(fileList),
				f.Name,
				f.Mode,
				f.Length,
				f.Uid,
				f.Gid)
		}
		fileList = append(fileList, f)
		if rt.Opts.Progress && len(fileList)%100 == 0 {
			fmt.Fprintf(rt.Env.Stdout, "\r%d files to consider", len(fileList))
		}
	}
	if rt.Opts.Progress {
		fmt.Fprintf(rt.Env.Stdout, "\r%d files to consider\n", len(fileList))
	}

	rt.sortFileList(fileList)

	if rt.Opts.PreserveUid || rt.Opts.PreserveGid {
		// receive the uid/gid list
		users, groups, err := rt.RecvIdList()
		if err != nil {
			return nil, err
		}
		rt.Users = users
		rt.Groups = groups
	}

	// read the i/o error flag
	ioErrors, err := rt.Conn.ReadInt32()
	if err != nil {
		return nil, err
	}
	if rt.Opts.DebugGTE(rsyncopts.DEBUG_FLIST, 2) {
		rt.Logger.Printf("ioErrors: %v", ioErrors)
	}
	rt.IOErrors = ioErrors

	return fileList, nil
}
