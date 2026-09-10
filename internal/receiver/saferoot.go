package receiver

import (
	"io/fs"
	"os"
	"path/filepath"
	"time"
)

// SafeRoot wraps *os.Root for the transfer destination.
//
// os.Root rejects every path containing a Windows reserved device name
// (AUX, NUL, CON, COM1… with any extension) with "path escapes from
// parent", because those names cannot be expressed through normal Win32
// path handling — but they are perfectly legal on NTFS (kernel trees are
// full of files like aux.c). The Windows build falls back to direct
// \\?\-prefixed paths for exactly those names (rootwrap_windows.go);
// every other path keeps os.Root's traversal-safe semantics.
type SafeRoot struct {
	*os.Root

	// absRoot is the absolute destination path backing the root; it feeds
	// the \\?\ fallback paths and the sub-root Path() accessor.
	absRoot string
}

func NewSafeRoot(path string) (*SafeRoot, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	root, err := os.OpenRoot(path)
	if err != nil {
		return nil, err
	}
	return &SafeRoot{Root: root, absRoot: abs}, nil
}

// Path returns the absolute destination directory.
func (r *SafeRoot) Path() string { return r.absRoot }

// OpenRoot resolves a sub-directory, keeping the SafeRoot wrapper (and its
// reserved-name fallback) active below it.
func (r *SafeRoot) OpenRoot(name string) (*SafeRoot, error) {
	if sub, abs, ok := r.fallbackRoot(name); ok {
		return &SafeRoot{Root: sub, absRoot: abs}, nil
	}
	sub, err := r.Root.OpenRoot(name)
	if err != nil {
		return nil, err
	}
	return &SafeRoot{Root: sub, absRoot: filepath.Join(r.absRoot, filepath.FromSlash(name))}, nil
}

// FS returns an fs.FS over the destination directory. Unlike os.DirFS, reads
// of reserved device-name components (see fallbackPath) go through the \\?\
// direct path — os.DirFS would open the AUX/COM device and block forever.
func (r *SafeRoot) FS() fs.FS { return safeFS{r} }

type safeFS struct{ root *SafeRoot }

func (f safeFS) join(name string) (string, error) {
	if !fs.ValidPath(name) {
		return "", fs.ErrInvalid
	}
	if abs, ok := f.root.fallbackPath(name); ok {
		return abs, nil
	}
	return filepath.Join(f.root.absRoot, filepath.FromSlash(name)), nil
}

func (f safeFS) Open(name string) (fs.File, error) {
	p, err := f.join(name)
	if err != nil {
		return nil, err
	}
	return os.Open(p)
}

func (f safeFS) Stat(name string) (fs.FileInfo, error) {
	p, err := f.join(name)
	if err != nil {
		return nil, err
	}
	return os.Stat(p)
}

func (f safeFS) ReadDir(name string) ([]os.DirEntry, error) {
	p, err := f.join(name)
	if err != nil {
		return nil, err
	}
	return os.ReadDir(p)
}

func (r *SafeRoot) Lstat(name string) (fs.FileInfo, error) {
	if abs, ok := r.fallbackPath(name); ok {
		return os.Lstat(abs)
	}
	return r.Root.Lstat(name)
}

func (r *SafeRoot) Stat(name string) (fs.FileInfo, error) {
	if abs, ok := r.fallbackPath(name); ok {
		return os.Stat(abs)
	}
	return r.Root.Stat(name)
}

func (r *SafeRoot) Open(name string) (*os.File, error) {
	if abs, ok := r.fallbackPath(name); ok {
		return os.Open(abs)
	}
	return r.Root.Open(name)
}

func (r *SafeRoot) OpenFile(name string, flag int, perm os.FileMode) (*os.File, error) {
	if abs, ok := r.fallbackPath(name); ok {
		return os.OpenFile(abs, flag, perm)
	}
	return r.Root.OpenFile(name, flag, perm)
}

func (r *SafeRoot) MkdirAll(name string, perm os.FileMode) error {
	if abs, ok := r.fallbackPath(name); ok {
		return os.MkdirAll(abs, perm)
	}
	return r.Root.MkdirAll(name, perm)
}

func (r *SafeRoot) Rename(oldname, newname string) error {
	if old, ok := r.fallbackPath(oldname); ok {
		return os.Rename(old, r.directPath(newname))
	}
	if _, ok := r.fallbackPath(newname); ok {
		return os.Rename(r.directPath(oldname), r.directPath(newname))
	}
	return r.Root.Rename(oldname, newname)
}

func (r *SafeRoot) Remove(name string) error {
	if abs, ok := r.fallbackPath(name); ok {
		return os.Remove(abs)
	}
	return r.Root.Remove(name)
}

func (r *SafeRoot) RemoveAll(name string) error {
	if abs, ok := r.fallbackPath(name); ok {
		return os.RemoveAll(abs)
	}
	return r.Root.RemoveAll(name)
}

func (r *SafeRoot) Chmod(name string, mode os.FileMode) error {
	if abs, ok := r.fallbackPath(name); ok {
		return os.Chmod(abs, mode)
	}
	return r.Root.Chmod(name, mode)
}

func (r *SafeRoot) Chtimes(name string, atime, mtime time.Time) error {
	if abs, ok := r.fallbackPath(name); ok {
		return os.Chtimes(abs, atime, mtime)
	}
	return r.Root.Chtimes(name, atime, mtime)
}

func (r *SafeRoot) Readlink(name string) (string, error) {
	if abs, ok := r.fallbackPath(name); ok {
		return os.Readlink(abs)
	}
	return r.Root.Readlink(name)
}

func (r *SafeRoot) Symlink(oldname, newname string) error {
	if abs, ok := r.fallbackPath(newname); ok {
		return os.Symlink(oldname, abs)
	}
	return r.Root.Symlink(oldname, newname)
}
