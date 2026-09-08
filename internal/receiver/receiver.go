package receiver

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/gokrazy/rsync"
	"github.com/gokrazy/rsync/internal/protocol"
	"github.com/gokrazy/rsync/internal/rsyncopts"
	"github.com/mmcloughlin/md4"
)

// rsync/receiver.c:recv_files
func (rt *Transfer) RecvFiles(fileList []*File) error {
	// rsync/receiver.c:recv_files: max_phase is 2 with protocol >= 29, so
	// the receiver reads three phase-done markers in total (two phase
	// transitions and the final marker).
	phase := 0
	maxPhase := 1
	if protocol.SupportsMultiPhase(rt.ProtocolVersion()) {
		maxPhase = 2
	}
	for {
		idx, err := rt.readNdx()
		if err != nil {
			return err
		}
		if idx == -1 {
			phase++
			if rt.Opts.DebugGTE(rsyncopts.DEBUG_RECV, 1) {
				rt.Logger.Printf("recvFiles phase=%d", phase)
			}
			if phase > maxPhase {
				break
			}
			continue
		}
		if idx < 0 {
			return fmt.Errorf("invalid file index %d (list has %d entries)", idx, len(fileList))
		}

		// rsync/rsync.c:read_ndx_and_attrs: with protocol >= 29, the file
		// index is followed by the itemize iflags shortint, optionally the
		// basis type byte and the xname vstring.
		iflags := uint16(rsync.ITEM_TRANSFER)
		fnamecmpType := byte(rsync.FNAMECMP_FNAME)
		if protocol.SupportsIFlags(rt.ProtocolVersion()) {
			var err error
			iflags, err = rt.Conn.ReadShortint()
			if err != nil {
				return err
			}
			// rsync/rsync.c:read_ndx_and_attrs: support the protocol-29
			// keep-alive style (index == list length, iflags == ITEM_IS_NEW).
			if rt.ProtocolVersion() < 30 &&
				int(idx) == len(fileList) &&
				iflags == rsync.ITEM_IS_NEW {
				continue
			}
			if iflags&rsync.ITEM_BASIS_TYPE_FOLLOWS != 0 {
				fnamecmpType, err = rt.Conn.ReadByte()
				if err != nil {
					return err
				}
			}
			if iflags&rsync.ITEM_XNAME_FOLLOWS != 0 {
				if _, err := rt.Conn.ReadVString(); err != nil {
					return err
				}
			}
		}
		if int(idx) >= len(fileList) {
			return fmt.Errorf("invalid file index %d (list has %d entries)", idx, len(fileList))
		}
		if iflags&rsync.ITEM_TRANSFER == 0 {
			// The sender echoes itemize messages for entries that do not
			// carry data (e.g. attribute-only updates). Nothing to receive
			// for them.
			if rt.Opts.DebugGTE(rsyncopts.DEBUG_RECV, 1) {
				rt.Logger.Printf("recvFiles idx=%d iflags=0x%x (no transfer)", idx, iflags)
			}
			continue
		}

		if rt.Opts.DebugGTE(rsyncopts.DEBUG_RECV, 1) {
			rt.Logger.Printf("receiving file idx=%d iflags=0x%x fnamecmpType=0x%x: %+v",
				idx, iflags, fnamecmpType, fileList[idx])
		}
		if rt.Opts.Progress {
			fmt.Fprintln(rt.Env.Stdout, fileList[idx].Name)
		}
		if err := rt.recvFile1(fileList[idx]); err != nil {
			return err
		}
	}
	if rt.Opts.DebugGTE(rsyncopts.DEBUG_RECV, 1) {
		rt.Logger.Printf("recvFiles finished")
	}
	return nil
}

func (rt *Transfer) recvFile1(f *File) error {
	if rt.Opts.DryRun {
		if !rt.Opts.Server {
			fmt.Fprintln(rt.Env.Stdout, f.Name)
		}
		return nil
	}

	localFile, err := rt.openLocalFile(f)
	if err != nil && !os.IsNotExist(err) {
		rt.Logger.Printf("opening local file failed, continuing: %v", err)
	}
	defer localFile.Close()
	if err := rt.receiveData(f, localFile); err != nil {
		return err
	}
	return nil
}

func (rt *Transfer) openLocalFile(f *File) (*os.File, error) {
	name := f.Name
	in, err := rt.DestRoot.Open(name)
	if err != nil && rt.Opts.KeepPartial && os.IsNotExist(err) {
		// Final absent: fall back to the retained partial as the delta basis.
		name = f.Name + partialSuffix
		in, err = rt.DestRoot.Open(name)
	}
	if err != nil {
		return nil, err
	}
	usedPartial := name != f.Name

	st, err := in.Stat()
	if err != nil {
		return nil, err
	}

	if st.IsDir() {
		return nil, fmt.Errorf("%s is a directory", filepath.Join(rt.Dest, name))
	}

	if !st.Mode().IsRegular() {
		return nil, nil
	}

	if !rt.Opts.PreservePerms && !usedPartial {
		// If the file exists already and we are not preserving permissions,
		// then act as though the remote sent us the existing permissions.
		// Skipped for a partial basis, whose temp perms aren't the target's.
		f.Mode = int32(st.Mode().Perm())
	}

	return in, nil
}

