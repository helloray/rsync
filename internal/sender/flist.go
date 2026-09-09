package sender

import (
	"bytes"
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
	"github.com/gokrazy/rsync/internal/flist"
	"github.com/gokrazy/rsync/internal/protocol"
	"github.com/gokrazy/rsync/internal/rsyncchecksum"
	"github.com/gokrazy/rsync/internal/rsyncopts"
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

	// inc is non-nil under incremental recursion: it owns the segment
	// scheduler and the ndx → file mapping for SendFiles.
	inc *incSched
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

	f, err := s.buildEntry(path, info)
	if err != nil {
		return err
	}

	s.fileList.Files = append(s.fileList.Files, f)

	if info.Mode().IsDir() && !opts.Recurse() {
		return filepath.SkipDir
	}

	return nil
}

// buildEntry constructs the wire entry for one walk-path, given its already
// resolved stat info: wire-name computation (strip/prefix/TOP_DIR), exclusion
// check, and the metadata fields (mode, uid/gid, rdev, link target,
// --always-checksum). Mirrors rsync/flist.c:make_file + send_file_entry's
// flag setup.
func (s *scopedWalker) buildEntry(path string, info fs.FileInfo) (file, error) {
	logger := s.st.Logger // for convenience
	opts := s.st.Opts     // for convenience

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
		return file{}, filepath.SkipDir
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
			return f, err // TODO
		}
		f.LinkTarget = target
	}

	if opts.AlwaysChecksum() {
		var emptyChecksum [rsyncchecksum.Size]byte
		checksum := emptyChecksum[:]
		if info.Mode().IsRegular() {
			fh, err := s.source.Open(path)
			if err != nil {
				return f, err
			}
			checksum, err = rsyncchecksum.ReaderChecksum(s.st.checksumAlgo(), fh)
			fh.Close()
			if err != nil {
				return f, err
			}
		} else {
			// send empty md4 checksum
		}
		copy(f.Checksum[:], checksum)
	}

	return f, nil
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
	fec := new(bytes.Buffer)

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

	// Encode every entry plus the terminator, trailing uid/gid id lists and the
	// i/o error word through the shared flist codec, mirroring
	// _c-rsync/flist.c:send_file_list + uidlist.c:send_id_lists. The encoder
	// owns the cross-entry compression state (SAME_NAME prefix, SAME_UID/GID,
	// rdev major) so no per-entry scratch is needed here.
	p := st.flistParams()
	entries := make([]*flist.FileEntry, len(fileList.Files))
	for i := range fileList.Files {
		f := &fileList.Files[i]
		fe := &flist.FileEntry{
			Name:       f.Wpath,
			Length:     f.Length,
			ModTime:    int32(f.ModTime.Unix()),
			Mode:       f.Mode,
			Uid:        f.Uid,
			Gid:        f.Gid,
			RdevMajor:  f.RdevMajor,
			RdevMinor:  f.RdevMinor,
			LinkTarget: f.LinkTarget,
			TopDir:     f.Flags&rsync.XMIT_TOP_DIR != 0,
		}
		if p.IncRecurse && !p.NumericIDs {
			// protocol >= 30 inc-recurse: uid/gid names ride inline in the
			// entries (XMIT_USER_NAME_FOLLOWS); there are no trailing lists.
			fe.User = uidMap[f.Uid]
			fe.Group = gidMap[f.Gid]
		}
		copy(fe.Checksum[:], f.Checksum[:])
		entries[i] = fe
	}

	if p.IncRecurse {
		// incremental recursion: send the initial list now; the per-dir
		// segments follow lazily from the SendFiles loop.
		enc := flist.NewEncoder(p)
		inc := newIncSched(st, enc, entries, ioErrors)
		if err := inc.emit(0); err != nil {
			return nil, err
		}
		inc.sentUpTo = 1 // segment 0 is on the wire; topUp starts at segment 1
		fileList.inc = inc
		return &fileList, nil
	}

	fec.Reset()
	if err := flist.WriteFileList(fec, p, entries, uidMap, gidMap, ioErrors); err != nil {
		return nil, err
	}
	if err := st.Conn.WriteString(fec.String()); err != nil {
		return nil, err
	}

	return &fileList, nil
}

// flistParams derives the flist codec parameters for this sender from the
// negotiated session and options. NumericIDs mirrors C's numeric_ids: when
// --numeric-ids was passed (or forwarded by the client), uid/gid names are
// neither sent inline nor in the trailing id lists. Under inc-recurse names
// ride inline and there are no trailing lists at all.
func (st *Transfer) flistParams() flist.Params {
	p := flist.Params{
		ProtocolVersion:  st.Opts.ProtocolVersion(),
		PreserveUid:      st.Opts.PreserveUid(),
		PreserveGid:      st.Opts.PreserveGid(),
		PreserveLinks:    st.Opts.PreserveLinks(),
		PreserveDevices:  st.Opts.PreserveDevices(),
		PreserveSpecials: st.Opts.PreserveSpecials(),
		AlwaysChecksum:   st.Opts.AlwaysChecksum(),
		NumericIDs:       st.Opts.NumericIds(),
	}
	if st.Session != nil {
		p.VarintFlags = st.Session.VarintFlistFlags
		p.IncRecurse = st.Session.IncRecurse
		p.ID0Names = st.Session.ID0Names
		p.SafeFlist = st.Session.SafeFlist
	}
	return p
}
