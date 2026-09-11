package receiver

import (
	"math/rand/v2"
	"os"
	"path/filepath"
	"strconv"
)

// openTempFileRoot creates a randomly named file in root and returns an open
// handle. It is similar to os.CreateTemp except that the directory must be
// given, the file permissions can be controlled and patterns in the name are
// not supported.  The name is always suffixed with a random number.
func openTempFileRoot(root *SafeRoot, name string, perm os.FileMode) (string, *os.File, error) {
	prefix := name

	for attempt := 0; ; {
		// Generate a reasonably random name which is unlikely to already
		// exist. O_EXCL ensures that existing files generate an error.
		name := prefix + strconv.FormatInt(rand.Int64(), 10)

		f, err := root.OpenFile(name, os.O_RDWR|os.O_CREATE|os.O_EXCL, perm)
		if !os.IsExist(err) {
			return name, f, err
		}

		if attempt++; attempt > 10000 {
			return "", nil, &os.PathError{
				Op:   "tempfile",
				Path: name,
				Err:  os.ErrExist,
			}
		}
	}
}

type pendingFile struct {
	root    *SafeRoot
	tmpname string
	fn      string
	f       *os.File
	sync    bool
}

func newPendingFile(root *SafeRoot, fn string, sync bool) (*pendingFile, error) {
	tmpname, f, err := openTempFileRoot(root, "."+filepath.Base(fn), 0o600)
	if err != nil {
		return nil, err
	}
	return &pendingFile{
		root:    root,
		tmpname: tmpname,
		fn:      fn,
		f:       f,
		sync:    sync,
	}, nil
}

func (p *pendingFile) Name() string {
	return p.fn
}

func (p *pendingFile) Write(buf []byte) (n int, _ error) {
	return p.f.Write(buf)
}

func (p *pendingFile) CloseAtomicallyReplace() error {
	if p.sync {
		// fsync was requested
		if err := p.f.Sync(); err != nil {
			return err
		}
	}
	if err := p.f.Close(); err != nil {
		return err
	}
	return p.root.renameReplace(p.tmpname, p.fn)
}

// renameReplace moves tmpname onto fn. POSIX rename ignores the replaced
// file's own permission bits, but Windows fails with ERROR_ACCESS_DENIED
// when the destination carries the read-only attribute — which a previous
// transfer set by preserving an r-- mode. Clear it and retry once;
// setPerms restores the final mode afterwards.
func (r *SafeRoot) renameReplace(tmpname, fn string) error {
	err := r.Rename(tmpname, fn)
	if err == nil {
		return nil
	}
	if st, serr := r.Lstat(fn); serr == nil && st.Mode().IsRegular() && st.Mode().Perm()&0o222 == 0 {
		if r.Chmod(fn, 0o666) == nil {
			if retry := r.Rename(tmpname, fn); retry == nil {
				return nil
			}
		}
	}
	return err
}

func (p *pendingFile) Cleanup() error {
	err := p.f.Close()
	if err := p.root.Remove(p.tmpname); err != nil {
		return err
	}
	return err
}