// rsync/receiver.c:receive_data
func (rt *Transfer) receiveData(f *File, localFile *os.File) error {
	rt.Progress.Reset(uint64(f.Length))
	var sh rsync.SumHead
	if err := sh.ReadFrom(rt.Conn); err != nil {
		return err
	}

	if rt.Opts.DebugGTE(rsyncopts.DEBUG_DELTASUM, 1) {
		rt.Logger.Printf("creating %s", filepath.Join(rt.Dest, f.Name))
	}

	// Default path: renameio (atomic temp+rename, discard on failure). With
	// KeepPartial an interrupt instead retains "<name>.partial" to resume from.
	var w io.Writer
	var commit func() error
	var abort func()
	// discardPartial drops the bytes on verification failure instead of keeping
	// a corrupt partial as a future basis.
	discardPartial := false

	if rt.Opts.KeepPartial {
		tmpName := f.Name + partialSuffix + ".tmp"
		partialName := f.Name + partialSuffix
		tmp, err := rt.DestRoot.OpenFile(tmpName, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
		if err != nil {
			return err
		}
		w = tmp
		commit = func() error {
			if rt.Opts.DoFsync {
				if err := tmp.Sync(); err != nil {
					return err
				}
			}
			if err := tmp.Close(); err != nil {
				return err
			}
			if err := rt.DestRoot.Rename(tmpName, f.Name); err != nil {
				return err
			}
			_ = rt.DestRoot.Remove(partialName)
			return nil
		}
		abort = func() {
			_ = tmp.Close()
			if discardPartial {
				_ = rt.DestRoot.Remove(tmpName)
				_ = rt.DestRoot.Remove(partialName)
				return
			}
			rt.retainPartial(tmpName, partialName)
		}
	} else {
		out, err := newPendingFile(rt.DestRoot, f.Name, rt.Opts.DoFsync)
		if err != nil {
			return err
		}
		w = out
		commit = out.CloseAtomicallyReplace
		abort = func() { _ = out.Cleanup() }
	}

	committed := false
	defer func() {
		if !committed {
			abort()
		}
	}()

	// The whole-file checksum mirrors C's sum_init (checksum.c): at protocol < 30
	// the negotiated/implicit "md4" is CSUM_MD4_OLD, which folds the checksum
	// seed (4 bytes) into the digest; at protocol >= 30 the negotiated modern
	// CSUM_MD4 does NOT. The seed feed must be gated on the protocol version so
	// the sender, receiver and C counterpart all compute the same digest.
	h := md4.New()
	if rt.ProtocolVersion() < 30 {
		binary.Write(h, binary.LittleEndian, rt.Seed)
	}

	wr := io.MultiWriter(w, h)

	offset := 0
	for {
		token, data, err := rt.recvToken()
		if err != nil {
			return err
		}
		if token == 0 {
			break
		}
		if rt.Opts.Progress && !rt.Opts.Server {
			rt.Progress.MaybeShow(uint64(offset), false)
			if offset == 0 {
				defer func() {
					rt.Progress.MaybeShow(uint64(offset), true)
				}()
			}
		}
		if token > 0 {
			n, err := wr.Write(data)
			if err != nil {
				return err
			}
			offset += n
			continue
		}
		if localFile == nil {
			return fmt.Errorf("BUG: local file %s not open for copying chunk", f.Name)
		}
		token = -(token + 1)
		offset2 := int64(token) * int64(sh.BlockLength)
		dataLen := sh.BlockLength
		if token == sh.ChecksumCount-1 && sh.RemainderLength != 0 {
			dataLen = sh.RemainderLength
		}
		data = make([]byte, dataLen)
		if _, err := localFile.ReadAt(data, offset2); err != nil {
			return err
		}

		n, err := wr.Write(data)
		if err != nil {
			return err
		}
		offset += n
	}
	localSum := h.Sum(nil)
	remoteSum := make([]byte, len(localSum))
	if _, err := io.ReadFull(rt.Conn.Reader, remoteSum); err != nil {
		return err
	}
	if !bytes.Equal(localSum, remoteSum) {
		discardPartial = true
		return fmt.Errorf("file corruption in %s", f.Name)
	}
	if rt.Opts.DebugGTE(rsyncopts.DEBUG_DELTASUM, 1) {
		rt.Logger.Printf("checksum %x matches!", localSum)
	}

	if localFile != nil {
		// Close the file earlier than the calling function’s deferred Close(),
		// so that we can rename files on Windows, which fails as long
		// as there are any open file handles.
		localFile.Close()
	}

	if err := commit(); err != nil {
		return err
	}
	committed = true

	if err := rt.setPerms(f, fs.FileMode(f.Mode)); err != nil {
		return err
	}

	return nil
}
