package receiver

import (
	"fmt"
	"io/fs"
	"path"
	"sort"
	"time"

	"github.com/gokrazy/rsync"
	"github.com/gokrazy/rsync/internal/flist"
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
func findInFileList(fileList []string, name string) bool {
	i := sort.Search(len(fileList), func(i int) bool {
		return fileList[i] >= name
	})
	return i < len(fileList) && fileList[i] == name
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

	// Ndx is the file's wire index: the position in a complete list, or the
	// ndx_start-chain value under incremental recursion. The generator sends
	// requests with it and the frame loop routes incoming data with it.
	Ndx int32
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

// rsync/flist.c:recv_file_list
func (rt *Transfer) ReceiveFileList() ([]*File, error) {
	// Install the stream-level control-frame filter before anything reads
	// from the connection: control frames can interleave between any two
	// DATA frames, including inside file-list segments, so every reader —
	// not just the ndx loops — must tolerate them.
	rt.filterControlFrames()
	p := rt.flistParams()
	if p.IncRecurse {
		// Incremental recursion: only the initial segment is read here; the
		// per-directory segments are decoded by the frame loop.
		return rt.receiveFileListInc(p)
	}
	if rt.Opts.Progress {
		fmt.Fprintln(rt.Env.Stdout, "receiving file list...")
		fmt.Fprint(rt.Env.Stdout, "0 files to consider")
	}
	r := flist.NewCompleteReader(rt.Conn, p)
	seg, err := r.Next()
	if err != nil {
		return nil, err
	}

	var fileList []*File
	for _, fe := range seg.Entries {
		f := rt.toFile(fe)
		// TODO: include depth in output?
		if rt.Opts.DebugGTE(rsyncopts.DEBUG_FLIST, 1) {
			rt.Logger.Printf("[Receiver] i=%d ? %s mode=%o len=%d uid=%d gid=%d flags=?",
				len(fileList), f.Name, f.Mode, f.Length, f.Uid, f.Gid)
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
	for i, f := range fileList {
		f.Ndx = int32(i)
	}

	// The trailing uid/gid id lists and the i/o error word are consumed inside
	// flist.ReadFileList; the error word is surfaced here for the transfer-level
	// error handling.
	rt.IOErrors = seg.IOError
	return fileList, nil
}

// flistParams derives the flist codec parameters for this receiver from the
// negotiated session and options. NumericIDs mirrors C's numeric_ids: when the
// peer passed --numeric-ids (forwarded through server_options), uid/gid names
// are neither sent inline nor in the trailing id lists. Under inc-recurse
// names ride inline and there are no trailing lists at all.
func (rt *Transfer) flistParams() flist.Params {
	p := flist.Params{
		ProtocolVersion:  rt.ProtocolVersion(),
		PreserveUid:      rt.Opts.PreserveUid,
		PreserveGid:      rt.Opts.PreserveGid,
		PreserveLinks:    rt.Opts.PreserveLinks,
		PreserveDevices:  rt.Opts.PreserveDevices,
		PreserveSpecials: rt.Opts.PreserveSpecials,
		AlwaysChecksum:   rt.Opts.AlwaysChecksum,
		NumericIDs:       rt.Opts.NumericIds,
	}
	if rt.Session != nil {
		p.VarintFlags = rt.Session.VarintFlistFlags
		p.IncRecurse = rt.Session.IncRecurse
		p.ID0Names = rt.Session.ID0Names
		p.SafeFlist = rt.Session.SafeFlist
	}
	return p
}

// toFile converts a flist.FileEntry (the wire model) into the receiver's File
// format. It mirrors the field-by-field assignment of the former in-line
// receive_file_entry, including the forward-slash-safe path.Clean on the name
// that keeps the server-side sort order identical to the client's.
func (rt *Transfer) toFile(fe *flist.FileEntry) *File {
	f := &File{
		Name:       path.Clean(fe.Name),
		Length:     fe.Length,
		ModTime:    time.Unix(int64(fe.ModTime), int64(fe.ModNsec)),
		Mode:       fe.Mode,
		Uid:        fe.Uid,
		Gid:        fe.Gid,
		LinkTarget: fe.LinkTarget,
		RdevMajor:  fe.RdevMajor,
		RdevMinor:  fe.RdevMinor,
	}
	f.Rdev = makedev(fe.RdevMajor, fe.RdevMinor)
	copy(f.Checksum[:], fe.Checksum[:])
	return f
}
