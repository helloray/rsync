package sender

import (
	"fmt"
	"io/fs"
	"os"
	"os/user"
	"path"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gokrazy/rsync"
	"github.com/gokrazy/rsync/internal/protocol"
	"github.com/gokrazy/rsync/internal/rsyncchecksum"
	"github.com/gokrazy/rsync/internal/rsyncopts"
	"github.com/gokrazy/rsync/internal/rsyncwire"
)

type file struct {
	source  FileSource
	path    string
	Wpath   string
	regular bool

	// Flags is the low byte of the xmit flags (TOP_DIR, LONG_NAME),
	// computed during the walk. High-byte flags (protocol >= 28) are
	// derived at encode time from the fields below.
	Flags byte

	// fields below are used by the receiver (TODO: unify)
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

func (f *file) isDir() bool {
	return f.Mode&rsync.S_IFMT == rsync.S_IFDIR
}

type fileList struct {
	TotalSize int64
	Files     []file
	Sources   []FileSource
}

// A fileList must not be used after calling Close().
func (fl *fileList) Close() {
	for _, source := range fl.Sources {
		source.Close()
	}
	fl.Sources = nil
}

// rsync/rsync.h defines chunkSize as 32 * 1024, which we match.
//
// Other parts of rsync have subtle dependencies on the chunkSize.
// For example, (*sender.Transfer).hashSearch declares a window
// of at least 256 KB and then reads from that window, requesting
// 2*chunkSize+alignFudge, which must not exceed the window.
// So raising chunkSize to 128 KB or 256 KB breaks transfers
// with "file has changed mid-transfer" (issue #53).
const chunkSize = 32 * 1024

var (
	lookupOnce      sync.Once
	lookupGroupOnce sync.Once
)

// getStrip operates on wire paths (slash space).
func getStrip(requested string) string {
	if requested == "/" {
		return ""
	}
	if strings.HasSuffix(requested, "/") {
		return strings.TrimPrefix(path.Clean(requested), "/") + "/"
	}
	return ""
}

type scopedWalker struct {
	st        *Transfer
	ioError   func(err error)
	excl      *filterRuleList
	uidMap    map[int32]string
	gidMap    map[int32]string
	fileList  *fileList
	source    FileSource
	localDir  string
	requested string
	strip     string
	subdir    string
	prefix    string
}

func (s *scopedWalker) walk() error {
	if s.source == nil {
		fi, err := os.Lstat(s.localDir)
		if err != nil {
			s.st.Logger.Printf("  Lstat(localDir=%q): %v", s.localDir, err)
			return fmt.Errorf("i/o error: requested module path is not accessible")
		}
		if fi.IsDir() {
			root, err := os.OpenRoot(s.localDir)
			if err != nil {
				s.st.Logger.Printf("  OpenRoot(localDir=%q): %v", s.localDir, err)
				return fmt.Errorf("i/o error: requested module path is not accessible")
			}
			s.source = newOSRootSource(root)
		} else {
			s.source = newSingleFileSource(s.localDir, fi)
		}
		s.fileList.Sources = append(s.fileList.Sources, s.source)
	}
	if s.subdir != "." {
		sub, err := newSubSource(s.source, s.subdir)
		if err != nil {
			s.st.Logger.Printf("  newSubSource(subdir=%q): %v", s.subdir, err)
			return fmt.Errorf("i/o error: requested module path is not accessible")
		}
		s.source = sub
	}

	rootname := s.requested
	// fs.WalkDir(root.FS(), …) does not accept absolute paths,
	// so make them relative by prepending a .
	if strings.HasPrefix(rootname, "/") {
		rootname = "." + rootname
	}
	if err := fs.WalkDir(s.source.FS(), path.Clean(rootname), s.walkFn); err != nil {
		return err
	}
	return nil
}

func (s *scopedWalker) walkFn(path string, d fs.DirEntry, err error) error {
	logger := s.st.Logger // for convenience
	opts := s.st.Opts     // for convenience
	if opts.DebugGTE(rsyncopts.DEBUG_FLIST, 1) {
		logger.Printf("filepath.WalkFn(path=%s)", path)
	}
	var info fs.FileInfo
	if err == nil {
		info, err = d.Info()
	}
	if err != nil {
		// set the I/O error flag, but keep walking
		s.ioError(err)
		return nil
	}

	if opts.DebugGTE(rsyncopts.DEBUG_FLIST, 1) {
		logger.Printf("isDir=%v, xferDirs=%v", info.Mode().IsDir(), opts.XferDirs())
	}
	if info.Mode().IsDir() && opts.XferDirs() == 0 {
		logger.Printf("skipping directory %s", path)
		return filepath.SkipDir
	}

	// Only ever transmit long names, like openrsync
	flags := byte(rsync.XMIT_LONG_NAME)

	name := path
	if s.strip != "" {
		if path+"/" == s.strip {
			// Transmit the top directory as ., like tridge rsync.
			name = "."
		} else {
			name = strings.TrimPrefix(name, s.strip)
		}
	}
	if s.prefix != "" {
		if path == "." {
			name = s.prefix
		} else {
			name = s.prefix + "/" + path
		}
	}
	if opts.DebugGTE(rsyncopts.DEBUG_FLIST, 1) {
		logger.Printf("Trim(path=%q) = %q", path, name)
	}
	if path == "." && s.prefix == "" {
		flags |= rsync.XMIT_TOP_DIR
	}
	// st.logger.Printf("flags for %q: %v", name, flags)

	if s.excl.matches(name) {
		return filepath.SkipDir
	}

	size := info.Size()
	if info.Mode().IsDir() {
		// tmpfs returns non-4K sizes for directories. Override with
		// 4096 to make the tests succeed regardless of the /tmp file
		// system type.
		size = 4096
	}
	s.fileList.TotalSize += size

	mode := int32(info.Mode() & os.ModePerm)
	isDev := false
	isSpecial := false
	if info.Mode().IsDir() {
		mode |= rsync.S_IFDIR
	} else if info.Mode().IsRegular() {
		mode |= rsync.S_IFREG
	} else if info.Mode().Type()&os.ModeSymlink != 0 {
		mode |= rsync.S_IFLNK
		// TODO: skip symlink if PreserveSymlinks is not set
	}

	if info.Mode().Type()&os.ModeCharDevice != 0 {
		mode |= rsync.S_IFCHR
		isDev = true
	} else if info.Mode().Type()&os.ModeDevice != 0 {
		mode |= rsync.S_IFBLK
		isDev = true
	}

	if info.Mode().Type()&os.ModeNamedPipe != 0 {
		mode |= rsync.S_IFIFO
		isSpecial = true
	}

	if info.Mode().Type()&os.ModeSocket != 0 {
		mode |= rsync.S_IFSOCK
		isSpecial = true
	}

	f := file{
		source:  s.source,
		path:    path,
		regular: info.Mode().IsRegular(),
		Wpath:   name,
		Flags:   flags,
		Length:  size,
		ModTime: info.ModTime(),
		Mode:    mode,
	}

	if opts.PreserveUid() {
		uid, ok := uidFromFileInfo(info)
		if ok {
			if _, ok := s.uidMap[uid]; !ok && uid != 0 {
				u, err := user.LookupId(strconv.Itoa(int(uid)))
				if err != nil {
					lookupOnce.Do(func() {
						logger.Printf("lookup(%d) = %v", uid, err)
					})
				} else {
					s.uidMap[uid] = u.Username
				}
			}
		}
		f.Uid = uid
	}

	if opts.PreserveGid() {
		gid, ok := gidFromFileInfo(info)
		if ok {
			if _, ok := s.gidMap[gid]; !ok && gid != 0 {
				g, err := user.LookupGroupId(strconv.Itoa(int(gid)))
				if err != nil {
					lookupGroupOnce.Do(func() {
						logger.Printf("lookupgroup(%d) = %v", gid, err)
					})
				} else {
					s.gidMap[gid] = g.Name
				}
			}
		}
		f.Gid = gid
	}

	if (opts.PreserveDevices() && isDev) ||
		(opts.PreserveSpecials() && isSpecial) {
		rdev, _ := rdevFromFileInfo(info)
		f.Rdev = rdev
		f.RdevMajor, f.RdevMinor = rdevMajorMinor(rdev)
	}

	if opts.PreserveLinks() && info.Mode().Type()&os.ModeSymlink != 0 {
		target, err := s.source.Readlink(path)
		if err != nil {
			return err // TODO
		}
		f.LinkTarget = target
	}

	if opts.AlwaysChecksum() {
		var emptyChecksum [rsyncchecksum.Size]byte
		checksum := emptyChecksum[:]
		if info.Mode().IsRegular() {
			fh, err := s.source.Open(path)
			if err != nil {
				return err
			}
			checksum, err = rsyncchecksum.ReaderChecksum(fh)
			fh.Close()
			if err != nil {
				return err
			}
		} else {
			// send empty md4 checksum
		}
		copy(f.Checksum[:], checksum)
	}

	s.fileList.Files = append(s.fileList.Files, f)

	if info.Mode().IsDir() && !opts.Recurse() {
		return filepath.SkipDir
	}

	return nil
}

// rdevMajorMinor splits a device number into its major/minor parts,
// inverting the glibc makedev() composition (see receiver.makedev).
// Device numbers with a major >= 4096 are not covered.
func rdevMajorMinor(rdev int32) (int32, int32) {
	u := uint32(rdev)
	major := int32(u>>8) & 0xfff
	minor := int32(u)&0xff | int32(u>>12)&^0xff
	return major, minor
}

// rsync/flist.c:send_file_list
func (st *Transfer) SendFileList(localDir string, paths []string, excl *filterRuleList) (*fileList, error) {
	var fileList fileList
	fec := &rsyncwire.Buffer{}

	uidMap := make(map[int32]string)
	gidMap := make(map[int32]string)

	// TODO: flush in between to keep the pipes filled when traversal takes long

	// TODO: handle info == nil case (permission denied?): should set an i/o
	// error flag, but traversal should continue

	st.Logger.Printf("building file list")
	if st.Opts.DebugGTE(rsyncopts.DEBUG_FLIST, 1) {
		st.Logger.Printf("sendFileList()")
	}
	ioErrors := int32(0)

	ioError := func(err error) {
		if os.IsNotExist(err) {
			st.Logger.Printf("file vanished: %v", err)
		} else {
			st.Logger.Printf("lstat: %v", err)
		}
		ioErrors = 1
	}

	for _, requested := range paths {
		subdir := "."
		prefix := ""
		local := localDir
		if local == rsync.FileSystemRoot {
			// Implicit module (/) and absolute requested path (/tmp/foo/),
			// turn the path into the local directory and request /.
			local = filepath.Clean(requested)
			if strings.HasSuffix(requested, "/") {

			} else {
				prefix = filepath.Base(requested)
			}
			requested = "/"
		} else if !strings.HasSuffix(requested, "/") {
			st.Logger.Printf("  handling requested=%q", requested)
			clean := path.Clean(strings.TrimPrefix(requested, "/"))
			subdir = path.Dir(clean)
			requested = path.Base(clean)
			st.Logger.Printf("  -> subdir=%q, requested=%q", subdir, requested)
		}

		if st.Opts.DebugGTE(rsyncopts.DEBUG_FLIST, 1) {
			st.Logger.Printf("  path %q (local dir %q)", requested, local)
		}
		// st.Logger.Printf("getRootStrip(requested=%q, localDir=%q", requested, localDir)
		strip := getStrip(requested)
		// st.Logger.Printf("root=%q, strip=%q", root, strip)
		if st.Opts.DebugGTE(rsyncopts.DEBUG_FLIST, 1) {
			st.Logger.Printf("  fs.Walk(%q, %q), strip=%q", local, requested)
		}

		sw := &scopedWalker{
			st:        st,
			excl:      excl,
			uidMap:    uidMap,
			gidMap:    gidMap,
			fileList:  &fileList,
			source:    st.Source,
			ioError:   ioError,
			localDir:  local,
			requested: requested,
			strip:     strip,
			subdir:    subdir,
			prefix:    prefix,
		}
		if err := sw.walk(); err != nil {
			return nil, err
		}
	}

	if st.Opts.InfoGTE(rsyncopts.INFO_PROGRESS, 1) {
		st.Logger.Printf("%d files to consider", len(fileList.Files))
	}

	// rsync/flist.c:send_file_list calls flist_sort_and_clean() before
	// transmission. For protocol >= 29 this orders directory contents
	// after plain entries at each level (f_name_cmp), and both C sides
	// sort identically, so the file indices stay aligned. Mirror that
	// order here.
	if !protocol.UsesOldPrefixes(st.Opts.ProtocolVersion()) {
		sort.Slice(fileList.Files, func(i, j int) bool {
			a, b := &fileList.Files[i], &fileList.Files[j]
			return protocol.FNameCmp(a.Wpath, a.isDir(), b.Wpath, b.isDir()) < 0
		})
	}

	rdevMajor := int32(0)
	for i := range fileList.Files {
		f := &fileList.Files[i]

		fec.Reset()

		mode := f.Mode & rsync.S_IFMT
		isDev := mode == rsync.S_IFCHR || mode == rsync.S_IFBLK
		isSpecial := mode == rsync.S_IFIFO || mode == rsync.S_IFSOCK
		sendRdev := (st.Opts.PreserveDevices() && isDev) ||
			(st.Opts.PreserveSpecials() && isSpecial)

		// 1.   status byte (integer)
		xflags := uint16(f.Flags)
		sameRdevMajor := false
		minorIsSmall := false
		minor := f.RdevMinor
		if sendRdev && st.Opts.ProtocolVersion() >= 28 {
			if isSpecial {
				// rsync/flist.c:send_file_entry: special files don't
				// need an rdev number, so just make the historical
				// transmission of the value efficient.
				minor = 0
				sameRdevMajor = true
				minorIsSmall = true
			} else {
				sameRdevMajor = f.RdevMajor == rdevMajor
				minorIsSmall = f.RdevMinor <= 0xFF
			}
			if sameRdevMajor {
				xflags |= rsync.XMIT_SAME_RDEV_MAJOR
			}
			if minorIsSmall {
				xflags |= rsync.XMIT_RDEV_MINOR_8_pre30
			}
		}
		if st.Opts.ProtocolVersion() >= 28 {
			// rsync/flist.c:send_file_entry: with protocol >= 28, emit
			// the flags as a shortint when the high byte is needed (or
			// the low byte would be zero), so that the receiver can
			// tell the two apart.
			if xflags == 0 && !f.isDir() {
				xflags |= rsync.XMIT_TOP_DIR
			}
			if xflags&0xFF00 != 0 || xflags == 0 {
				xflags |= rsync.XMIT_EXTENDED_FLAGS
				fec.WriteShortint(xflags)
			} else {
				fec.WriteByte(byte(xflags))
			}
		} else {
			fec.WriteByte(byte(xflags))
		}

		// 2.   inherited filename length (optional, byte)
		// 3.   filename length (integer or byte)
		// Only ever transmit long names, like openrsync
		fec.WriteInt32(int32(len(f.Wpath)))

		// 4.   file (byte array)
		fec.WriteString(f.Wpath)

		// 5.   file length (long)
		fec.WriteInt64(f.Length)

		// 6.   file modification time (optional, integer)
		// TODO: this will overflow in 2038! :(
		fec.WriteInt32(int32(f.ModTime.Unix()))

		// 7.   file mode (optional, mode_t, integer)
		fec.WriteInt32(f.Mode)

		if st.Opts.PreserveUid() {
			// 8.   if -o, the user id (integer)
			fec.WriteInt32(f.Uid)
		}

		if st.Opts.PreserveGid() {
			// 9.   if -g, the group id (integer)
			fec.WriteInt32(f.Gid)
		}

		if sendRdev {
			// 10.  if a special file and -D, the device “rdev” type
			if st.Opts.ProtocolVersion() < 28 {
				fec.WriteInt32(f.Rdev)
			} else {
				// rsync/flist.c:send_file_entry, protocol >= 28: the
				// device number is sent as separate major/minor parts.
				if !sameRdevMajor {
					rdevMajor = f.RdevMajor
					fec.WriteInt32(rdevMajor)
				}
				if minorIsSmall {
					fec.WriteByte(byte(minor))
				} else {
					fec.WriteInt32(minor)
				}
			}
		}

		if st.Opts.PreserveLinks() && mode == rsync.S_IFLNK {
			// 11.  if a symbolic link and -l, the link target's length (integer)
			// 12.  if a symbolic link and -l, the link target (byte array)
			fec.WriteInt32(int32(len(f.LinkTarget)))
			fec.WriteString(f.LinkTarget)
		}

		// rsync/flist.c:send_file_entry: with protocol >= 28, the whole-file
		// checksum is only transmitted for regular files (other entry types
		// carry an empty checksum).
		if st.Opts.AlwaysChecksum() &&
			(mode == rsync.S_IFREG || st.Opts.ProtocolVersion() < 28) {
			fec.WriteString(string(f.Checksum[:]))
		}

		if err := st.Conn.WriteString(fec.String()); err != nil {
			return nil, err
		}
	}

	fec.Reset()

	const endOfFileList = 0
	fec.WriteByte(endOfFileList)

	const endOfSet = 0
	if st.Opts.PreserveUid() {
		for uid, name := range uidMap {
			fec.WriteInt32(uid)
			fec.WriteByte(byte(len(name)))
			fec.WriteString(name)
		}
		fec.WriteInt32(endOfSet)
	}
	if st.Opts.PreserveGid() {
		for gid, name := range gidMap {
			fec.WriteInt32(gid)
			fec.WriteByte(byte(len(name)))
			fec.WriteString(name)
		}
		fec.WriteInt32(endOfSet)
	}

	fec.WriteInt32(ioErrors)

	if err := st.Conn.WriteString(fec.String()); err != nil {
		return nil, err
	}

	return &fileList, nil
}
